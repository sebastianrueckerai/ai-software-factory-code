package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A plan card stored with raises keeps them, and one stored with none keeps
// a null ceiling, which reads as the policy's.
func TestCreatePlanCardStoresInheritedRaises(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "inherit")
	base := Task{Kind: "agent", Status: "queued", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackDirect, Source: SourcePlan, Parent: "plan"}
	if err := s.CreateTask(ctx, Task{ID: "plan", Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackPlan}); err != nil {
		t.Fatal(err)
	}
	raised, plain := base, base
	raised.ID, raised.PlanKey, raised.CeilingEUR = "c1", "a", 9
	raised.Usage = TaskUsage{RaisedSessions: 1, RaisedTime: 30 * time.Minute, ExtraLines: 200}
	plain.ID, plain.PlanKey = "c2", "b"
	for _, c := range []Task{raised, plain} {
		if _, err := s.CreatePlanCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := s.Task(ctx, "c1")
	if got.Usage.RaisedSessions != 1 || got.Usage.RaisedTime != 30*time.Minute || got.Usage.ExtraLines != 200 || got.CeilingEUR != 9 {
		t.Errorf("c1 %+v ceiling %.2f", got.Usage, got.CeilingEUR)
	}
	got, _, _ = s.Task(ctx, "c2")
	if got.Usage.RaisedSessions != 0 || got.CeilingEUR != 0 {
		t.Errorf("c2 %+v ceiling %.2f; want none", got.Usage, got.CeilingEUR)
	}
}

// The per-card draw bound restarts on a continuation: the old card's pool
// draws stay its own, and the inherited row, which is no pool draw, adds
// none to the new card's.
func TestAContinuationStartsItsDrawBoundOver(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "bound")
	for _, id := range []string{"old", "new"} {
		if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackDirect}); err != nil {
			t.Fatal(err)
		}
	}
	draw := CaseDraw{Rung: 1, Extend: policy.Draw{Sessions: 2}, CardBound: policy.Draw{Sessions: 5}, Pool: policy.Draw{Sessions: 30, AgentTime: 10 * time.Hour, EUR: 60}, PolicyCeiling: 7,
		Event: Event{Repository: "demo-app/demo-app", TaskID: "old", Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"pool": true, "rung": 1}}}
	if err := s.DrawCard(ctx, "old", draw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "new", Kind: EventCardExtended, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"inherited_from": "old", "pool": false, "sessions": 2, "amount_eur": 0.0}}); err != nil {
		t.Fatal(err)
	}
	old, _ := s.CardDrawn(ctx, "old")
	fresh, _ := s.CardDrawn(ctx, "new")
	if old.Sessions != 2 || !fresh.IsZero() {
		t.Fatalf("old card drew %+v, new card %+v; want 2 sessions and nothing", old, fresh)
	}
}

// A proposal lands on the face of its own case: the question_asked row of
// that case, and for a case that is no monitor flag's the card's question.
// A flag's face, newer beside the ask, keeps its row untouched by the ask's
// proposal, and the flag's proposal leaves the ask's question alone.
func TestSetQuestionProposalWritesTheFaceOfItsCase(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "proposal")
	if err := s.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackDirect}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: IntakeEscalation, Author: "factoryd", CommentID: 5, Question: &HumanQuestion{Ask: "ask", Info: "i"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{3, 4} {
		if _, err := s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"case_id": id, "ask": "face"}}); err != nil {
			t.Fatal(err)
		}
	}
	pre := map[string]any{"action": ActRetry}
	if err := s.SetQuestionProposal(ctx, "t1", 3, false, "retry: because", pre); err != nil {
		t.Fatal(err)
	}
	faces := func() map[int]Event {
		evs, err := s.TaskEventsOfKind(ctx, "t1", EventQuestionAsked, 10)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int]Event{}
		for _, e := range evs {
			id, _ := toInt(e.Detail["case_id"])
			out[id] = e
		}
		return out
	}
	question := func() string {
		evs, _ := s.IntakeEvents(ctx, "t1")
		return evs[0].Question.Recommendation
	}
	f := faces()
	if f[3].Detail["recommendation"] != "retry: because" || f[3].Detail["preselected"] == nil || f[4].Detail["recommendation"] != nil || question() != "retry: because" {
		t.Fatalf("case 3's proposal: faces %+v, question %q; want case 3's face and the ask's question, not case 4's", f, question())
	}
	if err := s.SetQuestionProposal(ctx, "t1", 4, true, "close: flag", map[string]any{"action": ActClose}); err != nil {
		t.Fatal(err)
	}
	f = faces()
	if f[4].Detail["recommendation"] != "close: flag" || f[3].Detail["recommendation"] != "retry: because" || question() != "retry: because" {
		t.Fatalf("the flag's proposal: faces %+v, question %q; want case 4's face and the ask's question unchanged", f, question())
	}
}
