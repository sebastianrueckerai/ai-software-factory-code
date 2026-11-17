package factoryd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stored is detail as the events table gives it back: through JSON.
func stored(t *testing.T, detail map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A case's face is the one its ask stored, whatever its facts say now: the
// ask, the why line, the story, the answers with their labels and effects,
// the suggestion with who made it and why, and the agent's view.
func TestCaseFaceReadsTheStoredFace(t *testing.T) {
	card := Task{ID: "t1", Kind: "agent", IssueNumber: 1, Objective: "Count the skipped rows"}
	c := Case{ID: 3, Kind: CaseSessionCap, Facts: map[string]any{}}
	f := CaseFacts{Kind: CaseSessionCap, SessionCap: 3, Attempts: 3, BudgetEUR: 2, SpentEUR: 1.48}
	face := FaceOf(c, Facts{CaseFacts: f, Card: card, Recommend: Recommendation{Action: ActClose, By: "the factory agent", Grounds: "no session fixes the two findings"}})
	detail := FaceKeys(face)
	detail["info"] = "The factory agent found: two findings stand."
	got := CaseFace(Case{ID: 3, Kind: CaseSessionCap, Problem: "an older ask"}, card, CaseAsk{Detail: stored(t, detail)})
	if got.Ask != face.Ask || got.Why != face.Why || got.Story != face.Story || got.Info != "The factory agent found: two findings stand." {
		t.Errorf("face = %+v, want the stored %+v", got, face)
	}
	if got.Preselected != ActClose || got.Source != "the factory agent" || got.Recommendation != "no session fixes the two findings" {
		t.Errorf("suggestion %q by %q on %q, want the stored close", got.Preselected, got.Source, got.Recommendation)
	}
	if want := offersOfAnswers(face.Answers); !reflect.DeepEqual(got.Offers, want) {
		t.Errorf("offers = %+v, want %+v", got.Offers, want)
	}
	if got.Offers[0].Value != ActClose || got.Offers[0].Label != "End the card" {
		t.Errorf("the first offer = %+v, want the suggested close in the vocabulary's words", got.Offers[0])
	}
}

// A case whose ask stored no face, a request's case, is faced by the
// register on the case's own facts, its suggestion the ask row's.
func TestCaseFaceWithoutAStoredFaceReadsTheRegister(t *testing.T) {
	c := Case{Kind: CaseMergeApproval, Facts: map[string]any{"class": "migration", "pr": 9, "checks": "green", "pr_open": true}}
	card := Task{ID: "t1", Kind: "agent", IssueNumber: 4}
	ask := CaseAsk{Detail: map[string]any{"preselected": map[string]any{"action": ActApproveMerge}, "recommended_by": "the factory agent"}}
	got := CaseFace(c, card, ask)
	want := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: card, Recommend: Recommendation{Action: ActApproveMerge, By: "the factory agent"}})
	if got.Ask != want.Ask || got.Why != want.Why || got.Story != want.Story || !reflect.DeepEqual(got.Offers, offersOfAnswers(want.Answers)) {
		t.Errorf("face = %+v, want the register's %+v", got, want)
	}
	if got.Why != "The change alters the database, and the factory's rules let only a human merge it." {
		t.Errorf("why = %q, want the risk class named", got.Why)
	}
	if got.Preselected != ActApproveMerge || got.Source != "the factory agent" {
		t.Errorf("suggestion %q by %q, want the ask row's", got.Preselected, got.Source)
	}
}

// A merge whose model review did not run waits for a human for that: its
// why line says so.
func TestCaseFaceOfAMergeApprovalNamesTheReviewThatDidNotRun(t *testing.T) {
	line := "The model review did not run: the month cap is spent."
	c := Case{Kind: CaseMergeApproval, Facts: map[string]any{"class": "normal", "review_not_run": line}}
	if got := CaseFace(c, Task{ID: "t1"}, CaseAsk{}).Why; got != line {
		t.Errorf("Why = %q, want %q", got, line)
	}
}

// A monitor flag on a wait for dependencies names, as what is left, what
// each dependency waits on; a flag on any other wait names none.
func TestCaseFaceOfADependencyWaitNamesThePlansOwnWait(t *testing.T) {
	since := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	c := Case{Kind: CaseMonitorFlag, Facts: map[string]any{"flag": "wait_over_bound", "wait": "dependency",
		"dependencies": []any{map[string]any{"issue": 31, "status": "waiting-human", "plan": true, "ask": "Confirm the plan?", "since": since}}}}
	want := `The card waits on plan #31, which waits on a human to answer "Confirm the plan?" since 2026-11-01 08:00 UTC.`
	if got := CaseFace(c, Task{ID: "t1"}, CaseAsk{}).Story.Left; got != want {
		t.Errorf("Left = %q, want %q", got, want)
	}
	c.Facts["dependencies"] = []any{map[string]any{"issue": 5, "status": "running", "since": since}}
	if got, want := CaseFace(c, Task{ID: "t1"}, CaseAsk{}).Story.Left, "The card waits on card #5, which is running since 2026-11-01 08:00 UTC."; got != want {
		t.Errorf("Left = %q, want %q", got, want)
	}
	c.Facts["wait"] = "gate"
	if got := CaseFace(c, Task{ID: "t1"}, CaseAsk{}).Story.Left; strings.Contains(got, "waits on card") {
		t.Errorf("Left = %q, want no dependency sentence for a gate wait", got)
	}
}

// An agent's words that read as an answer of the vocabulary leave its
// answers: the face's "End the card" stays the card's own end, never a
// session that reads the words.
func TestAnAgentsWordsNeverStandForTheCardsOwnAnswer(t *testing.T) {
	c := Case{Kind: CaseAgentEscalate, Facts: map[string]any{"reason": AgentAsks + "Ship A or B?", "choices": []any{"Ship A", "end the card.", "Not now"}}}
	room := CaseFacts{Attempts: 1, SessionCap: 3, AgentTimeCapS: 7200, SessionLeastS: 300, BudgetEUR: 2, LeftEUR: 1.5, FloorEUR: 0.3}
	face := FaceOf(c, Facts{CaseFacts: room, Card: Task{ID: "t1", Kind: "agent"}})
	var keys []string
	for _, a := range face.Answers {
		keys = append(keys, a.Key())
	}
	if want := []string{"say:Ship A", ActNote, ActClose}; !reflect.DeepEqual(keys, want) {
		t.Errorf("answers %v, want %v", keys, want)
	}
}
