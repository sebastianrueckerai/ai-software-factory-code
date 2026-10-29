package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Roadmap 208: a plan's acceptance commands are the runs that prove a
// card's work. On a card waiting on a human whose trail shows one run
// green, the board offers its approvers Close: acceptance green beside
// the answer box, and the close ends the card done — the work is its own
// evidence, no answer and no further session owed.

// waitingCard puts card t-e of issue #13 of sf/code waiting on a human,
// with an escalation's reason on it.
func waitingCard(r *rig, t *testing.T) {
	t.Helper()
	r.tasks.set("which alias should the replica serve?", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2})
}

// greenRow records factoryd's acceptance green of task id as the session
// left it, and answers its row id.
func greenRow(r *rig, t *testing.T, id int64, taskID, command, session string) {
	t.Helper()
	if _, err := r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: taskID,
		IssueNumber: 13, Kind: factoryd.EventAcceptancePassed, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
		Detail: map[string]any{"command": command, "exit": float64(0), "session": session}}); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceCloseOffersAndClosesOnTheEvidence(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	waitingCard(r, t)
	greenRow(r, t, 7, "t-e", "bash check", "s-abc")
	// the board offers the close beside the answer box, the evidence on
	// the button, and the answer box itself is unchanged
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-e/answer"`, `action="/tasks/t-e/acceptance-close"`, `Close: acceptance green</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the escalated card lacks %s", want)
		}
	}
	var closed []string
	r.c.cfg.CloseCardDone = func(_ context.Context, taskID, by, evidence string) error {
		closed = append(closed, taskID+" by "+by+": "+evidence)
		return nil
	}
	rec := r.do(post("/tasks/t-e/acceptance-close", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("acceptance-close = %d: %s", rec.Code, rec.Body)
	}
	if len(closed) != 1 || closed[0] != "t-e by sebastian: acceptance \"bash check\" exited 0 in session s-abc" {
		t.Fatalf("the close ended %v, want t-e by sebastian on the evidence", closed)
	}
	// the close's audit row names the card and the evidence
	rows := r.events.rows
	if len(rows) != 2 {
		t.Fatalf("the close wrote %d rows, want the green and the close", len(rows))
	}
	e := rows[1]
	switch {
	case e.Kind != factoryd.EventCardClosed:
		t.Errorf("kind = %s, want %s", e.Kind, factoryd.EventCardClosed)
	case e.Actor != "sebastian" || e.Via != factoryd.ViaButton:
		t.Errorf("actor %s via %s, want sebastian via %s", e.Actor, e.Via, factoryd.ViaButton)
	case e.Detail["evidence"] == "":
		t.Errorf("the row's evidence is %v", e.Detail["evidence"])
	}
}

// A plain member gets no button and the action refused: an approver's
// close, as Fund's.
func TestAcceptanceCloseIsAnApproversAction(t *testing.T) {
	r := newRig(t)
	r.repoWriters["sf/code"] = []string{"sebastian", "morgan"}
	r.forgetWriters()
	ck, s := memberLogin(t, r, "morgan")
	waitingCard(r, t)
	greenRow(r, t, 7, "t-e", "bash check", "s-abc")
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "/tasks/t-e/acceptance-close") {
		t.Error("the board offers a plain member the acceptance close")
	}
	closed := 0
	r.c.cfg.CloseCardDone = func(context.Context, string, string, string) error { closed++; return nil }
	rec := r.do(post("/tasks/t-e/acceptance-close", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusForbidden ||
		strings.TrimSpace(rec.Body.String()) != "Only the approvers of project ai-software-factory-code may close a card on its acceptance." {
		t.Fatalf("acceptance-close = %d: %s", rec.Code, rec.Body)
	}
	if closed != 0 || len(r.events.rows) != 1 {
		t.Fatalf("a refused close ended %d cards and wrote %d rows", closed, len(r.events.rows)-1)
	}
}

// A card whose trail shows no green run never closes this way, on the
// board or by the route; nor does one not waiting on a human.
func TestAcceptanceCloseRefusesACardWithoutEvidence(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	waitingCard(r, t)
	// the board offers no close without a green run on the trail
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "/tasks/t-e/acceptance-close") {
		t.Error("the board offers the acceptance close without evidence")
	}
	closed := 0
	r.c.cfg.CloseCardDone = func(context.Context, string, string, string) error { closed++; return nil }
	if rec := r.do(post("/tasks/t-e/acceptance-close", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "shows no acceptance green") {
		t.Fatalf("acceptance-close without evidence = %d: %s", rec.Code, rec.Body)
	}
	// a red run is no evidence
	if _, err := r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-e",
		Kind: factoryd.EventAcceptancePassed, Detail: map[string]any{"command": "bash check", "exit": float64(1), "session": "s1"}}); err != nil {
		t.Fatal(err)
	}
	if rec := r.do(post("/tasks/t-e/acceptance-close", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("acceptance-close on a red = %d: %s", rec.Code, rec.Body)
	}
	// nor is a card in another state
	r.tasks.set("", factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code",
		IssueNumber: 14, Objective: "Speed up the board."})
	greenRow(r, t, 9, "t-r", "bash check", "s-1")
	if rec := r.do(post("/tasks/t-r/acceptance-close", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "not closed on its acceptance evidence") {
		t.Fatalf("acceptance-close on a running card = %d: %s", rec.Code, rec.Body)
	}
	if closed != 0 {
		t.Fatalf("a refused close ended %d cards", closed)
	}
}

// The evidence is the newest green by id, whatever order the store reads
// its rows back in, and a red newest-first does not hide an older green.
func TestAcceptanceEvidencePicksTheNewestGreen(t *testing.T) {
	r := newRig(t)
	greenRow(r, t, 7, "t-e", "go vet ./...", "s-1")
	if _, err := r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-e",
		Kind: factoryd.EventAcceptancePassed, Detail: map[string]any{"command": "bash check", "exit": float64(1), "session": "s-2"}}); err != nil {
		t.Fatal(err)
	}
	greenRow(r, t, 9, "t-e", "bash check", "s-3")
	evidence, ok := r.c.acceptanceEvidence(t.Context(), "t-e")
	if !ok || evidence != `acceptance "bash check" exited 0 in session s-3` {
		t.Fatalf("evidence = %q, %v; want the newest green run", evidence, ok)
	}
	// the red newest-first leaves the older green standing
	if _, err := r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-m",
		Kind: factoryd.EventAcceptancePassed, Detail: map[string]any{"command": "go vet ./...", "exit": float64(1), "session": "s-9"}}); err != nil {
		t.Fatal(err)
	}
	evidence, ok = r.c.acceptanceEvidence(t.Context(), "t-e")
	if !ok || evidence != `acceptance "bash check" exited 0 in session s-3` {
		t.Fatalf("evidence = %q, %v; want the newest green still", evidence, ok)
	}
	if _, ok := r.c.acceptanceEvidence(t.Context(), "t-none"); ok {
		t.Fatal("a card without a green run shows evidence")
	}
}
