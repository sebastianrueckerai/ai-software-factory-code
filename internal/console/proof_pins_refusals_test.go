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
		if refused.Reason != "The document could not be read: the document repository sf/docs did not answer. Open the document again and save your change once more." {
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
func failAnswer(status int, message string) *http.Response {
	body, _ := json.Marshal(map[string]string{"message": message})
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body))),
		Header: http.Header{"Content-Type": []string{"application/json"}}}
}
