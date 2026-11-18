package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// extend_form_drawer_only: a card that may extend shows no extend input on
// its asking face, the escalation of a card with no case and the face of a
// case whose ask stores no offers alike; the drawer's extend fold opens
// for the asking card and holds the form's inputs.
func TestProofExtendFormOffTheAskingFace(t *testing.T) {
	r := newRig(t)
	r.noStoredOffers = true
	r.openCases = map[string][]factoryd.Case{}
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
			Escalation: "the session cap is reached: every finding sent to a human returns unacted"},
		{Task: factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"},
			Escalation: "the gate stopped"},
	}
	seatAsk(r, "t-g", factoryd.CaseSessionCap, roomyFacts(nil))
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	for id, face := range map[string]string{"t-q": escalationOf(t, body, "t-q"), "t-g": escalationOf(t, body, "t-g")} {
		if strings.Contains(face, "/extend") || strings.Contains(face, `id="extend-`) {
			t.Errorf("the asking face of %s holds the extend form:\n%s", id, face)
		}
	}
	for _, id := range []string{"t-q", "t-g"} {
		drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
		_, fold, ok := strings.Cut(drawer, `<details class="sec" data-sec="extend"><summary>Extend</summary>`)
		fold, _, _ = strings.Cut(fold, "</details>")
		if !ok {
			t.Fatalf("the drawer of %s holds no extend fold:\n%s", id, drawer)
		}
		for _, want := range []string{`action="/tasks/` + id + `/extend"`, `name="sessions"`, `name="lines"`, `name="minutes"`, `name="eur"`, `name="reason"`} {
			if !strings.Contains(fold, want) {
				t.Errorf("the drawer's extend fold of %s lacks %s:\n%s", id, want, fold)
			}
		}
	}
}
