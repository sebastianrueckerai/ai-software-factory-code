package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// A card whose merge approval pends speaks for itself in Needs you: the
// four-column view does not also hold it in In progress, where it read as
// still verifying while only its approval had moved on. The plans yield
// the same way.
func TestMergeApprovalYieldsTheCardInNeedsYou(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 7, PRNumber: 9})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(col(t, body, "In progress"), `data-card="t-m"`) {
		t.Fatal("In progress holds a card whose merge approval speaks for it in Needs you")
	}
	if !strings.Contains(col(t, body, "Needs you"), `data-card="t-m"`) {
		t.Fatal("Needs you holds no approval for the card")
	}
}

// The drawer's merge card: the writer's detail renders as markup (its own
// lists, never a raw wall), each why-a-human entry its own line, and an
// agent pull request on red checks shows its return and close directly —
// the actions the brief names, not a disclosure button that hides them.
func TestMergeDrawerRendersTheAskAndItsActions(t *testing.T) {
	r := newRig(t)
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{
		ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-red", Project: "sf/code", PRNumber: 10, Status: "pending",
		Brief: "A red one.", Detail: "- first fact\n- second fact",
		Spec: json.RawMessage(`{"risk_class":"normal","red_checks":true,"why_human":["checks round 1 failed","review round 2 said the same"]}`),
	})
	r.tasks.set("", factoryd.Task{ID: "t-red", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-red/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(body, "<li>first fact</li>") {
		t.Fatal("the drawer renders the detail's facts as no list")
	}
	if got := strings.Count(body, `class="why"`); got != 2 {
		t.Fatalf("the drawer renders %d why lines, want one per entry", got)
	}
	if !strings.Contains(body, `action="/approvals/6/deny"`) || strings.Contains(body, `id="deny-6-d" hidden`) {
		t.Fatal("the return and close form hides behind a disclosure")
	}
	if strings.Contains(body, `aria-controls="deny-6-d"`) {
		t.Fatal("the card still offers a disclosure button for its actions")
	}
}

// The drawer's pill reads the card's state like the face's: a merged
// card's drawer says merged, a done card's says done.
func TestDrawerPillReadsTheState(t *testing.T) {
	r := newRig(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-merged", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 7, PRNumber: 9},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	ck, _ := r.loggedIn(t)
	for id, want := range map[string]string{
		"t-merged": `<span class="pill st-merged">merged</span>`,
		"t-done":   `<span class="pill st-done">done</span>`,
	} {
		body := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("%s's drawer lacks %s:\n%s", id, want, body)
		}
	}
}

// The merged card's chain word wears the chain's own hue: red where the
// chain failed, amber while it stalls, the merged teal while it runs —
// never green words for a red chain.
func TestAMergedCardWearsItsChainState(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	card := func(id, state string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "merged", Repository: "sf/code",
			PRNumber: 9, Objective: id + " change"}, ChainState: state}
	}
	r.tasks.tasks = []factoryd.BoardTask{card("t-red", "failure"), card("t-run", "running"), card("t-stall", "stalled")}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	progress := col(t, body, "In progress")
	for id, word := range map[string]string{
		"t-red":   `<span class="pill st-merged">merged</span> <span class="st st-failed">chain red</span>`,
		"t-run":   `<span class="pill st-merged">merged</span> <span class="st st-merged">chain running</span>`,
		"t-stall": `<span class="pill st-merged">merged</span> <span class="st st-waiting">stalled</span>`,
	} {
		if !strings.Contains(progress, `data-card="`+id+`"`) || !strings.Contains(progress, word) {
			t.Errorf("In progress lacks %s wearing %s:\n%s", id, word, progress)
		}
	}
}

// A merged card sits in In progress, its pill reading merged and its
// chain word beside it; one already done is a finished card in Done.
func TestAMergedCardWaitsInProgressDoneIsDone(t *testing.T) {
	r := newRig(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-w", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 7, PRNumber: 9},
		factoryd.Task{ID: "t-d", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	progress := col(t, body, "In progress")
	if !strings.Contains(progress, `data-card="t-w"`) || !strings.Contains(progress, `<span class="pill st-merged">merged</span> <span class="st st-merged">waiting for the chain</span>`) {
		t.Fatalf("In progress holds the merged card without its pill and chain word:\n%s", progress)
	}
	if strings.Contains(progress, `data-card="t-d"`) || !strings.Contains(col(t, body, "Done"), `data-card="t-d"`) {
		t.Fatal("a done card is not a finished card in Done")
	}
}
