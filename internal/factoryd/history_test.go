package factoryd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// Forgejo-shaped credentials (40 hex, as Forgejo mints them) and the
// commits of the task's repository, which share the shape.
const (
	histBase   = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	histCommit = "c0ffee00112233445566778899aabbccddeeff00"
)

var histSeeded = map[string]string{
	policy.SecretCIRegistrar: "5e1f0c2d3b4a59687766554433221100ffeeddcc",
	policy.SecretMerge:       "9a8b7c6d5e4f30211203f4e5d6c7b8a998877665",
	policy.SecretConfig:      "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c",
}

// histForgejo plays every Forgejo endpoint one task uses from intake to
// merge on sf/repo, its fork factory-agent/repo and pull request 12; the
// tokens it mints are 40 hex like Forgejo's.
type histForgejo struct {
	mu       sync.Mutex
	minted   []string
	tokens   []TokenRef
	nextID   int64
	opened   bool
	merged   bool
	comments []string
	labels   []string
	calls    []string
}

func (f *histForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		p := r.URL.Path
		for _, prefix := range []string{"/api/v1/repos/sf/repo", "/api/v1/repos/factory-agent/repo"} {
			p = strings.TrimPrefix(p, prefix)
		}
		pr := map[string]any{"number": 12, "html_url": "https://git.example/sf/repo/pulls/12", "state": "open", "merged": f.merged,
			"head": map[string]any{"sha": histCommit, "ref": "factory/t1", "repo": map[string]any{"full_name": "factory-agent/repo"}},
			"base": map[string]any{"ref": "main"}}
		if f.merged {
			pr["state"], pr["merged_by"] = "closed", map[string]any{"login": MergeUser}
		}
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			var body struct{ Name string }
			json.NewDecoder(r.Body).Decode(&body)
			f.nextID++
			secret := fmt.Sprintf("%040x", 0xa11ce000+f.nextID)
			f.minted = append(f.minted, secret)
			f.tokens = append(f.tokens, TokenRef{ID: f.nextID, Name: body.Name})
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"sha1":%q}`, f.nextID, secret)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode(f.tokens)
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/tokens/"):
			var id int64
			fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id)
			kept := f.tokens[:0]
			for _, tok := range f.tokens {
				if tok.ID != id {
					kept = append(kept, tok)
				}
			}
			f.tokens = kept
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/factory-agent/repo":
			io.WriteString(w, `{"full_name":"factory-agent/repo","fork":true,"private":true,"parent":{"full_name":"sf/repo"}}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, histCommit)
		case r.Method == "DELETE" && strings.HasPrefix(p, "/branches/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
			io.WriteString(w, `{"truncated":false,"total_count":1,"tree":[{"path":"Makefile","sha":"m"}]}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/pulls/main/"):
			if !f.opened || f.merged {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(pr)
		case r.Method == "POST" && p == "/pulls":
			f.opened = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(pr)
		case r.Method == "GET" && p == "/pulls":
			open := []any{}
			if f.opened && !f.merged {
				open = append(open, pr)
			}
			json.NewEncoder(w).Encode(open)
		case r.Method == "GET" && p == "/pulls/12/files":
			io.WriteString(w, `[{"filename":"Makefile","status":"modified"}]`)
		case r.Method == "GET" && p == "/pulls/12.diff":
			io.WriteString(w, "diff --git a/Makefile b/Makefile\n+check:\n")
		case r.Method == "GET" && p == "/pulls/12":
			json.NewEncoder(w).Encode(pr)
		case r.Method == "POST" && p == "/pulls/12/merge":
			f.merged = true
			io.WriteString(w, `{}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/commits/") && strings.HasSuffix(p, "/status"):
			fmt.Fprintf(w, `{"state":"success","sha":%q,"total_count":1,"statuses":[{"context":"ci / test","status":"success"}]}`, histCommit)
		case r.Method == "GET" && p == "/actions/tasks":
			io.WriteString(w, `{"workflow_runs":[],"total_count":0}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/labels"):
			out := []map[string]string{}
			for _, l := range f.labels {
				out = append(out, map[string]string{"name": l})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			var body struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&body)
			f.labels = body.Labels
			io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasSuffix(p, "/comments"):
			io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			f.comments = append(f.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 700+len(f.comments))
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/4"):
			io.WriteString(w, `{"number":4,"state":"open"}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingConverter keeps every payload the workflow's history would
// hold: workflow and activity arguments and results, heartbeats, signals
// and side effects all pass the data converter.
type recordingConverter struct {
	converter.DataConverter
	mu       sync.Mutex
	payloads []string
}

func (c *recordingConverter) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, s)
}

func (c *recordingConverter) ToPayload(v any) (*commonpb.Payload, error) {
	p, err := c.DataConverter.ToPayload(v)
	if p != nil {
		c.add(string(p.GetData()))
	}
	return p, err
}

func (c *recordingConverter) ToPayloads(v ...any) (*commonpb.Payloads, error) {
	ps, err := c.DataConverter.ToPayloads(v...)
	for _, p := range ps.GetPayloads() {
		c.add(string(p.GetData()))
	}
	return ps, err
}

// forgejoToken is Forgejo's token shape: 40 lowercase hex, the shape of a
// commit id too.
var forgejoToken = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// credentialHits are the credentials in s: whatever the redact rules mask
// (the configured secrets among them, sk- keys, JWTs, bearer and basic
// credentials, URL userinfo, long key runs) and every Forgejo-shaped
// token that is not one of commits.
func credentialHits(s string, secrets, commits []string) []string {
	var hits []string
	if scrubbed := redact.New(secrets...).String(s); scrubbed != s {
		hits = append(hits, "redact: "+s)
	}
	for _, m := range forgejoToken.FindAllString(s, -1) {
		if !strings.Contains(strings.Join(commits, " "), m) {
			hits = append(hits, "forgejo token shape: "+m)
		}
	}
	return hits
}

// A whole task, session to merge, in the Temporal test environment with
// the real activities: no payload its history holds, and no activity
// error, carries a credential. Every token lives inside the activity that
// mints or reads it; the workflow passes the task id. The canary run's
// agent echoes the merge token in its last message: the scan sees it in
// RunSession's result, so the recording covers what the history holds.
func TestTaskHistoryCarriesNoCredential(t *testing.T) {
	if hits := taskHistoryHits(t, "added the check target"); len(hits) != 0 {
		for _, hit := range hits {
			t.Errorf("history carries a credential (%s)", hit)
		}
	}
	if hits := taskHistoryHits(t, "added it, merge with "+histSeeded[policy.SecretMerge]); len(hits) == 0 {
		t.Error("canary: the leaked merge token went unseen")
	}
}

// taskHistoryHits runs task t1 from intake through a session whose agent
// ends with summary, green checks, the gates, a human's approval and the
// merge, and scans every recorded payload and activity error.
func taskHistoryHits(t *testing.T, summary string) []string {
	t.Helper()
	task := Task{ID: "t1", Status: "queued", Kind: "agent", Objective: "Add a make check target", Repository: "sf/repo", SHA: histBase,
		IssueNumber: 4, Source: "api", Author: "sebastian", AuthorWriter: true, RequiredCapabilities: AgentCapabilities, BudgetEUR: 2}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	fj := &histForgejo{nextID: 100}
	runner.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, PublicURL: "https://git.example", BotUser: "factory-agent", BotPassword: "bot-password-7f3a9c", Seeded: histSeeded}
	runner.completion.Forgejo = runner.forgejo
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"the target runs the tests","findings":[]}`}
	runner.supervisor = testSupervisorModel(ll.srv(t).URL)
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": `{"auths":{"reg":{"auth":"cmVnOnNlY3JldC1yZWdpc3RyeS1wYXNz"}}}`}

	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestWorkflowEnvironment()
	rec := &recordingConverter{DataConverter: converter.GetDefaultDataConverter()}
	env.SetDataConverter(rec)
	env.RegisterWorkflowWithOptions(TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	runner.RegisterActivities(env)
	var failures []string
	env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
		if err != nil {
			failures = append(failures, info.ActivityType.Name+": "+err.Error())
		}
	})
	logs := pushedLine(histCommit) + "\n" + resultLine(true, summary)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for assigned := 0; ; {
			kube.mu.Lock()
			created := len(kube.podOrder)
			kube.mu.Unlock()
			if created > assigned {
				bodies := make([]string, created-assigned)
				for i := range bodies {
					bodies[i] = logs
				}
				kube.scriptPods(assigned, bodies...)
				assigned = created
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalApproved, TaskSignal{Author: "sebastian", PR: 12})
	}, time.Minute)
	env.ExecuteWorkflow("TaskWorkflow", "t1")
	var final string
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil || env.GetWorkflowResult(&final) != nil || final != "done" {
		t.Fatalf("workflow: completed %v, error %v, final %q; activity errors %q", env.IsWorkflowCompleted(), env.GetWorkflowError(), final, failures)
	}
	fj.mu.Lock()
	defer fj.mu.Unlock()
	if !fj.merged || len(fj.minted) != 2 || store.tasks["t1"].Status != "done" {
		t.Fatalf("merged %v, %d tokens minted, task %s: want the merge, the session's two tokens, done", fj.merged, len(fj.minted), store.tasks["t1"].Status)
	}

	secrets := append([]string{runner.forgejo.BotPassword, runner.completion.LiteLLM.MasterKey, "gw-token"}, fj.minted...)
	for _, v := range histSeeded {
		secrets = append(secrets, v)
	}
	rec.mu.Lock()
	payloads := append(slices.Clone(rec.payloads), failures...)
	rec.mu.Unlock()
	if len(payloads) < 40 {
		t.Fatalf("%d payloads recorded: the converter saw too little of the history", len(payloads))
	}
	var hits []string
	for _, p := range payloads {
		hits = append(hits, credentialHits(p, secrets, []string{histBase, histCommit})...)
	}
	return hits
}

// The scan finds what it looks for: a minted token, a sk- key, a bearer
// credential and a seeded secret in a payload, and passes the commits.
func TestCredentialHits(t *testing.T) {
	secrets, commits := []string{"hunter2-master-key"}, []string{histBase, histCommit}
	for in, want := range map[string]int{
		`{"TaskID":"t1","Commit":"` + histCommit + `","Base":"` + histBase + `"}`: 0,
		`{"Token":"` + histSeeded[policy.SecretMerge] + `"}`:                      1,
		`{"Key":"sk-task-t1-abcdef"}`:                                             1,
		`clone failed: Authorization: Bearer abc.def.ghi`:                         1,
		`the master key hunter2-master-key`:                                       1,
	} {
		if got := credentialHits(in, secrets, commits); len(got) < want || (want == 0 && len(got) > 0) {
			t.Errorf("credentialHits(%s) = %q, want %d", in, got, want)
		}
	}
}
