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
