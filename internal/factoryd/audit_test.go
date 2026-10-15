package factoryd

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// auditVersion is testPolicyVersion with coding-frontier as the coder
// class's audit model and sf/repo's project switching the audit on.
func auditVersion(on bool) policy.Version {
	v := testPolicyVersion()
	coder := v.Document.Classes["coder"]
	coder.AuditModel = "coding-frontier"
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
		"findings": {`{"verdict":"findings","findings":[{"item":"network egress","file":"k8s/egress.yaml","line":12,"why":"allows api.example.org"}]}`,
			[]AuditFinding{{Item: "network egress", File: "k8s/egress.yaml", Line: 12, Why: "allows api.example.org"}}, ""},
		"clear with findings listed": {`{"verdict":"clear","findings":[{"item":"RBAC","file":"k8s/rbac.yaml","line":3,"why":"a new ClusterRole"}]}`,
			[]AuditFinding{{Item: "RBAC", File: "k8s/rbac.yaml", Line: 3, Why: "a new ClusterRole"}}, ""},
		"not json":             {"all good", nil, "the answer is not one JSON object"},
		"empty":                {"", nil, "the answer is not one JSON object"},
		"unknown verdict":      {`{"verdict":"pass"}`, nil, `unknown verdict "pass"`},
		"findings without any": {`{"verdict":"findings","findings":[]}`, nil, "verdict findings names none"},
		"extra field":          {`{"verdict":"clear","summary":"nothing touched"}`, nil, ""},
		"negative line": {`{"verdict":"findings","findings":[{"item":"RBAC","file":"k8s/rbac.yaml","line":-3,"why":"a new Role"}]}`,
			[]AuditFinding{{Item: "RBAC", File: "k8s/rbac.yaml", Why: "a new Role"}}, ""},
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

// A planted password in a test file: the pre-check finds it at its file
// and line without the value, the model's findings join, and the call is
// the coder class's audit model on the task's account with purpose audit.
// The timeline records the audit with its verdict, the deployment and
// its start; no comment reaches the pull request.
func TestGateAuditFindsAPlantedSecret(t *testing.T) {
	fj := &gateForgejo{diff: secretTestDiff}
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":"secrets","file":"internal/x/x_test.go","line":4,"why":"a password literal and a token ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789 in a test"}]}`,
		deployment: "zai/glm-5.3"}
	r, objects := newAuditRunner(t, fj, ll, true)
	from := time.Now()
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{
		{Item: "secret in the diff: " + "password, secret or token assigned a literal", File: "internal/x/x_test.go", Line: 4,
			Why: "an added line has the shape of a password, secret or token assigned a literal (pre-check)"},
		{Item: "secrets", File: "internal/x/x_test.go", Line: 4, Why: "a password literal and a token [redacted] in a test"},
	}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || res.Model != "coding-frontier" || res.Deployment != "zai/glm-5.3" {
		t.Fatalf("audit = %+v", res)
	}
	if len(ll.chats) != 1 || ll.chats[0]["model"] != "coding-frontier" || ll.purposes[0] != "audit" || ll.tasks[0] != "t1" {
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
	if step["verdict"] != AuditFindings || step["deployment"] != "zai/glm-5.3" || step["model"] != "coding-frontier" {
		t.Fatalf("audit step = %v", step)
	}
	if tl := timeline(t, objects); strings.Contains(tl, "hunter2") || strings.Contains(tl, "ghp_") {
		t.Fatal("the secret's value is on the timeline")
	}
	startedWithin(t, objects, "audit", from)
}

func TestGateAuditClear(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n--- a/Makefile\n+++ b/Makefile\n@@ -1 +1,2 @@\n all:\n+check:\n"}
	ll := &fakeLiteLLM{content: `{"verdict":"clear","findings":[]}`}
	r, _ := newAuditRunner(t, fj, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
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
			val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
			res := getInto[AuditResult](t, val, err)
			if res.Verdict != AuditFindings || len(res.Findings) != 1 || res.Findings[0].Item != auditNoVerdict || !strings.Contains(res.Findings[0].Why, tc.why) {
				t.Fatalf("audit = %+v, want the no-verdict finding with %q", res, tc.why)
			}
		})
	}
}

// A file over the audit's 48 KiB part: no model call, and the one finding
// that says why.
func TestGateAuditCutDiffIsAFinding(t *testing.T) {
	big := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -0,0 +1,5000 @@\n" + strings.Repeat("+0123456789abcdef\n", 5000)
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: big}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "not audited in full", Why: "1 file, 88 KiB; the audit reads at most 3 parts of 48 KiB (a.txt alone is 88 KiB)"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 0 {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
}

// Two files of 30 KiB: two audit calls, each with its part and the whole
// diff's concurrency summary, and the findings of both.
func TestGateAuditReadsTheDiffInParts(t *testing.T) {
	diff := diffFile("a.go", 30<<10, "\tgo func() {") + diffFile("b.go", 30<<10)
	ll := &fakeLiteLLM{contents: []string{
		`{"verdict":"findings","findings":[{"item":"network egress","file":"a.go","line":1,"why":"a new host"}]}`,
		`{"verdict":"findings","findings":[{"item":"RBAC and ServiceAccounts","file":"b.go","line":1,"why":"a new Role"}]}`}}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "network egress", File: "a.go", Line: 1, Why: "a new host"}, {Item: "RBAC and ServiceAccounts", File: "b.go", Line: 1, Why: "a new Role"}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 2 {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
	for i, part := range []string{`"part": "file 1 of 2, part 1 of 2"`, `"part": "file 2 of 2, part 2 of 2"`} {
		if user := userMessage(ll.chats[i]); !strings.Contains(user, part) || !strings.Contains(user, `"concurrency": "`+concurrencyHead+`\na.go:1 goroutine: go func() {"`) {
			t.Fatalf("call %d lacks its part or the concurrency summary", i+1)
		}
	}
}

// A part without a verdict is a finding naming the part; the other part's
// findings stay.
func TestGateAuditPartWithoutAVerdict(t *testing.T) {
	diff := diffFile("a.go", 30<<10) + diffFile("b.go", 30<<10)
	ll := &fakeLiteLLM{contents: []string{"LGTM", `{"verdict":"findings","findings":[{"item":"secrets","file":"b.go","line":1,"why":"w"}]}`}}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 2 || res.Findings[0].Item != auditNoVerdict || !strings.HasPrefix(res.Findings[0].Why, "part 1 of 2: the answer is not one JSON object") ||
		res.Findings[1] != (AuditFinding{Item: "secrets", File: "b.go", Line: 1, Why: "w"}) {
		t.Fatalf("audit = %+v", res)
	}
}

// Four parts: no model call, the pre-check still scans the whole diff,
// and the one finding says why the rest is not audited.
func TestGateAuditBeyondItsReachIsNotAuditedInFull(t *testing.T) {
	diff := diffFile("a_test.go", 40<<10, "\tpassword = \"hunter2hunter2\"") + diffFile("b.go", 40<<10) + diffFile("c.go", 40<<10) + diffFile("d.go", 40<<10)
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{
		{Item: "secret in the diff: password, secret or token assigned a literal", File: "a_test.go", Line: 1,
			Why: "an added line has the shape of a password, secret or token assigned a literal (pre-check)"},
		{Item: "not audited in full", Why: "4 files, 160 KiB; the audit reads at most 3 parts of 48 KiB (too many parts)"},
	}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) || len(ll.chats) != 0 {
		t.Fatalf("audit = %+v after %d calls", res, len(ll.chats))
	}
}

// A project that does not switch the audit on: no diff read, no call, a
// skipped stage on the timeline.
func TestGateAuditSkipsAProjectWithoutIt(t *testing.T) {
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, objects := newAuditRunner(t, &gateForgejo{}, ll, false)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	if res.Skipped != auditNotSwitchedOn || res.Verdict != "" || len(ll.chats) != 0 {
		t.Fatalf("audit = %+v, calls %d", res, len(ll.chats))
	}
	if step := timelineStep(t, objects, "t1", "stage_skipped"); step["stage"] != "audit" || step["reason"] != auditNotSwitchedOn {
		t.Fatalf("stage_skipped = %v", step)
	}
}

// The planted secret's value reaches neither the result (the card) nor
// the timeline, whatever the model echoes: in an unparseable answer, in a
// finding's file or why.
func TestGateAuditNeverShowsThePlantedSecret(t *testing.T) {
	for name, content := range map[string]string{
		"unparseable": `I cannot answer in JSON. The diff sets password = "hunter2hunter2" in a test.`,
		"finding": `{"verdict":"findings","findings":[{"item":"secrets hunter2hunter2","file":"internal/x/x_test.go hunter2hunter2","line":4,` +
			`"why":"password = \"hunter2hunter2\""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ll := &fakeLiteLLM{content: content}
			r, objects := newAuditRunner(t, &gateForgejo{diff: secretTestDiff}, ll, true)
			val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
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
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
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
	ll := &fakeLiteLLM{content: `{"verdict":"findings","findings":[{"item":"secrets","file":"k8s/hunter2hunter2.yaml","line":1,"why":"a password"}]}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: diff}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	if len(res.Findings) != 2 || res.Findings[0].File != "k8s/hunter2hunter2.yaml" || res.Findings[1].File != "k8s/[redacted].yaml" {
		t.Fatalf("findings = %+v", res.Findings)
	}
}
