package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// The card whose session is stopped at the task's wall-clock cap cannot end
// with its work committed but unshipped: the stop asks the pod's /wip for
// the work in progress and pushes it to the task branch, and where no push
// can land the outcome names the unpushed work. The session activity runs
// whole on the rig's fakes: the stopKube plays the worker pod (it streams
// model-call events and never pushes or reports), and the endpoint server
// answers /brief and /wip with a real bundle over the session's real start.

// The stop's finding leads, from wippush.go, where the acceptance package
// cannot reach them.
const (
	wipCommitterEmail = "factory-wip@factory.local"
	stopTakenNoteText = "the session's work, taken at the stop, is an interim commit on"
	stopUnpushedText  = "the session's work at the stop was not pushed: "
)

// stopPolicy is the maintenance policy with the agent time pinned to
// seconds, so the session's deadline falls seconds out and the supervisor
// trips wall_clock on the next event.
func stopPolicy() policy.Version {
	v := wipPolicy()
	v.Tree.Document.Limits.AgentTime = 6 * time.Second
	return v
}

// gitOut runs one git command in dir and answers its trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	return gitEnv(dir, nil, args...)
}

// gitEnv is gitOut with the command's extra environment.
func gitEnv(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Env = append(os.Environ(), append(append([]string{}, env...), "GIT_CONFIG_NOSYSTEM=1")...)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return strings.TrimSpace(buf.String()), nil
}

// stopGitSeeds seeds the task's repositories the way wipRig does: the work
// repository with one base commit on main, pushed to the upstream's bare
// demo-app/demo-app.git and the fork's bare factory-agent/demo-app.git with
// the task branch factory/t-wip at the base. It answers the base's sha.
func stopGitSeeds(t *testing.T, root string) string {
	t.Helper()
	seed := filepath.Join(root, "seed")
	if _, err := gitOut(root, "init", "-q", "-b", "main", seed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOut(seed, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	agent := []string{"GIT_AUTHOR_NAME=factory-agent", "GIT_AUTHOR_EMAIL=factory-agent@factory.local",
		"GIT_COMMITTER_NAME=factory-agent", "GIT_COMMITTER_EMAIL=factory-agent@factory.local"}
	if _, err := gitEnv(seed, agent, "commit", "-q", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := gitOut(seed, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	up := filepath.Join(root, "demo-app", "demo-app.git")
	fork := filepath.Join(root, "factory-agent", "demo-app.git")
	for _, dir := range []string{up, fork} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := gitOut(root, "init", "-q", "--bare", dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitOut(seed, "push", "-q", up, base+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOut(seed, "push", "-q", fork, base+":refs/heads/factory/t-wip"); err != nil {
		t.Fatal(err)
	}
	return base
}

// stopForkHead is the bare fork's task-branch head, "" when it holds none.
func stopForkHead(root string) string {
	head, err := gitOut(filepath.Join(root, "factory-agent", "demo-app.git"),
		"rev-parse", "--verify", "--quiet", "refs/heads/factory/t-wip")
	if err != nil {
		return ""
	}
	return head
}

// stopPod is one worker pod: a pipe whose writer streams worker event
// lines, closed when the pod is deleted.
type stopPod struct {
	pr *io.PipeReader
	pw *io.PipeWriter
}

// stopKube is the kube half of the worker pod: CreatePod starts a log
// stream of model-call events (never a push, never a result), DeletePod
// ends the pod and closes the stream, and PodIP keeps answering loopback
// for a deleted pod, so the stop's take reaches the endpoint server.
type stopKube struct {
	factoryd.Kube
	mu   sync.Mutex
	pods map[string]*stopPod
	gone map[string]bool
}

func (k *stopKube) key(ns, name string) string { return ns + "/" + name }

func (k *stopKube) CreateNamespace(context.Context, string, map[string]string) error { return nil }
func (k *stopKube) Create(context.Context, string, map[string]any) error             { return nil }
func (k *stopKube) CreateServiceAccount(context.Context, string, string) error       { return nil }
func (k *stopKube) CreateSecret(context.Context, string, string, string, map[string]string) error {
	return nil
}
func (k *stopKube) ApplyCiliumNetworkPolicy(context.Context, string, string, map[string]any) error {
	return nil
}
func (k *stopKube) GetSecretData(context.Context, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (k *stopKube) NamespacesByLabel(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (k *stopKube) ListNamespacesByLabel(context.Context, string, string) ([]factoryd.NamespaceInfo, error) {
	return nil, nil
}
func (*stopKube) DeleteNamespace(context.Context, string) error { return factoryd.ErrNotFound }
func (*stopKube) PersistentVolumes(context.Context) ([]factoryd.PersistentVolume, error) {
	return nil, nil
}
func (*stopKube) DeletePersistentVolume(context.Context, string) error { return nil }

func (k *stopKube) CreatePod(_ context.Context, ns, name string, _ map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pods == nil {
		k.pods, k.gone = map[string]*stopPod{}, map[string]bool{}
	}
	key := k.key(ns, name)
	if _, ok := k.pods[key]; ok {
		return factoryd.ErrAlreadyExists
	}
	pr, pw := io.Pipe()
	k.pods[key] = &stopPod{pr: pr, pw: pw}
	go func() {
		for {
			line, _ := json.Marshal(map[string]any{"type": "event", "event": map[string]any{"kind": "model_call"}})
			if _, err := fmt.Fprintf(pw, "%s\n", line); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return nil
}

func (k *stopKube) DeletePod(_ context.Context, ns, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := k.key(ns, name)
	p, ok := k.pods[key]
	if !ok || k.gone[key] {
		return factoryd.ErrNotFound
	}
	k.gone[key] = true
	p.pw.Close()
	return nil
}

// PodStatus answers Running while the pod lives, not-found once deleted,
// so the log follow ends when the stream does.
func (k *stopKube) PodStatus(_ context.Context, ns, name string) (string, int32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := k.key(ns, name)
	if _, ok := k.pods[key]; !ok || k.gone[key] {
		return "", -1, factoryd.ErrNotFound
	}
	return "Running", 0, nil
}

// PodIP keeps answering loopback for a deleted pod: the stop's ask of the
// /wip endpoint reaches the test's server.
func (k *stopKube) PodIP(_ context.Context, ns, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.pods[k.key(ns, name)]; !ok {
		return "", factoryd.ErrNotFound
	}
	return "127.0.0.1", nil
}

func (k *stopKube) PodLog(_ context.Context, ns, name string) (io.ReadCloser, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := k.key(ns, name)
	p, ok := k.pods[key]
	if !ok || k.gone[key] {
		return nil, factoryd.ErrNotFound
	}
	return io.NopCloser(p.pr), nil
}

func (k *stopKube) PodDiagnosis(_ context.Context, ns, name string) (factoryd.PodDiagnosis, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := k.key(ns, name)
	if _, ok := k.pods[key]; !ok {
		return factoryd.PodDiagnosis{}, factoryd.ErrNotFound
	}
	if k.gone[key] {
		return factoryd.PodDiagnosis{Phase: "Succeeded"}, nil
	}
	return factoryd.PodDiagnosis{Phase: "Running"}, nil
}

func (k *stopKube) PodInitFinished(context.Context, string, string) (time.Time, bool, error) {
	return time.Now(), true, nil
}

func (k *stopKube) PodLogTail(context.Context, string, string, int) (string, error) {
	return "", factoryd.ErrNotFound
}

func (k *stopKube) NewestPodEvent(context.Context, string, string) (string, error) { return "", nil }

// stopEndpoint is the worker pod's endpoint server: /brief answers, /wip
// ships one WIP commit of the worker's shape on the session's start as a
// real bundle with the commit in the Wip-Commit header — or, when fail is
// set, answers 500, so no push can land. Every other worker route answers
// with status.
type stopEndpoint struct {
	mu      sync.Mutex
	seed    string
	base    string
	fail    bool
	shipped string
}

func (e *stopEndpoint) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/brief", "/message", "/push", "/stop":
			w.WriteHeader(http.StatusNoContent)
		case "/wip":
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.fail {
				http.Error(w, "the pod's answer failed", http.StatusInternalServerError)
				return
			}
			if e.shipped != "" {
				w.WriteHeader(http.StatusNoContent) // nothing more to ship
				return
			}
			commit, body, ok := e.ship(t)
			if !ok {
				http.Error(w, "the pod could not stage its work", http.StatusInternalServerError)
				return
			}
			e.shipped = commit
			w.Header().Set("Wip-Commit", commit)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
}

// ship stages one edit of the worker's own shape on the session's start
// and answers its commit and a real bundle of it.
func (e *stopEndpoint) ship(t *testing.T) (string, []byte, bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.seed, "work.txt"), []byte("the session's work\n"), 0o644); err != nil {
		t.Errorf("work file: %v", err)
		return "", nil, false
	}
	if _, err := gitOut(e.seed, "add", "-A"); err != nil {
		t.Errorf("stage the work: %v", err)
		return "", nil, false
	}
	tree, err := gitOut(e.seed, "write-tree")
	if err != nil {
		t.Errorf("write tree: %v", err)
		return "", nil, false
	}
	worker := []string{"GIT_AUTHOR_NAME=factory-agent", "GIT_AUTHOR_EMAIL=factory-agent@factory.local",
		"GIT_COMMITTER_NAME=factory-wip", "GIT_COMMITTER_EMAIL=" + wipCommitterEmail}
	commit, err := gitEnv(e.seed, worker, "commit-tree", tree, "-p", e.base, "-m", "work in progress")
	if err != nil {
		t.Errorf("commit tree: %v", err)
		return "", nil, false
	}
	if _, err := gitOut(e.seed, "update-ref", "refs/heads/wip", commit); err != nil {
		t.Errorf("wip ref: %v", err)
		return "", nil, false
	}
	bundle := filepath.Join(t.TempDir(), "wip.bundle")
	if _, err := gitOut(e.seed, "bundle", "create", bundle, e.base+"..refs/heads/wip"); err != nil {
		t.Errorf("bundle: %v", err)
		return "", nil, false
	}
	body, err := os.ReadFile(bundle)
	if err != nil {
		t.Errorf("read bundle: %v", err)
		return "", nil, false
	}
	return commit, body, true
}

// timelineStep is one line of the task's timeline.
type timelineStep struct {
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	Commit  string `json:"commit"`
}

func TestWallClockStopShipsTheWork(t *testing.T) {
	for name, fail := range map[string]bool{"ship": false, "loss": true} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			ctx := context.Background()
			store, err := factoryd.OpenStore(ctx, accDatabase(t, "acceptance_wall_clock_"+name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })

			gitDir := t.TempDir()
			base := stopGitSeeds(t, gitDir)
			git := &gitForgejo{base: base, forkRoot: gitDir}
			srv := git.server(t)
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
			endpoint := &stopEndpoint{seed: filepath.Join(gitDir, "seed"), base: base, fail: fail}
			ep := endpoint.server(t)
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
				Policy:     stopPolicy,
				PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
					v := stopPolicy()
					return v, id == v.ID, nil
				},
				Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
				LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
				Workflows:   &acceptanceWorkflows{},
			}
			completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
			runner, err := factoryd.NewRunner(cfg, mergeProject{store}, &stopKube{}, forgejo, completion, objects,
				&factoryd.SupervisorModel{GatewayURL: srv.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
			if err != nil {
				t.Fatal(err)
			}
			_, port, _ := net.SplitHostPort(ep.Listener.Addr().String())
			workerPort, _ := strconv.Atoi(port)
			runner.SetTestEndpoints(workerPort, gitDir)
			if err := store.CreateTask(ctx, factoryd.Task{ID: "t-wip", Status: "queued", Objective: "ship the work a wall-clock stop took", Repository: "demo-app/demo-app",
				Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: 4, SHA: base, RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
				t.Fatal(err)
			}

			var suite testsuite.WorkflowTestSuite
			suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
			env := suite.NewTestWorkflowEnvironment()
			env.SetTestTimeout(60 * time.Second)
			runner.RegisterActivities(env)
			env.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
			// whatever ask the stop's outcome feeds, the issue's close
			// decides the card, so no second real session runs
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(factoryd.SignalIssueClosed, factoryd.TaskSignal{Author: "sebastian"}) }, 90*time.Minute)

			env.ExecuteWorkflow("TaskWorkflow", "t-wip")
			if !env.IsWorkflowCompleted() {
				t.Fatal("the card's workflow did not complete")
			}
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("the card's workflow: %v", err)
			}

			// the timeline: the stop's outcome and, in the ship case, the
			// wip push naming the taken head
			body, err := objects.Get(ctx, "sessions/t-wip/timeline.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			var wipCommit string
			var ended bool
			var sessionEnds int
			for _, line := range strings.Split(string(body), "\n") {
				var row timelineStep
				if json.Unmarshal([]byte(line), &row) != nil {
					continue
				}
				if row.Step == "wip_pushed" {
					wipCommit = row.Commit
				}
				if row.Step == "session_end" {
					sessionEnds++
					ended = ended || row.Outcome == string(factoryd.OutcomeWallClock)
				}
			}
			if !ended {
				t.Fatalf("the timeline holds no session_end of outcome wall_clock: %s", body)
			}
			if sessionEnds != 1 {
				t.Fatalf("the timeline holds %d session_end rows, want one: a second session would add one", sessionEnds)
			}

			// the stop's findings feed the ask the outcome carries: the
			// cap card opens a case whose face text holds them. This rig
			// runs no waker, so the case never climbs to its human face
			// and posts no ask comment; the face is the ask itself, and
			// no comment the flow posts carries a bare no-result.
			cardCases, err := store.CasesOf(ctx, "t-wip")
			if err != nil {
				t.Fatal(err)
			}
			askFace := func(contains string) bool {
				return slices.ContainsFunc(cardCases, func(c factoryd.Case) bool {
					f, _ := c.Facts["face"].(string)
					return strings.Contains(f, contains)
				})
			}
			for _, c := range git.postedComments() {
				if strings.Contains(c.body, "the stopped worker did not report") {
					t.Fatalf("a posted comment carries a bare no-result: %q", c.body)
				}
			}
			if fail {
				if !askFace(stopUnpushedText) {
					t.Fatalf("no ask the outcome feeds names the unpushed work %q", stopUnpushedText)
				}
				if head := stopForkHead(gitDir); head != base {
					t.Fatalf("the loss case pushed %q, want the branch at the base %q", head, base)
				}
				if wipCommit != "" {
					t.Fatalf("the loss case recorded a wip_pushed of %q, want none", wipCommit)
				}
				return
			}
			if wipCommit == "" {
				t.Fatalf("the timeline holds no wip_pushed row: %s", body)
			}
			if want := stopTakenNoteText + " factory/t-wip at " + wipCommit; !askFace(want) {
				t.Fatalf("no ask the outcome feeds carries the taken head %q", want)
			}
			if head := stopForkHead(gitDir); head != wipCommit {
				t.Fatalf("the bare fork's head is %q, want the taken head %q", head, wipCommit)
			}
			if branch, _ := git.snapshot(); !slices.Equal(branch, []string{wipCommit}) {
				t.Fatalf("the branch holds %v, want exactly the taken head %q", branch, wipCommit)
			}
			if head, err := runner.BranchHead(ctx, "t-wip"); err != nil || head != "" {
				t.Fatalf("BranchHead = %q, %v; want none: an interim head is no finished work", head, err)
			}
		})
	}
}
