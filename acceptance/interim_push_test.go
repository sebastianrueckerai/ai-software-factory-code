package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

const wipBase = "sha-demo-main"

// gitForgejo is a Forgejo whose fork holds the task branch as a list of
// commits past the base: its git target. An interim push replaces the
// branch's tip, the end push adds a commit on it.
type gitForgejo struct {
	mu       sync.Mutex
	base     string
	branch   []string        // the task branch's commits past the base, oldest first
	prs      []int64         // the pull requests opened
	wip      map[string]bool // the commits the interim push wrote
	statuses []string        // the commits whose checks were asked
	merged   string          // the head the merge took, "" before it
	issues   int64
}

func (g *gitForgejo) interim(commit string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wip == nil {
		g.wip = map[string]bool{}
	}
	g.wip[commit] = true
	if len(g.branch) > 0 {
		g.branch = g.branch[:len(g.branch)-1]
	}
	g.branch = append(g.branch, commit)
}

func (g *gitForgejo) end(commit string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branch = append(g.branch, commit)
}

func (g *gitForgejo) mergedHead() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.merged
}

func (g *gitForgejo) snapshot() (branch []string, prs int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.branch), len(g.prs)
}

func (g *gitForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		repo, fork := "/api/v1/repos/demo-app/demo-app", "/api/v1/repos/factory-agent/demo-app"
		p := r.URL.Path
		var n int64
		pull := func(num int64) map[string]any {
			state, mergedBy := "open", map[string]any{}
			if g.merged != "" {
				state, mergedBy = "closed", map[string]any{"login": factoryd.MergeUser}
			}
			return map[string]any{"number": num, "state": state, "merged": g.merged != "", "merged_by": mergedBy, "base": map[string]any{"ref": "main"}, "html_url": fmt.Sprintf("https://git.example/demo-app/demo-app/pulls/%d", num),
				"head": map[string]any{"ref": "factory/t-wip", "sha": g.branch[len(g.branch)-1], "repo": map[string]any{"full_name": "factory-agent/demo-app"}}}
		}
		switch {
		case r.Method == http.MethodGet && p == repo:
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == http.MethodGet && p == repo+"/branches/main":
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, g.base)
		case r.Method == http.MethodGet && strings.HasPrefix(p, fork+"/branches/"):
			if len(g.branch) == 0 {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, g.branch[len(g.branch)-1])
		case r.Method == http.MethodGet && strings.HasPrefix(p, fork+"/git/commits/"):
			email := "factory-agent@factory.local"
			if g.wip[strings.TrimPrefix(p, fork+"/git/commits/")] {
				email = "factory-wip@factory.local"
			}
			fmt.Fprintf(w, `{"commit":{"committer":{"email":%q}},"parents":[]}`, email)
		case r.Method == http.MethodPost && p == repo+"/issues":
			g.issues++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/demo-app/demo-app/issues/%d"}`, g.issues, g.issues)
		case r.Method == http.MethodGet && fmtScan(p, repo+"/issues/%d", &n):
			io.WriteString(w, `{"state":"open"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/timeline"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(p, "/labels"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(p, "/comments") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodPatch && strings.Contains(p, "/issues/"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && p == repo+"/actions/tasks":
			io.WriteString(w, `{"workflow_runs":[],"total_count":0}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(p, fork+"/branches/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(p, "/git/trees/"):
			io.WriteString(w, `{"total_count":0,"tree":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/users/factory-agent/tokens"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.Contains(p, "/dependencies"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && p == repo+"/pulls/12/merge":
			var body struct {
				Head string `json:"head_commit_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			g.merged = body.Head
			io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/pulls/main/"):
			if len(g.prs) == 0 || g.merged != "" {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(pull(g.prs[0]))
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/compare/main...factory-agent:"):
			fmt.Fprintf(w, `{"total_commits":1,"commits":[{"sha":%q,"parents":[{"sha":%q}]}]}`, strings.TrimPrefix(p, repo+"/compare/main...factory-agent:"), g.base)
		case r.Method == http.MethodGet && p == repo+"/commits":
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(p, ".diff"):
			// the merge's receipt reads the pull request's diff (roadmap 66)
			io.WriteString(w, "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,3 @@\n package main\n \n+const pushed = true\n")
		case r.Method == http.MethodGet && fmtScan(p, repo+"/pulls/%d", &n):
			json.NewEncoder(w).Encode(pull(n))
		case r.Method == http.MethodPost && p == repo+"/pulls":
			g.prs = append(g.prs, 12)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(pull(12))
		case r.Method == http.MethodGet && p == repo+"/pulls":
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/commits/"):
			g.statuses = append(g.statuses, strings.TrimSuffix(strings.TrimPrefix(p, repo+"/commits/"), "/status"))
			io.WriteString(w, `{"state":"success","total_count":1,"statuses":[{"context":"pr / test (pull_request)","status":"success"}]}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wipPolicy is the maintenance process with the checks, the approval and the merge as its stages.
func wipPolicy() policy.Version {
	v := acceptancePolicyVersion()
	v.Tree.Processes["maintenance"] = policy.Process{
		Tracks: policy.Tracks{Direct: policy.Track{Class: policy.ClassCoder}},
		Stages: []policy.Stage{{Name: "checks", OnFindings: "agent", Enabled: true}, {Name: "approval", OnFindings: "human", Enabled: true}, {Name: "merge", OnFindings: "human", Enabled: true}},
	}
	return v
}

// The card whose first session dies after two interim pushes keeps its work:
// the second session starts from the second interim head, no pull request
// exists before the end push, one after it, the Reconcile a healed run
// would run answers the branch head, and the run takes the end head through
// the gates to the merge. The session activity is the seam: it plays the
// worker's pushes onto the git target the faked Forgejo serves.
func TestWorkSurvivesTheDeadSession(t *testing.T) {
	ctx := context.Background()
	dsn := schematest.DSN(t, "acceptance_wip")
	store, err := factoryd.OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	git := &gitForgejo{base: wipBase}
	srv := git.server(t)
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(quiet.Close)
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list"):
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/key/block":
		default:
			t.Errorf("unexpected litellm call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(litellm.Close)
	objects := &stubObjects{data: map[string][]byte{}}
	forgejo := &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw",
		Seeded: map[string]string{policy.SecretMerge: "9a8b7c6d5e4f30211203f4e5d6c7b8a998877665"}}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc", SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL: "http://factory-gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor: factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:     wipPolicy,
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			v := wipPolicy()
			return v, id == v.ID, nil
		},
		Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &acceptanceWorkflows{},
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
	runner, err := factoryd.NewRunner(cfg, mergeProject{store}, &acceptanceKube{}, forgejo, completion, objects,
		&factoryd.SupervisorModel{GatewayURL: quiet.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, factoryd.Task{ID: "t-wip", Status: "queued", Objective: "keep the work of a dead session", Repository: "demo-app/demo-app",
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: 4, SHA: wipBase, RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
		t.Fatal(err)
	}

	var inputs []factoryd.SessionInput
	var healed factoryd.RunState
	var headAtSecondStart string
	session := func(ctx context.Context, in factoryd.SessionInput) (factoryd.SessionResult, error) {
		inputs = append(inputs, in)
		if len(inputs) == 1 {
			git.interim("w1")
			git.interim("w2")
			return factoryd.SessionResult{Outcome: factoryd.OutcomeNoPush, SessionID: "s-1", Commit: "w2",
				Summary: "the pod was killed", Findings: []string{"previous session ended unfinished: the pod was killed"}}, nil
		}
		var err error
		if headAtSecondStart, err = runner.BranchHead(ctx, "t-wip"); err != nil {
			t.Errorf("BranchHead: %v", err)
		}
		if healed, err = runner.Reconcile(ctx, "t-wip"); err != nil {
			t.Errorf("Reconcile: %v", err)
		}
		git.end("c3")
		return factoryd.SessionResult{Outcome: factoryd.OutcomePushed, SessionID: "s-2", Commit: "c3", Summary: "pushed branch factory/t-wip at c3"}, nil
	}

	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestWorkflowEnvironment()
	runner.RegisterActivities(skipping{env: env, skip: factoryd.ActivityRunSession})
	env.RegisterActivityWithOptions(session, activity.RegisterOptions{Name: factoryd.ActivityRunSession})
	env.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(factoryd.SignalApproved, factoryd.TaskSignal{Author: "sebastian", PR: 12}) }, 3*time.Hour)

	env.ExecuteWorkflow("TaskWorkflow", "t-wip")
	if !env.IsWorkflowCompleted() {
		t.Fatal("the card's workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the card's workflow: %v", err)
	}
	var final string
	if err := env.GetWorkflowResult(&final); err != nil {
		t.Fatal(err)
	}
	if final != "done" {
		t.Fatalf("the card ended %q, want done", final)
	}

	if len(inputs) != 2 || inputs[1].StartRef != "w2" || inputs[1].ParentSessionID != "s-1" {
		t.Fatalf("session inputs = %+v, want the second to start from the interim head w2 with parent s-1", inputs)
	}
	if headAtSecondStart != "" {
		t.Fatalf("at the second start BranchHead = %q, want none: an interim head is no finished work", headAtSecondStart)
	}
	if healed.Head != "w2" || healed.PR.Number != 0 {
		t.Fatalf("Reconcile = head %q pr %d, want the branch head w2 and no pull request", healed.Head, healed.PR.Number)
	}
	if branch, prs := git.snapshot(); !slices.Equal(branch, []string{"w2", "c3"}) || prs != 1 {
		t.Fatalf("after the end push the branch holds %v with %d pull requests, want [w2 c3] and 1", branch, prs)
	}
	if git.mergedHead() != "c3" {
		t.Fatalf("the merge took head %q, want the end head c3", git.mergedHead())
	}
	if !slices.Contains(git.statuses, "c3") || slices.Contains(git.statuses, "w2") {
		t.Fatalf("checks asked of %v, want the end head c3 and never the interim w2", git.statuses)
	}
}

// mergeProject answers the card's project with a merge token the rig seeds;
// every other read is the real store's.
type mergeProject struct{ *factoryd.PGStore }

func (mergeProject) ProjectByRepository(_ context.Context, repository string) (factoryd.Project, bool, error) {
	return factoryd.Project{Name: "demo-app", Repository: repository, File: true, Objects: policy.ObjectsSeed,
		Tokens: policy.SeedTokens{Merge: policy.SecretMerge}}, true, nil
}

// skipping registers a runner's activities on env but one.
type skipping struct {
	env  *testsuite.TestWorkflowEnvironment
	skip string
}

func (s skipping) RegisterActivityWithOptions(a any, o activity.RegisterOptions) {
	if o.Name != s.skip {
		s.env.RegisterActivityWithOptions(a, o)
	}
}
