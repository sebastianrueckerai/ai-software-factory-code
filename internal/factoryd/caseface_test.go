package factoryd

import (
	"context"
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

// A moment's words name UTC in every layout: as a word after the time, or
// as the Z of an RFC 3339 stamp, in winter and in summer alike.
func TestAMomentsWordsNameUTC(t *testing.T) {
	winter, summer := time.Date(2026, 1, 15, 5, 12, 0, 0, time.UTC), time.Date(2026, 7, 1, 10, 30, 5, 0, time.FixedZone("CEST", 2*3600))
	for _, c := range []struct {
		m    Moment
		want string
	}{
		{Moment{At: winter, Layout: LayoutWords}, "15 January at 05:12 UTC"},
		{Moment{At: summer, Layout: LayoutClock}, "08:30 UTC"},
		{Moment{At: summer, Layout: LayoutDay}, "1 Jul 08:30 UTC"},
		{Moment{At: winter, Layout: LayoutStamp}, "2026-01-15 05:12 UTC"},
		{Moment{At: winter, Layout: LayoutRFC3339}, "2026-01-15T05:12:00Z"},
		{Moment{At: summer, Layout: LayoutRFC3339}, "2026-07-01T08:30:05Z"},
	} {
		if got := c.m.Words(); got != c.want {
			t.Errorf("%s in %q = %q, want %q", c.m.At, c.m.Layout, got, c.want)
		}
	}
}

// A refusal of an answer on an ended case says when it ended in UTC, its
// instant beside the words: a summer withdrawal at 12:30 CEST reads 10:30
// UTC, a winter resolution at 10:04 UTC as it is.
func TestACaseEndedSaysWhenInUTC(t *testing.T) {
	summer, winter := time.Date(2026, 7, 1, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600)), time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	for _, c := range []struct {
		err  ErrCaseEnded
		want string
		at   time.Time
	}{
		{ErrCaseEnded{Card: "#7", Case: Case{Status: CaseWithdrawn, ResolvedAt: &summer, Resolution: &CaseResolution{Reason: "the card closed"}}},
			"was withdrawn at 10:30 UTC: the card closed", summer},
		{ErrCaseEnded{Card: "#7", Case: Case{Status: CaseResolved, ResolvedAt: &winter, Resolution: &CaseResolution{By: "sebastian", Action: "answer"}}},
			"was resolved by sebastian with answer at 10:04 UTC", winter},
	} {
		if got := c.err.Error(); !strings.HasSuffix(got, c.want) {
			t.Errorf("Error() = %q, want it to end %q", got, c.want)
		}
		if want := (Moments{{At: c.at.UTC(), Layout: LayoutClock}}); !reflect.DeepEqual(c.err.Moments(), want) {
			t.Errorf("Moments() = %+v, want %+v", c.err.Moments(), want)
		}
	}
	if m := (ErrCaseEnded{Case: Case{Status: CaseOpen}}).Moments(); m != nil {
		t.Errorf("an unresolved case's moments = %+v, want none", m)
	}
}

// The instants an ask names travel with it: its row stores them beside its
// words, its case takes them, and its face carries them, before the
// register stores the face and after. The texts keep their words in UTC:
// the face, and the comment that asks on the card's issue.
func TestAnAsksMomentsTravelToItsFace(t *testing.T) {
	since := time.Date(2026, 11, 16, 5, 12, 0, 0, time.UTC)
	q := StallQuestion("sf/code", "abc1234def", since, []string{"ci / test"}, nil)
	card := Task{ID: "t-stall", Kind: "human", Repository: "sf/code", IssueNumber: 7, Status: "waiting-human"}
	row := stored(t, HumanAsk{Q: q, By: "factoryd", Asker: SourceChainStall, Via: ViaFactoryd}.Event(card).Detail)
	c, err := CaseFromAsk(card, Event{Detail: row}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Moments{{At: since, Layout: LayoutStamp}, {At: since, Layout: LayoutWords}}
	face := CaseFace(c, card, CaseAsk{Detail: row})
	if !reflect.DeepEqual(face.Moments, want) {
		t.Errorf("moments = %+v, want %+v", face.Moments, want)
	}
	happened := "The chain at commit abc1234 has been pending since 16 November at 05:12 UTC, waiting on 1 check."
	if face.Story.Happened != happened || !strings.Contains(face.Info, "pending at commit abc1234 since 2026-11-16 05:12 UTC, past") {
		t.Errorf("happened %q, info %q: want the words in UTC", face.Story.Happened, face.Info)
	}
	keys := FaceKeys(FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: card}))
	for k, v := range stored(t, keys) {
		row[k] = v
	}
	if face := CaseFace(c, card, CaseAsk{Detail: row}); !reflect.DeepEqual(face.Moments, want) || face.Story.Happened != happened {
		t.Errorf("the stored face: moments %+v, happened %q", face.Moments, face.Story.Happened)
	}
	if comment := IssueAsk(keys); !strings.Contains(comment, happened) {
		t.Errorf("the issue's comment = %q, want %q", comment, happened)
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
// each dependency waits on, its since beside its words; a flag on any
// other wait names none.
func TestCaseFaceOfADependencyWaitNamesThePlansOwnWait(t *testing.T) {
	since := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	c := Case{Kind: CaseMonitorFlag, Facts: map[string]any{"flag": "wait_over_bound", "wait": "dependency",
		"dependencies": []any{map[string]any{"issue": 31, "status": "waiting-human", "plan": true, "ask": "Confirm the plan?", "since": since}}}}
	want := `The card waits on plan #31, which waits on a human to answer "Confirm the plan?" since 2026-11-01 08:00 UTC.`
	face := CaseFace(c, Task{ID: "t1"}, CaseAsk{})
	if got := face.Story.Left; got != want {
		t.Errorf("Left = %q, want %q", got, want)
	}
	if want := (Moments{{At: since, Layout: LayoutStamp}}); !reflect.DeepEqual(face.Moments, want) {
		t.Errorf("moments = %+v, want the since beside its words, %+v", face.Moments, want)
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

// A merge whose model review approved its head suggests the merge, by the
// model review, on a line composed from the review's facts. Red checks, a
// review with concerns and a review that did not run suggest nothing.
func TestAMergeSuggestsWhatItsReviewApproved(t *testing.T) {
	request := func(spec MergeSpec) ApprovalRequest {
		b, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		return ApprovalRequest{ID: 4, Kind: ApprovalMerge, TaskID: "t-m", Project: "sf/r", PRNumber: 9, Spec: b}
	}
	approved := Review{Verdict: "approve", Summary: "Adds the column safely."}
	for name, c := range map[string]struct {
		spec MergeSpec
		want string
	}{
		"approved":    {MergeSpec{RiskClass: "high", HeadSHA: "abc123", Review: approved, VerdictSummary: approved.Summary}, ActApproveMerge},
		"red checks":  {MergeSpec{RiskClass: "high", HeadSHA: "abc123", Review: approved, VerdictSummary: approved.Summary, RedChecks: true}, ""},
		"concerns":    {MergeSpec{RiskClass: "high", HeadSHA: "abc123", Review: Review{Verdict: "concerns", Summary: "The column lacks a default."}}, ""},
		"did not run": {MergeSpec{RiskClass: "high", HeadSHA: "abc123", Review: Review{Verdict: "approve", Note: "the review did not run"}}, ""},
	} {
		k, err := RequestCase(request(c.spec))
		if err != nil {
			t.Fatal(err)
		}
		face := CaseFace(k, Task{ID: "t-m", Kind: "agent"}, CaseAsk{})
		if face.Preselected != c.want || c.want != "" && (face.Source != "the model review" || face.Recommendation != "It approved the change with no finding.") {
			t.Errorf("%s: suggests %q by %q on %q, want %q by the model review", name, face.Preselected, face.Source, face.Recommendation, c.want)
		}
	}
}

// A budget floor on a card whose sessions left no work on its branch says
// the work is lost on its face, read from the case's stored facts, in the
// same words as the ask the human was sent.
func TestCaseFaceAndAskAgreeOnLostWork(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	card := g.store.tasks["t1"]
	card.SpentEUR, card.HeldEUR, card.IssueNumber = 4.4, 0, 0
	g.store.tasks["t1"] = card
	g.r.timeline = &Timeline{Objects: &fakeObjectStore{}, Events: g.store, Store: g.store}
	f, err := g.r.tripFacts(ctx, card, "", "")
	if err != nil {
		t.Fatal(err)
	}
	why, err := g.r.SessionFunded(ctx, "t1", "factory/small")
	if err != nil || !BudgetRefusal(why) {
		t.Fatalf("SessionFunded = (%q, %v), want the money ask", why, err)
	}
	c, _, err := g.r.tripCase(ctx, card, f, TripInput{TaskID: "t1", Reason: why})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.AskHuman(ctx, card, c, why, "next", nil, nil); err != nil {
		t.Fatal(err)
	}
	var asked Story
	var askedWhy string
	for _, e := range g.store.events {
		if e.Kind == EventQuestionAsked {
			asked, _ = e.Detail["story"].(Story)
			askedWhy, _ = e.Detail["why"].(string)
		}
	}
	const lost = "The last session's work was not pushed and is lost. The next session starts again from main."
	face := CaseFace(c, card, CaseAsk{})
	if asked.Left != lost || face.Story != asked || askedWhy == "" || face.Why != askedWhy {
		t.Errorf("the face's story %+v and why %q, the ask's story %+v and why %q; want both to say the work is lost", face.Story, face.Why, asked, askedWhy)
	}
}
