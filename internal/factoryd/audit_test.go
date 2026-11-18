package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// auditVersion is testPolicyVersion with big as the coder
// class's audit model and sf/repo's project switching the audit on.
func auditVersion(on bool) policy.Version {
	v := testPolicyVersion()
	coder := v.Document.Classes["coder"]
	coder.AuditModel = "big"
	v.Document.Classes["coder"] = coder
	v.Projects["repo"] = policy.ProjectConfig{Repository: "sf/repo", MergeRule: policy.MergeHuman, Objects: policy.ObjectsFactory, Agents: policy.AgentsCoder, Audit: on}
	return v
}

func newAuditRunner(t *testing.T, fj *gateForgejo, ll *fakeLiteLLM, on bool) (*Runner, *fakeObjectStore) {
	t.Helper()
	r, _, objects := newGateRunner(t, gateTask, fj)
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	v := auditVersion(on)
	r.cfg.Policy = func() policy.Version { return v }
	return r, objects
}

const secretTestDiff = "diff --git a/internal/x/x_test.go b/internal/x/x_test.go\n" +
	"--- a/internal/x/x_test.go\n" +
	"+++ b/internal/x/x_test.go\n" +
	"@@ -3,2 +3,4 @@ import \"testing\"\n" +
	" func TestX(t *testing.T) {\n" +
	"+\tpassword = \"hunter2hunter2\"\n" +
	"+\t_ = password\n" +
	" }\n"

// Checklist item 4 names configuration files by path and leaves source code
// that merely names policy or workflow types to the model review (#259's
// audit flagged internal/policy/policy.go and internal/factoryd/workflow.go).
func TestAuditPromptItemFourIsConfigurationFiles(t *testing.T) {
	var item string
	for _, line := range strings.Split(auditPrompt, "\n") {
		if strings.HasPrefix(line, "4. ") {
			item = line
		}
	}
	for _, want := range []string{"factory/**", ".forgejo/**", ".github/**", "CODEOWNERS", "opencode.json", "AGENTS.md",
		"Source code is not this item"} {
		if !strings.Contains(item, want) {
			t.Errorf("item 4 lacks %q: %q", want, item)
		}
	}
}

func TestParseAudit(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want []AuditFinding
		none string // the no-verdict finding's why, when the answer gives none
	}{
		"clear":  {`{"verdict":"clear","findings":[]}`, nil, ""},
		"fenced": {"```json\n{\"verdict\":\"clear\"}\n```", nil, ""},
		"findings": {`{"verdict":"findings","findings":[{"item":2,"file":"k8s/egress.yaml","line":12,"why":"allows api.example.org"}]}`,
			[]AuditFinding{{Item: "network egress", File: "k8s/egress.yaml", Line: 12, Why: "allows api.example.org"}}, ""},
		"clear with findings listed": {`{"verdict":"clear","findings":[{"item":3,"file":"k8s/rbac.yaml","line":3,"why":"a new ClusterRole"}]}`,
			[]AuditFinding{{Item: "RBAC and ServiceAccounts", File: "k8s/rbac.yaml", Line: 3, Why: "a new ClusterRole"}}, ""},
		"not json":             {"all good", nil, "the answer is not one JSON object"},
		"empty":                {"", nil, "the answer is not one JSON object"},
		"unknown verdict":      {`{"verdict":"pass"}`, nil, `unknown verdict "pass"`},
		"findings without any": {`{"verdict":"findings","findings":[]}`, nil, "verdict findings names none"},
		"extra field":          {`{"verdict":"clear","summary":"nothing touched"}`, nil, ""},
		"negative line": {`{"verdict":"findings","findings":[{"item":3,"file":"k8s/rbac.yaml","line":-3,"why":"a new Role"}]}`,
			[]AuditFinding{{Item: "RBAC and ServiceAccounts", File: "k8s/rbac.yaml", Why: "a new Role"}}, ""},
		"trailing text": {`{"verdict":"clear"} ok`, nil, "the answer is not one JSON object"},
	} {
		t.Run(name, func(t *testing.T) {
			got, why := parseAudit(tc.in)
			if why != "" || tc.none != "" {
				if tc.none == "" || !strings.Contains(why, tc.none) || got != nil {
					t.Fatalf("parseAudit = %+v, %q, want no verdict %q", got, why, tc.none)
				}
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseAudit = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The model answers an item by its checklist number, and the finding holds
// the item's name.
func TestParseAuditItemIsChecklistNumber(t *testing.T) {
	for n, want := range map[int]string{1: "secrets", 3: "RBAC and ServiceAccounts", 6: "sandbox contract"} {
		got, why := parseAudit(fmt.Sprintf(`{"verdict":"findings","findings":[{"item":%d,"file":"a.go","line":5,"why":"x"}]}`, n))
		if why != "" || !slices.Equal(got, []AuditFinding{{Item: want, File: "a.go", Line: 5, Why: "x"}}) {
			t.Errorf("item %d: parseAudit = %+v, %q, want one finding of %q", n, got, why, want)
		}
	}
}

// An item that is no checklist number makes the whole answer one without a
// verdict, its reason naming the item.
func TestParseAuditRefusesAnItemOutOfRange(t *testing.T) {
	for item, want := range map[string]string{
		`0`:                             "the answer named checklist item 0, which is not a number from 1 to 6",
		`7`:                             "the answer named checklist item 7, which is not a number from 1 to 6",
		`"3. RBAC and ServiceAccounts"`: `the answer named checklist item "3. RBAC and ServiceAccounts", which is not a number from 1 to 6`,
	} {
		got, why := parseAudit(`{"verdict":"findings","findings":[{"item":1,"file":"a.go","why":"x"},{"item":` + item + `,"file":"b.go","why":"y"}]}`)
		if got != nil || why != want {
			t.Errorf("item %s: parseAudit = %+v, %q, want no findings and %q", item, got, why, want)
		}
	}
}

// A finding at no file stays, at no file, beside the answer's other
// findings: no confirmation covers it.
func TestParseAuditKeepsAFindingAtNoFile(t *testing.T) {
	for file, line := range map[string]int{`"file":"","line":3,`: 3, `"file":"  ",`: 0, ``: 0} {
		got, why := parseAudit(`{"verdict":"findings","findings":[{"item":1,"file":"a.go","why":"x"},{"item":2,` + file + `"why":"y"}]}`)
		if want := []AuditFinding{{Item: "secrets", File: "a.go", Why: "x"}, {Item: "network egress", Line: line, Why: "y"}}; !slices.Equal(got, want) || why != "" {
			t.Errorf("%s: parseAudit = %+v, %q, want %+v", file, got, why, want)
		}
	}
}

// A finding's text is its item at its file, never its line.
func TestAuditFindingTextHasNoLine(t *testing.T) {
	for _, tc := range []struct {
		f    AuditFinding
		want string
	}{
		{AuditFinding{Item: "secrets", File: "a.go", Line: 5}, "secrets at a.go"},
		{AuditFinding{Item: "secrets", File: "a.go"}, "secrets at a.go"},
		{AuditFinding{Item: "secrets"}, "secrets"},
	} {
		if got := tc.f.Text(); got != tc.want {
			t.Errorf("%+v.Text() = %q, want %q", tc.f, got, tc.want)
		}
	}
}

// The card's own case: a confirmation of the secrets in image_look_test.go
// covers them at the line the next run cites, and covers no other file,
// not even one whose path its file starts.
func TestConfirmationSurvivesAnotherRun(t *testing.T) {
	confirmed := map[string]Adjudication{"secrets at internal/factoryd/image_look_test.go": {By: "sebastian", Why: "the rig's fake"}}
	for _, tc := range []struct {
		f    AuditFinding
		want bool
	}{
		{AuditFinding{Item: "secrets", File: "internal/factoryd/image_look_test.go", Line: 154}, true},
		{AuditFinding{Item: "secrets", File: "internal/factoryd/image_look_test.go", Line: 155}, true},
		{AuditFinding{Item: "secrets", File: "internal/factoryd/ci.go"}, false},
		{AuditFinding{Item: "secrets", File: "internal/factoryd/image_look_test.go.orig", Line: 154}, false},
		{AuditFinding{Item: "network egress", File: "internal/factoryd/image_look_test.go", Line: 154}, false},
	} {
		if got := auditConfirmedOf(tc.f, confirmed); got != tc.want {
			t.Errorf("auditConfirmedOf(%+v) = %t, want %t", tc.f, got, tc.want)
		}
	}
	// a finding at no file is never covered, though its text was recorded
	confirmed = map[string]Adjudication{"secrets": {By: "sebastian"}, "... and 3 more": {By: "sebastian"}, auditNoVerdict: {By: "sebastian"}}
	for _, f := range []AuditFinding{{Item: "secrets"}, {Item: "... and 3 more"}, {Item: auditNoVerdict}} {
		if got := auditConfirmedOf(f, confirmed); got {
			t.Errorf("auditConfirmedOf(%+v) = true, want false: it is at no file", f)
		}
	}
}

// The prompt's checklist is the table's: one numbered line for each item.
func TestChecklistIsBuiltFromTheTable(t *testing.T) {
	numbered := 0
	for _, line := range strings.Split(auditChecklist, "\n") {
		if len(line) > 1 && line[0] >= '1' && line[0] <= '9' && line[1] == '.' {
			numbered++
		}
	}
	if !strings.Contains(auditChecklist, "\n3. RBAC and ServiceAccounts: ") || numbered != len(auditItems) {
		t.Fatalf("the checklist has %d numbered lines, want %d:\n%s", numbered, len(auditItems), auditChecklist)
	}
}

// A planted password in a test file: the pre-check finds it at its file
// and line without the value, the model's findings join, and the call is
// the coder class's audit model on the task's account with purpose audit.
// The timeline records the audit with its verdict, the deployment and
// its start; no comment reaches the pull request.
func TestGateAuditFindsAPlantedSecret(t *testing.T) {
	fj := &gateForgejo{diff: secretTestDiff}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":1,"file":"internal/x/x_test.go","line":4,"why":"a password literal and a token ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789 in a test"}]}`,
		deployment: "zai/glm-5.3"}
	r, objects := newAuditRunner(t, fj, ll, true)
	from := time.Now()
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{
		{Item: "secret in the diff: " + "password, secret or token assigned a literal", File: "internal/x/x_test.go", Line: 4,
			Why: "an added line has the shape of a password, secret or token assigned a literal (pre-check)"},
		{Item: "secrets", File: "internal/x/x_test.go", Line: 4, Why: "a password literal and a token [redacted] in a test"},
	}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || res.Model != "big" || res.Deployment != "zai/glm-5.3" {
		t.Fatalf("audit = %+v", res)
	}
	if len(ll.chats) != 1 || ll.chats[0]["model"] != "big" || ll.purposes[0] != "audit" || ll.tasks[0] != "t1" {
		t.Fatalf("chats %v, purposes %v", ll.chats, ll.purposes)
	}
	msgs, _ := ll.chats[0]["messages"].([]any)
	system, _ := msgs[0].(map[string]any)["content"].(string)
	for _, item := range []string{"secrets", "network egress", "RBAC", "policy and workflow", "migrations", "sandbox contract"} {
		if !strings.Contains(system, item) {
			t.Fatalf("the prompt lacks the checklist item %q", item)
		}
	}
	if len(fj.comments) != 0 {
		t.Fatalf("comments = %q", fj.comments)
	}
	step := timelineStep(t, objects, "t1", "audit")
	if step["verdict"] != AuditFindings || step["deployment"] != "zai/glm-5.3" || step["model"] != "big" {
		t.Fatalf("audit step = %v", step)
	}
	if tl := timeline(t, objects); strings.Contains(tl, "hunter2") || strings.Contains(tl, "ghp_") {
		t.Fatal("the secret's value is on the timeline")
	}
	startedWithin(t, objects, "audit", from)
}

// A person's confirmations (audit_finding_confirmed steps on the
// timeline, who, why and when each) settle the audit's verdict past the
// findings they cover: the two findings here, the pre-check hit and the
// model's, stand covered and the verdict is clear, the confirmations
// beside it on the step.
func TestGateAuditSettlesPastAPersonsConfirmations(t *testing.T) {
	fj := &gateForgejo{diff: secretTestDiff}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":1,"file":"internal/x/x_test.go","line":4,"why":"a password literal in a test"}]}`}
	r, objects := newAuditRunner(t, fj, ll, true)
	ctx := context.Background()
	from := time.Now()
	for _, c := range []struct{ finding, why string }{
		{"secret in the diff: password, secret or token assigned a literal at internal/x/x_test.go", "the rig's fake, not a real credential"},
		{"secrets at internal/x/x_test.go", "test rig fixture"},
	} {
		if err := r.timeline.Append(ctx, "t1", stepAuditConfirmed, map[string]any{"finding": c.finding, "by": "sebastian", "why": c.why}); err != nil {
			t.Fatal(err)
		}
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if res.Verdict != AuditClear || len(res.Findings) != 0 {
		t.Fatalf("audit = %+v, want a clear verdict past the two confirmations", res)
	}
	if len(res.Confirmed) != 2 {
		t.Fatalf("confirmed = %+v, want both confirmations", res.Confirmed)
	}
	adj := res.Confirmed["secrets at internal/x/x_test.go"]
	if adj.By != "sebastian" || adj.Why != "test rig fixture" || adj.When.IsZero() || adj.When.Before(from) {
		t.Fatalf("the model finding's confirmation = %+v, want sebastian's, recorded with the step's time since %s", adj, from)
	}
	step := timelineStep(t, objects, "t1", "audit")
	if step["verdict"] != AuditClear {
		t.Fatalf("audit step = %v, want the settled clear verdict", step)
	}
	confirmed, _ := step["confirmed"].(map[string]any)
	by, _ := confirmed["secrets at internal/x/x_test.go"].(map[string]any)
	if by["by"] != "sebastian" || by["why"] != "test rig fixture" {
		t.Fatalf("the step's confirmations = %v, want sebastian's beside the verdict", confirmed)
	}
}

// A confirmation that covers one finding of two leaves the other to hold
// the verdict: the fixtures' clearance clears the fixtures alone.
func TestGateAuditKeepsAFindingItsConfirmationDoesNotCover(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/internal/x/x_test.go b/internal/x/x_test.go\n--- a/internal/x/x_test.go\n+++ b/internal/x/x_test.go\n@@ -1 +1,2 @@\n package x\n+var rig = 1\n" +
		"diff --git a/internal/y/y.go b/internal/y/y.go\n--- a/internal/y/y.go\n+++ b/internal/y/y.go\n@@ -1 +1,2 @@\n package y\n+var key = 1\n"}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[` +
		`{"item":1,"file":"internal/x/x_test.go","line":4,"why":"a rig literal"},` +
		`{"item":1,"file":"internal/y/y.go","line":9,"why":"a real credential"}]}`}
	r, _ := newAuditRunner(t, fj, ll, true)
	at := time.Now().UTC().Round(time.Second)
	if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed,
		map[string]any{"finding": "secrets at internal/x/x_test.go", "by": "sebastian", "why": "test rig fixture", "time": at}); err != nil {
		t.Fatal(err)
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "secrets", File: "internal/y/y.go", Line: 9, Why: "a real credential"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) {
		t.Fatalf("audit = %+v, want the uncovered finding %+v holding the verdict", res, want)
	}
	if len(res.Confirmed) != 1 || res.Confirmed["secrets at internal/x/x_test.go"].By != "sebastian" {
		t.Fatalf("confirmed = %+v, want the one confirmation beside the verdict", res.Confirmed)
	}
}

// A confirmation covers its own finding alone: one of the finding at line
// 4 leaves the finding at line 41, whose text it begins, to hold the
// verdict.
func TestGateAuditConfirmationCoversItsFindingAlone(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[` +
		`{"item":"secrets","file":"internal/x/x_test.go","line":4,"why":"a rig literal"},` +
		`{"item":"secrets","file":"internal/x/x_test.go","line":41,"why":"a real credential"}]}`}
	r, _ := newAuditRunner(t, fj, ll, true)
	if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed,
		map[string]any{"finding": "secrets at internal/x/x_test.go:4", "by": "sebastian", "why": "test rig fixture"}); err != nil {
		t.Fatal(err)
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "secrets", File: "internal/x/x_test.go", Line: 41, Why: "a real credential"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) {
		t.Fatalf("audit = %+v, want the finding at line 41 holding the verdict", res)
	}
}

// A confirmation's cover leaves a trace on the audit step of the head it
// covered on: covered lists the finding as the audit raised it at c1, its
// text keying the confirmation the step carries beside it, while the
// finding it does not cover holds the verdict.
func TestGateAuditRecordsWhatAConfirmationCovered(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/internal/x/x_test.go b/internal/x/x_test.go\n--- a/internal/x/x_test.go\n+++ b/internal/x/x_test.go\n@@ -1 +1,2 @@\n package x\n+var rig = 1\n" +
		"diff --git a/internal/y/y.go b/internal/y/y.go\n--- a/internal/y/y.go\n+++ b/internal/y/y.go\n@@ -1 +1,2 @@\n package y\n+var key = 1\n"}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[` +
		`{"item":1,"file":"internal/x/x_test.go","line":4,"why":"a rig literal"},` +
		`{"item":1,"file":"internal/y/y.go","line":9,"why":"a real credential"}]}`}
	r, objects := newAuditRunner(t, fj, ll, true)
	if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed,
		map[string]any{"finding": "secrets at internal/x/x_test.go", "by": "sebastian", "why": "test rig fixture"}); err != nil {
		t.Fatal(err)
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	covered := []AuditFinding{{Item: "secrets", File: "internal/x/x_test.go", Line: 4, Why: "a rig literal"}}
	if !slices.Equal(res.Covered, covered) {
		t.Fatalf("covered = %+v, want %+v", res.Covered, covered)
	}
	step := timelineStep(t, objects, "t1", "audit")
	raw, _ := json.Marshal(step["covered"])
	if want := `[{"file":"internal/x/x_test.go","item":"secrets","line":4,"why":"a rig literal"}]`; step["commit"] != "c1" || string(raw) != want {
		t.Fatalf("the audit step's commit %v, covered %s; want c1 and %s", step["commit"], raw, want)
	}
	confirmed, _ := step["confirmed"].(map[string]any)
	if by, _ := confirmed["secrets at internal/x/x_test.go"].(map[string]any); by["by"] != "sebastian" {
		t.Fatalf("the step's confirmations = %v, want sebastian's beside what it covered", confirmed)
	}
}

// A finding that names no verdict is no person's to confirm: the
// audit that did not run still holds the verdict, whatever stands
// confirmed.
func TestGateAuditNeverSettlesItsNoVerdictFinding(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
	r, _ := newAuditRunner(t, fj, &fakeLiteLLM{content: "LGTM"}, true)
	if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed,
		map[string]any{"finding": "the audit gave no verdict", "by": "sebastian", "why": "it was fine last time", "time": time.Now().UTC().Round(time.Second)}); err != nil {
		t.Fatal(err)
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if res.Verdict != AuditFindings || len(res.Findings) != 1 || res.Findings[0].Item != auditNoVerdict {
		t.Fatalf("audit = %+v, want the no-verdict finding holding the verdict", res)
	}
}

// No confirmation on the timeline, the audit holds as before: both
// findings stand.
func TestGateAuditWithoutAConfirmationHoldsAsBefore(t *testing.T) {
	fj := &gateForgejo{diff: secretTestDiff}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":1,"file":"internal/x/x_test.go","line":4,"why":"a password literal in a test"}]}`}
	r, _ := newAuditRunner(t, fj, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if res.Verdict != AuditFindings || len(res.Findings) != 2 {
		t.Fatalf("audit = %+v, want both findings holding the verdict as before", res)
	}
}

// An audit beside the checks wait leaves the task's row in gate checks,
// which the wait holds; its step starts at its own schedule.
func TestGateAuditBesideTheChecksStartsAtItsSchedule(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
	r, objects := newAuditRunner(t, fj, &fakeLiteLLM{content: `{"verdict":"clear","findings":[]}`}, true)
	if err := r.store.SetGate(context.Background(), "t1", GateChecks, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	from := time.Now()
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{Beside: true})
	if res := getInto[AuditResult](t, val, err); res.Verdict != AuditClear {
		t.Fatalf("audit = %+v", res)
	}
	if got, _, err := r.store.Task(context.Background(), "t1"); err != nil || got.Gate != GateChecks {
		t.Fatalf("task in gate %q (%v), want checks: the audit beside the wait opens none", got.Gate, err)
	}
	startedWithin(t, objects, "audit", from)
}

func TestGateAuditClear(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
	ll := &fakeLiteLLM{content: `{"verdict":"clear","findings":[]}`}
	r, _ := newAuditRunner(t, fj, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if res := getInto[AuditResult](t, val, err); res.Verdict != AuditClear || len(res.Findings) != 0 {
		t.Fatalf("audit = %+v", res)
	}
}

// No verdict is never a pass: an unparseable answer, a gateway refusal, a
// failed call and a policy without an audit model are each a finding.
func TestGateAuditWithoutAVerdictIsAFinding(t *testing.T) {
	for name, tc := range map[string]struct {
		ll    *fakeLiteLLM
		model bool
		why   string
	}{
		"unparseable":    {&fakeLiteLLM{content: "LGTM"}, true, "the answer is not one JSON object"},
		"empty":          {&fakeLiteLLM{content: ""}, true, "the answer is not one JSON object"},
		"refused_budget": {&fakeLiteLLM{status: http.StatusPaymentRequired, decision: "refused_budget"}, true, "refused by the gateway (refused_budget)"},
		"upstream error": {&fakeLiteLLM{status: http.StatusBadGateway}, true, "status 502"},
		"no audit model": {&fakeLiteLLM{content: "{}"}, false, "the policy names no audit model"},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newAuditRunner(t, &gateForgejo{diff: "d"}, tc.ll, true)
			if !tc.model {
				v := testPolicyVersion()
				v.Projects["repo"] = policy.ProjectConfig{Repository: "sf/repo", Audit: true}
				r.cfg.Policy = func() policy.Version { return v }
			}
			val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
			res := getInto[AuditResult](t, val, err)
			if res.Verdict != AuditFindings || len(res.Findings) != 1 || res.Findings[0].Item != auditNoVerdict || !strings.Contains(res.Findings[0].Why, tc.why) {
				t.Fatalf("audit = %+v, want the no-verdict finding with %q", res, tc.why)
			}
		})
	}
}

// A diff past the 8 MiB bound: the audit makes no call and gives one
// finding that says why.
func TestGateAuditOfADiffPastTheBoundIsOneFinding(t *testing.T) {
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: strings.Repeat("d", 8<<20+1)}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: auditNoVerdict, Why: "the audit did not run: the diff is longer than 8 MiB, the most factoryd reads: review the change yourself, or replan the card"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 0 {
		t.Fatalf("audit = %+v after %d calls, want %+v and no call", res, len(ll.chats), want)
	}
}

// An audit the card cannot pay for is not started: four calls of 9,000
// output tokens at 1e-5 EUR cost 0.36 EUR with no call on the ref to read,
// past the 0.30 EUR free. One finding says so; no call is made.
func TestGateAuditTheCardCannotPayForIsOneFinding(t *testing.T) {
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	fj := &gateForgejo{diff: fourParts}
	task := gateTask
	task.BudgetEUR = 0.30
	r, _, _ := newGateRunner(t, task, fj)
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	v := auditVersion(true)
	r.cfg.Policy = func() policy.Version { return v }
	r.cfg.Prices = stubPrices{price: policy.Price{In: 1e-9, Out: 1e-5}}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: auditNoVerdict, Why: "the audit did not run: its 4 calls would cost about 0.36 EUR, and the card has 0.30 EUR free. " +
		"Read the diff, then Approve the merge or Deny it (Close on a pull request factoryd opened)."}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 0 {
		t.Fatalf("audit = %+v after %d calls, want %+v and no call", res, len(ll.chats), want)
	}
}

// thirtyParts is thirty files of 40 KiB: thirty parts of the audit's 48 KiB.
func thirtyParts() string {
	var b strings.Builder
	for i := range 30 {
		b.WriteString(diffFile(fmt.Sprintf("f%02d.go", i), 40<<10))
	}
	return b.String()
}

// A budget refused for every call: the retry gives all thirty parts the
// one refusal, which is one finding naming them.
func TestGateAuditOfThirtyRefusedPartsIsOneFinding(t *testing.T) {
	ll := &fakeLiteLLM{status: http.StatusTooManyRequests, decision: policy.RefusedBudget}
	r, _ := newAuditRunner(t, &gateForgejo{diff: thirtyParts()}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: auditNoVerdict, Why: "parts 1 to 30 of 30: the audit call was refused by the gateway (refused_budget): task headroom 0.000000 EUR"}}
	if !slices.Equal(res.Findings, want) || len(ll.chats) > reviewParallel+1 {
		t.Fatalf("audit = %+v after %d calls, want %+v", res, len(ll.chats), want)
	}
}

// Five calls the upstream answers 429, each with its own body: one
// finding, keyed on the status, names the five parts.
func TestGateAuditOfFailedPartsCollapsesByStatus(t *testing.T) {
	var n atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"message":"rate limited, try again (%d)"}}`, n.Add(1))
	}))
	defer gw.Close()
	r, _ := newAuditRunner(t, &gateForgejo{diff: fourParts + diffFile("e.go", 40<<10)}, &fakeLiteLLM{}, true)
	r.supervisor = testSupervisorModel(gw.URL)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: auditNoVerdict, Why: "parts 1 to 5 of 5: the audit call failed: status 429"}}
	if !slices.Equal(res.Findings, want) || n.Load() != 5 {
		t.Fatalf("audit = %+v after %d calls, want %+v", res, n.Load(), want)
	}
}

// Six parts of the same ten findings, network egress at f00.go: one text,
// so one finding, its why counting the other 59, as one confirmation
// covers them all.
func TestGateAuditCountsThePartsFindingsByText(t *testing.T) {
	var ten []string
	for i := range 10 {
		ten = append(ten, fmt.Sprintf(`{"item":2,"file":"f00.go","line":%d,"why":"a new host"}`, i+1))
	}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[` + strings.Join(ten, ",") + `]}`}
	diff := ""
	for i := range 6 {
		diff += diffFile(fmt.Sprintf("f%02d.go", i), 40<<10)
	}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "network egress", File: "f00.go", Line: 1, Why: "a new host; 59 more at this file"}}
	if !slices.Equal(res.Findings, want) || len(ll.chats) != 6 {
		t.Fatalf("findings = %+v after %d calls, want %+v", res.Findings, len(ll.chats), want)
	}
}

// The model may list as many findings as the card does, so no cut is
// silent: 25 findings are all listed, and clear once a human confirmed the
// 25. An answer of 50, the most one answer lists, may have left findings
// out, and one more finding at no file says so: confirming the 50 leaves
// the verdict at findings. 60 keep the first 50 and count the other 10.
func TestGateAuditNeverClearsPastWhatTheModelListed(t *testing.T) {
	diff := ""
	for f := range 10 {
		diff += diffFile(fmt.Sprintf("x%d.go", f), 200)
	}
	answer := func(items, files int) (string, []AuditFinding) {
		var entries []string
		var listed []AuditFinding
		for i := range items {
			for f := range files {
				entries = append(entries, fmt.Sprintf(`{"item":%d,"file":"x%d.go","line":1,"why":"w"}`, i+1, f))
				listed = append(listed, AuditFinding{Item: auditItems[i].Name, File: fmt.Sprintf("x%d.go", f), Line: 1, Why: "w"})
			}
		}
		return `{"verdict":"findings","findings":[` + strings.Join(entries, ",") + `]}`, listed
	}
	full := func(n int) AuditFinding {
		return AuditFinding{Item: "the audit model listed its maximum; more may stand", Why: fmt.Sprintf("the answer listed %d findings, and one answer lists at most 50", n)}
	}
	run := func(r *Runner) AuditResult {
		val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
		return getInto[AuditResult](t, val, err)
	}
	confirm := func(r *Runner, fs []AuditFinding) {
		for _, f := range fs {
			if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed, map[string]any{"finding": f.Text(), "by": "sebastian", "why": "fixtures"}); err != nil {
				t.Fatal(err)
			}
		}
	}

	content, listed := answer(5, 5)
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, &fakeLiteLLM{content: content}, true)
	if res := run(r); !slices.Equal(res.Findings, listed) || res.Verdict != AuditFindings {
		t.Fatalf("25 findings: %v %+v, want all 25 listed", res.Verdict, res.Findings)
	}
	confirm(r, listed)
	if res := run(r); res.Findings != nil || res.Verdict != AuditClear {
		t.Fatalf("25 findings, all confirmed: %v %+v, want clear", res.Verdict, res.Findings)
	}

	content, listed = answer(5, 10)
	r, _ = newAuditRunner(t, &gateForgejo{diff: diff}, &fakeLiteLLM{content: content}, true)
	if res := run(r); !slices.Equal(res.Findings, append([]AuditFinding{full(50)}, listed...)) || res.Verdict != AuditFindings {
		t.Fatalf("50 findings: %v %d findings, the first %+v, want the 50 after %+v", res.Verdict, len(res.Findings), res.Findings[0], full(50))
	}
	confirm(r, listed)
	if res := run(r); !slices.Equal(res.Findings, []AuditFinding{full(50)}) || res.Verdict != AuditFindings {
		t.Fatalf("50 findings, all confirmed: %v %+v, want findings, the maximum's alone", res.Verdict, res.Findings)
	}

	content, listed = answer(6, 10)
	r, _ = newAuditRunner(t, &gateForgejo{diff: diff}, &fakeLiteLLM{content: content}, true)
	want := append(append([]AuditFinding{full(60)}, listed[:50]...), AuditFinding{Item: "... and 10 more", Why: "the card lists the first 50 of the parts' findings"})
	if res := run(r); !slices.Equal(res.Findings, want) {
		t.Fatalf("60 findings: %d findings, the first %+v, the last %+v", len(res.Findings), res.Findings[0], res.Findings[len(res.Findings)-1])
	}
}

// Findings a confirmation covers leave the list before its cap. Every one
// of 60 model findings (10 a part, six parts) at f00.go covered: a clear
// verdict. 60 pre-check hits in x.env, confirmed: clear. 60 more in y.env
// beside them: the card lists y.env's, its why counting the other 59.
func TestGateAuditListsTheUnconfirmedFindings(t *testing.T) {
	var ten []string
	for i := range 10 {
		ten = append(ten, fmt.Sprintf(`{"item":2,"file":"f00.go","line":%d,"why":"a new host"}`, i+1))
	}
	diff := ""
	for i := range 6 {
		diff += diffFile(fmt.Sprintf("f%02d.go", i), 40<<10)
	}
	hits := func(path string) string {
		return "diff --git a/" + path + " b/" + path + "\n--- /dev/null\n+++ b/" + path + "\n@@ -0,0 +1,60 @@\n" + strings.Repeat("+PASSWORD=envpass0001\n", 60)
	}
	const kind = "password, secret or token assigned a literal"
	y := []AuditFinding{{Item: "secret in the diff: " + kind, File: "y.env", Line: 1, Why: "an added line has the shape of a " + kind + " (pre-check); 59 more at this file"}}
	for _, tc := range []struct {
		name, diff, answer, confirmed string
		want                          []AuditFinding
	}{
		{"60 model findings", diff, `{"verdict":"findings","findings":[` + strings.Join(ten, ",") + `]}`, "network egress at f00.go", nil},
		{"60 pre-check hits", hits("x.env"), `{"verdict":"clear"}`, "secret in the diff: " + kind + " at x.env", nil},
		{"60 more in another file", hits("x.env") + hits("y.env"), `{"verdict":"clear"}`, "secret in the diff: " + kind + " at x.env", y},
	} {
		r, _ := newAuditRunner(t, &gateForgejo{diff: tc.diff}, &fakeLiteLLM{content: tc.answer}, true)
		if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed, map[string]any{"finding": tc.confirmed, "by": "sebastian", "why": "fixtures"}); err != nil {
			t.Fatal(err)
		}
		val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
		res := getInto[AuditResult](t, val, err)
		if !slices.Equal(res.Findings, tc.want) || (res.Verdict == AuditClear) != (tc.want == nil) {
			t.Errorf("%s: audit = %v %d findings, want %d", tc.name, res.Verdict, len(res.Findings), len(tc.want))
		}
	}
}

// The model's file is a path of the diff: itself, or itself without a
// leading b/, ./ or /, so one confirmation covers it however a run wrote
// it. A file the diff does not hold leaves the finding at no file, which no
// confirmation covers, and the why names what the model wrote; a finding
// the model gave no file stays as it is.
func TestGateAuditMapsTheModelsFileOntoTheDiff(t *testing.T) {
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":1,"file":"b/internal/x/x_test.go","line":4,"why":"w1"},` +
		`{"item":1,"file":"./internal/x/x_test.go","line":4,"why":"w2"},{"item":1,"file":"/internal/x/x_test.go","line":5,"why":"w3"},` +
		`{"item":1,"file":"N/A","why":"w4"},{"item":1,"file":"","why":"w5"}]}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: secretTestDiff}, ll, true)
	for _, finding := range []string{"secrets at internal/x/x_test.go", "secrets"} {
		if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed, map[string]any{"finding": finding, "by": "sebastian", "why": "fixtures"}); err != nil {
			t.Fatal(err)
		}
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{
		{Item: "secret in the diff: password, secret or token assigned a literal", File: "internal/x/x_test.go", Line: 4,
			Why: "an added line has the shape of a password, secret or token assigned a literal (pre-check)"},
		{Item: "secrets", Why: `w4 (at "N/A", no path of the diff)`},
		{Item: "secrets", Why: "w5"},
	}
	if !slices.Equal(res.Findings, want) {
		t.Fatalf("findings = %+v, want %+v", res.Findings, want)
	}
}

// A file over the audit's 48 KiB part is cut by hunks: two calls, no
// finding.
func TestGateAuditReadsABigFileInPieces(t *testing.T) {
	big := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -0,0 +1,5000 @@\n" + strings.Repeat("+0123456789abcdef\n", 5000)
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: big}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if res.Verdict == AuditFindings || len(res.Findings) != 0 || len(ll.chats) != 2 {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
}

// Every audit call reads the whole checklist: no call sees another's.
func TestAuditPromptsCarryTheChecklist(t *testing.T) {
	for name, prompt := range map[string]string{"first": auditPrompt, "part": auditPartPrompt} {
		if !strings.HasSuffix(prompt, auditChecklist+"\n\n"+writingguide.For(writingguide.JobFinding)) || !strings.Contains(auditChecklist, "\n6. the sandbox contract") {
			t.Errorf("the %s prompt does not end with the checklist and the finding form", name)
		}
	}
}

// Two files of 30 KiB: two audit calls, each with its part, and the
// findings of both. The call on part 1 alone carries the pull request and
// the whole diff's concurrency summary under the audit prompt; the call on
// part 2 carries its diff and part under the part prompt.
func TestGateAuditReadsTheDiffInParts(t *testing.T) {
	diff := diffFile("a.go", 30<<10, "\tgo func() {") + diffFile("b.go", 30<<10)
	ll := &fakeLiteLLM{answer: func(user string) string {
		if strings.Contains(user, "part 1 of 2") {
			return `{"verdict":"findings","findings":[{"item":2,"file":"a.go","line":1,"why":"a new host"}]}`
		}
		return `{"verdict":"findings","findings":[{"item":3,"file":"b.go","line":1,"why":"a new Role"}]}`
	}}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "network egress", File: "a.go", Line: 1, Why: "a new host"}, {Item: "RBAC and ServiceAccounts", File: "b.go", Line: 1, Why: "a new Role"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 2 {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
	chats := byPart(ll)
	for i, want := range []struct {
		part, prompt string
		keys         []string
	}{
		{`"part":"file 1 of 2, part 1 of 2"`, auditPrompt, []string{"concurrency", "diff", "head", "part", "pull_request", "repository"}},
		{`"part":"file 2 of 2, part 2 of 2"`, auditPartPrompt, []string{"diff", "part"}},
	} {
		if user := userMessage(chats[i]); !strings.Contains(user, want.part) {
			t.Fatalf("call %d lacks its part", i+1)
		}
		if keys := inputKeys(t, chats[i]); !slices.Equal(keys, want.keys) || systemMessage(chats[i]) != want.prompt {
			t.Fatalf("call %d reads %v under its prompt %t, want %v", i+1, keys, systemMessage(chats[i]) == want.prompt, want.keys)
		}
	}
	if user := userMessage(chats[0]); !strings.Contains(user, `"concurrency":"`+concurrencyHead+`\na.go:1 goroutine: go func() {"`) {
		t.Fatal("the first call lacks the whole diff's concurrency summary")
	}
}

// A part without a verdict is a finding naming the part; the other part's
// findings stay.
func TestGateAuditPartWithoutAVerdict(t *testing.T) {
	diff := diffFile("a.go", 30<<10) + diffFile("b.go", 30<<10)
	ll := &fakeLiteLLM{answer: func(user string) string {
		if strings.Contains(user, "part 1 of 2") {
			return "LGTM"
		}
		return `{"verdict":"findings","findings":[{"item":1,"file":"b.go","line":1,"why":"w"}]}`
	}}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 2 || res.Findings[0].Item != auditNoVerdict || !strings.HasPrefix(res.Findings[0].Why, "part 1 of 2: the answer is not one JSON object") ||
		res.Findings[1] != (AuditFinding{Item: "secrets", File: "b.go", Line: 1, Why: "w"}) {
		t.Fatalf("audit = %+v", res)
	}
}

// A part's answer that does not parse is never cut by the parts' cap: part
// 1 lists 50 findings at no path of the diff, each a text of its own, and
// part 2 answers no JSON. The list keeps part 1's maximum, part 2's
// no-verdict finding and part 1's 50, with no "... and N more", and the
// attempt's fact row records the audit as not run, its reason part 2's.
func TestGateAuditKeepsAPartsNoVerdictPastTheCap(t *testing.T) {
	diff := diffFile("a.go", 30<<10) + diffFile("b.go", 30<<10)
	var entries []string
	for i := range 50 {
		entries = append(entries, fmt.Sprintf(`{"item":1,"file":"n%d.go","line":1,"why":"w"}`, i))
	}
	ll := &fakeLiteLLM{answer: func(user string) string {
		if strings.Contains(user, "part 1 of 2") {
			return `{"verdict":"findings","findings":[` + strings.Join(entries, ",") + `]}`
		}
		return "LGTM"
	}}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{}, 1)
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 52 || res.Findings[0].Item != auditListFull || res.Findings[1].Item != auditNoVerdict ||
		!strings.HasPrefix(res.Findings[1].Why, "part 2 of 2: the answer is not one JSON object") || res.Findings[51].Why != `w (at "n49.go", no path of the diff)` {
		t.Fatalf("audit = %d findings %+v, want the maximum, part 2's no-verdict and the 50", len(res.Findings), res.Findings)
	}
	rows := eventsOfKind(r.store.(*fakeStore), EventGateAudit)
	if len(rows) != 1 || rows[0].Detail["ran"] != false || !strings.HasPrefix(fmt.Sprint(rows[0].Detail["reason"]), "part 2 of 2: ") {
		t.Fatalf("gate_audit rows = %+v, want one not run for part 2's answer", rows)
	}
}

// Four parts: four calls, no finding.
func TestGateAuditReadsFourParts(t *testing.T) {
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: fourParts}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 0 || len(ll.chats) != 4 {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
}

// The four parts' calls wait on the gateway at once, and the findings
// stay in part order whatever order the answers came in. The gateway
// counts the calls at once: the secret pre-check over 160 KiB takes over a
// second under the race detector, so the activity's time says nothing.
func TestGateAuditPartsRunInParallel(t *testing.T) {
	ll := &fakeLiteLLM{delay: 200 * time.Millisecond, answer: func(user string) string {
		for k, file := range []string{"a.go", "b.go", "c.go", "d.go"} {
			if strings.Contains(user, fmt.Sprintf("part %d of 4", k+1)) {
				return `{"verdict":"findings","findings":[{"item":2,"file":"` + file + `","line":1,"why":"a new host"}]}`
			}
		}
		return "no part"
	}}
	r, _ := newAuditRunner(t, &gateForgejo{diff: fourParts}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	var want []AuditFinding
	for _, file := range []string{"a.go", "b.go", "c.go", "d.go"} {
		want = append(want, AuditFinding{Item: "network egress", File: file, Line: 1, Why: "a new host"})
	}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 4 || ll.peak != 4 {
		t.Fatalf("audit = %+v after %d calls, %d at once: want the four parts' findings in order, four calls at once", res, len(ll.chats), ll.peak)
	}
}

// A project that does not switch the audit on: no diff read, no call, a
// skipped stage on the timeline.
func TestGateAuditSkipsAProjectWithoutIt(t *testing.T) {
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, objects := newAuditRunner(t, &gateForgejo{}, ll, false)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if res.Skipped != auditNotSwitchedOn || res.Verdict != "" || len(ll.chats) != 0 {
		t.Fatalf("audit = %+v, calls %d", res, len(ll.chats))
	}
	if step := timelineStep(t, objects, "t1", "stage_skipped"); step["stage"] != "audit" || step["reason"] != auditNotSwitchedOn {
		t.Fatalf("stage_skipped = %v", step)
	}
}

// The planted secret's value reaches neither the result (the card) nor
// the timeline, whatever the model echoes: in an unparseable answer, in an
// item that is no checklist number, in a finding's file or why.
func TestGateAuditNeverShowsThePlantedSecret(t *testing.T) {
	for name, content := range map[string]string{
		"unparseable":          `I cannot answer in JSON. The diff sets password = "hunter2hunter2" in a test.`,
		"an item not a number": `{"verdict":"findings","findings":[{"item":"secrets hunter2hunter2","file":"internal/x/x_test.go","line":4,"why":"w"}]}`,
		"finding": `{"verdict":"findings","findings":[{"item":1,"file":"internal/x/x_test.go hunter2hunter2","line":4,` +
			`"why":"password = \"hunter2hunter2\""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ll := &fakeLiteLLM{content: content}
			r, objects := newAuditRunner(t, &gateForgejo{diff: secretTestDiff}, ll, true)
			val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
			res := getInto[AuditResult](t, val, err)
			b, _ := json.Marshal(res)
			if res.Verdict != AuditFindings || len(res.Findings) != 2 || strings.Contains(string(b), "hunter2") {
				t.Fatalf("audit = %s", b)
			}
			if tl := timeline(t, objects); strings.Contains(tl, "hunter2") {
				t.Fatalf("timeline = %s", tl)
			}
		})
	}
}

// The pre-check's hits count toward its cap of 50 by their text, an item
// at a file: 60 hits in f.env are one finding, at the first line, its why
// counting the other 59, and g.env's hit stands beside it. One hit in each
// of 60 files: 50 findings and one that counts the other 10.
func TestGateAuditCapsThePreCheck(t *testing.T) {
	const kind = "password, secret or token assigned a literal"
	hits := func(path string, n int) string {
		return "diff --git a/" + path + " b/" + path + "\n--- /dev/null\n+++ b/" + path + fmt.Sprintf("\n@@ -0,0 +1,%d @@\n", n) + strings.Repeat("+PASSWORD=envpass0001\n", n)
	}
	hit := func(path, more string) AuditFinding {
		return AuditFinding{Item: "secret in the diff: " + kind, File: path, Line: 1, Why: "an added line has the shape of a " + kind + " (pre-check)" + more}
	}
	files := ""
	var fifty []AuditFinding
	for i := range 60 {
		files += hits(fmt.Sprintf("p%02d.env", i), 1)
		if i < 50 {
			fifty = append(fifty, hit(fmt.Sprintf("p%02d.env", i), ""))
		}
	}
	fifty = append(fifty, AuditFinding{Item: "secret in the diff: 10 more pre-check hits", Why: "the card lists the first 50"})
	for _, tc := range []struct {
		name, diff string
		want       []AuditFinding
	}{
		{"60 hits in one file", hits("f.env", 60) + hits("g.env", 1), []AuditFinding{hit("f.env", "; 59 more at this file"), hit("g.env", "")}},
		{"one hit in each of 60 files", files, fifty},
	} {
		r, _ := newAuditRunner(t, &gateForgejo{diff: tc.diff}, &fakeLiteLLM{content: `{"verdict":"clear"}`}, true)
		val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
		if res := getInto[AuditResult](t, val, err); !slices.Equal(res.Findings, tc.want) {
			t.Errorf("%s: %d findings, the first %+v, the last %+v; want %d", tc.name, len(res.Findings), res.Findings[0], res.Findings[len(res.Findings)-1], len(tc.want))
		}
	}
}

// A pre-check hit keeps its own path even where the path holds the value
// the scrubber hides, and so does the model's finding at that path of the
// diff: one path, one identity.
func TestGateAuditKeepsThePreChecksPath(t *testing.T) {
	diff := "diff --git a/k8s/hunter2hunter2.yaml b/k8s/hunter2hunter2.yaml\n--- /dev/null\n+++ b/k8s/hunter2hunter2.yaml\n@@ -0,0 +1 @@\n+password: \"hunter2hunter2\"\n"
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":1,"file":"k8s/hunter2hunter2.yaml","line":1,"why":"a password"}]}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 2 || res.Findings[0].File != "k8s/hunter2hunter2.yaml" || res.Findings[1].File != "k8s/hunter2hunter2.yaml" {
		t.Fatalf("findings = %+v", res.Findings)
	}
}

// Every audit attempt is a gate_audit row beside its timeline line, keyed
// by the card, the head and the stages' round: a clear audit ran, a
// second attempt of one round on one head writes no second row, a second
// round adds its own, and an audit the card cannot pay for did not run
// and says why.
func TestAuditRecordsItsAttemptAsAnEvent(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
	r, _ := newAuditRunner(t, fj, &fakeLiteLLM{content: `{"verdict":"clear","findings":[]}`}, true)
	store := r.store.(*fakeStore)
	for _, round := range []int{1, 1, 2} {
		val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{}, round)
		if res := getInto[AuditResult](t, val, err); res.Verdict != AuditClear {
			t.Fatalf("round %d: audit = %+v", round, res)
		}
	}
	rows := eventsOfKind(store, EventGateAudit)
	if len(rows) != 2 {
		t.Fatalf("gate_audit rows = %d (%+v), want one per round", len(rows), rows)
	}
	for i, row := range rows {
		if key := fmt.Sprintf("gate_audit t1 c1 %d", i+1); row.IdentityKey != key || row.TaskID != "t1" || row.Repository != "sf/repo" || row.IssueNumber != 4 ||
			row.Actor != FactoryDecider || row.Via != ViaFactoryd {
			t.Fatalf("row %d = %+v, want t1's card keyed %q", i, row, key)
		}
		want := map[string]any{"pr": int64(9), "commit": "c1", "model": "big", "ran": true, "verdict": AuditClear}
		if !reflect.DeepEqual(row.Detail, want) {
			t.Fatalf("row %d detail = %v, want %v", i, row.Detail, want)
		}
	}

	task := gateTask
	task.BudgetEUR = 0.30
	r, store, _ = newGateRunner(t, task, &gateForgejo{diff: fourParts})
	r.supervisor = testSupervisorModel((&fakeLiteLLM{content: `{"verdict":"clear"}`}).srv(t).URL)
	v := auditVersion(true)
	r.cfg.Policy = func() policy.Version { return v }
	r.cfg.Prices = stubPrices{price: policy.Price{In: 1e-9, Out: 1e-5}}
	if _, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{}, 1); err != nil {
		t.Fatal(err)
	}
	rows = eventsOfKind(store, EventGateAudit)
	want := map[string]any{"pr": int64(9), "commit": "c1", "model": "big", "ran": false, "verdict": "",
		"reason": "the audit did not run: its 4 calls would cost about 0.36 EUR, and the card has 0.30 EUR free. " +
			"Read the diff, then Approve the merge or Deny it (Close on a pull request factoryd opened)."}
	if len(rows) != 1 || rows[0].IdentityKey != "gate_audit t1 c1 1" || !reflect.DeepEqual(rows[0].Detail, want) {
		t.Fatalf("gate_audit rows = %+v, want one not run: %v", rows, want)
	}
}

// A not-run audit's reason is cut at maxFactReason: a head named past 500
// bytes makes its why longer, and the row keeps its first 500 bytes and
// the ellipsis.
func TestAuditFactCutsItsReason(t *testing.T) {
	r, _ := newAuditRunner(t, &gateForgejo{diff: "d"}, &fakeLiteLLM{content: `{"verdict":"clear"}`}, true)
	head := "c2, " + strings.Repeat("a head named in words, ", 30) // words, which the scrubber leaves
	if _, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, head, GateResults{}, 1); err != nil {
		t.Fatal(err)
	}
	why := "the audit did not run: pull request #9's head moved to c1 after its checks passed on " + head
	rows := eventsOfKind(r.store.(*fakeStore), EventGateAudit)
	if len(rows) != 1 || rows[0].Detail["ran"] != false || rows[0].Detail["reason"] != why[:maxFactReason]+"…" {
		t.Fatalf("gate_audit rows %+v, want one not run with the reason cut to %d bytes", rows, maxFactReason)
	}
}
