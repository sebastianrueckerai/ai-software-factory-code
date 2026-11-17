package pg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The helpers the second move's files (draws, heal_ask, offers,
// old_records, sessionkill) shared with factoryd's own test files and
// cannot reach across the package line: copies, kept verbatim, edited only
// where they build a Runner or touch a store's db handle.

// fakeObjectStore is the in-memory object store the moved rigs build on.
type fakeObjectStore struct {
	mu   sync.Mutex
	puts []struct {
		key  string
		body []byte
	}
}

func (s *fakeObjectStore) Put(ctx context.Context, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, struct {
		key  string
		body []byte
	}{key, body})
	return nil
}

// Get returns the last body put under key, nil when none was.
func (s *fakeObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.puts) - 1; i >= 0; i-- {
		if s.puts[i].key == key {
			return s.puts[i].body, nil
		}
	}
	return nil, nil
}

// Delete forgets every body put under key.
func (s *fakeObjectStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.puts {
		if s.puts[i].key == key {
			s.puts[i].body = nil
		}
	}
	return nil
}

// fakeLeases is the registry's live leased addresses by task.
type fakeLeases struct {
	mu           sync.Mutex
	byID         map[string][]string
	commissioned []cloudres.Commissioned // what the next claim returns
}

func (f *fakeLeases) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.commissioned
	f.commissioned = nil
	return out, nil
}

func (f *fakeLeases) LiveAddresses(context.Context) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.byID {
		out[k] = v
	}
	return out, nil
}

// fakeWorkflows is the scripted workflow client the moved rigs run on.
type fakeWorkflows struct {
	started      []string
	signals      []string // "<task> <signal> <author> #<pr>: <body>"
	err          error    // returned by Signal when set
	signalFails  int      // Signal fails this many calls first
	onStart      func(taskID string)
	routineRuns  []RoutineRunInput
	reopens      []reopenCall // the reopens that started a run
	reopenFails  int          // Reopen fails this many calls first
	terminated   []string     // "<task> <reason>" per Terminate
	aliveErr     error
	terminateErr error             // returned by Terminate when set
	gone         map[string]bool   // tasks whose workflow does not run
	running      []RunningRun      // runs RunningRuns lists
	current      map[string]string // the run id each task runs now, when it is set
	lists        int               // RunningRuns calls
}

func (w *fakeWorkflows) RunningRuns(ctx context.Context) ([]RunningRun, error) {
	w.lists++
	return w.running, nil
}

func (w *fakeWorkflows) Terminate(ctx context.Context, taskID, runID, reason string) error {
	if w.terminateErr != nil {
		return w.terminateErr
	}
	if now, ok := w.current[taskID]; ok && runID != "" && runID != now {
		return nil
	}
	w.terminated = append(w.terminated, taskID+" "+reason)
	return nil
}

func (w *fakeWorkflows) Alive(ctx context.Context, taskID string) (bool, error) {
	if w.aliveErr != nil {
		return false, w.aliveErr
	}
	return !w.gone[taskID], nil
}

// reopenCall is a Workflows.Reopen a fake took.
type reopenCall struct {
	taskID, signal string
	sig            TaskSignal
}

func (w *fakeWorkflows) Reopen(ctx context.Context, taskID, signal string, sig TaskSignal) error {
	if w.reopenFails > 0 {
		w.reopenFails--
		return errors.New("temporal down")
	}
	w.reopens = append(w.reopens, reopenCall{taskID, signal, sig})
	return nil
}

func (w *fakeWorkflows) Start(ctx context.Context, taskID string) error {
	w.started = append(w.started, taskID)
	if w.onStart != nil {
		w.onStart(taskID)
	}
	return nil
}

func (w *fakeWorkflows) StartRoutineRun(ctx context.Context, in RoutineRunInput) error {
	w.routineRuns = append(w.routineRuns, in)
	return nil
}

func (w *fakeWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	if w.signalFails > 0 {
		w.signalFails--
		return errors.New("temporal down")
	}
	if w.err != nil {
		return w.err
	}
	comment := ""
	if sig.Comment != 0 {
		comment = fmt.Sprintf(" c%d", sig.Comment)
	}
	w.signals = append(w.signals, fmt.Sprintf("%s %s %s #%d%s: %s", taskID, name, sig.Author, sig.PR, comment, sig.Body))
	return nil
}

// testRunnerConfig is the moved copy of the runner_test one: the same
// fields over this package's testPolicyVersion and fakes.
func testRunnerConfig() RunnerConfig {
	return RunnerConfig{
		AgentNamespacePrefix: "factory-agents-",
		WorkerImage:          "reg/worker@sha256:abc",
		SandboxBaseImage:     "reg/sandbox-base@sha256:bcd",
		RegistryHost:         "store.test",
		PullSecret:           "forgejo-registry",
		FactoryNamespace:     "factory",
		GatewayURL:           "http://factory-gateway.factory.svc.cluster.local:8081",
		GitBaseURL:           "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:               ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens:         8192,
		HeadroomTokens:       16384,
		ChecksPoll:           time.Millisecond,
		EgressCanary:         "example.com:443",
		EgressDirectIP:       "192.0.2.7:443",
		Supervisor:           SupervisorConfig{RepetitionEvents: 24},
		Policy:               testPolicyVersion,
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			return testPolicyVersion(), id == testPolicyVersion().ID, nil
		},
		Leases:      &fakeLeases{},
		Gates:       &GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &fakeWorkflows{},
	}
}

// demoFile is demoProject's projects/demo-app.yaml.
const demoFile = `autonomy: autonomous
repository: demo-app/demo-app
merge_rule: auto
objects: factory
agents: coder
process: maintenance
namespaces: [demo-staging, demo-prod]
environments:
  - {namespace: demo-staging, path: deploy/staging, hostname: demo-staging.aisoftwarefactory.world}
  - {namespace: demo-prod, path: deploy/prod, hostname: demo.aisoftwarefactory.world}
quota: {cpu: "2", memory: 4Gi, storage: 10Gi}
secrets: [demo-write-token/token]
envelope: {monthly_eur: 5, server_types: [cx23], max_count: 1, max_lease_hours: 2, volume_max_gb: 0}
egress: [api.example.com]
`

// codeFile is the factory's own code as a seed project agents work on.
const codeFile = `repository: sf/ai-software-factory-code
merge_rule: human
objects: seed
agents: coder
process: maintenance
namespaces: [factory]
tokens: {read: forgejo-ci-registrar, actions: forgejo-ci-registrar, merge: forgejo-merge}
`

// projectTree is testTree with the code and demo-app files beside infra's.
func projectTree(t *testing.T) policy.Tree {
	t.Helper()
	files := testFiles(testPolicyYAML)
	for name, body := range map[string]string{"ai-software-factory-code": codeFile, "demo-app": demoFile} {
		files["projects/"+name+".yaml"] = []byte(policy.Header("projects/"+name+".yaml") + "\n" + body)
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// firstPage serves h for a paged list's first page and an empty page
// after it, for stubs that answer a list whatever the page asked.
func firstPage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Query().Get("page"); p != "" && p != "1" && !strings.Contains(r.URL.Path, "/git/trees/") && !strings.HasSuffix(r.URL.Path, "/actions/tasks") {
			io.WriteString(w, `[]`)
			return
		}
		h(w, r)
	}
}

// isWriterRead is one of RepoWriters' reads.
func isWriterRead(path string) bool {
	return strings.HasSuffix(path, "/assignees") || strings.HasSuffix(path, "/collaborators") ||
		strings.HasPrefix(path, "/api/v1/orgs/") || strings.HasPrefix(path, "/api/v1/teams/")
}

// servePage answers a paged list the way a Forgejo with a smaller
// MAX_RESPONSE_ITEMS does: per items a page whatever the limit asked,
// then an empty page.
func servePage(w http.ResponseWriter, r *http.Request, per int, items ...string) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	lo := min(max(page-1, 0)*per, len(items))
	io.WriteString(w, "["+strings.Join(items[lo:min(lo+per, len(items))], ",")+"]")
}

// serveWriters answers RepoWriters' reads for a code repository whose
// Owners team holds sebastian and whose bot, merge gate user and chat user
// are write collaborators, each read on its token.
func serveWriters(t *testing.T, w http.ResponseWriter, r *http.Request) {
	want := "token registrar-tok"
	if strings.HasPrefix(r.URL.Path, "/api/v1/orgs/") || strings.HasPrefix(r.URL.Path, "/api/v1/teams/") {
		want = "token org-read-tok"
	}
	if got := r.Header.Get("Authorization"); got != want {
		t.Errorf("%s read with %q, want %q", r.URL.Path, got, want)
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/orgs/"):
		servePage(w, r, 50, `{"id":1,"permission":"owner","includes_all_repositories":true,"units_map":{"repo.code":"owner"}}`)
	case r.URL.Path == "/api/v1/teams/1/members":
		servePage(w, r, 50, `{"login":"sebastian"}`)
	case strings.HasSuffix(r.URL.Path, "/collaborators"):
		servePage(w, r, 50, `{"login":"factory-agent"}`, `{"login":"factory-gate"}`, `{"login":"factory-chat"}`)
	case strings.HasSuffix(r.URL.Path, "/assignees"):
		io.WriteString(w, `[{"login":"sebastian"},{"login":"factory-agent"},{"login":"factory-gate"},{"login":"factory-chat"}]`)
	default:
		http.NotFound(w, r)
	}
}

// noneCompleted is the heal pass's run check where no run completed.
func noneCompleted(context.Context, string) (bool, error) { return false, nil }

// caseSignals records the signals that reached the workflows.
type caseSignals struct {
	Workflows
	mu  sync.Mutex
	got []reopenCall
}

func (w *caseSignals) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, reopenCall{taskID, name, sig})
	return nil
}

func (w *caseSignals) Terminate(context.Context, string, string, string) error { return nil }

func (w *caseSignals) Alive(context.Context, string) (bool, error)       { return true, nil }
func (w *caseSignals) RunningRuns(context.Context) ([]RunningRun, error) { return nil, nil }

// actRig holds the store whose rows the moved rig reads; the full rig
// stays in factoryd beside the tests that use its runner and forge.
type actRig struct {
	s *factoryd.PGStore
}

func (g actRig) rows(t *testing.T, kind string) []Event {
	t.Helper()
	events, err := g.s.TaskEvents(context.Background(), "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			out = append(out, events[i])
		}
	}
	return out
}

// tripForge is the Forgejo the trip stub serves.
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

// tripRig is a runner on the trip stub's Forgejo.
type tripRig struct {
	r       *Runner
	s       *factoryd.PGStore
	forge   *tripForge
	signals *caseSignals
}

func newTripRig(t *testing.T, checks string) tripRig {
	t.Helper()
	ctx := context.Background()
	// The db name is pg's own: the ladder rig still in factoryd uses
	// "ladder", and the suites share one postgres when they run in
	// parallel.
	s := cardCaseStore(t, "pg_ladder")
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
	r := factoryd.PgRigRunner(s, objects, cfg, &Timeline{Objects: objects, Events: s, Store: s},
		&Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"})
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

// asks are t1's question_asked rows, oldest first.
func (g tripRig) asks(t *testing.T) []Event { return actRig{s: g.s}.rows(t, EventQuestionAsked) }

const tripCap = capReached + "\nlast: no session produced a result"
