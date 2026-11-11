package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The real factoryd Act on Postgres behind the console rig: a human card's case
// in a Runner whose policy names the project at a level, and the console's case
// reads on the same store, so a test sees what Act accepts, refuses or proposes.

type raLeases struct{}

func (raLeases) LiveAddresses(context.Context) (map[string][]string, error) { return nil, nil }
func (raLeases) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	return nil, nil
}

type raWorkflows struct{}

func (raWorkflows) Start(context.Context, string) error                               { return nil }
func (raWorkflows) StartRoutineRun(context.Context, factoryd.RoutineRunInput) error   { return nil }
func (raWorkflows) Signal(context.Context, string, string, factoryd.TaskSignal) error { return nil }
func (raWorkflows) Reopen(context.Context, string, string, factoryd.TaskSignal) error { return nil }

type raObjects struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (o *raObjects) Put(_ context.Context, k string, b []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = map[string][]byte{}
	}
	o.m[k] = b
	return nil
}
func (o *raObjects) Get(_ context.Context, k string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.m[k], nil
}
func (o *raObjects) Delete(_ context.Context, k string) error { return nil }

type realAct struct {
	runner   *factoryd.Runner
	pg       *factoryd.PGStore
	level    string
	mu       sync.Mutex
	comments []string // the runner's own Forgejo calls
	caseID   int64
}

const raRepo = "linkshort/linkshort"

func raVersion(t *testing.T, level string) policy.Version {
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	v := policy.Version{ID: 7, Commit: "c7", Tree: tree}
	name, p, ok := v.ProjectOf(raRepo)
	if !ok {
		t.Fatal("no linkshort project")
	}
	p.Autonomy, p.MergeRule = level, policy.MergeHuman
	if level == policy.AutonomyFullAuto {
		p.Autonomy, p.MergeRule = policy.AutonomyAutonomous, policy.MergeAuto
	}
	projects := map[string]policy.ProjectConfig{}
	for k, pc := range v.Projects {
		projects[k] = pc
	}
	projects[name] = p
	v.Projects = projects
	return v
}

var raN int
var raMu sync.Mutex

// newRealAct opens a fresh database with human card id (#issue, source)
// asking q, and its case at the human, and a Runner on it at level.
func newRealAct(t *testing.T, id string, issue int64, source string, q factoryd.HumanQuestion, level string) *realAct {
	t.Helper()
	ra := newRealRunner(t, level)
	ctx := context.Background()
	if err := ra.pg.CreateTask(ctx, factoryd.Task{ID: id, Kind: "human", Status: "waiting-human", Source: source, Repository: raRepo,
		IssueNumber: issue, Objective: "needs you", SHA: "abc1234"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.pg.InsertIntakeEvent(ctx, factoryd.IntakeEvent{TaskID: id, Kind: factoryd.IntakeEscalation, Author: "factoryd", CommentID: 1, Question: &q}); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.pg.Record(ctx, factoryd.Event{Repository: raRepo, TaskID: id, IssueNumber: issue, Kind: factoryd.EventQuestionAsked,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"source": source, "ask": q.Ask, "comment": 1, "offers": q.Choices}}); err != nil {
		t.Fatal(err)
	}
	k, err := ra.pg.NewestAskCase(ctx, id)
	if err != nil || k.ID == 0 {
		t.Fatalf("no case opened: %+v %v", k, err)
	}
	ra.caseID = k.ID
	return ra
}

// newRealRunner opens a fresh database and a Runner on it at level, with no card.
func newRealRunner(t *testing.T, level string) *realAct {
	t.Helper()
	raMu.Lock()
	raN++
	name := fmt.Sprintf("realact%d", raN)
	raMu.Unlock()
	ctx := context.Background()
	pg, err := factoryd.OpenStore(ctx, schematest.DSN(t, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	ra := &realAct{pg: pg, level: level}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ra.mu.Lock()
		ra.comments = append(ra.comments, req.Method+" "+req.URL.Path)
		ra.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 9001}`)
	}))
	t.Cleanup(srv.Close)
	versions := map[string]policy.Version{}
	for _, l := range []string{policy.AutonomyManual, policy.AutonomyAssisted, policy.AutonomyAutonomous, policy.AutonomyFullAuto} {
		versions[l] = raVersion(t, l)
	}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-",
		WorkerImage:          "reg/worker@sha256:abc",
		SandboxBaseImage:     "reg/sandbox-base@sha256:bcd",
		RegistryHost:         "store.test",
		GatewayURL:           "http://factory-gateway.factory.svc.cluster.local:8081",
		GitBaseURL:           "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:               factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens:         8192,
		HeadroomTokens:       16384,
		EgressCanary:         "example.com:443",
		EgressDirectIP:       "192.0.2.7:443",
		Supervisor:           factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy: func() policy.Version {
			ra.mu.Lock()
			defer ra.mu.Unlock()
			return versions[ra.level]
		},
		PolicyVersion: func(context.Context, int64) (policy.Version, bool, error) { return versions[level], true, nil },
		Leases:        raLeases{},
		Gates:         &factoryd.GateWatch{},
		LastFailure:   func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:     raWorkflows{},
	}
	fj := &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ra.runner, err = factoryd.NewRunner(cfg, pg, nil, fj, nil, &raObjects{}, &factoryd.SupervisorModel{})
	if err != nil {
		t.Fatal(err)
	}
	return ra
}

// seed puts card k in the database, the card a console rig seats in its own
// fake: the real Act reads it there.
func (ra *realAct) seed(t *testing.T, k factoryd.Task) {
	t.Helper()
	k.Repository = raRepo
	if err := ra.pg.CreateTask(context.Background(), k); err != nil {
		t.Fatal(err)
	}
}

// wire puts the real Act and the real cases behind the console; stale
// keeps the console's own case reads (the rig's openCases) so it reads a
// case the store ended as live.
func (ra *realAct) wire(r *rig, id string, stale bool) {
	r.c.cfg.Act = ra.runner.Act
	ra.runner.DeployNow = r.c.cfg.DeployNow
	if stale {
		r.openCases = map[string][]factoryd.Case{id: {{ID: ra.caseID, TaskID: id, Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman}}}
		return
	}
	r.c.cfg.OpenCasesOf = ra.pg.OpenCasesOf
	r.c.cfg.NewestAskCase = ra.pg.NewestAskCase
	r.c.cfg.CaseByID = ra.pg.CaseByID
	r.c.cfg.CaseSteps = ra.pg.CaseSteps
}

func (ra *realAct) caseNow(t *testing.T) factoryd.Case {
	k, err := ra.pg.CaseByID(context.Background(), ra.caseID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (ra *realAct) face(t *testing.T, id string) map[string]any {
	evs, err := ra.pg.TaskEventsOfKind(context.Background(), id, factoryd.EventQuestionAsked, 10)
	if err != nil || len(evs) == 0 {
		t.Fatalf("faces %v %v", evs, err)
	}
	return evs[0].Detail
}

func raHeldQuestion() factoryd.HumanQuestion {
	return factoryd.HumanQuestion{Ask: "Production has waited 7 hours. Wait, deploy anyway, or replan?", Choices: []factoryd.QuestionChoice{
		{Value: factoryd.AnswerClose, Label: "Done"},
		{Value: factoryd.ActDeployNow, Label: "Deploy anyway"},
		{Value: factoryd.AnswerReplanOnTop + ":t-pl", Label: "Replan plan #12 on top"},
		{Value: factoryd.AnswerTakeOffMain + ":t-pl", Label: "Take plan #12 off main and replan"}}}
}

// held is holdAskRig with card t-hq #51 held-production asking q
func raHeldRig(t *testing.T, q factoryd.HumanQuestion) (*rig, *[]replanCall, *[]deployCall) {
	r, _, deploys, replans := holdAskRig(t)
	bt := factoryd.BoardTask{Task: factoryd.Task{ID: "t-hq", Kind: "human", Status: "waiting-human", Source: factoryd.SourceProductionHeld,
		Repository: "sf/code", IssueNumber: 51, Objective: "needs you: production is held"}, Escalation: q.Ask, Question: &q}
	r.tasks.tasks = append(r.tasks.tasks, bt)
	return r, replans, deploys
}

func raErrText(err error) string {
	if err == nil {
		return ""
	}
	var no *refusal
	if errors.As(err, &no) {
		return fmt.Sprintf("%d %s", no.status, no.msg)
	}
	return err.Error()
}

// Every choice of a human card, at manual, assisted and full auto, for rung 2
// and for a human, with the real Act deciding. By hand: a human's pick runs
// its work and Act accepts it. Rung 2's pick below full auto is a proposal:
// no work runs, the case stays live and its face carries the value preselected
// (rerun_again, which keeps the card waiting, included). Rung 2's pick at full
// auto runs, except Deploy anyway, which no chat answers at any level. A
// choice that ends its case resolves it; rerun_again, park and keep_waiting
// leave it live.
func TestEveryHumanCardChoiceMeetsTheRealAct(t *testing.T) {
	type setup struct {
		task, source string
		issue        int64
		q            factoryd.HumanQuestion
		values       []string
	}
	held, stall := raHeldQuestion(), factoryd.HumanQuestion{Ask: "Main's run is stuck.", Choices: []factoryd.QuestionChoice{
		{Value: factoryd.AnswerRerunAgain, Label: "Re-run again"}, {Value: factoryd.AnswerCheckAgain, Label: "Check again"}, {Value: factoryd.AnswerDismiss, Label: "Dismiss"}}}
	plain := factoryd.HumanQuestion{Ask: "Wait?", Choices: []factoryd.QuestionChoice{
		{Value: factoryd.AnswerPark, Label: "Park"}, {Value: factoryd.AnswerKeepWaiting, Label: "Keep waiting"}, {Value: factoryd.AnswerClose, Label: "Done"}}}
	for _, su := range []setup{
		{"t-hq", factoryd.SourceProductionHeld, 51, held, []string{factoryd.AnswerClose, factoryd.ActDeployNow, factoryd.AnswerReplanOnTop + ":t-pl", factoryd.AnswerTakeOffMain + ":t-pl"}},
		{"t-st", factoryd.SourceChainStall, 61, stall, []string{factoryd.AnswerRerunAgain, factoryd.AnswerCheckAgain, factoryd.AnswerDismiss}},
		{"t-hq", "", 51, plain, []string{factoryd.AnswerPark, factoryd.AnswerKeepWaiting, factoryd.AnswerClose}},
	} {
		for _, value := range su.values {
			for _, caller := range []string{"member", "agent"} {
				for _, level := range []string{policy.AutonomyManual, policy.AutonomyAssisted, policy.AutonomyFullAuto} {
					t.Run(su.task+"/"+value+"/"+caller+"/"+level, func(t *testing.T) {
						var r *rig
						var replans *[]replanCall
						var deploys *[]deployCall
						switch su.source {
						case factoryd.SourceProductionHeld:
							r, replans, deploys = raHeldRig(t, su.q)
						default:
							r = newRig(t)
							replans, deploys = &[]replanCall{}, &[]deployCall{}
							bt := factoryd.BoardTask{Task: factoryd.Task{ID: su.task, Kind: "human", Status: "waiting-human", Source: su.source,
								Repository: "sf/code", IssueNumber: su.issue, SHA: "abc1234", Objective: "needs you"}, Escalation: su.q.Ask, Question: &su.q}
							r.tasks.tasks = append(r.tasks.tasks, bt)
						}
						ra := newRealAct(t, su.task, su.issue, su.source, su.q, level)
						ra.wire(r, su.task, false)
						_, s := r.loggedIn(t)
						a := actor{s: s, repository: "sf/code", via: factoryd.ViaButton, decider: "member", level: level}
						if caller == "agent" {
							a = actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: level, wake: true, birth: factoryd.WakeAsk}
						}
						said, err := r.c.doAnswerCase(t.Context(), a, su.task, ra.caseID, "", value, nil, 0)
						work := len(r.fg.patched) + len(*replans) + len(*deploys) + len(r.reran) + len(r.dismissed)
						k := ra.caseNow(t)
						pre, _ := ra.face(t, su.task)["preselected"].(map[string]any)
						keeps := value == factoryd.AnswerRerunAgain || value == factoryd.AnswerPark || value == factoryd.AnswerKeepWaiting
						switch {
						case caller == "agent" && level != policy.AutonomyFullAuto:
							if err != nil || work != 0 || k.Status != factoryd.CaseAtHuman || pre["action"] != value || !strings.HasPrefix(said, "Proposed "+value) {
								t.Fatalf("err %v, work %d, case %s, preselected %v, said %q; want a proposal and no work", err, work, k.Status, pre, said)
							}
						default:
							if err != nil || (k.Status == factoryd.CaseResolved) == keeps || pre != nil {
								t.Fatalf("err %v, case %s, preselected %v; want the answer accepted, the case ended unless the card keeps waiting", err, k.Status, pre)
							}
							if value != factoryd.AnswerPark && value != factoryd.AnswerKeepWaiting && work == 0 {
								t.Fatalf("no work ran for %s", value)
							}
						}
					})
				}
			}
		}
	}
}

// A human card's close closes the issue before it posts its note: a close
// Forgejo refuses posts nothing, and the case stays at the human with its
// issue open. A rung 2 close at manual is a proposal, and the issue stays
// open too.
func TestAHumanCardCloseOrdersItsWorkAndMeetsTheLevel(t *testing.T) {
	q := factoryd.HumanQuestion{Ask: "Which store?", Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerClose, Label: "Done"}}}
	newCard := func(t *testing.T, level string) (*rig, *realAct) {
		r := newRig(t)
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerClose, Label: "Done"})}
		ra := newRealAct(t, "t-hq", 51, "", q, level)
		ra.wire(r, "t-hq", false)
		return r, ra
	}
	comments := func(r *rig) []string { return r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments") }

	r, ra := newCard(t, policy.AutonomyManual)
	r.fg.mu.Lock()
	r.fg.failEdit = true
	r.fg.mu.Unlock()
	_, s := r.loggedIn(t)
	human := actor{s: s, repository: "sf/code", via: factoryd.ViaButton, decider: "member", level: policy.AutonomyManual}
	if _, err := r.c.doAnswerCase(t.Context(), human, "t-hq", ra.caseID, "", factoryd.AnswerClose, nil, 0); err == nil ||
		len(comments(r)) != 0 || len(r.fg.patched) != 0 || ra.caseNow(t).Status != factoryd.CaseAtHuman {
		t.Fatalf("a failing PATCH: err %v, comments %q, case %s; want the refusal, no comment and the case at the human", err, comments(r), ra.caseNow(t).Status)
	}

	// the chat's close tool (doClose) closes before it posts as well
	r, ra = newCard(t, policy.AutonomyManual)
	r.fg.mu.Lock()
	r.fg.failEdit = true
	r.fg.mu.Unlock()
	_, s = r.loggedIn(t)
	if _, err := r.c.doClose(t.Context(), human, "t-hq", "done by hand"); err == nil || len(comments(r)) != 0 || len(r.fg.patched) != 0 || ra.caseNow(t).Status != factoryd.CaseAtHuman {
		t.Fatalf("doClose with a failing PATCH: err %v, comments %q, case %s; want the refusal, no comment and the case at the human", err, comments(r), ra.caseNow(t).Status)
	}

	r, ra = newCard(t, policy.AutonomyManual)
	_, s = r.loggedIn(t)
	agent := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: policy.AutonomyManual, wake: true, birth: factoryd.WakeAsk}
	if _, err := r.c.doClose(t.Context(), agent, "t-hq", "done by the agent"); err != nil || len(r.fg.patched) != 0 || ra.caseNow(t).Status != factoryd.CaseAtHuman {
		t.Fatalf("rung 2's close at manual: err %v, patched %v, case %s; want a proposal and the issue open", err, r.fg.patched, ra.caseNow(t).Status)
	}
	r, ra = newCard(t, policy.AutonomyFullAuto)
	_, s = r.loggedIn(t)
	agent.s, agent.level = s, policy.AutonomyFullAuto
	if _, err := r.c.doClose(t.Context(), agent, "t-hq", "done by the agent"); err != nil || len(r.fg.patched) != 1 || ra.caseNow(t).Status != factoryd.CaseResolved {
		t.Fatalf("rung 2's close at full auto: err %v, patched %v, case %s; want the issue closed and the case resolved", err, r.fg.patched, ra.caseNow(t).Status)
	}
}

// A press whose case ended before Act read it (the webhook's copy resolved
// it, the heal pass withdrew it) runs none of the work, at every level, for
// rung 2 and for a human: the console still reads the case as live, Act
// refuses, and the issue stays open and no replan runs.
func TestAPressOnAnEndedCaseRunsNoWorkAtAnyLevel(t *testing.T) {
	for _, end := range []string{"resolved", "withdrawn"} {
		for _, value := range []string{factoryd.AnswerClose, factoryd.AnswerReplanOnTop + ":t-pl"} {
			for _, caller := range []string{"member", "agent"} {
				for _, level := range []string{policy.AutonomyManual, policy.AutonomyAssisted, policy.AutonomyFullAuto} {
					t.Run(end+"/"+value+"/"+caller+"/"+level, func(t *testing.T) {
						q := raHeldQuestion()
						r, replans, _ := raHeldRig(t, q)
						ra := newRealAct(t, "t-hq", 51, factoryd.SourceProductionHeld, q, level)
						ctx := context.Background()
						if end == "resolved" {
							if _, err := ra.pg.Record(ctx, factoryd.Event{Repository: raRepo, TaskID: "t-hq", IssueNumber: 51, Kind: factoryd.EventQuestionAnswered,
								Actor: "sebastian", Via: "webhook", Detail: map[string]any{"answer": "done by hand", "comment": 77}}); err != nil {
								t.Fatal(err)
							}
						} else if err := ra.pg.WithdrawCase(ctx, ra.caseID, "card ended closed"); err != nil {
							t.Fatal(err)
						}
						ra.wire(r, "t-hq", true)
						_, s := r.loggedIn(t)
						a := actor{s: s, repository: "sf/code", via: factoryd.ViaButton, decider: "member", level: level}
						if caller == "agent" {
							a = actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: level, wake: true, birth: factoryd.WakeAsk}
						}
						_, err := r.c.doAnswerCase(t.Context(), a, "t-hq", ra.caseID, "", value, nil, 0)
						no := refusedWith(err)
						if no == nil || no.status != http.StatusConflict || len(r.fg.patched) != 0 || len(*replans) != 0 {
							t.Fatalf("err %v, patched %v, replans %v; want the 409 and no work", err, r.fg.patched, *replans)
						}
					})
				}
			}
		}
	}
}

// A month cap's human card keeps the month cap kind, and its choices answer
// it: "Decided, close" sends the choice's own value to the real Act, which
// closes the card's issue and resolves the case; the console renders the
// choices as the old value forms, not as a continue with words.
func TestAMonthCapHumanCardClosesByItsChoice(t *testing.T) {
	q := factoryd.HumanQuestion{Ask: "The month cap is reached: raise it, or let the factory stand?",
		Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerPark, Label: "Let it stand"}, {Value: factoryd.AnswerClose, Label: "Decided, close"}}}
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-hq", Kind: "human", Status: "waiting-human", Source: factoryd.SourceMonthCap,
		Repository: "sf/code", IssueNumber: 51, SHA: "abc1234", Objective: "needs you"}, Escalation: q.Ask, Question: &q}}
	ra := newRealAct(t, "t-hq", 51, factoryd.SourceMonthCap, q, policy.AutonomyManual)
	ra.wire(r, "t-hq", false)
	if k := ra.caseNow(t); k.Kind != factoryd.CaseMonthCap {
		t.Fatalf("the case is %s, want the month cap kept", k.Kind)
	}
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	face := escalationOf(t, body, "t-hq")
	if !strings.Contains(face, `name="value" value="close"`) || strings.Contains(face, `name="premade"`) {
		t.Fatalf("the month cap card's face =\n%s\nwant its choices as value buttons, no premade continue", face)
	}
	rec := r.do(post("/tasks/t-hq/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerClose}, "case": {fmt.Sprint(ra.caseID)}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("Decided, close = %d %q", rec.Code, rec.Body)
	}
	if k := ra.caseNow(t); k.Status != factoryd.CaseResolved || k.Resolution == nil || k.Resolution.Action != factoryd.AnswerClose ||
		!slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Fatalf("case %s %+v, patched %v; want it resolved by close and the issue closed", k.Status, k.Resolution, r.fg.patched)
	}
}

// The chat's answer by case number on a month cap human card routes by the
// card's kind, so its close closes the issue and resolves the case.
func TestTheChatClosesAMonthCapCardByItsCase(t *testing.T) {
	q := factoryd.HumanQuestion{Ask: "The month cap is reached: raise it, or let the factory stand?",
		Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerPark, Label: "Let it stand"}, {Value: factoryd.AnswerClose, Label: "Decided, close"}}}
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-hq", Kind: "human", Status: "waiting-human", Source: factoryd.SourceMonthCap,
		Repository: "sf/code", IssueNumber: 51, SHA: "abc1234", Objective: "needs you"}, Escalation: q.Ask, Question: &q}}
	ra := newRealAct(t, "t-hq", 51, factoryd.SourceMonthCap, q, policy.AutonomyManual)
	ra.wire(r, "t-hq", false)
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", CaseID: ra.caseID, Action: factoryd.AnswerClose}); err != nil {
		t.Fatalf("the chat's close by case number: %v", err)
	}
	if k := ra.caseNow(t); k.Status != factoryd.CaseResolved || !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Fatalf("case %s, patched %v; want it resolved and the issue closed", k.Status, r.fg.patched)
	}
}

// Free words on a card whose question offers choices meet Act's one refusal,
// naming the choices, on the button and in the chat, whatever the card's
// source; nothing is posted and no event is written, and a choice takes the
// note beside it.
func TestFreeWordsOnAChoiceCardMeetActsRefusal(t *testing.T) {
	held := raHeldQuestion()
	stall := factoryd.HumanQuestion{Ask: "Main's run is stuck.", Choices: []factoryd.QuestionChoice{
		{Value: factoryd.AnswerRerunAgain, Label: "Re-run again"}, {Value: factoryd.AnswerCheckAgain, Label: "Check again"}, {Value: factoryd.AnswerDismiss, Label: "Dismiss"}}}
	roll := factoryd.HumanQuestion{Ask: "The roll failed.", Choices: []factoryd.QuestionChoice{
		{Value: factoryd.AnswerRetryRoll, Label: "Retry the roll"}, {Value: factoryd.AnswerPark, Label: "Park"}, {Value: factoryd.AnswerClose, Label: "Close"}}}
	plain := factoryd.HumanQuestion{Ask: "Which store?", Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerClose, Label: "Done"}}}
	for _, su := range []struct {
		source string
		q      factoryd.HumanQuestion
	}{{factoryd.SourceProductionHeld, held}, {factoryd.SourceChainStall, stall}, {factoryd.SourceRollFailed, roll}, {factoryd.SourceRoutine, plain}} {
		t.Run(su.source, func(t *testing.T) {
			r, ev := newRigWithEvents(t)
			q := su.q
			r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-hq", Kind: "human", Status: "waiting-human", Source: su.source,
				Repository: "sf/code", IssueNumber: 51, SHA: "abc1234", Objective: "needs you"}, Escalation: q.Ask, Question: &q}}
			ra := newRealAct(t, "t-hq", 51, su.source, q, policy.AutonomyManual)
			ra.wire(r, "t-hq", false)
			var choices []string
			for _, c := range q.Choices {
				choices = append(choices, c.Label+" ("+c.Value+")")
			}
			want := "card #51 takes one of its choices, with your note beside it: " + strings.Join(choices, ", ")
			ck, s := r.loggedIn(t)
			before := ev.count()
			rec := r.do(post("/tasks/t-hq/answer", url.Values{"csrf": {s.csrf}, "answer": {"retry it next week"}}), ck)
			if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want {
				t.Errorf("the button's words = %d %q, want 409 %q", rec.Code, rec.Body, want)
			}
			if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", Text: "retry it next week"}); err == nil || err.Error() != want {
				t.Errorf("the chat's words = %v, want %q", err, want)
			}
			if k := ra.caseNow(t); k.Status != factoryd.CaseAtHuman || len(r.fg.patched) != 0 || len(r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments")) != 0 || ev.count() != before {
				t.Errorf("case %s, patched %v, comments %v, %d events written; want it waiting, nothing posted and no event", k.Status, r.fg.patched, r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"), ev.count()-before)
			}
		})
	}
}

func (raWorkflows) Terminate(context.Context, string, string, string) error { return nil }

func (raWorkflows) Alive(context.Context, string) (bool, error)                { return true, nil }
func (raWorkflows) RunningRuns(context.Context) ([]factoryd.RunningRun, error) { return nil, nil }
