package factoryd

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

func (g tripRig) trip(t *testing.T, reason string) TripOutcome {
	t.Helper()
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: reason}, tripAttempt{N: 1})
	if err != nil {
		t.Fatalf("OpenTrip: %v", err)
	}
	return out
}

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

// projectStore answers the project row of demo-app, which no policy stored
// in the rig's database names.
type projectStore struct{ *PGStore }

func (projectStore) ProjectByRepository(context.Context, string) (Project, bool, error) {
	return Project{Name: "demo-app", Repository: "demo-app/demo-app", File: true}, true, nil
}

// settle waits for the rung 1 goroutines the trips started.
func (g rungRig) settle() { g.r.rungs.Wait() }

func (g rungRig) steps(t *testing.T, id int64) []CaseStep { return caseStepsOf(t, g.s, id) }

// policyRungsOff is v with every project at manual under escalation.judgment
// autonomous_and_above: no rung judges, so a stop faces the human at once.
func policyRungsOff(v policy.Version) policy.Version {
	projects := maps.Clone(v.Projects)
	for name, p := range projects {
		p.Autonomy = policy.AutonomyManual
		p.Escalation = judgesAtAutonomous
		projects[name] = p
	}
	v.Projects = projects
	return v
}

func rungsOff(r *Runner) {
	v := policyRungsOff(r.cfg.Policy())
	r.cfg.Policy = func() policy.Version { return v }
}

func (g rungRig) stepsOfRung(t *testing.T, id int64, rung int) []CaseStep {
	t.Helper()
	return slices.DeleteFunc(g.steps(t, id), func(s CaseStep) bool { return s.Rung != rung })
}

// afterRung2 lets rung 2's step on case id run past its bound, as the sweep
// finds it: the step ends handed up and the case climbs to the human.
func (g rungRig) afterRung2(t *testing.T, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 2`, id); err != nil {
		t.Fatal(err)
	}
	own := g.stepsOfRung(t, id, 2)
	if len(own) != 1 || own[0].DueAt == nil {
		t.Fatalf("rung 2 steps = %+v, want one with a bound", own)
	}
	g.r.now = func() time.Time { return own[0].DueAt.Add(time.Minute) }
	expired, err := g.s.ExpireSteps(ctx, g.r.Clock())
	if err != nil {
		t.Fatal(err)
	}
	if err := g.r.ClimbExpired(ctx, expired); err != nil {
		t.Fatal(err)
	}
	g.settle()
}
