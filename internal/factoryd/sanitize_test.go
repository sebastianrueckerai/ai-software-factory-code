package factoryd

import (
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// linkPolicy allows links to docs.example.org besides the Forgejo host.
func linkPolicy(r *Runner) {
	v := testPolicyVersion()
	v.Document.Approvers = []string{"sebastian"}
	v.Document.LinkHosts = []string{"docs.example.org"}
	r.cfg.Policy = func() policy.Version { return v }
	r.forgejo.PublicURL = "https://git.example"
}

const hostile = "@mallory fixes #1, see https://evil.test/x, https://git.example/sf/repo/issues/4 and https://docs.example.org/a"

const hostileOut = "＠mallory (fixes) #1, see hxxps://evil[.]test/x, https://git.example/sf/repo/issues/4 and https://docs.example.org/a"

// What factoryd posts from an agent's, a model's or an issue author's text
// carries no live mention, closing keyword or link off the allowlist;
// factoryd's own mentions and closing keyword stay.
func TestPostedTextIsSanitised(t *testing.T) {
	t.Run("information request", func(t *testing.T) {
		task := Task{ID: "t1", Status: "running", Objective: "the slug breaks", Repository: "sf/repo", IssueNumber: 4, Kind: "agent", Author: "visitor"}
		r, _, _ := newActivityRunner(t, task, &prForgejo{})
		fj := &requestInfoForgejo{}
		r.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
		linkPolicy(r)
		if _, err := execActivity(t, r, ActivityRequestInfo, "t1", agentAsksAuthor+hostile); err != nil {
			t.Fatal(err)
		}
		if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "```\n"+hostileOut+"\n```") || !strings.HasSuffix(fj.comments[0], "cc @visitor @sebastian") {
			t.Fatalf("comment = %q", fj.comments)
		}
	})
	t.Run("escalation", func(t *testing.T) {
		fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}}
		r, _, _ := newActivityRunner(t, prTask, fj)
		linkPolicy(r)
		if _, err := execActivity(t, r, ActivityEscalate, "t1", hostile); err != nil {
			t.Fatal(err)
		}
		if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "```\n"+hostileOut+"\n```") {
			t.Fatalf("comment = %q", fj.comments)
		}
	})
	t.Run("pull request", func(t *testing.T) {
		fj := &prForgejo{}
		task := prTask
		task.Objective = "Fix the slug @bob\n\n" + hostile
		r, _, _ := newActivityRunner(t, task, fj)
		linkPolicy(r)
		if _, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1"); err != nil {
			t.Fatal(err)
		}
		if body := fj.created[0]["body"]; body != "Fix the slug ＠bob\n\n"+hostileOut+"\n\nCloses #4\n\nOpened by factoryd for task t1." || fj.created[0]["title"] != "Fix the slug ＠bob" {
			t.Fatalf("title %q, body %q", fj.created[0]["title"], body)
		}
	})
	t.Run("model review", func(t *testing.T) {
		fj := &gateForgejo{diff: "d"}
		r, _, _ := newGateRunner(t, gateTask, fj)
		linkPolicy(r)
		ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"` + hostile + `","findings":["resolves #2"]}`}
		r.supervisor = testSupervisorModel(ll.srv(t).URL)
		if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err != nil {
			t.Fatal(err)
		}
		if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "Verdict: **approve**. "+hostileOut+"\n\n- (resolves) #2") {
			t.Fatalf("comment = %q", fj.comments)
		}
	})
	t.Run("failure reason", func(t *testing.T) {
		fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
		task := prTask
		task.Status = "in-review"
		r, _, _ := newActivityRunner(t, task, fj)
		linkPolicy(r)
		if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "escalation: "+hostile); err != nil {
			t.Fatal(err)
		}
		if len(fj.comments) != 1 || fj.comments[0] != "factoryd: task t1 failed: escalation: "+hostileOut {
			t.Fatalf("comments = %q", fj.comments)
		}
	})
}

// A changed path is a code span no backtick in its name can close early.
func TestListPathsKeepsNamesInTheirSpan(t *testing.T) {
	got := listPaths([]string{"a.go", "x` @admin `y", "`edge`"})
	if want := "- `a.go`\n- ``x` @admin `y``\n- `` `edge` ``\n"; got != want {
		t.Fatalf("listPaths = %q, want %q", got, want)
	}
}
