package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The replay harness runs one exported card through TaskWorkflow in
// Temporal's test environment, on a Postgres store, with every model role
// stubbed by its recorded answer: the sessions, the review, the audit and the
// checks come from the fixture in the order it holds them, and rung 1's chat
// and rung 2's turn from its script. The activities that decide who is
// asked (OpenTrip, EndCase, RecordGrant, AuditDraw) and everything the
// ladder reads or writes (usage, status, steps, approvals, Act) are the real
// Runner's, against a fake Forgejo that holds the pull request and the checks
// the fixture records.
//
// The task is seeded as "t1" in the project demo-app/demo-app: the fixture's
// own task id and repository are masks. The project takes the fixture's
// autonomy and merge rule. A session past the recorded ones may start only
// as often as expect.sessions counts, then ends the run; a case that reaches
// the human with no scripted answer ends it there.

const replayTask = "t1"

type fixtureCaseResult struct {
	Kind CaseKind
	Rung int // the rung that resolved it, ResolvedOpen for none, ResolvedWithdrawn for a withdrawn case
	Face *Face
}

type fixtureReplay struct {
	Cases      []fixtureCaseResult
	Sessions   []SessionInput
	HumanCases int
	Rung2Turns int
	Status     string
	Notes      []string // the note each session started with, in order
	Finals     [][2]string
	Signalled  map[string][]string // the signals sent to the other cards, by task
	WaitsOn    []int64             // the issues the card waits on at the end
}

type replayForge struct {
	mu       sync.Mutex
	prHead   string            // the head of the pull request factoryd opened for the card, "" for none
	checks   map[string]string // head -> success or failure
	failed   map[string][]FixtureFailedTest
	heads    []string // heads with failed tests, whose index names their run
	comments []string
}

func (f *replayForge) setHead(head, state string, failed []FixtureFailedTest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[head] = state
	if state == "failure" && !slices.Contains(f.heads, head) {
		f.heads = append(f.heads, head)
	}
	f.failed[head] = failed
}

func (f *replayForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const repo = "/api/v1/repos/demo-app/demo-app"
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/comments"):
			b, _ := io.ReadAll(r.Body)
			f.comments = append(f.comments, string(b))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.comments))
		case r.Method == http.MethodGet && p == repo+"/pulls/9" && f.prHead != "":
			fmt.Fprintf(w, `{"number":9,"state":"open","html_url":"https://git.example/sf/r/pulls/9","head":{"sha":%q}}`, f.prHead)
		case strings.HasPrefix(p, repo+"/commits/") && strings.HasSuffix(p, "/status"):
			head := strings.TrimSuffix(strings.TrimPrefix(p, repo+"/commits/"), "/status")
			state := f.checks[head]
			if state == "" {
				state = "pending"
			}
			target := ""
			if i := slices.Index(f.heads, head); state == "failure" && i >= 0 {
				target = fmt.Sprintf("/demo-app/demo-app/actions/runs/%d/jobs/0", 100+i)
			}
			fmt.Fprintf(w, `{"state":%q,"statuses":[{"status":%[1]q,"context":"ci / test","target_url":%q}]}`, state, target)
		case p == repo+"/actions/runs":
			var runs []string
			for i := range f.heads {
				runs = append(runs, fmt.Sprintf(`{"id":%d,"index_in_repo":%d}`, 1000+i, 100+i))
			}
			fmt.Fprintf(w, `{"workflow_runs":[%s]}`, strings.Join(runs, ","))
		case strings.HasPrefix(p, repo+"/actions/runs/") && strings.HasSuffix(p, "/jobs"):
			id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, repo+"/actions/runs/"), "/jobs"))
			fmt.Fprintf(w, `[{"id":%d,"name":"test","status":"failure","attempt":1}]`, id+1000)
		case strings.HasPrefix(p, repo+"/actions/jobs/") && strings.HasSuffix(p, "/logs"):
			id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, repo+"/actions/jobs/"), "/logs"))
			fmt.Fprint(w, f.log(f.heads[id-2000]))
		case strings.HasSuffix(p, "/commits"), strings.HasSuffix(p, "/labels"), strings.HasSuffix(p, "/comments"),
			strings.HasSuffix(p, "/dependencies"), strings.HasSuffix(p, "/blocks"), strings.HasSuffix(p, "/files"):
			fmt.Fprint(w, `[]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// log is the log go test writes for the failed tests of head.
func (f *replayForge) log(head string) string {
	var b strings.Builder
	packages := map[string]bool{}
	for _, ft := range f.failed[head] {
		if ft.Test != "" {
			fmt.Fprintf(&b, "--- FAIL: %s (0.00s)\n", ft.Test)
		}
		if ft.Package != "" && !packages[ft.Package] {
			packages[ft.Package] = true
			fmt.Fprintf(&b, "FAIL\t%s\t0.1s\n", ft.Package)
		}
	}
	return b.String()
}

// replayWorkflows delivers the signals the ladder sends the card under test
// into the test environment and records those sent to any other card, and
// the starts and reopens of runs.
type replayWorkflows struct {
	Workflows
	rig *replayRig
}

func (w replayWorkflows) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	if taskID == replayTask {
		w.rig.deliver(name, sig)
		return nil
	}
	w.rig.note(taskID + " " + name)
	return nil
}

func (w replayWorkflows) Start(_ context.Context, taskID string) error {
	w.rig.note(taskID + " start")
	return nil
}

func (w replayWorkflows) Reopen(_ context.Context, taskID, signal string, _ TaskSignal) error {
	w.rig.note(taskID + " reopen " + signal)
	return nil
}

func (w replayWorkflows) StartRoutineRun(context.Context, RoutineRunInput) error { return nil }

// replayChat answers rung 1's conversation from the script, by case kind.
type replayChat struct {
	mu      sync.Mutex
	rig     *replayRig
	used    []bool
	failure string
}

func (c *replayChat) Call(_ context.Context, _, _ string, in CheckinRequest) (CheckinResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, a := range c.rig.fx.Script.Rung1 {
		if c.used[i] || string(a.CaseKind) != c.rig.rung1Kind {
			continue
		}
		c.used[i] = true
		raw, err := json.Marshal(a.Answer)
		if err != nil {
			return CheckinResponse{}, err
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			return CheckinResponse{}, err
		}
		return CheckinResponse{ToolName: checkinAnswerTool, ToolArgs: args}, nil
	}
	c.failure = fmt.Sprintf("rung 1 was asked about a %s case and the script has no answer left for it; the statement:\n%s", c.rig.rung1Kind, in.Input)
	return CheckinResponse{}, errors.New(c.failure)
}

type replayRig struct {
	t     *testing.T
	fx    Fixture
	s     *PGStore
	r     *Runner
	forge *replayForge
	env   *testsuite.TestWorkflowEnvironment
	chat  *replayChat

	mu        sync.Mutex
	queue     []func()
	rung1Kind string
	rung2Used []bool
	sessions  []SessionInput
	finals    [][2]string
	over      bool
	quiet     bool
	tripped   bool
	errs      []string
	cancelled bool
	rung2Turn int
	signalled map[string][]string
	reviewN   map[string]int
	auditN    map[string]int
	checksN   map[string]int
}

func replayTree(t *testing.T, fx Fixture) policy.Tree {
	t.Helper()
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	body := string(files["projects/demo-app.yaml"])
	changed := strings.Replace(body, "merge_rule: human", "merge_rule: "+fx.Card.MergeRule+"\nautonomy: "+fx.Card.Autonomy, 1)
	if changed == body {
		t.Fatal("the testdata project file holds no merge_rule line to set")
	}
	if fx.Card.Plan != nil {
		// a plan's cards follow the spec process, which holds a card behind its dependencies
		withProcess := strings.Replace(changed, "process: maintenance", "process: spec", 1)
		if withProcess == changed {
			t.Fatal("the testdata project file holds no process line to set")
		}
		changed = withProcess
	}
	files["projects/demo-app.yaml"] = []byte(changed)
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func newReplayRig(t *testing.T, fx Fixture) *replayRig {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "replay"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE cases, events, intake_events, tasks, approval_requests, plan_revisions, plan_questions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	g := &replayRig{t: t, fx: fx, s: s, signalled: map[string][]string{}, reviewN: map[string]int{}, auditN: map[string]int{}, checksN: map[string]int{}, forge: &replayForge{checks: map[string]string{}, failed: map[string][]FixtureFailedTest{}}}
	v := policy.Version{ID: 7, Tree: replayTree(t, fx)}
	v.Document.Budget.SessionFloorCheapEUR, v.Document.Budget.SessionFloorFrontierEUR = 0.3, 0.3
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, id == v.ID, nil }
	cfg.Workflows = replayWorkflows{rig: g}
	cfg.Prices = aliasPrices{"small": {In: 1e-7, Out: 4e-7, MaxOutput: 32768}, "big": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}}
	objects := &fakeObjectStore{}
	g.r = &Runner{store: projectStore{s}, objects: objects, cfg: cfg, timeline: &Timeline{Objects: objects, Events: s, Store: s},
		forgejo: &Forgejo{BaseURL: g.forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"}}
	g.chat = &replayChat{rig: g, used: make([]bool, len(fx.Script.Rung1))}
	g.r.checkin = g.chat
	g.r.launchRung1 = func(run func()) {
		g.mu.Lock()
		g.queue = append(g.queue, run)
		g.mu.Unlock()
	}
	g.rung2Used = make([]bool, len(fx.Script.Rung2))
	g.seed()
	return g
}

// seed writes the card, its plan with one revision that holds it, and its
// dependencies as the fixture records them.
func (g *replayRig) seed() {
	g.t.Helper()
	ctx := context.Background()
	c := g.fx.Card
	repo := "demo-app/demo-app"
	task := Task{ID: replayTask, Kind: c.Kind, Status: "queued", Objective: c.Objective, Repository: repo, IssueNumber: c.Issue,
		Track: c.Track, Source: c.Source, Size: c.Size, BudgetEUR: c.BudgetEUR, CeilingEUR: c.CeilingEUR,
		RequiredCapabilities: []string{"coder"}}
	if c.Plan != nil {
		if err := g.s.CreateTask(ctx, Task{ID: "plan", Kind: "agent", Status: "done", Objective: "the plan", Repository: repo,
			IssueNumber: c.Plan.Issue, Track: policy.TrackPlan, PlanName: c.Plan.Name, BudgetEUR: 5}); err != nil {
			g.t.Fatalf("seed plan: %v", err)
		}
		task.Parent, task.PlanKey = "plan", "card"
		cards := []PlanCard{{Key: "card", Role: "feature", Title: "the card", Brief: "the card's work", Directories: []string{"."}, DependsOn: []string{}, Size: c.Size}}
		for i := range c.DependsOn {
			cards = append(cards, PlanCard{Key: fmt.Sprintf("dep%d", i), Role: "feature", Title: "a dependency", Brief: "its work", Directories: []string{"."}, DependsOn: []string{}, Size: "small"})
		}
		raw, err := json.Marshal(Plan{Summary: "the plan", Cards: cards})
		if err != nil {
			g.t.Fatal(err)
		}
		if _, err := g.s.AddPlanRevision(ctx, "plan", raw, "rev1"); err != nil {
			g.t.Fatalf("seed plan revision: %v", err)
		}
	}
	if factoryPRSources(c.Source) {
		// a card of a pull request factoryd opened has it on its row, at the first head the record gates
		task.PRNumber = 9
		g.forge.prHead = g.fx.Gates[0].Head
	}
	if err := g.s.CreateTask(ctx, task); err != nil {
		g.t.Fatalf("seed card: %v", err)
	}
	for i, d := range c.DependsOn {
		id := fmt.Sprintf("dep%d", i)
		dep := Task{ID: id, Kind: "agent", Status: d.Status, Objective: "a dependency", Repository: repo, IssueNumber: d.Issue, Track: policy.TrackDirect, BudgetEUR: 5}
		if c.Plan != nil {
			dep.Source, dep.Parent, dep.PlanKey = SourcePlan, "plan", id
		}
		if err := g.s.CreateTask(ctx, dep); err != nil {
			g.t.Fatalf("seed dependency: %v", err)
		}
		if Final(d.Status) {
			if _, err := g.s.Record(ctx, Event{Repository: repo, TaskID: id, IssueNumber: d.Issue, Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd,
				Detail: map[string]any{"final": d.Status, "reason": d.Why}, IdentityKey: "ended " + id}); err != nil {
				g.t.Fatalf("seed dependency end: %v", err)
			}
		}
		if _, err := g.s.db.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) VALUES ($1, $2)`, replayTask, id); err != nil {
			g.t.Fatalf("seed dependency edge: %v", err)
		}
	}
}

// fail is a failure of the replay itself, kept instead of reported when
// the rig is quiet.
func (g *replayRig) fail(format string, a ...any) {
	g.t.Helper()
	if g.quiet {
		g.mu.Lock()
		g.errs = append(g.errs, fmt.Sprintf(format, a...))
		g.mu.Unlock()
		return
	}
	g.t.Errorf(format, a...)
}

func (g *replayRig) note(s string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.signalled[strings.SplitN(s, " ", 2)[0]] = append(g.signalled[strings.SplitN(s, " ", 2)[0]], strings.SplitN(s, " ", 2)[1])
}

func (g *replayRig) deliver(name string, sig TaskSignal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.over {
		g.env.SignalWorkflow(name, sig)
	}
}

// drain runs what the trip started, in the activity's goroutine: rung 1's
// conversations, then rung 2's turns, until no rung is at work.
func (g *replayRig) drain(ctx context.Context) {
	for {
		g.mu.Lock()
		queue := g.queue
		g.queue = nil
		g.mu.Unlock()
		if len(queue) > 0 {
			for _, run := range queue {
				g.rung1Kind = g.rungKind(ctx, 1)
				run()
			}
			continue
		}
		if !g.playRung2(ctx) {
			return
		}
	}
}

// rungKind is the kind of the open case rung n has a step in progress on.
func (g *replayRig) rungKind(ctx context.Context, n int) string {
	cases, err := g.s.OpenCasesOf(ctx, replayTask)
	if err != nil {
		g.t.Errorf("open cases: %v", err)
		return ""
	}
	for _, c := range cases {
		steps, err := g.s.CaseSteps(ctx, c.ID)
		if err != nil {
			g.t.Errorf("steps: %v", err)
			continue
		}
		if _, ok := inProgress(steps, n); ok {
			return string(c.Kind)
		}
	}
	return ""
}

// playRung2 plays the turn of every case that waits on rung 2: the script's
// next answer of the case's kind, else a hand-up with nothing to add. It
// reports whether a turn was played.
func (g *replayRig) playRung2(ctx context.Context) bool {
	cases, err := g.s.OpenCasesOf(ctx, replayTask)
	if err != nil {
		g.t.Errorf("open cases: %v", err)
		return false
	}
	for _, c := range cases {
		steps, err := g.s.CaseSteps(ctx, c.ID)
		if err != nil {
			g.t.Errorf("steps: %v", err)
			return false
		}
		if _, ok := inProgress(steps, 2); !ok {
			continue
		}
		turn := FixtureRung2Answer{Answer: FixtureTool{Tool: "attach_analysis", Args: map[string]any{"found": "the factory agent read the case and has nothing to add"}}}
		for i, a := range g.fx.Script.Rung2 {
			if !g.rung2Used[i] && a.CaseKind == string(c.Kind) {
				g.rung2Used[i], turn = true, a
				break
			}
		}
		g.mu.Lock()
		g.rung2Turn++
		g.mu.Unlock()
		args := turn.Answer.Args
		str := func(k string) string { s, _ := args[k].(string); return s }
		switch turn.Answer.Tool {
		case "answer":
			act, _ := args["args"].(map[string]any)
			_, err = g.r.Act(ctx, ActInput{TaskID: replayTask, CaseID: c.ID, By: ChatUser, Via: ViaFactoryd, Rung: 2,
				Res: Resolution{Action: str("action"), Args: act, Note: str("note")}, Found: str("found")})
		default:
			act, _ := args["args"].(map[string]any)
			_, err = g.r.HandUpRung2(ctx, replayTask, c.ID, str("found"), RungTwoRecommendation{Action: str("action"), Grounds: str("grounds"), Args: act}, nil)
		}
		if err != nil {
			g.fail("rung 2's %s on a %s case: %v", turn.Answer.Tool, c.Kind, err)
		}
		return true
	}
	return false
}

func (g *replayRig) run() fixtureReplay {
	g.t.Helper()
	ctx := context.Background()
	var suite testsuite.WorkflowTestSuite
	g.env = suite.NewTestWorkflowEnvironment()
	w := TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}
	g.env.RegisterWorkflowWithOptions(w.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	g.r.RegisterActivities(g.env)
	g.stubs()
	g.env.SetWorkflowRunTimeout(90 * 24 * time.Hour)
	g.env.ExecuteWorkflow("TaskWorkflow", replayTask)
	g.mu.Lock()
	g.over = true
	g.mu.Unlock()
	if !g.env.IsWorkflowCompleted() {
		g.t.Fatal("workflow did not complete")
	}
	out := g.result(ctx)
	if err := g.env.GetWorkflowError(); err != nil {
		var timeout *temporal.TimeoutError
		var cancelled *temporal.CanceledError
		switch {
		case errors.As(err, &cancelled) && g.cancelled:
		case errors.As(err, &timeout) && len(out.Cases) > 0 && out.Cases[len(out.Cases)-1].Face != nil:
		default:
			g.fail("the workflow ended in an error the replay did not cause: %v", err)
		}
	}
	if g.chat.failure != "" {
		g.fail("%s", g.chat.failure)
	}
	return out
}

// endRun ends the run: the workflow is cancelled, the end the replay
// accepts.
func (g *replayRig) endRun() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.cancelled {
		g.cancelled = true
		g.env.CancelWorkflow()
	}
}

const replayEnds = "the replay ends here"

func (g *replayRig) result(ctx context.Context) fixtureReplay {
	g.t.Helper()
	out := fixtureReplay{Sessions: g.sessions, Finals: g.finals, Rung2Turns: g.rung2Turn, Signalled: g.signalled}
	for _, in := range g.sessions {
		out.Notes = append(out.Notes, replayNote(in))
	}
	cases, err := g.s.CasesOf(ctx, replayTask)
	if err != nil {
		g.t.Fatal(err)
	}
	repo := "demo-app/demo-app"
	desk, err := g.s.CaseDesk(ctx, repo)
	if err != nil {
		g.t.Fatal(err)
	}
	task, _, err := g.s.Task(ctx, replayTask)
	if err != nil {
		g.t.Fatal(err)
	}
	out.Status = task.Status
	deps, err := g.s.Dependencies(ctx, replayTask)
	if err != nil {
		g.t.Fatal(err)
	}
	for _, d := range deps {
		out.WaitsOn = append(out.WaitsOn, d.Issue)
	}
	for _, c := range cases {
		res := fixtureCaseResult{Kind: c.Kind, Rung: ResolvedOpen}
		if c.Status == CaseResolved && c.Resolution != nil {
			res.Rung = c.Resolution.Rung
		}
		if c.Status == CaseWithdrawn {
			res.Rung = ResolvedWithdrawn
		}
		atHuman := c.Status == CaseAtHuman || c.Status == CaseResolved && c.Resolution != nil && c.Resolution.Rung == 3
		if c.Status == CaseAtHuman {
			head, _ := c.Facts["head"].(string)
			model, _ := c.Facts["model"].(string)
			f, err := g.r.tripFacts(ctx, task, head, model)
			if err != nil {
				g.t.Fatal(err)
			}
			f.atCase(c)
			face := CaseFace(c, desk.Steps[c.ID], f, desk.Asks[c.ID], nil)
			res.Face = &face
		}
		if atHuman {
			out.HumanCases++
		}
		out.Cases = append(out.Cases, res)
	}
	return out
}

// stubs puts the fixture's recorded answers in place of the model roles.
func (g *replayRig) stubs() {
	ok := func(name string, n int, fn any) {
		args := make([]any, n)
		for i := range args {
			args[i] = mock.Anything
		}
		g.env.OnActivity(name, args...).Return(fn)
	}
	ok(ActivityRunSession, 2, g.runSession)
	ok(ActivityOpenPullRequest, 3, func(ctx context.Context, taskID, sha string) (PullRequest, error) {
		_, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = $1`, taskID)
		return PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, err
	})
	ok(ActivitySessionAnswered, 2, func(ctx context.Context, in SessionAnswerInput) (bool, error) {
		n, _ := strconv.Atoi(strings.TrimPrefix(in.SessionID, "s"))
		return n >= 1 && n <= len(g.fx.Sessions) && g.fx.Sessions[n-1].Calls > 0, nil
	})
	ok(ActivityWaitChecks, 3, g.waitChecks)
	ok(ActivityModelReview, 8, g.modelReview)
	ok(ActivityGateAudit, 6, g.gateAudit)
	ok(ActivityGatePolicy, 5, func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
		gate := PolicyGate{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2, MergeRule: g.fx.Card.MergeRule}
		if rec, ok := g.gatesOf("policy")[commit]; ok {
			gate.Class = rec[len(rec)-1].Verdict
			if gate.Class == "migration" {
				gate.Migrations = []string{"internal/schema/migrations/factoryd/20261108120000_metrics_identities.sql"}
			}
		}
		return gate, nil
	})
	ok(ActivityGateMigration, 4, func(ctx context.Context, taskID string, pr int64, migrations []string) error { return nil })
	ok(ActivityGateReproduction, 4, func(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
		return ReproResult{}, nil
	})
	ok(ActivityGateCriteria, 4, func(ctx context.Context, taskID string, pr int64, commit string) (CriteriaResult, error) {
		return CriteriaResult{}, nil
	})
	ok(ActivityHoldoutSettlement, 6, func(ctx context.Context, taskID string, pin, pr int64, commit string, reds []HoldoutGateResult) (HoldoutSettlement, error) {
		return HoldoutSettlement{Settled: true}, nil
	})
	ok(ActivityGateCompanion, 2, func(ctx context.Context, taskID string) ([]CompanionGate, error) { return nil, nil })
	ok(ActivityRerunChecks, 3, func(ctx context.Context, taskID, sha string) (string, error) { return sha, nil })
	ok(ActivityFinish, 4, func(ctx context.Context, taskID, final, reason string) error {
		g.mu.Lock()
		g.finals = append(g.finals, [2]string{final, reason})
		g.mu.Unlock()
		return nil
	})
	ok(ActivityOpenTrip, 2, func(ctx context.Context, in TripInput) (TripOutcome, error) {
		out, err := g.r.openTrip(ctx, in)
		g.drain(ctx)
		g.mu.Lock()
		g.tripped = true
		g.mu.Unlock()
		return out, err
	})
	// a card that waits on its dependencies again after a trip waits on what
	// the trip put in their place: the run ends there
	ok(ActivityDependencies, 2, func(ctx context.Context, taskID string) (DepState, error) {
		st, err := g.r.Dependencies(ctx, taskID)
		g.mu.Lock()
		again := g.tripped
		g.mu.Unlock()
		if again && len(st.Waiting) > 0 {
			g.endRun()
		}
		return st, err
	})
}

// runSession answers the nth session with the nth recorded one. The first
// session past the record starts only while expect.sessions counts it, and
// the run ends there; any other fails the test.
func (g *replayRig) runSession(ctx context.Context, in SessionInput) (SessionResult, error) {
	g.mu.Lock()
	g.sessions = append(g.sessions, in)
	n := len(g.sessions)
	g.mu.Unlock()
	if n > len(g.fx.Sessions) {
		if g.fx.Expect == nil || n > g.fx.Expect.Sessions {
			g.fail("the ladder started a session the record does not have (session %d of %d recorded)", n, len(g.fx.Sessions))
		}
		g.endRun()
		return SessionResult{}, temporal.NewNonRetryableApplicationError(replayEnds, "replay", nil)
	}
	s := g.fx.Sessions[n-1]
	id := fmt.Sprintf("s%d", n)
	res := SessionResult{Outcome: SessionOutcome(s.Outcome), SessionID: id, Commit: s.Commit, Summary: s.Summary}
	if s.CheckIn != nil {
		res.CheckIn = &CheckInOutcome{Decision: s.CheckIn.Decision, ReasonKind: strings.ReplaceAll(s.CheckIn.ReasonKind, " ", "_"), Reason: s.CheckIn.Reason,
			Record: &CheckInRecord{Moment: s.CheckIn.Moment, Session: id, Decision: s.CheckIn.Decision, Reason: s.CheckIn.Reason}}
		if res.Outcome == OutcomeCheckinHuman {
			res.Reason = s.CheckIn.Reason
		}
	} else if res.Outcome == OutcomeCheckinHuman {
		res.Reason = s.Summary
		res.CheckIn = &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: s.Summary,
			Record: &CheckInRecord{Moment: CheckInSession, Session: id, Decision: CheckHuman, ReasonKind: CheckSpend, Reason: s.Summary}}
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1`, replayTask, s.CostEUR); err != nil {
		return SessionResult{}, err
	}
	if res.Outcome == OutcomePushed {
		if err := g.r.timeline.Append(ctx, replayTask, "pushed", map[string]any{"session": id, "commit": s.Commit}); err != nil {
			return SessionResult{}, err
		}
		if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: replayTask, IssueNumber: g.fx.Card.Issue, Kind: EventSessionPushed,
			Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"session": id, "commit": s.Commit}, IdentityKey: "pushed " + id}); err != nil {
			return SessionResult{}, err
		}
	}
	return res, nil
}

// replayNote is the note a session started with: its handover, else the
// newest answer it carries.
func replayNote(in SessionInput) string {
	if in.Handover != "" {
		return in.Handover
	}
	if n := len(in.Answers); n > 0 {
		return in.Answers[n-1]
	}
	return ""
}

// served is the nth call's record on head: the head's nth record, its
// newest once the calls pass them, and a test failure for a head the
// record does not have.
func replayServed[T any](g *replayRig, what, head string, byHead map[string][]T, calls map[string]int) (T, bool) {
	g.mu.Lock()
	calls[head]++
	n := calls[head]
	g.mu.Unlock()
	list := byHead[head]
	if len(list) == 0 {
		var zero T
		g.fail("the ladder asked for %s of head %s, which the record does not have", what, head)
		g.endRun()
		return zero, false
	}
	return list[min(n, len(list))-1], true
}

func (g *replayRig) waitChecks(ctx context.Context, taskID, sha string) (ChecksResult, error) {
	byHead := map[string][]FixtureChecks{}
	for _, c := range g.fx.Checks {
		byHead[c.Head] = append(byHead[c.Head], c)
	}
	c, ok := replayServed(g, "checks", sha, byHead, g.checksN)
	if !ok {
		return ChecksResult{}, temporal.NewNonRetryableApplicationError(replayEnds, "replay", nil)
	}
	state := ChecksSuccess
	if c.State != "success" {
		state = ChecksFailure
	}
	g.forge.setHead(sha, c.State, c.FailedTests)
	if err := g.r.timeline.Append(ctx, taskID, "checks", map[string]any{"commit": sha, "state": c.State, "findings": replayStrings(c.Findings)}); err != nil {
		return ChecksResult{}, err
	}
	return ChecksResult{State: state, Findings: c.Findings}, nil
}

func replayStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// gatesOf are the recorded results of gate by head, in order.
func (g *replayRig) gatesOf(gate string) map[string][]FixtureGate {
	out := map[string][]FixtureGate{}
	for _, r := range g.fx.Gates {
		if r.Gate == gate {
			out[r.Head] = append(out[r.Head], r)
		}
	}
	return out
}

func replayTexts(r FixtureGate) []string {
	texts := []string{}
	for _, f := range r.Findings {
		texts = append(texts, f.Text)
	}
	return texts
}

func (g *replayRig) modelReview(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults, earlier []Review, round int) (Review, error) {
	rec, ok := replayServed(g, "a review", commit, g.gatesOf("review"), g.reviewN)
	if !ok {
		return Review{}, temporal.NewNonRetryableApplicationError(replayEnds, "replay", nil)
	}
	rv := Review{Verdict: rec.Verdict, Summary: rec.NotRun, Findings: replayTexts(rec)}
	if rec.Verdict == "" {
		rv.Note = rec.NotRun
	}
	fact := ReviewAttemptFact{PR: pr.Number, Commit: commit, Round: round, Model: "big", Verdict: rec.Verdict, Note: rec.NotRun}
	if _, err := g.s.Record(ctx, fact.Event(taskID, "demo-app/demo-app", g.fx.Card.Issue)); err != nil {
		return Review{}, err
	}
	if err := g.r.timeline.Append(ctx, taskID, "model_review", map[string]any{"commit": commit, "verdict": rec.Verdict, "findings": rv.Findings}); err != nil {
		return Review{}, err
	}
	return rv, nil
}

func (g *replayRig) gateAudit(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults, round int) (AuditResult, error) {
	rec, ok := replayServed(g, "an audit", commit, g.gatesOf("audit"), g.auditN)
	if !ok {
		return AuditResult{}, temporal.NewNonRetryableApplicationError(replayEnds, "replay", nil)
	}
	res := AuditResult{Verdict: rec.Verdict}
	for _, f := range rec.Findings {
		res.Findings = append(res.Findings, AuditFinding{Item: f.Text})
	}
	if err := g.r.timeline.Append(ctx, taskID, "audit", map[string]any{"commit": commit, "verdict": rec.Verdict, "findings": res.Findings}); err != nil {
		return AuditResult{}, err
	}
	return res, nil
}

// The expects of the batch fixtures, by hand from the card's record and the
// design of the card flow, before any replay:
//
//   - 1454, a plan child whose first session the check-in stopped for its
//     size (27550 changed lines against the 1150 its size plans): the plan
//     takes the work back, so no case opens, the plan is reopened (the signal
//     the plan receives) and the card ends closed after its one session.
//   - 1414 and 1462, cards whose first push drew review concerns and whose
//     next session pushed a head the review approved: no stop ever opens a case,
//     and the run ends at the merge approval of the second head, a merge
//     the merge rule leaves to a human, after two sessions, with the card in review.
//   - 1333, a revert card of a pull request factoryd opened, with no session:
//     its review was refused by the gateway (a price error), so the merge
//     approval, the only case, waits at the human and its why names the refusal,
//     as the approval of a merge whose review did not run must.
var replayFixtureNames = []string{"1347", "1410", "1452", "1381", "1454", "1414", "1462", "1333"}

// replayFixture runs fx through the ladder.
func replayFixture(t *testing.T, fx Fixture) fixtureReplay {
	t.Helper()
	return newReplayRig(t, fx).run()
}

// replayMismatch lists what the run did that the fixture's expect does not
// say: the rung that resolved each case, the sessions, the human cases, each
// face's substrings, the turns of rung 2, the card's status and the end of
// the last session's note.
func replayMismatch(fx Fixture, run fixtureReplay) []string {
	x := fx.Expect
	if x == nil {
		return []string{"the fixture has no expect"}
	}
	var out []string
	rungs := []int{}
	for _, c := range run.Cases {
		rungs = append(rungs, c.Rung)
	}
	if !slices.Equal(rungs, append([]int{}, x.ResolvedByRung...)) {
		out = append(out, fmt.Sprintf("resolved_by_rung = %v, want %v", rungs, x.ResolvedByRung))
	}
	if len(run.Sessions) != x.Sessions {
		out = append(out, fmt.Sprintf("sessions = %d, want %d", len(run.Sessions), x.Sessions))
	}
	if run.HumanCases != x.HumanCases {
		out = append(out, fmt.Sprintf("human_cases = %d, want %d", run.HumanCases, x.HumanCases))
	}
	if run.Rung2Turns != x.Rung2Turns {
		out = append(out, fmt.Sprintf("rung2_turns = %d, want %d", run.Rung2Turns, x.Rung2Turns))
	}
	final := ""
	if len(run.Finals) > 0 {
		final = run.Finals[0][0]
	}
	if final != x.Final {
		out = append(out, fmt.Sprintf("the card ended %q, want %q", run.Finals, x.Final))
	}
	if x.Status != "" && run.Status != x.Status {
		out = append(out, fmt.Sprintf("status = %q, want %q", run.Status, x.Status))
	}
	if x.NoteSuffix != "" {
		note := ""
		if n := len(run.Notes); n > 0 {
			note = run.Notes[n-1]
		}
		if !strings.HasSuffix(note, x.NoteSuffix) {
			out = append(out, fmt.Sprintf("the last session's note = %q, want it to end with %q", note, x.NoteSuffix))
		}
	}
	if x.WaitsOn != nil && !slices.Equal(run.WaitsOn, x.WaitsOn) {
		out = append(out, fmt.Sprintf("waits_on = %v, want %v", run.WaitsOn, x.WaitsOn))
	}
	if x.Signalled != nil && !reflect.DeepEqual(run.Signalled, x.Signalled) {
		out = append(out, fmt.Sprintf("signalled = %v, want %v", run.Signalled, x.Signalled))
	}
	var faces []Face
	for _, c := range run.Cases {
		if c.Face != nil {
			faces = append(faces, *c.Face)
		}
	}
	if len(faces) != len(x.Face) {
		return append(out, fmt.Sprintf("%d faces reached the human, want %d", len(faces), len(x.Face)))
	}
	for i, want := range x.Face {
		for part, got := range map[string][2]string{"ask": {faces[i].Ask, want.Ask}, "why": {faces[i].Why, want.Why}, "recommendation": {faces[i].Recommendation, want.Recommendation}} {
			if !strings.Contains(got[0], got[1]) {
				out = append(out, fmt.Sprintf("face %d %s = %q, want it to hold %q", i, part, got[0], got[1]))
			}
		}
	}
	return out
}

// A card of one pushed session whose review approves, in a project whose
// merge rule is human: the run ends at the merge approval, which no rung
// takes. By hand: one case of kind merge_approval, open at the human (rung
// ResolvedOpen), one session, one human case, whose ask names the merge
// approval; no rung 2 turn; the card in review.
func syntheticFixture() Fixture {
	return Fixture{
		Card: FixtureCard{Issue: 7, TaskID: "t-x", Repository: "owner/repo", Kind: "agent", Track: "direct", Source: "webhook", Size: "small",
			BudgetEUR: 5, Objective: "a card of one session", Autonomy: "manual", MergeRule: "human"},
		Sessions: []FixtureSession{{Attempt: 1, Outcome: "pushed", Commit: "c1", Pushed: true, CostEUR: 0.2, Calls: 10, AgentTimeS: 300, Summary: "pushed c1"}},
		Gates:    []FixtureGate{{Head: "c1", Gate: "review", Verdict: "approve"}},
		Checks:   []FixtureChecks{{Head: "c1", State: "success"}},
		Expect:   &FixtureExpect{ResolvedByRung: []int{ResolvedOpen}, Sessions: 1, HumanCases: 1, Face: []FixtureFace{{Ask: "merge approval", Why: "The merge gate classes the change normal"}}, Status: "in-review"},
	}
}

// A card whose three sessions push, each head's review raising a finding,
// reaches its session cap of 3 at the last round. Rung 1's scripted extend
// of one session resolves the cap's case; the fourth session starts and
// ends the run. By hand: one case, session_cap, resolved at rung 1; four
// sessions (the three recorded, the fourth past them); no human case; no
// rung 2 turn.
func capFixture() Fixture {
	fx := Fixture{
		Card: FixtureCard{Issue: 8, TaskID: "t-y", Repository: "owner/repo", Kind: "agent", Track: "direct", Source: "webhook", Size: "small",
			BudgetEUR: 5, Objective: "a card that reaches its cap", Autonomy: "manual", MergeRule: "human"},
		Expect: &FixtureExpect{ResolvedByRung: []int{1}, Sessions: 4, HumanCases: 0, Face: []FixtureFace{}},
	}
	for i, c := range []string{"c1", "c2", "c3"} {
		fx.Sessions = append(fx.Sessions, FixtureSession{Attempt: i + 1, Outcome: "pushed", Commit: c, Pushed: true, CostEUR: 0.2, Calls: 10, AgentTimeS: 300, Summary: "pushed " + c})
		fx.Gates = append(fx.Gates, FixtureGate{Head: c, Gate: "review", Verdict: "concerns", Findings: []FixtureFinding{{Text: "the flag is unchecked in " + c}}})
		fx.Checks = append(fx.Checks, FixtureChecks{Head: c, State: "success"})
	}
	fx.Script.Rung1 = []FixtureRung1Answer{{CaseKind: "session_cap", Answer: RungAnswer{Decision: "extend", ReasonKind: "session_cap",
		Verdicts: map[string]RungVerdict{findingID("the flag is unchecked in c3"): {Verdict: "confirmed", Evidence: "main.go:3 at c3: the flag is read unchecked"}},
		Note:     "check the flag", Recommendation: RungRecommendation{Action: "extend", Grounds: "one more session fixes the finding"}, Draw: &RungDraw{Sessions: 1}}}}
	return fx
}

// The cap's card with rung 2's scripted answer: by hand, rung 1 is asked
// first, so the case reaches rung 2 by rung 1's hand-up. Rung 1 hands up
// recommending extend; rung 2 answers extend with one session; at manual
// autonomy rung 2 proposes, so the case reaches the human with the
// proposal: one human case, rung 2 took one turn, three sessions.
func rung2Fixture() Fixture {
	fx := capFixture()
	fx.Script.Rung1[0].Answer = RungAnswer{Decision: "hand_up", ReasonKind: "session_cap",
		Verdicts: map[string]RungVerdict{findingID("the flag is unchecked in c3"): {Verdict: "confirmed", Evidence: "main.go:3 at c3: the flag is read unchecked"}},
		Note:     "a human or the factory agent decides", Recommendation: RungRecommendation{Action: "extend", Grounds: "one session fixes it"}}
	fx.Script.Rung2 = []FixtureRung2Answer{{CaseKind: "session_cap", Answer: FixtureTool{Tool: "answer",
		Args: map[string]any{"action": "extend", "args": map[string]any{"sessions": 1}, "note": "one more session", "found": "the finding stands"}}}}
	fx.Expect = &FixtureExpect{ResolvedByRung: []int{ResolvedOpen}, Sessions: 3, HumanCases: 1, Face: []FixtureFace{{Ask: "session cap"}}, Rung2Turns: 1}
	return fx
}

// The twin of #1347, hand-built: a migration-class head whose first review
// raises a concern and whose second, on the re-verify, approves. By hand: the
// push c1 is reviewed (concerns), the second session stops on the check-in's
// policy class, rung 0 routes it (case 1, rung 0), the re-verify's review of
// c1 approves, and the merge approval opens at the human (case 2, open): two
// sessions, one human case whose why names the migration class.
func migrationTwinFixture() Fixture {
	return Fixture{
		Card: FixtureCard{Issue: 9, TaskID: "t-z", Repository: "owner/repo", Kind: "agent", Track: "direct", Source: "webhook", Size: "small",
			BudgetEUR: 5, Objective: "hand-built twin of #1347: a migration-class head whose re-review approves", Autonomy: "manual", MergeRule: "human"},
		Sessions: []FixtureSession{
			{Attempt: 1, Outcome: "pushed", Commit: "c1", Pushed: true, CostEUR: 0.2, Calls: 10, AgentTimeS: 300, Summary: "pushed c1"},
			{Attempt: 2, Outcome: "checkin_human", CostEUR: 0.1, Calls: 5, AgentTimeS: 100, Summary: "the migration needs a human",
				CheckIn: &FixtureCheckIn{Moment: "session", Decision: "human", ReasonKind: "policy class", Reason: "the migration needs a human to approve it"}},
		},
		Gates: []FixtureGate{
			{Head: "c1", Gate: "policy", Verdict: "migration"},
			{Head: "c1", Gate: "review", Verdict: "concerns", Findings: []FixtureFinding{{Text: "the flag is unchecked"}}},
			{Head: "c1", Gate: "review", Verdict: "approve"},
		},
		Checks: []FixtureChecks{{Head: "c1", State: "success"}},
		Expect: &FixtureExpect{ResolvedByRung: []int{0, ResolvedOpen}, Sessions: 2, HumanCases: 1,
			Face: []FixtureFace{{Ask: "merge approval", Why: "migration"}}},
	}
}

// The harness asserts exactly: each hand-derived expect holds, and each part
// of it one off fails.
func TestReplayHarnessAssertsTheExpectExactly(t *testing.T) {
	for name, tc := range map[string]struct {
		fx     Fixture
		mutate map[string]func(x *FixtureExpect)
	}{
		"the merge approval": {syntheticFixture(), map[string]func(x *FixtureExpect){
			"a case resolved at rung 0": func(x *FixtureExpect) { x.ResolvedByRung = []int{0} },
			"one session more":          func(x *FixtureExpect) { x.Sessions = 2 },
			"no human case":             func(x *FixtureExpect) { x.HumanCases, x.Face = 0, nil },
			"another face":              func(x *FixtureExpect) { x.Face = []FixtureFace{{Ask: "how should it go on?"}} },
			"another status":            func(x *FixtureExpect) { x.Status = "waiting-human" },
		}},
		"the migration twin": {migrationTwinFixture(), map[string]func(x *FixtureExpect){
			"resolved at rung 1": func(x *FixtureExpect) { x.ResolvedByRung = []int{1, ResolvedOpen} },
			"another class":      func(x *FixtureExpect) { x.Face = []FixtureFace{{Ask: "merge approval", Why: "normal"}} },
			"no human case":      func(x *FixtureExpect) { x.HumanCases, x.Face = 0, nil },
		}},
		"rung 1 resolves the cap": {capFixture(), map[string]func(x *FixtureExpect){
			"resolved at rung 0": func(x *FixtureExpect) { x.ResolvedByRung = []int{0} },
			"resolved at rung 2": func(x *FixtureExpect) { x.ResolvedByRung = []int{2} },
			"three sessions":     func(x *FixtureExpect) { x.Sessions = 3 },
		}},
		"rung 2 takes a turn": {rung2Fixture(), map[string]func(x *FixtureExpect){
			"no rung 2 turn":     func(x *FixtureExpect) { x.Rung2Turns = 0 },
			"resolved at rung 2": func(x *FixtureExpect) { x.ResolvedByRung, x.HumanCases, x.Face = []int{2}, 0, []FixtureFace{} },
		}},
	} {
		t.Run(name, func(t *testing.T) {
			run := replayFixture(t, tc.fx)
			if got := replayMismatch(tc.fx, run); len(got) != 0 {
				t.Fatalf("the hand-derived expect does not hold: %v", got)
			}
			for what, mutate := range tc.mutate {
				x := *tc.fx.Expect
				mutate(&x)
				wrong := tc.fx
				wrong.Expect = &x
				if got := replayMismatch(wrong, run); len(got) == 0 {
					t.Errorf("%s: the run matched an expect one off", what)
				}
			}
		})
	}
}

// A session past the record and past the expect fails the test, and the run
// ends there.
func TestReplayHarnessRefusesASessionTheRecordDoesNotHave(t *testing.T) {
	fx := capFixture()
	fx.Expect.Sessions = 3
	g := newReplayRig(t, fx)
	g.quiet = true
	run := g.run()
	if len(g.errs) != 1 || !strings.Contains(g.errs[0], "started a session the record does not have (session 4 of 3 recorded)") {
		t.Errorf("failures = %q, want the one session past the record", g.errs)
	}
	if len(run.Sessions) != 4 {
		t.Errorf("sessions = %d, want the run to end at the fourth", len(run.Sessions))
	}
}

// The five cards of the redesign's first fixtures run through the ladder;
// each file's expect is derived by hand in the comment above its name.
func TestReplayFixtures(t *testing.T) {
	for _, name := range replayFixtureNames {
		t.Run(name, func(t *testing.T) {
			fx, err := LoadFixture("testdata/fixtures/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			run := replayFixture(t, fx)
			if mismatch := replayMismatch(fx, run); len(mismatch) > 0 {
				t.Fatalf("the replay: %v", mismatch)
			}
			x := *fx.Expect
			if x.ResolvedByRung = slices.Clone(x.ResolvedByRung); len(x.ResolvedByRung) == 0 {
				x.ResolvedByRung = []int{0}
			} else {
				x.ResolvedByRung[0]++
			}
			fx.Expect = &x
			if len(replayMismatch(fx, run)) == 0 {
				t.Error("the run matched an expect whose resolved_by_rung is one off")
			}
		})
	}
}

// replayCoverage maps every stuck point (S) and incident (I) of the card flow
// registers to what proves its construct: a fixture file, the name of an
// existing test, or "open: <reason>".
var replayCoverage = map[string]string{
	"I1":  "TestFilePlanRefileOfTheNewestIsANoop",
	"I2":  "open: declined, the ask quotes the refusal instead",
	"I3":  "open: a batch fixture of a press whose label names money",
	"I4":  "open: a chat turn, no card run",
	"I5":  "TestIdenticalPlanRefileResolvesItsCase",
	"I6":  "testdata/fixtures/1347.json",
	"I7":  "TestCaseFaceBudgetFloorOnAGreenHeadAsksTheMoney",
	"I8":  "TestVerifyReturnsTheAdoptedHead",
	"I9":  "open: a batch fixture of a planner session that died on its read set",
	"I10": "internal/console TestChatModelRefusal",
	"I11": "internal/console TestMonthMeterReadsTheEnforcedSpend",
	"I12": "TestMonthAlarmNamesTheOuterGuard",
	"I13": "open: a batch fixture of a review refused for money",
	"I14": "internal/console TestChatCompactionHalvesAnEmptySummariser",
	"I15": "open: a batch fixture of a revert card behind a red main",
	"I16": "testdata/fixtures/1452.json",
	"I17": "testdata/fixtures/1410.json",
	"I18": "open: a batch fixture of an alert outliving its card",
	"I19": "internal/console TestMCPDoorAdmitsAWriterOfTheProjectAlone",
	"I20": "internal/policy TestLoaderCanonicalisesPolicyChatAndClassTools",
	"S1":  "open: a wait at the human has no clock, by decision",
	"S2":  "open: a wait at the human has no clock, by decision",
	"S3":  "TestCapWaitsWake",
	"S4":  "open: the plan wait has no clock, by decision; a batch fixture takes a plan's question",
	"S5":  "open: no fix is built, a human-lane card has no workflow; TestRollHeldCaseHoldsThePlansStates holds a roll's case, not this",
	"S6":  "TestOpenTripRetriesAFailedDependencyOnce",
	"S7":  "open: the impasse's refusal is tested for its words, not for the dependents it leaves waiting, and no fix is built",
	"S8":  "open: the chained wait of a card moved onto its plan card has no clock and no face; the monitor of the design builds them",
	"S9":  "testdata/fixtures/1452.json",
	"S10": "open: by design, a merged dependency waits for its green chain run",
	"S11": "open: a batch fixture of a leaf card that failed with no dependents",
	"S12": "open: a batch fixture of a plan child's successor",
	"S13": "TestHealSignalsWaitingCards",
	"S14": "TestAdmitRecordsTheRefusalAndClearsItOnAdmission",
	"S15": "open: a batch fixture of five cards at approval holding the work bound",
	"S16": "open: the pause and the fix-forward card are tested, not admission filling behind the paused queue",
	"S17": "open: a plan child whose issue never linked has no run to replay",
	"S18": "open: a batch fixture of a deadlock ask answered overrule",
	"S19": "testdata/fixtures/1381.json",
	"S20": "open: a batch fixture of a repeated stop after a restart",
	"S21": "TestRunSessionCheckinJudgmentGateMomentRunsOnTheChatTask",
	"S22": "testdata/fixtures/1347.json",
	"S23": "open: the audit refuses an unfunded card, but the plan loop and the report sessions open sessions the budget cannot pay, and no fix covers them",
	"S24": "open: by decision, a done integration card re-runs no review",
	"S25": "open: a batch fixture of a stop whose recommendation a rung follows",
	"S26": "TestPassLoopBoundsARunAndFlagsIt",
	"S27": "TestReplayOfAHistoryWithoutTheCasesMarkerIsRefused",
	"S28": "open: a batch fixture of a run restarted on a closing card",
	"S29": "open: TestCaseSignalsWaitInTheStoreUntilASendDelivers proves a case's signal, not a signal to a completed workflow, which is dropped",
	"S30": "open: an untrusted commenter's answer is no card run",
	"S31": "open: a batch fixture of a report card whose gate supervisor escalates",
	"S32": "open: no fix, an Escalate that cannot post for 24 hours still ends its card failed; TestAFailedFaceAtTheDueKeepsTheWake proves the ladder's wake, not that",
	"S33": "internal/policy TestLoaderCanonicalisesPolicyChatAndClassTools",
	"S34": "open: the planner's read set is a hand step on the infra tree",
	"S35": "TestWorkflowReplaysDevServerHistories",
	"S36": "TestFileChildrenCarriesTheRaisesOfTheCardItContinues",
	"S37": "TestSizeGateBudgetMoveRecordsAFund",
	"S38": "open: a batch fixture of an answer on a failed card",
	"S39": "open: dormant under the shipped process",
}

// registerProblems is every way rows breaks the register's rules for ids: a
// row per id and no other, and a proof that is "open: <reason>", a fixture
// file, a test name declared in this package's test files, or a package
// path from the module root and a test name declared in that package's.
func registerProblems(rows map[string]string, ids []string) []string {
	declared := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	read := map[string]map[string]bool{}
	testsOf := func(dir string) (map[string]bool, error) {
		if tests, ok := read[dir]; ok {
			return tests, nil
		}
		files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil || len(files) == 0 {
			return nil, fmt.Errorf("no test files in %s", dir)
		}
		tests := map[string]bool{}
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			for _, m := range declared.FindAllSubmatch(src, -1) {
				tests[string(m[1])] = true
			}
		}
		read[dir] = tests
		return tests, nil
	}
	var out []string
	for _, id := range ids {
		proof, ok := rows[id]
		pkg, name, other := strings.Cut(proof, " ")
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s is in no row of the register", id))
		case strings.HasPrefix(proof, "open: "):
		case strings.HasPrefix(proof, "testdata/fixtures/"):
			if _, err := os.Stat(proof); err != nil {
				out = append(out, fmt.Sprintf("%s names the fixture %s: %v", id, proof, err))
			}
		case other:
			tests, err := testsOf(filepath.Join("..", "..", pkg))
			if err != nil {
				out = append(out, fmt.Sprintf("%s names the package %s: %v", id, pkg, err))
			} else if !tests[name] {
				out = append(out, fmt.Sprintf("%s names the test %s, which no test file of %s declares", id, name, pkg))
			}
		default:
			tests, err := testsOf(".")
			if err != nil {
				out = append(out, err.Error())
			} else if !tests[proof] {
				out = append(out, fmt.Sprintf("%s names the test %s, which no test file declares", id, proof))
			}
		}
	}
	if len(rows) != len(ids) {
		out = append(out, fmt.Sprintf("the register has %d rows, want %d: a row names no stuck point or incident", len(rows), len(ids)))
	}
	return out
}

func registerIDs() []string {
	var ids []string
	for n := 1; n <= 39; n++ {
		ids = append(ids, fmt.Sprintf("S%d", n))
	}
	for n := 1; n <= 20; n++ {
		ids = append(ids, fmt.Sprintf("I%d", n))
	}
	return ids
}

func TestReplayCoverageNamesEveryStuckPointAndIncident(t *testing.T) {
	for _, p := range registerProblems(replayCoverage, registerIDs()) {
		t.Error(p)
	}
}

// A row naming a test that no test file declares, in this package or in
// another, a package without test files and a fixture that is gone are all
// refused.
func TestReplayCoverageRefusesAMissingTest(t *testing.T) {
	rows := map[string]string{}
	for id, proof := range replayCoverage {
		rows[id] = proof
	}
	rows["S1"] = "TestNoSuchTestAnywhere"
	rows["S2"] = "internal/console TestNoSuchTestAnywhere"
	rows["S3"] = "internal/nosuchpackage TestChatModelRefusal"
	rows["S4"] = "testdata/fixtures/none.json"
	rows["I1"] = "internal/policy TestChatModelRefusal"
	got := strings.Join(registerProblems(rows, registerIDs()), "\n")
	for _, want := range []string{
		"S1 names the test TestNoSuchTestAnywhere, which no test file declares",
		"S2 names the test TestNoSuchTestAnywhere, which no test file of internal/console declares",
		"S3 names the package internal/nosuchpackage: no test files in ../../internal/nosuchpackage",
		"S4 names the fixture testdata/fixtures/none.json",
		"I1 names the test TestChatModelRefusal, which no test file of internal/policy declares",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the register's refusals lack %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "\n") + 1; n != 5 {
		t.Errorf("the register refused %d things, want the 5 planted:\n%s", n, got)
	}
}

func (w replayWorkflows) Terminate(context.Context, string, string, string) error { return nil }

func (w replayWorkflows) Alive(context.Context, string) (bool, error)       { return true, nil }
func (w replayWorkflows) RunningRuns(context.Context) ([]RunningRun, error) { return nil, nil }
