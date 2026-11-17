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

// A finding points at a file: one at none makes the whole answer one
// without a verdict, naming its item.
func TestParseAuditRefusesAFindingAtNoFile(t *testing.T) {
	for _, file := range []string{`"file":"","line":3,`, `"file":"  ",`, ``} {
		got, why := parseAudit(`{"verdict":"findings","findings":[{"item":1,"file":"a.go","why":"x"},{"item":2,` + file + `"why":"y"}]}`)
		if want := "the answer named a finding of checklist item 2 at no file"; got != nil || why != want {
			t.Errorf("%s: parseAudit = %+v, %q, want no findings and %q", file, got, why, want)
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

// A finding's id folds its item's words and keeps its path as written. By
// sha256sum: "secrets at A.go" b78e8fc0c2a5, "secrets at a.go" 99f3acc590cc,
// "rbac and serviceaccounts at x.go" 05e61f6763d2.
func TestAuditFindingIDKeepsThePathsCase(t *testing.T) {
	for _, tc := range []struct {
		f    AuditFinding
		want string
	}{
		{AuditFinding{Item: "secrets", File: "A.go", Line: 3}, "b78e8fc0c2a5"},
		{AuditFinding{Item: "secrets", File: "a.go", Line: 4}, "99f3acc590cc"},
		{AuditFinding{Item: "RBAC and ServiceAccounts", File: "x.go"}, "05e61f6763d2"},
		{AuditFinding{Item: auditNoVerdict, Why: "w"}, FindingID(auditNoVerdict)},
	} {
		if got := tc.f.ID(); got != tc.want {
			t.Errorf("%+v.ID() = %q, want %q", tc.f, got, tc.want)
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
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
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

// Six parts of ten findings each: the parts' findings keep the first 50
// and one that counts the other 10.
func TestGateAuditCapsThePartsFindings(t *testing.T) {
	var ten []string
	for i := range 10 {
		ten = append(ten, fmt.Sprintf(`{"item":2,"file":"f.go","line":%d,"why":"a new host"}`, i+1))
	}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[` + strings.Join(ten, ",") + `]}`}
	diff := ""
	for i := range 6 {
		diff += diffFile(fmt.Sprintf("f%02d.go", i), 40<<10)
	}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	last := AuditFinding{Item: "... and 10 more", Why: "the card lists the first 50 of the parts' findings"}
	if len(res.Findings) != 51 || res.Findings[49].Line != 10 || res.Findings[50] != last || len(ll.chats) != 6 {
		t.Fatalf("%d findings after %d calls, the last %+v, want 51 ending %+v", len(res.Findings), len(ll.chats), res.Findings[len(res.Findings)-1], last)
	}
}

// The count of findings past the card's list stays a finding whatever
// stands confirmed: with every listed finding covered, the 10 findings
// never listed hold the verdict, as no human saw them.
func TestGateAuditNeverCoversTheFindingsPastTheList(t *testing.T) {
	var ten []string
	for i := range 10 {
		ten = append(ten, fmt.Sprintf(`{"item":2,"file":"f.go","line":%d,"why":"a new host"}`, i+1))
	}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[` + strings.Join(ten, ",") + `]}`}
	diff := ""
	for i := range 6 {
		diff += diffFile(fmt.Sprintf("f%02d.go", i), 40<<10)
	}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	for _, finding := range []string{"network egress at f.go", "... and 10 more"} {
		if err := r.timeline.Append(context.Background(), "t1", stepAuditConfirmed, map[string]any{"finding": finding, "by": "sebastian", "why": "fixtures"}); err != nil {
			t.Fatal(err)
		}
	}
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "... and 10 more", Why: "the card lists the first 50 of the parts' findings"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) {
		t.Fatalf("audit = %+v, want the count past the list alone holding the verdict", res)
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
		if !strings.HasSuffix(prompt, auditChecklist) || !strings.Contains(auditChecklist, "\n6. sandbox contract: ") {
			t.Errorf("the %s prompt does not end with the checklist", name)
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

// The pre-check's hits are capped at 50 findings and one that counts the
// rest.
func TestGateAuditCapsThePreCheck(t *testing.T) {
	diff := "diff --git a/f.env b/f.env\n--- a/f.env\n+++ b/f.env\n@@ -0,0 +1,60 @@\n" + strings.Repeat("+PASSWORD=envpass0001\n", 60)
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 51 || res.Findings[49].Line != 50 ||
		res.Findings[50] != (AuditFinding{Item: "secret in the diff: 10 more pre-check hits", Why: "the card lists the first 50"}) {
		t.Fatalf("findings = %d, last %+v", len(res.Findings), res.Findings[len(res.Findings)-1])
	}
}

// A pre-check hit keeps its own path even where the path holds the value
// the scrubber hides; the model's file is scrubbed.
func TestGateAuditKeepsThePreChecksPath(t *testing.T) {
	diff := "diff --git a/k8s/hunter2hunter2.yaml b/k8s/hunter2hunter2.yaml\n--- /dev/null\n+++ b/k8s/hunter2hunter2.yaml\n@@ -0,0 +1 @@\n+password: \"hunter2hunter2\"\n"
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":1,"file":"k8s/hunter2hunter2.yaml","line":1,"why":"a password"}]}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 2 || res.Findings[0].File != "k8s/hunter2hunter2.yaml" || res.Findings[1].File != "k8s/[redacted].yaml" {
		t.Fatalf("findings = %+v", res.Findings)
	}
}

// lookHunk is the hunk of image_look_test.go a rename card changed, lines
// 151 to 157 of newLookEnv, its one pair a use of the field kube renamed
// Kube; more adds lines after it. runnerKube renames the field in
// runner.go, runnerHead at the head.
func lookHunk(more ...string) (diff, head string) {
	const path = "internal/factoryd/image_look_test.go"
	ctx := []string{"\tcoder.VisionModel = \"vision\"", "\tpv.Document.Classes[\"coder\"] = coder", "\tcfg.Policy = func() policy.Version { return pv }"}
	line := "\te.runner = &Runner{cfg: cfg, store: e.store, %s: kube, forgejo: &Forgejo{BaseURL: fj.URL, BotUser: \"factory-agent\", BotPassword: \"bot-pw\"}}"
	diff = fmt.Sprintf("diff --git a/%s b/%s\nindex a892a1edc..22741d60b 100644\n--- a/%s\n+++ b/%s\n@@ -151,7 +151,%d @@ func newLookEnv(t *testing.T) *lookEnv {\n",
		path, path, path, path, 7+len(more))
	diff += " " + strings.Join(ctx, "\n ") + "\n-" + fmt.Sprintf(line, "kube") + "\n+" + fmt.Sprintf(line, "Kube") + "\n \te.runner.workerPort, _ = strconv.Atoi(port)\n"
	for _, l := range more {
		diff += "+" + l + "\n"
	}
	diff += " \treturn e\n }\n"
	head = "package factoryd\n" + strings.Repeat("\n", 148) + "func newLookEnv(t *testing.T) *lookEnv {\n" +
		strings.Join(append(append(ctx, fmt.Sprintf(line, "Kube"), "\te.runner.workerPort, _ = strconv.Atoi(port)"), more...), "\n") + "\n\treturn e\n}\n"
	return diff, head
}

const (
	runnerKube = "diff --git a/internal/factoryd/runner.go b/internal/factoryd/runner.go\n--- a/internal/factoryd/runner.go\n+++ b/internal/factoryd/runner.go\n" +
		"@@ -4 +4 @@ type Runner struct {\n-\tkube Kube\n+\tKube Kube\n"
	runnerHead = "package factoryd\n\ntype Runner struct {\n\tKube Kube\n}\n"
)

// auditDiff is the diff the audit call chat read.
func auditDiff(t *testing.T, chat map[string]any) string {
	t.Helper()
	var in auditInput
	if err := json.Unmarshal([]byte(userMessage(chat)), &in); err != nil {
		t.Fatal(err)
	}
	return in.Diff
}

// The audit model reads a use of a renamed field as the context line that
// holds the new line, beside the hunk's real change and the field's
// renamed declaration, from the files read at the audited head; the step
// counts the pair.
func TestGateAuditSendsNoRenameLines(t *testing.T) {
	diff, head := lookHunk("\te.look = true")
	fj := &gateForgejo{diff: runnerKube + diff, raw: map[string]string{"sf/repo/raw/internal/factoryd/image_look_test.go": head,
		"sf/repo/raw/internal/factoryd/runner.go": runnerHead}}
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, objects := newAuditRunner(t, fj, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if res := getInto[AuditResult](t, val, err); res.Verdict != AuditClear || len(ll.chats) != 1 || res.Renames != (RenameCounts{Pairs: 1, Files: 1}) {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
	got := auditDiff(t, ll.chats[0])
	if !strings.Contains(got, "\n \te.runner = &Runner{cfg: cfg, store: e.store, Kube: kube,") || strings.Contains(got, "kube: kube") ||
		!strings.Contains(got, "\n+\te.look = true\n") || !strings.Contains(got, "\n-\tkube Kube\n+\tKube Kube\n") {
		t.Fatalf("the audit read %q, want the use as context, the declaration and the added line", got)
	}
	if !slices.Equal(fj.rawRefs, []string{"c1", "c1"}) {
		t.Fatalf("raw reads at %q, want the two at the audited head", fj.rawRefs)
	}
	if renames, _ := timelineStep(t, objects, "t1", "audit")["renames"].(map[string]any); renames["pairs"] != 1.0 || renames["files"] != 1.0 {
		t.Fatalf("the step's renames = %v, want one pair in one file", renames)
	}
}

// A card of renames alone reaches the model as its declarations: the uses
// of the field kube and of the field password, whose credential-shaped
// literal stood in the base, are context, so the pre-check raises nothing.
func TestGateAuditReadsARenameCardAtItsDeclarations(t *testing.T) {
	diff, head := lookHunk()
	diff = runnerKube + diff + "diff --git a/internal/x/x.go b/internal/x/x.go\n--- a/internal/x/x.go\n+++ b/internal/x/x.go\n@@ -4 +4 @@ type Config struct {\n" +
		"-\tpassword string\n+\tPassword string\n" +
		"diff --git a/internal/x/x_test.go b/internal/x/x_test.go\n--- a/internal/x/x_test.go\n+++ b/internal/x/x_test.go\n@@ -4 +4 @@ func f() {\n" +
		"-\tc := Config{password: \"hunter2hunter2\"}\n+\tc := Config{Password: \"hunter2hunter2\"}\n"
	fj := &gateForgejo{diff: diff, raw: map[string]string{"sf/repo/raw/internal/factoryd/image_look_test.go": head, "sf/repo/raw/internal/factoryd/runner.go": runnerHead,
		"sf/repo/raw/internal/x/x.go":      "package x\n\ntype Config struct {\n\tPassword string\n}\n",
		"sf/repo/raw/internal/x/x_test.go": "package x\n\nfunc f() {\n\tc := Config{Password: \"hunter2hunter2\"}\n}\n"}}
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, fj, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if res := getInto[AuditResult](t, val, err); res.Verdict != AuditClear || len(res.Findings) != 0 || len(ll.chats) != 1 || res.Renames != (RenameCounts{Pairs: 2, Files: 2}) {
		t.Fatalf("audit = %+v after %d calls, want clear after one call", res, len(ll.chats))
	}
	got := auditDiff(t, ll.chats[0])
	if want := splitDiff(runnerKube)[0].Text + "diff --git a/internal/x/x.go b/internal/x/x.go\n--- a/internal/x/x.go\n+++ b/internal/x/x.go\n@@ -4 +4 @@ type Config struct {\n" +
		"-\tpassword string\n+\tPassword string\n"; got != want {
		t.Fatalf("the audit read %q, want the two declarations alone %q", got, want)
	}
}

// A file the audit cannot read at the head filters nothing, and the step
// counts the failed read.
func TestGateAuditCountsAFailedHeadRead(t *testing.T) {
	diff, _ := lookHunk()
	fj := &gateForgejo{diff: runnerKube + diff, raw: map[string]string{"sf/repo/raw/internal/factoryd/runner.go": runnerHead}}
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, objects := newAuditRunner(t, fj, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if res := getInto[AuditResult](t, val, err); res.Renames != (RenameCounts{ReadsFailed: 1}) {
		t.Fatalf("audit = %+v, want one failed read and nothing quiet", res)
	}
	if got := auditDiff(t, ll.chats[0]); !strings.Contains(got, "\n-\te.runner = &Runner{cfg: cfg, store: e.store, kube: kube,") {
		t.Fatalf("the audit read %q, want the use as it is", got)
	}
	if renames, _ := timelineStep(t, objects, "t1", "audit")["renames"].(map[string]any); renames["reads_failed"] != 1.0 || renames["pairs"] != 0.0 {
		t.Fatalf("the step's renames = %v, want one failed read", renames)
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
