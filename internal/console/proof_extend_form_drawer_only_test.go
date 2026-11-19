package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// extend_form_drawer_only: a card that may extend shows no extend input on
// its small card, the card with no case and the card whose case asks
// alike; the opened card holds the one extend form with its inputs, among
// the ask's answers where the case offers it, else among the other
// actions.
func TestProofExtendFormOffTheAskingFace(t *testing.T) {
	r := newRig(t)
	r.openCases = map[string][]factoryd.Case{}
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
			Escalation: "the session cap is reached: every finding sent to a human returns unacted"},
		{Task: factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"},
			Escalation: "the gate stopped"},
	}
	seatAsk(r, "t-g", factoryd.CaseSessionCap, roomyFacts(nil))
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	for _, id := range []string{"t-q", "t-g"} {
		if face := faceOf(t, body, id); strings.Contains(face, "extend") || strings.Contains(face, "<input") {
			t.Errorf("the small card of %s holds the extend form:\n%s", id, face)
		}
	}
	for id, where := range map[string]func(string) string{
		"t-q": func(opened string) string { return otherActionsOf(t, opened) },
		"t-g": func(opened string) string { return askBlockOf(t, opened) },
	} {
		opened := drawer(t, r, ck, id, "overview")
		if n := strings.Count(opened, `data-answer="extend"`); n != 1 || answerForm(t, opened, factoryd.ActExtend).Action != "/tasks/"+id+"/resolve" {
			t.Fatalf("the opened card of %s holds %d extend forms, want the one that posts its answer:\n%s", id, n, opened)
		}
		_, form, _ := strings.Cut(where(opened), `data-answer="extend"`)
		form, _, _ = strings.Cut(form, "</form>")
		for _, want := range []string{`name="extend.sessions"`, `name="extend.lines"`, `name="extend.minutes"`} {
			if !strings.Contains(form, want) {
				t.Errorf("the extend form of %s lacks %s:\n%s", id, want, form)
			}
		}
	}
}
