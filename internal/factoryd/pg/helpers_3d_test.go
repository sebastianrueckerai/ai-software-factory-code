package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// The rungRig methods the act family's moved files reach and factoryd's
// rung files still hold; tripCap, projectTree and the file fixtures they
// need moved before, with the trip rig into helpers_2d_test.go.

var boughtNothing = factoryd.PgBoughtNothing

// leaky is the moved copy of the act_propose one, which lands in this
// package with its file: a URL and a bearer token the refusals must scrub.
const leaky = "see https://user:pass@host/x with Authorization: Bearer abc123def456"

// poolDraws reaches the store's own SQL for the pool's draw rows, for the
// moved propose tests that count them directly.
const poolDraws = factoryd.PgPoolDraws

// rungFinding is the finding id rungAnswerText's dropped verdicts carry.
const rungFinding = "3f2a1c9b0e44"

// rungAnswerText, answerCall, answer and handUp are the moved copies of
// the rungconv and rung1 ladder ones: the propose tests' rung 2 answers
// hand the case up recommending close.
func rungAnswerText(t *testing.T, over map[string]any) string {
	t.Helper()
	a := map[string]any{
		"decision":       "extend",
		"reason_kind":    "gate_stop",
		"verdicts":       map[string]any{rungFinding: map[string]any{"verdict": "dropped", "evidence": "x.go:3 at head: the line is gone"}},
		"note":           "one more session",
		"recommendation": map[string]any{"action": "successor", "grounds": "the finding is dropped"},
		"draw":           map[string]any{"sessions": 1},
	}
	for k, v := range over {
		a[k] = v
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func answerCall(t *testing.T, over map[string]any) CheckinResponse {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(rungAnswerText(t, over)), &args); err != nil {
		t.Fatal(err)
	}
	return CheckinResponse{ToolName: CheckinAnswerTool, ToolArgs: args}
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

// raisedSessions and stepsOfRung are the moved copies of the rung1 ladder
// and rung2 ones, for the propose tests that read the card's raised row
// and one rung's steps.
func (g rungRig) raisedSessions(t *testing.T) int {
	t.Helper()
	var n int
	if err := g.s.PgDB().QueryRowContext(context.Background(), `SELECT raised_sessions FROM tasks WHERE id = 't1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (g rungRig) stepsOfRung(t *testing.T, id int64, rung int) []CaseStep {
	t.Helper()
	return slices.DeleteFunc(g.steps(t, id), func(s CaseStep) bool { return s.Rung != rung })
}

// projectStore answers the project row of demo-app, which no policy stored
// in the rig's database names. The moved copy of the rung1 ladder one.
type projectStore struct{ *PGStore }

func (projectStore) ProjectByRepository(context.Context, string) (Project, bool, error) {
	return Project{Name: "demo-app", Repository: "demo-app/demo-app", File: true}, true, nil
}

// rows, unjudgedSuffix, noJudgmentLine, spendChat and newAlertForgejo are
// the moved propose tests' slice of the rung and judgment-budget fixtures:
// rows and spendChat read the rig's rows through the surface, the words
// the spent judgment budget pins come through it, and the alert forge's
// own fake records nothing this file asserts, so its clean forge — the
// chain fake's empty answers — serves the chat budget's card writes.
const unjudgedSuffix = " (not judged, judgment budget spent)"

const noJudgmentLine = factoryd.PgNoJudgmentLine

const monitorFlagPrefix = factoryd.PgMonitorFlagPrefix

func (g rungRig) rows(t *testing.T, kind string) []Event { return actRig{s: g.s}.rows(t, kind) }

// spendChat spends the whole month budget of the chat task of demo-app.
func (g rungRig) spendChat(t *testing.T) Task {
	t.Helper()
	ctx := context.Background()
	chat, err := g.r.JudgmentTask(ctx, Task{Repository: "demo-app/demo-app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur WHERE id = $1`, chat.ID); err != nil {
		t.Fatal(err)
	}
	chat.SpentEUR = chat.BudgetEUR
	return chat
}

func newAlertForgejo(t *testing.T) (*Forgejo, *chainForgejo) {
	f := &chainForgejo{}
	return f.srv(t), f
}

// codeRepo and maintenanceTracks are the moved copies of the rolltask and
// tracks test fixtures; seedProject of the store one: test-file helpers
// cannot cross the package line. seedProject is repository as the code
// repository's project: seed, agents coder, the registrar token reading
// and running its Actions, the merge token merging, the gate token
// approving the merge gate's merges, the chat token the project agent's
// own turns.
const codeRepo = "sf/ai-software-factory-code"

var maintenanceTracks = policy.Tracks{Direct: policy.Track{Class: policy.ClassCoder}}

func seedProject(repository string) Project {
	return Project{Name: strings.ReplaceAll(repository, "/", "-"), Repository: repository, MergeRule: "human", Objects: "seed", Agents: "coder",
		File: true, Tokens: policy.SeedTokens{Read: policy.SecretCIRegistrar, Actions: policy.SecretCIRegistrar, Merge: policy.SecretMerge, Gate: policy.SecretGate,
			Chat: policy.SecretChat},
		Process: "maintenance", Tracks: maintenanceTracks}
}

// wakeCall and fakeWakeStarter are the moved copies of the wakes one: the
// deploy tests' rung-two rig starts its wake turns on the recording fake.
type wakeCall struct {
	project, login string
	conversation   int64
	birth, text    string
	cases          []int64
}

type fakeWakeStarter struct {
	mu    sync.Mutex
	calls []wakeCall
	err   error
	gone  map[int64]bool // conversations whose venue is gone: the wake refuses for good
	// block holds the wakes of one project on release, a turn in flight
	// the Waker must not wait for
	block   string
	release chan struct{}
}

func (f *fakeWakeStarter) StartWake(ctx context.Context, project, login string, conversation int64, birth, text string, cases ...int64) error {
	f.mu.Lock()
	f.calls = append(f.calls, wakeCall{project, login, conversation, birth, text, cases})
	gone := f.gone[conversation]
	wait := f.block == project && f.release != nil
	err := f.err
	f.mu.Unlock()
	if wait {
		<-f.release
	}
	if gone {
		return fmt.Errorf("conversation %d is archived: %w", conversation, ErrWakeVenueGone)
	}
	return err
}

func (f *fakeWakeStarter) recorded() []wakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// drainPass is the moved copy of the wakes one: a seeding pass, drained.
func drainPass(t *testing.T, w *Waker) error {
	t.Helper()
	err := w.Pass(t.Context())
	w.Drain()
	return err
}

// rungTwoRig is the moved copy of the rung2 one: a rung rig whose console
// wakes rung 2, on a manual project whose member sebastian the Waker
// wakes, under judgment.
func rungTwoRig(t *testing.T, judgment string) (rungRig, *Waker, *fakeWakeStarter) {
	t.Helper()
	g := newRungRig(t, "success")
	for _, table := range []string{"chat_messages", "chat_conversations", "chat_wake_cursor", "chat_wake_state"} {
		if _, err := g.s.PgDB().ExecContext(context.Background(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	v := g.r.PgCfg().Policy()
	p := v.Projects["demo-app"]
	p.Autonomy = policy.AutonomyManual
	p.Notify = []string{"sebastian"}
	p.Escalation = &policy.Escalation{Judgment: judgment}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.PgSetPolicy(func() policy.Version { return v })
	starter := &fakeWakeStarter{}
	w := &Waker{Store: g.s, Start: starter, Now: g.r.Clock, Current: g.r.PgCfg().Policy,
		Statement: g.r.RungTwoStatement, Board: g.r.BoardText}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("the seeding pass: %v", err)
	}
	return g, w, starter
}

// chainForgejo is the moved deploy tests' slice of the chainpoll fake:
// they construct only its empty instance — no runs, no carries, no
// statuses and no open issues — and assert only its comments, so this
// copy holds the fields they read and answers every route that empty
// instance answers, its seeded-state branches aside: a clean forge where
// every list is empty, every write records, every combined status is
// green, ci.yml is the one workflow file and the bot's fork holds no
// branch of a card.
type chainForgejo struct {
	mu       sync.Mutex
	issues   []string // "title\n\nbody" of every issue filed
	comments []string // "repository#issue body" of every comment posted
	closed   []string // "repository#issue" of every issue closed
}

func (f *chainForgejo) srv(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
		owner, rest, ok := strings.Cut(rest, "/")
		name, after, ok2 := strings.Cut(rest, "/")
		if !ok || owner == "" || name == "" {
			t.Errorf("call outside a repository: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		repo := owner + "/" + name
		p := ""
		if ok2 {
			p = "/" + after
		}
		switch {
		case p == "/actions/runs":
			reply(map[string]any{"workflow_runs": nil})
		case r.Method == "GET" && p == "/commits" && r.URL.Query().Get("not") != "":
			q := r.URL.Query()
			base, head := q.Get("not"), q.Get("sha")
			if q.Get("limit") == "1" { // holds: one commit head has that base lacks
				if base == head {
					reply([]any{})
				} else {
					reply([]map[string]any{{"sha": head}})
				}
				return
			}
			reply([]any{})
		case r.Method == "GET" && p == "/commits":
			reply(nil)
		case strings.HasPrefix(p, "/git/commits/"):
			reply(map[string]any{"parents": []map[string]string{{"sha": "parent-of-" + strings.TrimPrefix(p, "/git/commits/")}}})
		case strings.HasPrefix(p, "/commits/") && strings.HasSuffix(p, "/status"):
			reply(map[string]any{"state": "success", "statuses": nil})
		case r.Method == "GET" && p == "/issues":
			reply([]any{})
		case r.Method == "POST" && p == "/issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.issues = append(f.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 30 + len(f.issues)})
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			reply([]any{})
		case r.Method == "POST" && strings.HasPrefix(p, "/actions/runs/") && strings.HasSuffix(p, "/rerun"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && strings.HasPrefix(p, "/actions/runs/") && strings.HasSuffix(p, "/jobs"):
			http.NotFound(w, r)
		case r.Method == "POST" && strings.HasPrefix(p, "/actions/jobs/") && strings.HasSuffix(p, "/rerun"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments"):
			prefix := repo + "#" + strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments") + " "
			out := []map[string]any{}
			for i, c := range f.comments {
				if body, ok := strings.CutPrefix(c, prefix); ok {
					out = append(out, map[string]any{"id": i + 1, "body": body})
				}
			}
			reply(out)
		case r.Method == "POST" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, repo+"#"+strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")+" "+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"id": len(f.comments)})
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/dependencies"):
			reply([]any{})
		case r.Method == "PATCH" && strings.HasPrefix(p, "/issues/"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["state"] == "closed" {
				f.closed = append(f.closed, repo+"#"+strings.TrimPrefix(p, "/issues/"))
			}
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{})
		case p == "/contents/.forgejo/workflows" || p == "/contents/.gitea/workflows":
			reply([]map[string]any{{"name": "ci.yml", "type": "file"}})
		case r.Method == "GET" && p == "":
			reply(map[string]any{"name": repo})
		case r.Method == "GET" && owner == "factory-agent" && strings.HasPrefix(p, "/branches/"):
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// askOwn is the moved copy of the act_propose_retry one: test-file helpers
// cannot cross the package line. It records the question_asked row of case
// caseID, offering choices.
func askOwn(t *testing.T, store interface {
	Record(context.Context, Event) (int64, error)
}, taskID string, caseID int64, choices ...QuestionChoice) {
	t.Helper()
	if _, err := store.Record(context.Background(), Event{TaskID: taskID, Repository: "demo-app/demo-app", Kind: EventQuestionAsked,
		Detail: map[string]any{"case_id": caseID, "ask": "what now", "offers": choices}}); err != nil {
		t.Fatal(err)
	}
}

// wantSums, testMetrics, sums, collect and attrKey are the moved copies of
// the metrics and analytics test helpers, for the act tests that count what
// their grants publish.
func wantSums(t *testing.T, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("series = %v, want %v", got, want)
		}
	}
}

func testMetrics(t *testing.T) (*telemetry.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	m, err := telemetry.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	return m, reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Aggregation, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data, true
			}
		}
	}
	return nil, false
}

func attrKey(s attribute.Set) string { return s.Encoded(attribute.DefaultEncoder()) }

func sums(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	data, ok := collect(t, reader, name)
	if !ok {
		return out
	}
	switch d := data.(type) {
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = dp.Value
		}
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = float64(dp.Value)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = dp.Value
		}
	default:
		t.Fatalf("%s: unexpected %T", name, data)
	}
	return out
}

// firstPage already moved with the trip rig (helpers_2d_test.go); the
// recording backend below reaches it there.

// recordingBackend and newCompletion are the moved copies of the
// completion test's backend and constructor: the salvage close runs the
// console's issue close against the servers it records.
type recordingBackend struct {
	mu            sync.Mutex
	forgejoCalls  []string
	litellmCalls  []string
	forgejoStatus int
	litellmStatus int
	forgejoTokens string // GET /users/<bot>/tokens body
}

func (r *recordingBackend) srv(t *testing.T) (forgejo, litellm string) {
	t.Helper()
	fj := httptest.NewServer(firstPage(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.forgejoCalls = append(r.forgejoCalls, req.Method+" "+req.URL.Path)
		status := r.forgejoStatus
		tokens := r.forgejoTokens
		r.mu.Unlock()
		if tokens == "" {
			tokens = `[{"id":77,"name":"task-t1"},{"id":78,"name":"other"},{"id":79,"name":"task-t1-base"}]`
		}
		if req.Method == "GET" {
			io.WriteString(w, tokens)
			return
		}
		w.WriteHeader(status)
	}))
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.litellmCalls = append(r.litellmCalls, req.Method+" "+req.URL.Path)
		status := r.litellmStatus
		r.mu.Unlock()
		if req.URL.Path == "/key/list" {
			io.WriteString(w, `{"keys":[{"token":"h-t1","blocked":null,"metadata":{"task_id":"t1"}}],"total_count":1,"current_page":1,"total_pages":1}`)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(fj.Close)
	t.Cleanup(ll.Close)
	return fj.URL, ll.URL
}

func newCompletion(store Store, fjURL, llURL string) *Completion {
	return &Completion{
		Store:   store,
		Forgejo: &Forgejo{BaseURL: fjURL, BotUser: "factory-bot", BotPassword: "bot-pw"},
		LiteLLM: &LiteLLMAdmin{BaseURL: llURL, MasterKey: "mk"},
	}
}

// fakeKube is the moved tests' empty cluster: factoryd's scripted fake —
// the runner tests' whole seam — is a fixture the size of a file of its
// own, and the salvage close only needs the answers a fresh one holds: no
// pod, no namespace and no secret exists, writes change nothing the runner
// can read back, and NodeStorage answers its one default node.
type fakeKube struct{}

func newFakeKube() *fakeKube { return &fakeKube{} }

func (k *fakeKube) CreateNamespace(context.Context, string, map[string]string) error { return nil }

func (k *fakeKube) DeleteNamespace(ctx context.Context, name string) error {
	return factoryd.ErrNotFound
}

func (k *fakeKube) NamespacesByLabel(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (k *fakeKube) ListNamespacesByLabel(context.Context, string, string) ([]factoryd.NamespaceInfo, error) {
	return nil, nil
}

func (k *fakeKube) CreateServiceAccount(context.Context, string, string) error { return nil }

func (k *fakeKube) CreateSecret(context.Context, string, string, string, map[string]string) error {
	return nil
}

func (k *fakeKube) GetSecretData(context.Context, string, string) (map[string]string, error) {
	return nil, factoryd.ErrNotFound
}

func (k *fakeKube) CreateCiliumNetworkPolicy(context.Context, string, string, map[string]any) error {
	return nil
}

func (k *fakeKube) ApplyCiliumNetworkPolicy(context.Context, string, string, map[string]any) error {
	return nil
}

func (k *fakeKube) CiliumNetworkPolicySpec(context.Context, string, string) (map[string]any, error) {
	return nil, factoryd.ErrNotFound
}

func (k *fakeKube) Create(context.Context, string, map[string]any) error { return nil }

func (k *fakeKube) CreatePod(context.Context, string, string, map[string]any) error { return nil }

func (k *fakeKube) NodeStorage(context.Context) ([]factoryd.NodeStorage, []int64, error) {
	return []factoryd.NodeStorage{{Name: "n1", Allocatable: 1 << 50}}, nil, nil
}

func (k *fakeKube) DeletePod(context.Context, string, string) error { return factoryd.ErrNotFound }

func (k *fakeKube) PodStatus(context.Context, string, string) (string, int32, error) {
	return "", -1, factoryd.ErrNotFound
}

func (k *fakeKube) PodIP(context.Context, string, string) (string, error) {
	return "", factoryd.ErrNotFound
}

func (k *fakeKube) PodEnv(context.Context, string, string, string) (map[string]string, error) {
	return nil, factoryd.ErrNotFound
}

func (k *fakeKube) PodInitFinished(context.Context, string, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (k *fakeKube) PodLog(context.Context, string, string) (io.ReadCloser, error) {
	return nil, factoryd.ErrNotFound
}

func (k *fakeKube) PodDiagnosis(context.Context, string, string) (factoryd.PodDiagnosis, error) {
	return factoryd.PodDiagnosis{}, factoryd.ErrNotFound
}

func (k *fakeKube) PodLogTail(context.Context, string, string, int) (string, error) {
	return "", factoryd.ErrNotFound
}

func (k *fakeKube) NewestPodEvent(context.Context, string, string) (string, error) {
	return "", nil
}

func (k *fakeKube) PersistentVolumes(context.Context) ([]factoryd.PersistentVolume, error) {
	return nil, nil
}

func (k *fakeKube) DeletePersistentVolume(context.Context, string) error { return nil }

func (k *fakeKube) CronJobs(context.Context, string) ([]factoryd.CronJobState, error) {
	return nil, nil
}

func (k *fakeKube) ScheduledBackups(context.Context, string) ([]factoryd.CronJobState, error) {
	return nil, nil
}

func (k *fakeKube) Deployment(ctx context.Context, namespace, name string) (factoryd.Deployment, error) {
	return factoryd.Deployment{}, factoryd.ErrNotFound
}

// settle waits for the rung 1 conversations the rig launched, through the
// runner's rungs surface.
func (g rungRig) settle() { g.r.PgWaitRungs() }

func (g rungRig) steps(t *testing.T, id int64) []CaseStep { return caseStepsOf(t, g.s, id) }

// atAutonomy puts the rig's project at state: full auto is autonomous with
// merge rule auto.
func (g rungRig) atAutonomy(state string) {
	v := g.r.PgCfg().Policy()
	p := v.Projects["demo-app"]
	p.Autonomy, p.MergeRule = state, policy.MergeHuman
	if state == policy.AutonomyFullAuto {
		p.Autonomy, p.MergeRule = policy.AutonomyAutonomous, policy.MergeAuto
	}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.PgSetPolicy(func() policy.Version { return v })
}
