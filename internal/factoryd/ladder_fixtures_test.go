package factoryd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tripRig is a runner on Postgres with card t1, a large agent card of
// demo-app/demo-app on the direct track, running on issue 12 with a
// budget of 3 EUR, under the policy of the act rig with session floors of
// 0.30 EUR. Forgejo answers the checks of the heads in checks by their
// state, takes comments on issue 12 (ids 501 on, failing the next one
// when failComment is set) and its labels, and knows nothing else. The
// workflows record every signal.
type tripRig struct {
	r       *Runner
	s       *PGStore
	forge   *tripForge
	signals *caseSignals
}

type tripForge struct {
	mu          sync.Mutex
	checks      map[string]string // a head's combined state: success or failure
	comments    []string
	failComment bool
	// files holds a repository file by "ref:path", prCommits the body of
	// pull request 9's commits, and redLog the log of the one failed job of
	// run 7, which the red heads' statuses point at when it is set
	files     map[string]string
	prCommits string
	redLog    string
}

func (f *tripForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const issue = "/api/v1/repos/demo-app/demo-app/issues/12"
		const repo = "/api/v1/repos/demo-app/demo-app"
		switch {
		case strings.HasPrefix(r.URL.Path, repo+"/raw/"):
			body, ok := f.files[r.URL.Query().Get("ref")+":"+strings.TrimPrefix(r.URL.Path, repo+"/raw/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, body)
		case r.URL.Path == repo+"/pulls/9/commits" && r.URL.Query().Get("page") == "1" && f.prCommits != "":
			fmt.Fprint(w, f.prCommits)
		case r.URL.Path == repo+"/pulls/9/commits":
			fmt.Fprint(w, `[]`)
		case r.URL.Path == repo+"/actions/runs":
			fmt.Fprint(w, `{"workflow_runs":[{"id":70,"index_in_repo":7}]}`)
		case r.URL.Path == repo+"/actions/runs/70/jobs":
			fmt.Fprint(w, `[{"id":12,"name":"test","status":"failure","attempt":1}]`)
		case r.URL.Path == repo+"/actions/jobs/12/logs":
			fmt.Fprint(w, f.redLog)
		case r.Method == http.MethodPost && r.URL.Path == issue+"/comments" && f.failComment:
			f.failComment = false
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == issue+"/comments":
			b, _ := io.ReadAll(r.Body)
			f.comments = append(f.comments, string(b))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.comments))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/commits/") && strings.HasSuffix(r.URL.Path, "/status") &&
			f.checks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/commits/"), "/status")] != "":
			state := f.checks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/commits/"), "/status")]
			target := ""
			if state == "failure" && f.redLog != "" {
				target = "/demo-app/demo-app/actions/runs/7/jobs/0"
			}
			fmt.Fprintf(w, `{"state":%q,"statuses":[{"status":%[1]q,"context":"ci / test","target_url":%q}]}`, state, target)
		case strings.HasPrefix(r.URL.Path, issue+"/labels"):
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTripRig(t *testing.T, checks string) tripRig {
	t.Helper()
	ctx := context.Background()
	s := cardCaseStore(t, "ladder")
	if err := s.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 12, Track: policy.TrackDirect, Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	v.Document.Budget.SessionFloorCheapEUR, v.Document.Budget.SessionFloorFrontierEUR = 0.3, 0.3
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	signals, forge := &caseSignals{}, &tripForge{checks: map[string]string{"h2": checks}}
	cfg.Workflows = signals
	objects := &fakeObjectStore{}
	r := &Runner{store: s, objects: objects, cfg: cfg, timeline: &Timeline{Objects: objects, Events: s, Store: s},
		forgejo: &Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"}}
	return tripRig{r: r, s: s, forge: forge, signals: signals}
}

// atHead puts pull request #9 on t1 with head h2, whose newest review
// approved it when approved.
func (g tripRig) atHead(t *testing.T, approved bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	verdict := "concerns"
	if approved {
		verdict = "approve"
	}
	for _, e := range []Event{
		{Kind: EventGateHead, Detail: map[string]any{"gate": GateMerge, "head": "h2"}},
		{Kind: EventModelReview, Detail: map[string]any{"commit": "h2", "round": 1, "reviewed": true, "verdict": verdict}},
	} {
		e.Repository, e.TaskID, e.IssueNumber, e.Actor, e.Via = "demo-app/demo-app", "t1", 12, FactoryDecider, ViaFactoryd
		if _, err := g.s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func (g tripRig) trip(t *testing.T, reason string) TripOutcome {
	t.Helper()
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: reason}, tripAttempt{N: 1})
	if err != nil {
		t.Fatalf("OpenTrip: %v", err)
	}
	return out
}

// asks are t1's question_asked rows, oldest first.
func (g tripRig) asks(t *testing.T) []Event { return actRig{s: g.s}.rows(t, EventQuestionAsked) }

const tripCap = CapReached + "\nlast: no session produced a result"

const rungFindingText = "TasksByPR is undefined"

// rungRig is tripRig's card t1 with a check-in model: a medium card of the
// coder class, whose head h2 on pull request 9 is green and not approved,
// and the review's one finding, rungFindingText, on h2. The check-in's
// model is a script that notes the task each call booked on.
type rungRig struct {
	tripRig
	chat *idChat
	id   string // the finding's id
}

type idChat struct {
	scriptedChat
	mu  sync.Mutex
	ids []string
}

func (c *idChat) Call(ctx context.Context, taskID, alias string, in CheckinRequest) (CheckinResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, taskID)
	return c.scriptedChat.Call(ctx, taskID, alias, in)
}

func newRungRig(t *testing.T, checks string, answers ...CheckinResponse) rungRig {
	t.Helper()
	ctx := context.Background()
	g := rungRig{tripRig: newTripRig(t, checks), chat: &idChat{scriptedChat: scriptedChat{answers: answers}}, id: FindingID(rungFindingText)}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET required_capabilities = '["coder"]'::jsonb, size = 'medium', budget_eur = 5 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.atHead(t, false)
	if err := g.r.timeline.Append(ctx, "t1", "model_review", map[string]any{"commit": "h2", "verdict": "concerns", "findings": []string{rungFindingText}}); err != nil {
		t.Fatal(err)
	}
	g.r.Checkin = g.chat
	g.r.cfg.Prices = aliasPrices{"big": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}}
	return g
}

// answer is the rung's answer call on rungFindingText, a dropped verdict
// with its evidence, over's keys over the defaults.
func (g rungRig) answer(t *testing.T, over map[string]any) CheckinResponse {
	t.Helper()
	a := map[string]any{"recommendation": map[string]any{"action": "successor", "grounds": "the finding is dropped"},
		"verdicts": map[string]any{g.id: map[string]any{"verdict": "dropped", "evidence": "tasks.go:12 at head: TasksByPR is declared"}}}
	for k, v := range over {
		a[k] = v
	}
	return answerCall(t, a)
}

// handUp is the answer that hands the case up recommending close.
func (g rungRig) handUp(t *testing.T) CheckinResponse {
	return g.answer(t, map[string]any{"decision": "hand_up", "draw": nil,
		"recommendation": map[string]any{"action": "close", "grounds": "the card's work is done on main"}})
}

// projectStore answers the project row of demo-app, which no policy stored
// in the rig's database names.
type projectStore struct{ *PGStore }

func (projectStore) ProjectByRepository(context.Context, string) (Project, bool, error) {
	return Project{Name: "demo-app", Repository: "demo-app/demo-app", File: true}, true, nil
}

// settle waits for the rung 1 goroutines the trips started.
func (g rungRig) settle() { g.r.rungs.Wait() }

func (g rungRig) rows(t *testing.T, kind string) []Event { return actRig{s: g.s}.rows(t, kind) }

func (g rungRig) steps(t *testing.T, id int64) []CaseStep { return caseStepsOf(t, g.s, id) }

func (g rungRig) signalsOf(name string) []reopenCall {
	g.signals.mu.Lock()
	defer g.signals.mu.Unlock()
	var out []reopenCall
	for _, s := range g.signals.got {
		if s.signal == name {
			out = append(out, s)
		}
	}
	return out
}
