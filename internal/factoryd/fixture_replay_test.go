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
	Rung int // the rung that resolved it, ResolvedOpen for none
	Face *Face
}

type fixtureReplay struct {
	Cases      []fixtureCaseResult
	Sessions   []SessionInput
	HumanCases int
	Finals     [][2]string
	Notes      []string // the notes the sessions after a case started with
}

type replayForge struct {
	mu       sync.Mutex
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

// replayWorkflows delivers the signals the ladder sends a run into the test
// environment; Start and the rest are never called.
type replayWorkflows struct {
	Workflows
	deliver func(name string, sig TaskSignal)
}

func (w replayWorkflows) Signal(_ context.Context, _, name string, sig TaskSignal) error {
	w.deliver(name, sig)
	return nil
}

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
	reviewN   int
	auditN    int
	checksN   int
	over      bool
}

func replayTree(t *testing.T, fx Fixture) policy.Tree {
	t.Helper()
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	body := string(files["projects/demo-app.yaml"])
	body = strings.Replace(body, "merge_rule: human", "merge_rule: "+fx.Card.MergeRule+"\nautonomy: "+fx.Card.Autonomy, 1)
	files["projects/demo-app.yaml"] = []byte(body)
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
	g := &replayRig{t: t, fx: fx, s: s, forge: &replayForge{checks: map[string]string{}, failed: map[string][]FixtureFailedTest{}}}
	v := policy.Version{ID: 7, Tree: replayTree(t, fx)}
	v.Document.Budget.SessionFloorCheapEUR, v.Document.Budget.SessionFloorFrontierEUR = 0.3, 0.3
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, id == v.ID, nil }
	cfg.Workflows = replayWorkflows{deliver: g.deliver}
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

// seed writes the card, its plan and its dependencies as the fixture
// records them.
func (g *replayRig) seed() {
	g.t.Helper()
	ctx := context.Background()
	c := g.fx.Card
	repo := "demo-app/demo-app"
	task := Task{ID: replayTask, Kind: c.Kind, Status: "queued", Objective: c.Objective, Repository: repo, IssueNumber: c.Issue,
		Track: c.Track, Source: c.Source, Size: c.Size, BudgetEUR: c.BudgetEUR, CeilingEUR: c.CeilingEUR,
		RequiredCapabilities: []string{"coder"}}
	if c.Plan != nil {
		if err := g.s.CreateTask(ctx, Task{ID: "plan", Kind: "agent", Status: "waiting-human", Objective: "the plan", Repository: repo,
			IssueNumber: c.Plan.Issue, Track: policy.TrackPlan, PlanName: c.Plan.Name, BudgetEUR: 5}); err != nil {
			g.t.Fatalf("seed plan: %v", err)
		}
		task.Parent, task.PlanKey = "plan", "card"
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
		if _, err := g.s.db.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) VALUES ($1, $2)`, replayTask, id); err != nil {
			g.t.Fatalf("seed dependency edge: %v", err)
		}
	}
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
			g.t.Errorf("rung 2's %s on a %s case: %v", turn.Answer.Tool, c.Kind, err)
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
	if err := g.env.GetWorkflowError(); err != nil {
		var timeout *temporal.TimeoutError
		var app *temporal.ApplicationError
		if !errors.As(err, &timeout) && !(errors.As(err, &app) && strings.Contains(app.Error(), replayEnds)) {
			g.t.Logf("workflow error: %v", err)
		}
	}
	if g.chat.failure != "" {
		g.t.Error(g.chat.failure)
	}
	return g.result(ctx)
}

const replayEnds = "the replay ends here"

func (g *replayRig) result(ctx context.Context) fixtureReplay {
	g.t.Helper()
	out := fixtureReplay{Sessions: g.sessions, Finals: g.finals}
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
	for _, c := range cases {
		res := fixtureCaseResult{Kind: c.Kind, Rung: ResolvedOpen}
		if c.Status == CaseResolved && c.Resolution != nil {
			res.Rung = c.Resolution.Rung
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
		for _, s := range g.fx.Sessions {
			if s.CheckIn != nil && strings.ReplaceAll(s.CheckIn.ReasonKind, " ", "_") == CheckPolicyClass {
				gate.Class, gate.Migrations = "migration", []string{"internal/schema/migrations/factoryd/20261108120000_metrics_identities.sql"}
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
		return out, err
	})
}

// runSession answers the nth session with the nth recorded one. A session
// past the record starts only while the expect allows it, then ends the run.
func (g *replayRig) runSession(ctx context.Context, in SessionInput) (SessionResult, error) {
	g.mu.Lock()
	g.sessions = append(g.sessions, in)
	n := len(g.sessions)
	g.mu.Unlock()
	if n > len(g.fx.Sessions) {
		if g.fx.Expect == nil || n > g.fx.Expect.Sessions {
			g.t.Errorf("the ladder started a session the record does not have (session %d of %d recorded)", n, len(g.fx.Sessions))
		}
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

// next is the nth entry of list, the last one past its end, and false when
// the list is empty.
func replayNext[T any](list []T, n int) (T, bool) {
	var zero T
	if len(list) == 0 {
		return zero, false
	}
	return list[min(n, len(list))-1], true
}

func (g *replayRig) waitChecks(ctx context.Context, taskID, sha string) (ChecksResult, error) {
	g.mu.Lock()
	g.checksN++
	n := g.checksN
	g.mu.Unlock()
	c, ok := replayNext(g.fx.Checks, n)
	if !ok {
		c = FixtureChecks{State: "success"}
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

// gatesOf are the recorded results of gate in order.
func (g *replayRig) gatesOf(gate string) []FixtureGate {
	var out []FixtureGate
	for _, r := range g.fx.Gates {
		if r.Gate == gate {
			out = append(out, r)
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
	g.mu.Lock()
	g.reviewN++
	n := g.reviewN
	g.mu.Unlock()
	rec, ok := replayNext(g.gatesOf("review"), n)
	if !ok {
		rec = FixtureGate{Verdict: "approve"}
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
	g.mu.Lock()
	g.auditN++
	n := g.auditN
	g.mu.Unlock()
	rec, ok := replayNext(g.gatesOf("audit"), n)
	if !ok {
		rec = FixtureGate{Verdict: AuditClear}
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

// replayFixture runs fx through the ladder.
func replayFixture(t *testing.T, fx Fixture) fixtureReplay {
	t.Helper()
	return newReplayRig(t, fx).run()
}

func runFixture(t *testing.T, path string) fixtureReplay {
	t.Helper()
	fx, err := LoadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	return replayFixture(t, fx)
}

// replayMismatch lists what the run did that the fixture's expect does not
// say: the rung that resolved each case, the sessions, the human cases and
// each face's substrings.
func replayMismatch(fx Fixture, run fixtureReplay) []string {
	x := fx.Expect
	if x == nil {
		return []string{"the fixture has no expect"}
	}
	var out []string
	var rungs []int
	for _, c := range run.Cases {
		rungs = append(rungs, c.Rung)
	}
	if !slices.Equal(rungs, x.ResolvedByRung) && !(len(rungs) == 0 && len(x.ResolvedByRung) == 0) {
		out = append(out, fmt.Sprintf("resolved_by_rung = %v, want %v", rungs, x.ResolvedByRung))
	}
	if len(run.Sessions) != x.Sessions {
		out = append(out, fmt.Sprintf("sessions = %d, want %d", len(run.Sessions), x.Sessions))
	}
	if run.HumanCases != x.HumanCases {
		out = append(out, fmt.Sprintf("human_cases = %d, want %d", run.HumanCases, x.HumanCases))
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
		if !strings.Contains(faces[i].Ask, want.Ask) {
			out = append(out, fmt.Sprintf("face %d ask = %q, want it to hold %q", i, faces[i].Ask, want.Ask))
		}
		if !strings.Contains(faces[i].Recommendation, want.Recommendation) {
			out = append(out, fmt.Sprintf("face %d recommendation = %q, want it to hold %q", i, faces[i].Recommendation, want.Recommendation))
		}
	}
	return out
}

// A card of one pushed session whose review approves, in a project whose
// merge rule is human: the run ends at the merge approval, which no rung
// takes. By hand: one case of kind merge_approval, open at the human (rung
// ResolvedOpen), one session, one human case, whose ask names the merge
// approval.
func syntheticFixture() Fixture {
	return Fixture{
		Card: FixtureCard{Issue: 7, TaskID: "t-x", Repository: "owner/repo", Kind: "agent", Track: "direct", Source: "webhook", Size: "small",
			BudgetEUR: 5, Objective: "a card of one session", Autonomy: "manual", MergeRule: "human"},
		Sessions: []FixtureSession{{Attempt: 1, Outcome: "pushed", Commit: "c1", Pushed: true, CostEUR: 0.2, Calls: 10, AgentTimeS: 300, Summary: "pushed c1"}},
		Gates:    []FixtureGate{{Head: "c1", Gate: "review", Verdict: "approve"}},
		Checks:   []FixtureChecks{{Head: "c1", State: "success"}},
		Expect:   &FixtureExpect{ResolvedByRung: []int{ResolvedOpen}, Sessions: 1, HumanCases: 1, Face: []FixtureFace{{Ask: "merge approval"}}},
	}
}

// The harness asserts exactly: the hand-derived expect holds, and each part
// of it one off fails.
func TestReplayHarnessAssertsTheExpectExactly(t *testing.T) {
	fx := syntheticFixture()
	run := replayFixture(t, fx)
	if got := replayMismatch(fx, run); len(got) != 0 {
		t.Fatalf("the hand-derived expect does not hold: %v", got)
	}
	for name, mutate := range map[string]func(x *FixtureExpect){
		"a case resolved at rung 0": func(x *FixtureExpect) { x.ResolvedByRung = []int{0} },
		"one session more":          func(x *FixtureExpect) { x.Sessions = 2 },
		"no human case":             func(x *FixtureExpect) { x.HumanCases, x.Face = 0, nil },
		"another face":              func(x *FixtureExpect) { x.Face = []FixtureFace{{Ask: "how should it go on?"}} },
	} {
		x := *fx.Expect
		mutate(&x)
		wrong := fx
		wrong.Expect = &x
		if got := replayMismatch(wrong, run); len(got) == 0 {
			t.Errorf("%s: the run matched an expect one off", name)
		}
	}
}

// The five named cards run through the ladder with the rungs the incident
// never ran scripted by hand. An expect is the brief's line for the card; a
// fixture that differs is a ladder fault, named in faults with what the
// replay did, and its test skips while the fault stands and fails once the
// card goes through as the line says.
var replayFaults = map[string]string{
	// #1347: the policy gate classed e5908fbb a migration and the check-in
	// answered human, with the review's four concerns on that head. By hand
	// from the brief: rung 0 takes the policy_class case with route_approval
	// (case 1, rung 0); the head then reaches its approval and the merge
	// approval is the one human case (case 2, open, its ask the merge
	// approval); two sessions started (the push and the check-in's stop).
	// Rung 0 takes route_approval only when the newest review of the head
	// approved it, and the recorded review on that head says concerns, so the
	// case goes up to the human at once and no merge approval opens.
	"1347": "route_approval is refused on a head whose newest review recorded concerns, so the policy_class case reaches the human unresolved",
	// #1410: the review refused head 6c6813 with four findings on the third
	// session, the cap. By hand: rung 1 (script: two confirmed, two dropped,
	// handover on the same model with a draw of one session) resolves the
	// session_cap case (rung 1); the draw buys the fourth session; no human
	// case. The run opens no session_cap case: the third session is the
	// last round, so the head's concerns go to the merge approval, which the
	// human decides and no rung judges first.
	"1410": "at the last round the review's concerns open the merge approval at the human; no session_cap case opens for a rung to judge",
}

// The cards the harness cannot drive yet, with why.
var replayUndrivable = map[string]string{
	// #1451 by hand: five dependents each open a dependency_failed case,
	// resolved at rung 0 (the impasse replan), no session, no human case. The
	// exported card is the failed dependency: it holds no dependent, and a run
	// of the card itself starts a session the record does not have.
	"1451": "the fixture format holds no dependents of the card, so the five waiting cards cannot be seeded",
	// #1345 by hand: the planner's identical re-file is a no-op (the plan
	// stands as filed), its one confirmation reaches the human once (one
	// case, open) after one planner session; no second planner session.
	"1345": "a plan card's sessions are the planner's (RunPlan and the plan filing rules), not RunSession's, which the harness stubs",
	// #1381 by hand: rung 0 re-runs the red head's checks once (case 1, rung
	// 0); the head still red, rung 1 hands over (case 2, rung 1); two
	// sessions; no human case. The red head a6397c came from a push the agent
	// made outside its session, which the recorded sessions do not carry.
	"1381": "the red head came from a push outside any recorded session, so the sessions cannot produce it",
}

func TestReplayFixtures(t *testing.T) {
	for _, name := range []string{"1347", "1410", "1345", "1451", "1381"} {
		t.Run(name, func(t *testing.T) {
			if why, ok := replayUndrivable[name]; ok {
				t.Skip("not driven: " + why)
			}
			path := "testdata/fixtures/" + name + ".json"
			fx, err := LoadFixture(path)
			if err != nil {
				t.Fatal(err)
			}
			mismatch := replayMismatch(fx, replayFixture(t, fx))
			fault, known := replayFaults[name]
			switch {
			case len(mismatch) == 0 && known:
				t.Fatalf("the card goes through as the brief says: remove its fault %q", fault)
			case len(mismatch) > 0 && known:
				t.Skipf("ladder fault: %s\nthe replay: %v", fault, mismatch)
			case len(mismatch) > 0:
				t.Fatalf("the replay: %v", mismatch)
			}
		})
	}
}

// replayCoverage maps every stuck point (S) and incident (I) of the card flow
// registers to what proves its construct: a fixture file, the name of an
// existing test, or "open: <reason>".
var replayCoverage = map[string]string{
	"I1":  "testdata/fixtures/1345.json",
	"I2":  "open: declined, the ask quotes the refusal instead",
	"I3":  "open: a batch fixture of a press whose label names money",
	"I4":  "open: a chat turn, no card run",
	"I5":  "testdata/fixtures/1345.json",
	"I6":  "testdata/fixtures/1347.json",
	"I7":  "testdata/fixtures/1347.json",
	"I8":  "testdata/fixtures/1381.json",
	"I9":  "open: a batch fixture of a planner session that died on its read set",
	"I10": "open: a chat tool refusal, no card run",
	"I11": "open: a month cap spent factory-wide, no card run",
	"I12": "open: a gateway backstop, no card run",
	"I13": "open: a batch fixture of a review refused for money",
	"I14": "open: a chat compaction, no card run",
	"I15": "open: a batch fixture of a revert card behind a red main",
	"I16": "testdata/fixtures/1451.json",
	"I17": "testdata/fixtures/1410.json",
	"I18": "open: a batch fixture of an alert outliving its card",
	"I19": "open: the chat door, no card run",
	"I20": "open: a loader rename, no card run",
	"S1":  "open: a wait at the human has no clock, by decision",
	"S2":  "open: a wait at the human has no clock, by decision",
	"S3":  "TestCapWaitsWake",
	"S4":  "open: the plan wait has no clock, by decision; a batch fixture takes a plan's question",
	"S5":  "open: a human-lane card has no workflow to replay",
	"S6":  "testdata/fixtures/1451.json",
	"S7":  "TestImpasseRefusalNamesTheState",
	"S8":  "open: a batch fixture of a card moved onto its plan card",
	"S9":  "testdata/fixtures/1451.json",
	"S10": "open: by design, a merged dependency waits for its green chain run",
	"S11": "open: a batch fixture of a leaf card that failed with no dependents",
	"S12": "open: a batch fixture of a plan child's successor",
	"S13": "open: a delay the heal pass covers, no stop",
	"S14": "TestAdmitRecordsTheRefusalAndClearsItOnAdmission",
	"S15": "open: a batch fixture of five cards at approval holding the work bound",
	"S16": "TestRollerPausesTheQueueOnARedMain",
	"S17": "open: a plan child whose issue never linked has no run to replay",
	"S18": "open: a batch fixture of a deadlock ask answered overrule",
	"S19": "open: a batch fixture of a deadlock that needs an extend",
	"S20": "open: a batch fixture of a repeated stop after a restart",
	"S21": "testdata/fixtures/1347.json",
	"S22": "testdata/fixtures/1347.json",
	"S23": "open: a batch fixture of an answer on an unfunded card",
	"S24": "open: by decision, a done integration card re-runs no review",
	"S25": "open: a batch fixture of a stop whose recommendation a rung follows",
	"S26": "open: the minute loop is no card run, and no test names it",
	"S27": "TestReplayOfALiveHistoryIsStrict",
	"S28": "open: a batch fixture of a run restarted on a closing card",
	"S29": "open: a signal to a completed workflow is no card run",
	"S30": "open: an untrusted commenter's answer is no card run",
	"S31": "open: a batch fixture of a report card whose gate supervisor escalates",
	"S32": "open: a batch fixture of an escalation that cannot post",
	"S33": "open: a policy tree rename is infrastructure, not a card run",
	"S34": "open: the planner's read set is a hand step on the infra tree",
	"S35": "TestReplayOfALiveHistoryIsStrict",
	"S36": "open: a batch fixture of a replan's refiled cards",
	"S37": "open: a batch fixture of the size gate's budget move",
	"S38": "open: a batch fixture of an answer on a failed card",
	"S39": "open: dormant under the shipped process",
}

func TestReplayCoverageNamesEveryStuckPointAndIncident(t *testing.T) {
	tests := map[string]bool{}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range declared.FindAllSubmatch(src, -1) {
			tests[string(m[1])] = true
		}
	}
	var ids []string
	for n := 1; n <= 39; n++ {
		ids = append(ids, fmt.Sprintf("S%d", n))
	}
	for n := 1; n <= 20; n++ {
		ids = append(ids, fmt.Sprintf("I%d", n))
	}
	for _, id := range ids {
		proof, ok := replayCoverage[id]
		switch {
		case !ok:
			t.Errorf("%s is in no row of the register", id)
		case strings.HasPrefix(proof, "open: "):
		case strings.HasPrefix(proof, "testdata/fixtures/"):
			if _, err := os.Stat(proof); err != nil {
				t.Errorf("%s names the fixture %s: %v", id, proof, err)
			}
		case !tests[proof]:
			t.Errorf("%s names the test %s, which no test file declares", id, proof)
		}
	}
	if len(replayCoverage) != len(ids) {
		t.Errorf("the register has %d rows, want %d: a row names no stuck point or incident", len(replayCoverage), len(ids))
	}
}
