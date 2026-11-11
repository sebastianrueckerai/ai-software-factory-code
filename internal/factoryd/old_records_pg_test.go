package factoryd

import (
	"context"
	"fmt"
	"slices"
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
// tagged with its first line as before, and none after the face's
// withdrawal does.
func TestReconcileReadsTheAnswersByTheAnchorsOfTheAskCases(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	ctx := context.Background()
	out := g.trip(t, tripCap)
	if out.Comment != 501 {
		t.Fatalf("the face's comment = %d, want 501", out.Comment)
	}
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "go on", CommentID: 520, Trusted: true, DeliveryID: "d1"}); err != nil {
		t.Fatal(err)
	}
	events, err := g.s.IntakeEvents(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeEscalation }); i < 0 || events[i].CommentID != 501 || events[i].Body != tripCap ||
		events[i].Question == nil || events[i].Question.Ask == "" {
		t.Fatalf("intake events %+v, want the stop's escalation anchor at comment 501 with its words and question", events)
	}
	answers, question, _ := answersFrom(events)
	tail := fmt.Sprintf(", to %q: go on", textutil.Truncate(escalationQuestion(tripCap), 300))
	if len(answers) != 1 || !strings.HasPrefix(answers[0], "sebastian") || !strings.HasSuffix(answers[0], tail) || question != escalationQuestion(tripCap) {
		t.Fatalf("answers %q, question %q; want one answer by sebastian ending %q to %q", answers, question, tail, escalationQuestion(tripCap))
	}
	if err := g.r.EndCase(ctx, out.CaseID, "the run ended: issue closed"); err != nil {
		t.Fatal(err)
	}
	events, err = g.s.IntakeEvents(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeEscalationWithdrawn }); i < 0 || events[i].Withdraws != 501 || events[i].CommentID != 502 {
		t.Fatalf("intake events %+v, want the withdrawal of 501 at the comment 502", events)
	}
	if answers, _, _ := answersFrom(events); len(answers) != 0 {
		t.Fatalf("answers %q after the face's withdrawal, want none: comment 520 follows it", answers)
	}
}
