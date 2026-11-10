package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// refusal_pins_error_action: each touched Go file's refusal that fails
// on an internal error names what failed, the planted error and the
// next action — one subtest per file, each driving its site through the
// rig and asserting the literal sentence and its exact HTTP status.
func TestProofRefusalPinsErrorAndAction(t *testing.T) {
	t.Run("actions.go retry", func(t *testing.T) {
		r := newRig(t)
		withLinkshort(r)
		r.tasks.tasks = append(planTasks(), failedCard())
		ck, s := r.loggedIn(t)
		r.retryErr = errors.New("the retry of t-c7 as t-x stopped at filing its issue: forgejo 502")
		req := post("/tasks/t-c7/retry", url.Values{"csrf": {s.csrf}})
		req.Header.Set("HX-Request", "true")
		rec := r.do(req, ck)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("the failed retry = %d: %s", rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "The card could not be retried: the retry of t-c7 as t-x stopped at filing its issue: forgejo 502. "+
			"Press Retry again, or replan the card when it keeps failing." {
			t.Errorf("the refusal = %q", got)
		}
	})
	t.Run("projects.go merge pull read", func(t *testing.T) {
		r := newRig(t)
		infra := withInfra(t, r)
		infra.set(openPull)
		ck, s := r.loggedIn(t)
		r.filed(t, 12)
		r.c.cfg.HTTP = &http.Client{Transport: failPath{http.DefaultTransport,
			"repos/sf/infra/pulls/12", "linkshort's pull request 12 did not answer"}}
		rec := r.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), ck)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("the failed pull read = %d: %s", rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "The pull request could not be read: linkshort's pull request 12 did not answer. "+
			"Load the project's page again, or merge the pull request on Forgejo." {
			t.Errorf("the refusal = %q", got)
		}
	})
	t.Run("docs_write.go document read", func(t *testing.T) {
		r, _, _ := newChatRig(t)
		r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
		withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
		ck, s := r.loggedIn(t)
		r.c.cfg.HTTP = &http.Client{Transport: failStatus{http.DefaultTransport,
			"repos/sf/docs/contents", http.StatusBadGateway, "the document repository sf/docs did not answer"}}
		body, err := json.Marshal(docSave{Text: "# The cache plan\n\nThe store: Postgres.\n", Base: factoryd.ContentHash(docsPlan)})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/docs/docs/plans/cache.md", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", s.csrf)
		rec := r.do(req, ck)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("the failed save = %d: %s", rec.Code, rec.Body.String())
		}
		var refused struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
			t.Fatalf("the answer = %s: %v", rec.Body.String(), err)
		}
		if refused.Reason != "The document could not be read: forgejo GET /api/v1/repos/sf/docs/contents/docs/plans/cache.md?ref=master: "+
			"status 502: the document repository sf/docs did not answer. Open the document again and save your change once more." {
			t.Errorf("the refusal = %q", refused.Reason)
		}
	})
	t.Run("board.go drawer plan read", func(t *testing.T) {
		r := newRig(t)
		r.tasks.tasks = []factoryd.BoardTask{
			{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "queued", Track: "plan", Process: "spec", Objective: "Link shortener",
				Repository: "sf/code", IssueNumber: 1, PlanName: "fix prod latency"}},
		}
		ck, _ := r.loggedIn(t)
		r.c.cfg.Tasks = failingPlans{r.c.cfg.Tasks, errors.New("plan t-plan's revision 1 did not answer")}
		rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?tab=overview", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("the drawer = %d: %s", rec.Code, rec.Body.String())
		}
		want := "The plan could not be read: plan t-plan&#39;s revision 1 did not answer. Open the card again, or read the plan on its issue."
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the drawer's problem sentence is missing %q", want)
		}
		if strings.Contains(rec.Body.String(), "factoryd log") {
			t.Errorf("the drawer's problem sentence still points at the factoryd log")
		}
	})
	t.Run("chat.go catch-all", func(t *testing.T) {
		r, _ := batchAnswerRig(t)
		_, s := r.loggedIn(t)
		r.c.cfg.Tasks = failingPlans{r.c.cfg.Tasks, errors.New("plan t-p's revision 1 did not answer")}
		var events []string
		trn := r.chatTurnOf(t, s, &events)
		tool, _ := r.c.chatTool("plan")
		got, _ := trn.runTool(tool, `{"task_id":"t-p"}`, toolInput{TaskID: "t-p"})
		if got != "Refused: The action failed: plan t-p's revision 1 did not answer. "+
			"Try the call again, or read the board to see the factory's state." {
			t.Errorf("the tool result = %q", got)
		}
	})
	t.Run("answer.go escalate", func(t *testing.T) {
		r, _ := batchAnswerRig(t)
		_, s := r.loggedIn(t)
		r.c.cfg.Escalate = func(context.Context, string, factoryd.HumanQuestion, string) (int64, error) {
			return 0, errors.New("forgejo rejected the card: the inbox is closed for sf/code")
		}
		var events []string
		trn := r.chatTurnOf(t, s, &events)
		tool, _ := r.c.chatTool("escalate")
		args := `{"ask":"Stop the roll?","why":"the ledger is short","info":"the pool is dry","recommendation":"stop it","choices":["stop it","go on"]}`
		in := toolInput{Ask: "Stop the roll?", Why: "the ledger is short", Info: "the pool is dry",
			Recommendation: "stop it", Choices: []string{"stop it", "go on"}}
		got, code := trn.runTool(tool, args, in)
		if got != "Refused: The escalation could not be filed: forgejo rejected the card: the inbox is closed for sf/code. "+
			"Try again, or post the question on the card's issue." {
			t.Errorf("the tool result = %q", got)
		}
		if code != http.StatusBadGateway {
			t.Errorf("the refusal's status = %d, want 502", code)
		}
	})
	t.Run("approvals.go plan card", func(t *testing.T) {
		r, _ := batchAnswerRig(t)
		ck, _ := r.loggedIn(t)
		r.c.cfg.Tasks = failingPlans{r.c.cfg.Tasks, errors.New("plan t-p's revision 1 did not answer")}
		rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("the board = %d: %s", rec.Code, rec.Body.String())
		}
		want := "The plan could not be read: plan t-p&#39;s revision 1 did not answer. Open the plan&#39;s approval again, or read the plan on its issue."
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the approval card's problem sentence is missing %q", want)
		}
		if strings.Contains(rec.Body.String(), "factoryd log") {
			t.Errorf("the approval card's problem sentence still points at the factoryd log")
		}
	})
	t.Run("autonomy.go configuration check", func(t *testing.T) {
		r := newRig(t)
		infra := withInfra(t, r)
		infra.files = map[string]string{"factory/projects/demo-app.yaml": "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\n"}
		r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) {
			return nil, errors.New("the check of projects/demo-app.yaml did not answer")
		}
		ck, s := r.loggedIn(t)
		rec := r.do(post("/projects/demo-app/autonomy", url.Values{"csrf": {s.csrf}, "autonomy": {"full auto"}}), ck)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("the failed switch = %d: %s", rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "The configuration at infra's head could not be read: the check of projects/demo-app.yaml did not answer. "+
			"Try the switch again, or edit the project file on Forgejo." {
			t.Errorf("the refusal = %q", got)
		}
	})
	t.Run("chat_page_tools.go resource promote", func(t *testing.T) {
		r := newRig(t)
		ck, s := r.loggedIn(t)
		r.c.cfg.Resources = failingPromote{r.c.cfg.Resources, errors.New("cloud: the commission 7 is gone")}
		rec := r.do(post("/resources/7/promote", url.Values{"csrf": {s.csrf}}), ck)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("the failed promote = %d: %s", rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "The resource could not be promoted: cloud: the commission 7 is gone. "+
			"Press Promote again, or release it and commission it anew." {
			t.Errorf("the refusal = %q", got)
		}
	})
	t.Run("chat_tools.go request_image", func(t *testing.T) {
		r, _ := batchAnswerRig(t)
		_, s := r.loggedIn(t)
		r.imageErr = errors.New("image ask: the runner is gone")
		var events []string
		trn := r.chatTurnOf(t, s, &events)
		tool, ok := r.c.chatTool("request_image")
		if !ok {
			t.Fatal("the agent is offered no request_image tool")
		}
		got, code := trn.runTool(tool, `{"tool":"opencode","need":"run the tests"}`, toolInput{Tool: "opencode", Need: "run the tests"})
		if got != "Refused: The tool card could not be filed: image ask: the runner is gone. "+
			"Ask again, or file the tool card by hand." {
			t.Errorf("the tool result = %q", got)
		}
		if code != http.StatusBadGateway {
			t.Errorf("the refusal's status = %d, want 502", code)
		}
	})
	t.Run("routine_drawer.go runs read", func(t *testing.T) {
		r := newRig(t)
		r.routines.views = []factoryd.RoutineView{weeklyBump()}
		r.routines.runsErr = errors.New("runs of weekly-bump: the store did not answer")
		ck, _ := r.loggedIn(t)
		rec := r.do(httptest.NewRequest(http.MethodGet, "/routines/ai-software-factory-code/weekly-bump/drawer", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("the drawer = %d: %s", rec.Code, rec.Body.String())
		}
		want := "The routine&#39;s runs could not be read: runs of weekly-bump: the store did not answer. Open the routine again, or read its runs on Forgejo."
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the routine drawer's problem sentence is missing %q", want)
		}
		if strings.Contains(rec.Body.String(), "factoryd log") {
			t.Errorf("the routine drawer's problem sentence still points at the factoryd log")
		}
	})
}

// failingPromote is a Resources seam whose Promote answers err.
type failingPromote struct {
	Resources
	err error
}

func (f failingPromote) Promote(context.Context, int64, string, func(string) policy.Standing) error {
	return f.err
}

// failingPlans is a Tasks seam whose plan reads answer err.
type failingPlans struct {
	Tasks
	err error
}

func (f failingPlans) PlanRevision(context.Context, string, int) (factoryd.PlanRevision, bool, error) {
	return factoryd.PlanRevision{}, false, f.err
}

// failPath is an http.RoundTripper that answers a request whose path
// holds marker with 502 and err's words as the body; every other
// request goes to base.
type failPath struct {
	base   http.RoundTripper
	marker string
	err    string
}

func (f failPath) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, f.marker) {
		return failAnswer(http.StatusBadGateway, f.err), nil
	}
	return f.base.RoundTrip(req)
}

// failStatus is an http.RoundTripper that answers a request whose path
// holds marker with status and body; every other request goes to base.
type failStatus struct {
	base   http.RoundTripper
	marker string
	status int
	body   string
}

func (f failStatus) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, f.marker) {
		return failAnswer(f.status, f.body), nil
	}
	return f.base.RoundTrip(req)
}

// failAnswer is a Forgejo answer that carries body.
func failAnswer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"text/plain"}}}
}
