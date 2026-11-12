package factoryd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// A trip that reaches the human writes its face and its question_asked row
// and nothing else: no escalation intake row and no escalation alert.
func TestATripWritesNoEscalationRowAndNoAlert(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	out := g.trip(t, tripCap)
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman {
		t.Fatalf("case %+v, want it at the human", c)
	}
	var rows, alerts int
	ctx := context.Background()
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_events WHERE task_id = 't1' AND kind IN ('escalation', 'escalation_withdrawn')`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM alerts WHERE task_id = 't1' AND kind = 'escalation'`).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || alerts != 0 || len(g.asks(t)) != 1 {
		t.Fatalf("%d escalation rows, %d escalation alerts, %d question_asked rows; want 0, 0 and 1", rows, alerts, len(g.asks(t)))
	}
}

// The reconcile reads the human's answers by the anchors of the card's ask
// cases: a trusted comment after the case's face answers the stop it asked,
// tagged with its first line, and none after the face's withdrawal does.
// The withdrawal goes through the activity a gate beside the checks uses.
func TestReconcileReadsTheAnswersByTheAnchorsOfTheAskCases(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	ctx := context.Background()
	out := g.trip(t, tripCap)
	if out.Comment != 501 {
		t.Fatalf("the face's comment = %d, want 501", out.Comment)
	}
	for _, id := range []int64{510, 520} {
		if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: fmt.Sprint("go on ", id), CommentID: id, Trusted: true, DeliveryID: fmt.Sprint("d", id)}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []string {
		t.Helper()
		events, err := g.s.IntakeEvents(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		asks, err := g.s.AskAnchors(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(asks) != 1 || asks[0].Comment != 501 || asks[0].Body != tripCap || asks[0].Shaped == nil || asks[0].Shaped.Ask == "" {
			t.Fatalf("anchors %+v, want the stop's face at comment 501 with its words and question", asks)
		}
		answers, _, _ := answersFrom(events, asks)
		return answers
	}
	tail := fmt.Sprintf(", to %q: go on ", textutil.Truncate(escalationQuestion(tripCap), 300))
	if answers := read(); len(answers) != 2 || !strings.HasSuffix(answers[0], tail+"510") || !strings.HasSuffix(answers[1], tail+"520") {
		t.Fatalf("answers %q, want 510 and 520 to the stop", answers)
	}
	if _, err := g.r.WithdrawQuestion(ctx, "t1", tripCap, out.Comment, "the checks failed", out.CaseID); err != nil {
		t.Fatal(err)
	}
	if answers := read(); len(answers) != 0 {
		t.Fatalf("answers %q after the face's withdrawal, want none: 510 and 520 follow the withdrawal at 502", answers)
	}
}
