package factoryd

import (
	"fmt"
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
		"extra field":          {`{"verdict":"clear","merge":true}`, nil, "the answer is not one JSON object"},
		"trailing text":        {`{"verdict":"clear"} ok`, nil, "the answer is not one JSON object"},
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

// A diff cut at the review's 48 KiB is itself a finding.
func TestGateAuditCutDiffIsAFinding(t *testing.T) {
	big := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -0,0 +1,5000 @@\n" + strings.Repeat("+0123456789abcdef\n", 5000)
	ll := &fakeLiteLLM{content: `{"verdict":"clear"}`}
	r, _ := newAuditRunner(t, &gateForgejo{diff: big}, ll, true)
	val, err := execActivity(t, r, ActivityGateAudit, "t1", PullRequest{Number: 9}, "c1", int64(0))
	res := getInto[AuditResult](t, val, err)
	want := []AuditFinding{{Item: "diff too large to audit in full", Why: fmt.Sprintf("the audit read the first %d bytes of the diff", reviewDiffMax)}}
	if res.Verdict != AuditFindings || !slices.Equal(res.Findings, want) {
		t.Fatalf("audit = %+v", res)
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
