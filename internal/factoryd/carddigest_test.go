package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The skeleton's two findings and their ids: the first 12 hex of the
// sha256 of each lowercased text, by sha256sum.
const (
	findingA = "metricDivideReplans lacks e.repository = $1"
	findingB = "metricDividePlansByRounds lacks e.repository = $1"
	idA      = "d13e822977d1"
	idB      = "e190a5db3b2b"
)

var digestStart = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// digestAt is m minutes into the life of the fixtures' card.
func digestAt(m int) time.Time { return digestStart.Add(time.Duration(m) * time.Minute) }

// digestRig is a runner on a fake store and a fake object store, with
// card t1410 filed at digestStart by a webhook: medium, budget 5, running.
// Its clock stands at digestStart until a test moves it (at).
type digestRig struct {
	r     *Runner
	store *fakeStore
	tl    *Timeline
}

func newDigestRig(change func(*Task)) digestRig {
	store := newFakeStore()
	objects := &fakeObjectStore{}
	card := Task{ID: "t1410", Kind: "agent", Repository: "sf/app", IssueNumber: 1410, Source: "webhook", Status: "running",
		Size: "medium", BudgetEUR: 5, CreatedAt: digestStart}
	if change != nil {
		change(&card)
	}
	store.tasks[card.ID] = card
	return digestRig{r: &Runner{store: store, objects: objects, cfg: testRunnerConfig(), now: func() time.Time { return digestStart },
		timeline: &Timeline{Objects: objects, Events: store, Store: store}},
		store: store, tl: &Timeline{Objects: objects}}
}

// at moves the rig's clock to minute m.
func (g digestRig) at(m int) { g.r.now = func() time.Time { return digestAt(m) } }

// line appends a timeline line of step at minute m, by the timeline's own
// writer.
func (g digestRig) line(t *testing.T, m int, step string, detail map[string]any) {
	t.Helper()
	g.tl.now = func() time.Time { return digestAt(m) }
	if err := g.tl.Append(context.Background(), "t1410", step, detail); err != nil {
		t.Fatalf("append %s: %v", step, err)
	}
}

func (g digestRig) event(m int, kind, actor string, detail map[string]any) {
	g.store.events = append(g.store.events, Event{ID: int64(len(g.store.events) + 1), At: digestAt(m), Repository: "sf/app",
		TaskID: "t1410", IssueNumber: 1410, Kind: kind, Actor: actor, Detail: detail})
}

// session writes a session of attempt on model small from minute from to
// minute to, ended with outcome.
func (g digestRig) session(t *testing.T, from, to, attempt int, outcome string) string {
	t.Helper()
	id := fmt.Sprintf("s-%d-%d", attempt, from)
	g.line(t, from, "session_start", map[string]any{"session": id, "model": "factory/small", "attempt": attempt})
	g.line(t, to, "session_end", map[string]any{"session": id, "model": "factory/small", "attempt": attempt, "outcome": outcome,
		"summary": "done", "started": rfc3339(digestAt(from))})
	return id
}

func (g digestRig) spend(at time.Time, eur float64) {
	g.store.spends = append(g.store.spends, fakeSpend{task: "t1410", at: at, eur: eur})
}

func (g digestRig) digest(t *testing.T) CardDigest {
	t.Helper()
	d, err := g.r.CardDigest(context.Background(), "t1410")
	if err != nil {
		t.Fatalf("CardDigest: %v", err)
	}
	return d
}

// skeletonRig is the render skeleton's card: plan #1344's card on #1402,
// pushed once, reviewed with two findings the check-in confirmed, stopped
// at its session cap, granted one more session by sebastian that pushed
// nothing twice, now at the merge approval with a budget floor ask open.
// Its project flags two sessions in a row without a push.
func skeletonRig(t *testing.T) digestRig {
	g := newDigestRig(func(c *Task) {
		c.Source, c.Parent, c.PlanRevision = SourcePlan, "t1344", 2
		c.Status, c.Gate, c.SpentEUR = "in-review", GateApproval, 4.92
	})
	v := testPolicyVersion()
	v.Projects["app"] = policy.ProjectConfig{Repository: "sf/app", Waits: &policy.Waits{NoPushSessions: 2}}
	g.r.cfg.Policy = func() policy.Version { return v }
	g.store.tasks["t1344"] = Task{ID: "t1344", Kind: "agent", Repository: "sf/app", IssueNumber: 1344, Track: policy.TrackPlan, PlanName: "session-metrics"}
	if _, err := g.store.AddPlanRevision(context.Background(), "t1344", mustJSON(Plan{}), "r1"); err != nil {
		t.Fatal(err)
	}
	g.store.tasks["t1402"] = Task{ID: "t1402", Kind: "agent", Repository: "sf/app", IssueNumber: 1402, Status: "done"}
	g.store.deps = map[string][]string{"t1410": {"t1402"}}
	first := g.session(t, 41, 55, 1, "pushed")
	g.line(t, 55, "pushed", map[string]any{"session": first, "branch": "factory/t1410", "commit": "a1b2c3d4e5f6a7b8"})
	// 0.19 + 22 × 0.01 = 0.41 over 23 rows inside [+41, +55); the rows at
	// +40 and +55 lie outside
	g.spend(digestAt(40), 0.50)
	g.spend(digestAt(42), 0.19)
	for i := range 22 {
		g.spend(digestAt(43).Add(time.Duration(i)*20*time.Second), 0.01)
	}
	g.spend(digestAt(55), 0.07)
	g.line(t, 58, "model_review", map[string]any{"pr": 12, "commit": "a1b2c3d4e5f6a7b8", "model": "factory/big", "verdict": "concerns",
		"summary": "two reads miss the repository", "findings": []string{findingA, findingB}, "round": 1})
	g.line(t, 62, "checkin", map[string]any{"round": 1, "decision": CheckHandover,
		"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}, findingB: {State: CheckConfirmed}},
		"record":   CheckInRecord{Moment: CheckInGate, Decision: CheckHandover, ToolCalls: 4}})
	g.event(130, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Card 1410 asks: grant one more session?",
		"comment": 27001, "source": "cap", "reason": "the card used its sessions"})
	g.event(580, EventQuestionAnswered, "sebastian", map[string]any{"answer": "Grant one session", "comment": 27002})
	g.session(t, 581, 590, 4, "no_push")
	g.session(t, 600, 610, 5, "no_push")
	g.store.cases = []Case{
		{ID: 1, TaskID: "t1410", Kind: CaseSessionCap, StateKey: "k-cap", Status: CaseResolved, CommentID: 27001,
			Money: &CaseMoney{Kind: string(CaseSessionCap), BudgetEUR: 5, SpentEUR: 4.10}},
		{ID: 2, TaskID: "t1410", Kind: CaseBudgetFloor, StateKey: "k-floor", Status: CaseAtHuman,
			Money: &CaseMoney{Kind: string(CaseBudgetFloor), BudgetEUR: 5, SpentEUR: 4.92}},
	}
	return g
}

func TestCardDigestRendersTheSkeleton(t *testing.T) {
	d := skeletonRig(t).digest(t)
	want := "#1410 · plan #1344 session-metrics · budget 5.00 spent 4.92 holds 0.00 EUR · in-review, approval\n" +
		"   1 +0:00   filed by planner       → revision 2, depends on #1402 (done)\n" +
		"   2 +0:41   session 1 small        → pushed a1b2c3d, 0.41 EUR, 23 calls\n" +
		"   3 +0:58   review                 → concerns, findings " + idA + " " + idB + "\n" +
		"   4 +1:02   checkin gate           → confirmed " + idA + " " + idB + ", handover\n" +
		"   5 +2:10   ask                    → session_cap: Card 1410 asks: grant one more session?\n" +
		"   6 +9:40   answer by sebastian    → Grant one session\n" +
		"   7 +9:41   session 4 small        → no push ×2\n" +
		"flags: grant_bought_nothing(session 4), money_ask_hides_findings, session_no_push(session 4)\n"
	if got := d.Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
	if got := findingID("  MetricDivideReplans\tLACKS   e.repository = $1 "); got != idA {
		t.Fatalf("findingID of the spaced, upper-case text = %s, want %s", got, idA)
	}
	ask := d.Steps[4]
	if ask.Kind != "ask" || ask.Ref != (DigestRef{Comment: "27001", Case: "1", Event: "1"}) {
		t.Fatalf("step 5 = %+v, want the ask with comment 27001 of case 1, event 1", ask)
	}
	if s := d.Steps[1]; s.Ref.Session != "s-1-41" || s.CostEUR < 0.4099 || s.CostEUR > 0.4101 {
		t.Fatalf("step 2 = %+v, want session s-1-41 at 0.41 EUR", s)
	}
}

func TestCardDigestFoldsConsecutiveIdenticalSteps(t *testing.T) {
	d := skeletonRig(t).digest(t)
	if len(d.Steps) != 8 {
		t.Fatalf("Steps = %d, want 8", len(d.Steps))
	}
	folded := d.Folded()
	if len(folded) != 7 {
		t.Fatalf("Folded = %d steps, want 7: %+v", len(folded), folded)
	}
	last := folded[6]
	if last.Kind != "session" || last.Actor != "4 small" || last.Count != 2 || last.N != 7 || last.Ref.Session != "s-4-581" {
		t.Fatalf("folded last = %+v, want session s-4-581, step 7, count 2", last)
	}
	for _, s := range folded[:6] {
		if s.Count != 1 {
			t.Fatalf("folded step %d count = %d, want 1", s.N, s.Count)
		}
	}
}

// Two sessions without a push fold into one line with the run's cost and
// calls: 0.10 + 0.20 over two rows.
func TestCardDigestFoldsSessionsWhoseCostsDiffer(t *testing.T) {
	g := newDigestRig(nil)
	g.session(t, 5, 10, 2, "no_push")
	g.session(t, 15, 20, 3, "no_push")
	g.spend(digestAt(6), 0.10)
	g.spend(digestAt(16), 0.20)
	want := "   2 +0:05   session 2 small        → no push ×2, 0.30 EUR, 2 calls\n"
	if got := g.digest(t).Render(); !strings.Contains(got, want) {
		t.Fatalf("Render =\n%s\nwant the line\n%s", got, want)
	}
}

// A session with no end before another session costs up to the next
// one's start: the row at +25 is the second session's alone.
func TestCardDigestBoundsASessionWithNoEndAtTheNextStart(t *testing.T) {
	g := newDigestRig(nil)
	g.r.now = func() time.Time { return digestAt(60) }
	g.line(t, 10, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
	g.session(t, 20, 30, 2, "no_push")
	g.spend(digestAt(12), 0.10)
	g.spend(digestAt(25), 0.20)
	d := g.digest(t)
	first, second := d.Steps[1], d.Steps[2]
	if first.Ref.Session != "s-1" || first.Outcome != "no end" || first.CostEUR != 0.10 {
		t.Fatalf("first session = %+v, want s-1 with no end at 0.10 EUR", first)
	}
	if second.Ref.Session != "s-2-20" || second.CostEUR != 0.20 {
		t.Fatalf("second session = %+v, want s-2-20 at 0.20 EUR", second)
	}
}

func TestCardDigestFlagsAGrantThatBoughtNothing(t *testing.T) {
	bought := []DigestFlag{{"grant_bought_nothing", "session 4"}}
	for _, tc := range []struct {
		name   string
		budget float64
		seed   func(t *testing.T, g digestRig)
		want   []DigestFlag
	}{
		{"an answer", 5, func(t *testing.T, g digestRig) {
			g.event(10, EventQuestionAnswered, "sebastian", map[string]any{"answer": "Grant one session"})
			g.session(t, 11, 20, 4, "no_push")
		}, bought},
		{"a granted session", 5, func(t *testing.T, g digestRig) {
			g.event(10, EventSessionGranted, "sebastian", map[string]any{"comment": 27002})
			g.session(t, 11, 20, 4, "escalate")
		}, bought},
		{"a fund", 6, func(t *testing.T, g digestRig) {
			g.event(10, EventCardFunded, "sebastian", map[string]any{"amount_eur": 1.0, "from_eur": 5.0, "to_eur": 6.0})
			g.session(t, 11, 20, 4, "no_push")
		}, bought},
		{"a later push", 5, func(t *testing.T, g digestRig) {
			g.event(10, EventQuestionAnswered, "sebastian", map[string]any{"answer": "Grant one session"})
			g.session(t, 11, 20, 4, "no_push")
			g.session(t, 21, 30, 5, "pushed")
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDigestRig(func(c *Task) { c.BudgetEUR = tc.budget })
			tc.seed(t, g)
			if got := g.digest(t).Flags; !slices.Equal(got, tc.want) {
				t.Fatalf("Flags = %v, want %v", got, tc.want)
			}
		})
	}
}

// planAnswerRig is the digest rig with t1410 a plan that holds revision 1,
// the human's answer to its question at +0:10, and the planner's session
// after it: the answer is the grant, and what the session bought shows in
// whether it filed a revision.
func planAnswerRig(t *testing.T) digestRig {
	t.Helper()
	g := newDigestRig(func(c *Task) { c.Track, c.PlanName = policy.TrackPlan, "session-metrics" })
	if _, err := g.store.AddPlanRevision(context.Background(), "t1410", mustJSON(Plan{}), "r1"); err != nil {
		t.Fatal(err)
	}
	g.event(10, EventPlanQuestionAnswered, "sebastian", map[string]any{"revision": 1, "index": 0, "answer": "Keep the table"})
	g.session(t, 13, 20, 0, "reported")
	return g
}

// A plan's grant is the answer to its question, and the planner's session
// that filed a revision bought that revision: the grant closes on the
// revision the session filed, as a direct card's closes on a push, and the
// revision stands on the digest as a step of its own. A planner session
// that files nothing leaves the grant open.
func TestCardDigestCountsAFiledRevisionAsTheGrantsResult(t *testing.T) {
	filed := planAnswerRig(t)
	filed.line(t, 15, "plan_revision", map[string]any{"revision": 2, "cards": 3, "sha256": "5e4b081"})
	d := filed.digest(t)
	if len(d.Flags) != 0 {
		t.Fatalf("Flags after the planner's session filed revision 2 = %v, want none", d.Flags)
	}
	want := "#1410 · plan #1410 session-metrics · budget 5.00 spent 0.00 holds 0.00 EUR · slot held · running\n" +
		"   1 +0:00   filed by webhook       → direct card\n" +
		"   2 +0:10   answer by sebastian    → Keep the table\n" +
		"   3 +0:13   session 0 small        → reported\n" +
		"   4 +0:15   revision               → revision 2, 3 cards\n" +
		"flags: none\n"
	if got := d.Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
	if got, want := planAnswerRig(t).digest(t).Flags, []DigestFlag{{"grant_bought_nothing", "session 0"}}; !slices.Equal(got, want) {
		t.Fatalf("Flags after a planner session that filed nothing = %v, want %v", got, want)
	}
}

func TestCardDigestFlagsAFindingConfirmedTwice(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 10, "model_review", map[string]any{"verdict": "concerns", "findings": []string{findingA, findingB}, "round": 1})
	g.line(t, 12, "checkin", map[string]any{"round": 1, "decision": CheckHandover,
		"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}, findingB: {State: CheckConfirmed}},
		"record":   CheckInRecord{Moment: CheckInGate, Decision: CheckHandover}})
	g.line(t, 30, "checkin", map[string]any{"round": 1, "decision": RungHandUp,
		"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}, findingB: {State: CheckDropped}},
		"record":   CheckInRecord{Moment: CheckInGate, Decision: RungHandUp}})
	d := g.digest(t)
	if want := []DigestFlag{{"finding_confirmed_twice", idA}}; !slices.Equal(d.Flags, want) {
		t.Fatalf("Flags = %v, want %v", d.Flags, want)
	}
	if s := d.Steps[3]; s.Outcome != "confirmed "+idA+", dropped "+idB+", hand_up" || !slices.Equal(s.Findings, []string{idA}) {
		t.Fatalf("second check-in = %+v, want A confirmed and B dropped, findings [A]", s)
	}
}

func TestCardDigestFlagsMoneyAskHidingFindings(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 10, "model_review", map[string]any{"verdict": "concerns", "findings": []string{findingA}, "round": 1})
	g.store.cases = []Case{{ID: 1, TaskID: "t1410", Kind: CaseSessionCap, StateKey: "k1", Status: CaseOpen,
		Money: &CaseMoney{Kind: string(CaseSessionCap), BudgetEUR: 5, SpentEUR: 4}}}
	if got, want := g.digest(t).Flags, []DigestFlag{{"money_ask_hides_findings", ""}}; !slices.Equal(got, want) {
		t.Fatalf("Flags = %v, want %v", got, want)
	}
	g.line(t, 15, "model_review", map[string]any{"round": 2})
	if got, want := g.digest(t).Flags, []DigestFlag{{"money_ask_hides_findings", ""}}; !slices.Equal(got, want) {
		t.Fatalf("Flags after a review with no verdict = %v, want %v", got, want)
	}
	g.line(t, 20, "model_review", map[string]any{"verdict": "approve", "round": 3})
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags after an approving review = %v, want none", got)
	}
}

func TestCardDigestFlagsAnswerChangedNothing(t *testing.T) {
	g := newDigestRig(nil)
	g.store.cases = []Case{
		{ID: 1, TaskID: "t1410", Kind: CaseAgentEscalate, StateKey: "k1", Status: CaseResolved},
		{ID: 2, TaskID: "t1410", Kind: CaseAgentEscalate, StateKey: "k2", Status: CaseWithdrawn},
		{ID: 3, TaskID: "t1410", Kind: CaseAgentEscalate, StateKey: "k1", Status: CaseAtHuman},
		{ID: 4, TaskID: "t1410", Kind: CaseAgentEscalate, StateKey: "k2", Status: CaseOpen},
	}
	if got, want := g.digest(t).Flags, []DigestFlag{{"answer_changed_nothing", "k1"}}; !slices.Equal(got, want) {
		t.Fatalf("Flags = %v, want %v", got, want)
	}
}

func TestCardDigestFlagsTheOtherPatterns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Task)
		seed   func(t *testing.T, g digestRig)
		want   []DigestFlag
	}{
		{"a check-in cut before its decision", nil, func(t *testing.T, g digestRig) {
			g.line(t, 10, "checkin", map[string]any{"round": 1, "record": CheckInRecord{Moment: CheckInGate, ToolCalls: 3}})
		}, []DigestFlag{{"checkin_cut", "+0:10"}}},
		{"a push between them", nil, func(t *testing.T, g digestRig) {
			g.session(t, 5, 10, 1, "no_push")
			g.session(t, 15, 20, 2, "pushed")
			g.session(t, 25, 30, 3, "no_push")
			g.session(t, 35, 40, 4, "no_push")
		}, nil},
		{"a failed dependency", nil, func(t *testing.T, g digestRig) {
			g.store.tasks["t7"] = Task{ID: "t7", Kind: "agent", Repository: "sf/app", IssueNumber: 7, Status: "failed"}
			g.store.tasks["t8"] = Task{ID: "t8", Kind: "agent", Repository: "sf/app", IssueNumber: 8, Status: "done"}
			g.store.deps = map[string][]string{"t1410": {"t8", "t7"}}
		}, []DigestFlag{{"dependency_failed", "#7"}}},
		{"an open case after the end", func(c *Task) { c.Status = "done" }, func(t *testing.T, g digestRig) {
			g.store.cases = []Case{{ID: 1, TaskID: "t1410", Kind: CaseSessionCap, StateKey: "k1", Status: CaseAtHuman}}
		}, []DigestFlag{{"issue_open_after_end", "session_cap"}}},
		{"a budget no event moved", func(c *Task) { c.BudgetEUR = 7 }, nil, []DigestFlag{{"budget_moved_no_event", "7.00 EUR not 5.00 EUR"}}},
		{"a successor's budget, copied from its card", func(c *Task) { c.Source, c.BudgetEUR = SourceSuccessor, 7 }, nil, nil},
		{"a retry's budget, copied from the card it replaces", func(c *Task) { c.Source, c.BudgetEUR = SourcePlan, 7 }, func(t *testing.T, g digestRig) {
			g.event(0, EventCardCreated, FactoryDecider, map[string]any{"by": "sebastian", "issue": 1410, "plan": "t1344", "replaces": "t1300"})
		}, nil},
		{"a filing budget the policy no longer gives", func(c *Task) { c.BudgetEUR = 7 }, func(t *testing.T, g digestRig) {
			g.event(0, EventCardCreated, FactoryDecider, map[string]any{"by": "sebastian", "issue": 1410, "budget_eur": 7.0})
		}, nil},
		{"a budget no event moved from its filing", func(c *Task) { c.BudgetEUR = 9 }, func(t *testing.T, g digestRig) {
			g.event(0, EventCardCreated, FactoryDecider, map[string]any{"by": "sebastian", "issue": 1410, "budget_eur": 7.0})
		}, []DigestFlag{{"budget_moved_no_event", "9.00 EUR not 7.00 EUR"}}},
		{"a human card, which spends no budget", func(c *Task) { c.Kind, c.BudgetEUR = "human", 0 }, nil, nil},
		{"a budget the size gate moved on a card filed before the rows recorded it", func(c *Task) { c.Size, c.BudgetEUR = "small", 5 }, func(t *testing.T, g digestRig) {
			g.event(1, EventCardFunded, FactoryDecider, map[string]any{"size": "small", "from_eur": 5.0, "to_eur": 4.0, "amount_eur": -1.0, "by": "size gate"})
			g.event(2, EventCardFunded, "sebastian", map[string]any{"amount_eur": 1.0})
		}, nil},
		{"a budget a fund and an extend moved", func(c *Task) { c.BudgetEUR = 7.5 }, func(t *testing.T, g digestRig) {
			g.event(5, EventCardFunded, "sebastian", map[string]any{"amount_eur": 2.0})
			g.event(6, EventCardExtended, "sebastian", map[string]any{"sessions": 1, "lines": 0, "amount_eur": 0.5})
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDigestRig(tc.change)
			if tc.seed != nil {
				tc.seed(t, g)
			}
			if got := g.digest(t).Flags; !slices.Equal(got, tc.want) {
				t.Fatalf("Flags = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCardDigestNamesThePlanOfASuccessorsParent(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.Source, c.Parent, c.PlanKey = SourceSuccessor, "t1344", "k1" })
	g.store.tasks["t1344"] = Task{ID: "t1344", Kind: "agent", Repository: "sf/app", IssueNumber: 1344, Track: policy.TrackPlan, PlanName: "session-metrics"}
	if h := g.digest(t).Head; h.Plan != 1344 || h.PlanName != "session-metrics" {
		t.Fatalf("Head = %+v, want plan #1344 session-metrics", h)
	}
}

func TestCardDigestWithoutATimelineRendersTheEventsAlone(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.Status, c.SpentEUR = "waiting-human", 1 })
	g.event(30, EventTaskStatus, FactoryDecider, map[string]any{"status": "waiting-human", "gate": ""})
	g.event(30, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Which file should change?", "comment": 27003,
		"source": "agent", "reason": "Which file should change?"})
	want := "#1410 · budget 5.00 spent 1.00 holds 0.00 EUR · waiting-human\n" +
		"   1 +0:00   filed by webhook       → direct card\n" +
		"   2 +0:30   ask                    → agent_escalate: Which file should change?\n" +
		"flags: none\n"
	if got := g.digest(t).Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}

// A raise of the ceiling is a step of its own, which names the ceiling it
// raised to, and an extend that bought agent time says so beside its
// sessions; an extend without agent time reads as it always did.
func TestCardDigestReadsTheCeilingAndTheAgentTime(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.BudgetEUR = 5.5 })
	g.event(1, EventCardFunded, "sebastian", map[string]any{"ceiling_eur": 9.0, "amount_eur": 0.0, "from_eur": 5.0, "to_eur": 5.0})
	g.event(2, EventCardExtended, "sebastian", map[string]any{"sessions": 1, "agent_time_s": 1800, "lines": 0, "amount_eur": 0.0})
	g.event(3, EventCardExtended, "sebastian", map[string]any{"sessions": 2, "agent_time_s": 0, "lines": 100, "amount_eur": 0.5})
	want := "#1410 · budget 5.50 spent 0.00 holds 0.00 EUR · slot held · running\n" +
		"   1 +0:00   filed by webhook       → direct card\n" +
		"   2 +0:01   ceiling by sebastian   → raised to 9.00 EUR\n" +
		"   3 +0:02   extended by sebastian  → 1 session, 30 minutes of agent time, 0 lines, 0.00 EUR\n" +
		"   4 +0:03   extended by sebastian  → 2 sessions, 100 lines, 0.50 EUR\n" +
		"flags: none\n"
	if got := g.digest(t).Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestCardDigestSkipsAStepKindItDoesNotKnow(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 5, "zebra", map[string]any{"stripes": 3})
	g.line(t, 10, "model_review", map[string]any{"verdict": "approve", "round": 1})
	d := g.digest(t)
	var kinds []string
	for _, s := range d.Steps {
		kinds = append(kinds, s.Kind)
	}
	if want := []string{"filed", "review"}; !slices.Equal(kinds, want) {
		t.Fatalf("step kinds = %v, want %v", kinds, want)
	}
	if d.Steps[1].Ref.Step != "2" {
		t.Fatalf("review ref = %+v, want timeline line 2", d.Steps[1].Ref)
	}
}

func TestCardDigestReadsEveryStepKind(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.Status, c.PRNumber, c.BudgetEUR = "done", 12, 6.5 })
	g.store.tasks["t9"] = Task{ID: "t9", Kind: "agent", Repository: "sf/app", IssueNumber: 1409, Status: "done"}
	g.store.deps = map[string][]string{"t1410": {"t9"}}
	g.event(1, EventDependencySet, "sebastian", map[string]any{"depends_on": "t9", "removed": false})
	g.event(2, EventTaskStatus, FactoryDecider, map[string]any{"status": "queued", "gate": GateIntake})
	g.event(3, EventTaskStatus, FactoryDecider, map[string]any{"status": "running", "gate": GateSession})
	g.event(4, EventTaskStatus, FactoryDecider, map[string]any{"status": "running", "gate": GateChecks})
	g.event(5, EventCardFunded, "sebastian", map[string]any{"amount_eur": 1.0})
	g.event(6, EventCardExtended, "sebastian", map[string]any{"sessions": 2, "lines": 100, "amount_eur": 0.5})
	g.line(t, 7, "checks", map[string]any{"commit": "f00dfeed1234", "state": "failure", "findings": []string{"vet: x unused"}})
	g.line(t, 8, "audit", map[string]any{"commit": "f00dfeed1234", "verdict": AuditFindings,
		"findings": []AuditFinding{{Item: "Token in a log line", Why: "leaks"}}})
	g.line(t, 9, "handover", map[string]any{"handover": "Fix the vet finding first.\nThen rerun.", "session": "s-1"})
	g.line(t, 10, "escalated", map[string]any{"reason": "the agent asks", "comment": 27009})
	g.event(11, EventApprovalRequested, FactoryDecider, map[string]any{"approval": 3, "kind": ApprovalMerge, "pr": 12})
	g.event(12, EventCardRetried, "sebastian", map[string]any{"replacement": 1411})
	g.event(13, EventCardReplanned, "sebastian", map[string]any{"comment": 27010})
	g.event(14, EventCut, FactoryDecider, map[string]any{"plan": "t1344", "revision": 3, "into": []any{1412.0, 1413.0}, "session": 2})
	g.event(15, EventSessionGranted, "sebastian", map[string]any{"comment": 27011})
	g.event(16, EventPlanQuestionAnswered, "sebastian", map[string]any{"revision": 2, "index": 0, "answer": "Keep the table"})
	g.event(17, EventTaskStatus, FactoryDecider, map[string]any{"status": "merged", "gate": GateMerge})
	g.event(18, EventTaskStatus, FactoryDecider, map[string]any{"status": "done", "gate": ""})
	g.event(18, EventTaskEnded, FactoryDecider, map[string]any{"state": "done"})
	g.store.criteriaVerdicts = map[string][]CriterionVerdict{"t1410": {
		{Criterion: "C1", Verdict: "met", Source: FinderReview, DecidedBy: FactoryDecider, CreatedAt: digestAt(19)},
		{Criterion: "C2", Verdict: "", Source: FinderReview, DecidedBy: FactoryDecider, CreatedAt: digestAt(19)},
		{Criterion: "C2", Verdict: "met", Source: FinderApprover, DecidedBy: "sebastian", CreatedAt: digestAt(20)},
	}}
	want := "#1410 · budget 6.50 spent 0.00 holds 0.00 EUR · done\n" +
		"   1 +0:00   filed by webhook       → depends on #1409 (done)\n" +
		"   2 +0:01   dependency by sebastian → depends on #1409\n" +
		"   3 +0:02   status                 → queued, intake\n" +
		"   4 +0:03   status                 → running, session\n" +
		"   5 +0:05   funded by sebastian    → 1.00 EUR\n" +
		"   6 +0:06   extended by sebastian  → 2 sessions, 100 lines, 0.50 EUR\n" +
		"   7 +0:07   checks                 → failure at f00dfee, findings 4285c299662c\n" +
		"   8 +0:08   audit                  → findings d095c64ba7a3\n" +
		"   9 +0:09   handover               → Fix the vet finding first.\n" +
		"  10 +0:11   approval               → merge, pull request #12\n" +
		"  11 +0:12   retried by sebastian   → as #1411\n" +
		"  12 +0:13   replanned by sebastian → asked for a new plan\n" +
		"  13 +0:14   cut by factoryd        → revision 3 into #1412, #1413 at session 2\n" +
		"  14 +0:15   granted by sebastian   → one more session\n" +
		"  15 +0:16   answer by sebastian    → Keep the table\n" +
		"  16 +0:17   merged                 → pull request #12\n" +
		"  17 +0:18   ended                  → done\n" +
		"  18 +0:19   criteria by review     → C1 met, C2 undecided\n" +
		"  19 +0:20   criteria by sebastian  → C2 met\n" +
		"flags: none\n"
	if got := g.digest(t).Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}

// stateGuardLine is the gate's line for a test that failed; its id is the
// first 12 hex of the sha256 of the lowercased text, by sha256sum.
const (
	stateGuardLine = "criterion state_guard: TestState in ./internal/auth failed: Failed --- FAIL: TestState (0.01s)"
	stateGuardID   = "765189771a32"
)

// The gate writes its rows before its line: the step stands at the line's
// start, before the rows it produced.
func TestCardDigestShowsTheGatesRunAsAProofsStep(t *testing.T) {
	g := newDigestRig(nil)
	loginLine := "criterion green_login: TestLogin in ./internal/auth passed"
	g.line(t, 32, "gate_criteria", map[string]any{"pr": 12, "commit": "8e3a1b0c9d", "started": rfc3339(digestAt(29)),
		"proofs": []gateProof{
			{ID: "green_login", Kind: CriterionTest, Suite: "./internal/auth", Name: "TestLogin", Outcome: proofPassed, Met: true, Line: loginLine},
			{ID: "state_guard", Kind: CriterionTest, Suite: "./internal/auth", Name: "TestState", Outcome: proofFailed, Line: stateGuardLine,
				Message: "Failed\n--- FAIL: TestState (0.01s)"},
			{ID: "all_green", Kind: CriterionInvariant, Suite: "./...", Outcome: proofPassed, Met: true,
				Line: "criterion all_green: suite ./... passed (1200 passed, 14 skipped)", Message: "1200 passed, 14 skipped"}},
		"holdouts": []map[string]any{{"id": "holdout-1", "verdict": "red"}}, "findings": []string{stateGuardLine}})
	g.store.criteriaVerdicts = map[string][]CriterionVerdict{"t1410": {
		{Criterion: "green_login", Verdict: "met", Source: CriteriaSourceGate, DecidedBy: FactoryDecider, Evidence: loginLine, CreatedAt: digestAt(31)},
		{Criterion: "state_guard", Verdict: "unmet", Source: CriteriaSourceGate, DecidedBy: FactoryDecider, Evidence: stateGuardLine, CreatedAt: digestAt(31)},
	}}
	d := g.digest(t)
	want := []DigestStep{
		{Kind: "proofs", Actor: "gate", Outcome: "at 8e3a1b0: 1 met, 1 unmet, invariant ./... green, findings " + stateGuardID,
			Findings: []string{stateGuardID}},
		{Kind: "criteria", Actor: "gate", Outcome: "green_login met, state_guard unmet"},
	}
	if len(d.Steps) != 3 || d.Steps[0].Kind != "filed" || !d.Steps[1].At.Equal(digestAt(29)) {
		t.Fatalf("Steps = %+v, want filed, proofs at +0:29, criteria", d.Steps)
	}
	for i, w := range want {
		if s := d.Steps[i+1]; s.Kind != w.Kind || s.Actor != w.Actor || s.Outcome != w.Outcome || !slices.Equal(s.Findings, w.Findings) {
			t.Fatalf("step %d = %+v, want %+v", i+2, s, w)
		}
	}
	render := d.Render()
	for _, w := range []string{"proofs by gate", "criteria by gate"} {
		if !strings.Contains(render, w) {
			t.Fatalf("Render =\n%s\nwant it to hold %q", render, w)
		}
	}
	proofs, err := g.r.CardStep(context.Background(), "t1410", 2, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(2): %v", err)
	}
	for _, w := range []string{"\npull request #12 at 8e3a1b0c9d\n", "\ngreen_login met: " + loginLine + "\n",
		"\nstate_guard unmet: " + stateGuardLine + "\n  Failed\n  --- FAIL: TestState (0.01s)\n",
		"\nall_green met: criterion all_green: suite ./... passed (1200 passed, 14 skipped)\n  1200 passed, 14 skipped\n"} {
		if !strings.Contains(proofs, w) {
			t.Fatalf("CardStep(2) =\n%s\nwant it to hold\n%s", proofs, w)
		}
	}
	verdicts, err := g.r.CardStep(context.Background(), "t1410", 3, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(3): %v", err)
	}
	if w := "state_guard unmet, source gate\n  evidence: " + stateGuardLine; !strings.Contains(verdicts, w) {
		t.Fatalf("CardStep(3) =\n%s\nwant it to hold\n%s", verdicts, w)
	}
	for _, surface := range []string{render, proofs} {
		for _, hidden := range []string{"door_state", "holdout"} {
			if strings.Contains(surface, hidden) {
				t.Fatalf("a digest surface holds %q:\n%s", hidden, surface)
			}
		}
	}
}

func TestCardDigestReadsEveryShapeOfTheGatesLine(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 5, "gate_criteria", map[string]any{"pr": 9, "commit": "c1", "criteria": []map[string]any{{"id": "a", "verdict": "no test", "exit": 1}}})
	g.line(t, 6, "gate_criteria", map[string]any{"pr": 9, "commit": "c1", "skipped": "not the plan's integration card"})
	g.line(t, 7, "gate_criteria", map[string]any{"pr": 9, "commit": "c2c2c2c2", "proofs": []gateProof{},
		"findings": []string{"criteria gate: the plan names no criteria"}})
	g.line(t, 8, "gate_criteria", map[string]any{"pr": 9, "commit": "c3c3c3c3", "start_failed": "image pull: container fetch waits with ErrImagePull"})
	d, err := g.r.CardDigest(context.Background(), "t1410")
	if err != nil {
		t.Fatalf("CardDigest: %v", err)
	}
	var kinds []string
	for _, s := range d.Steps {
		kinds = append(kinds, s.Kind)
	}
	if want := []string{"filed", "proofs", "proofs"}; !slices.Equal(kinds, want) {
		t.Fatalf("step kinds = %v, want %v", kinds, want)
	}
	if !d.Steps[1].At.Equal(digestAt(7)) || !d.Steps[2].At.Equal(digestAt(8)) {
		t.Fatalf("proofs steps at %s and %s, want their lines' times with no started: +0:07 and +0:08", d.Steps[1].At, d.Steps[2].At)
	}
	for i, w := range []string{"at c2c2c2c: no criterion ran, findings 1f2887f692d5",
		"at c3c3c3c: the sandbox did not start: image pull: container fetch waits with ErrImagePull"} {
		if got := d.Steps[i+1].Outcome; got != w {
			t.Fatalf("step %d outcome = %q, want %q", i+2, got, w)
		}
	}
	for n, w := range map[int]string{2: "\n- 1f2887f692d5 criteria gate: the plan names no criteria\n",
		3: "\nstart failed: image pull: container fetch waits with ErrImagePull\n"} {
		got, err := g.r.CardStep(context.Background(), "t1410", n, "card", readPointer)
		if err != nil {
			t.Fatalf("CardStep(%d): %v", n, err)
		}
		if !strings.Contains(got, w) {
			t.Fatalf("CardStep(%d) =\n%s\nwant it to hold\n%s", n, got, w)
		}
	}
}

// A card queued 3 h on its reason, the first refusal at +1:00 against the
// 2 h bound, flags its wait since the refusal's offset and its queue with
// the wait's length. A project that shortens the bound to 1 h flags a card
// queued 1 h 30 m.
func TestWaitOverBoundOnAQueuedCard(t *testing.T) {
	since := digestAt(60)
	g := newDigestRig(func(c *Task) { c.Status, c.QueueReason, c.QueueSince = "queued", "work bound 3 of 3", &since })
	g.at(240)
	d := g.digest(t)
	want := []DigestFlag{{"queued_over_bound", "work bound 3 of 3 for 3h"}, {"wait_over_bound", "queued since +1:00"}}
	if !slices.Equal(d.Flags, want) {
		t.Fatalf("Flags = %v, want %v", d.Flags, want)
	}
	if !strings.HasSuffix(d.Render(), "\nflags: queued_over_bound(work bound 3 of 3 for 3h), wait_over_bound(queued since +1:00)\n") {
		t.Fatalf("Render =\n%s\nwant both flags on the flags line", d.Render())
	}
	if h := d.Head; h.QueueReason != "work bound 3 of 3" || h.QueueSince == nil || !h.QueueSince.Equal(since) || !h.StatusSince.Equal(digestStart) {
		t.Fatalf("Head = %+v, want the reason since +1:00 and the status since the filing", h)
	}
	g.at(150)
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags at 1 h 30 m under the default = %v, want none", got)
	}
	v := testPolicyVersion()
	v.Projects["app"] = policy.ProjectConfig{Repository: "sf/app", Waits: &policy.Waits{Queued: time.Hour}}
	g.r.cfg.Policy = func() policy.Version { return v }
	d = g.digest(t)
	want = []DigestFlag{{"queued_over_bound", "work bound 3 of 3 for 1h30m"}, {"wait_over_bound", "queued since +1:00"}}
	if d.Waits.Queued != time.Hour || !slices.Equal(d.Flags, want) {
		t.Fatalf("Waits.Queued = %s, Flags = %v; want 1h0m0s and %v", d.Waits.Queued, d.Flags, want)
	}
}

// A card queued on a dependency from its filing waits ninety minutes: the
// queue's flag names the dependency and the wait's length, for the filing's
// offset is +0:00 and says nothing of how long the wait is.
func TestQueuedOverBoundNamesTheWaitsLength(t *testing.T) {
	since := digestAt(0)
	g := newDigestRig(func(c *Task) { c.Status, c.QueueReason, c.QueueSince = "queued", "waits on #7", &since })
	v := testPolicyVersion()
	v.Projects["app"] = policy.ProjectConfig{Repository: "sf/app", Waits: &policy.Waits{Queued: time.Hour}}
	g.r.cfg.Policy = func() policy.Version { return v }
	g.at(90)
	want := []DigestFlag{{"queued_over_bound", "waits on #7 for 1h30m"}, {"wait_over_bound", "queued since +0:00"}}
	if d := g.digest(t); !slices.Equal(d.Flags, want) {
		t.Fatalf("Flags = %v, want %v", d.Flags, want)
	}
}

// A running card whose newest timeline step, session 2's start at +0:20,
// is 40 m old against the 30 m bound: the run is quiet since then, later
// than its status move at +0:05.
func TestWaitOverBoundOnAQuietRun(t *testing.T) {
	g := newDigestRig(nil)
	g.event(5, EventTaskStatus, FactoryDecider, map[string]any{"status": "running", "gate": GateSession})
	g.session(t, 10, 15, 1, "no_push")
	g.line(t, 20, "session_start", map[string]any{"session": "s-2", "model": "factory/small", "attempt": 2})
	g.at(60)
	d := g.digest(t)
	if want := []DigestFlag{{"wait_over_bound", "run_quiet since +0:20"}}; !slices.Equal(d.Flags, want) {
		t.Fatalf("Flags = %v, want %v", d.Flags, want)
	}
	if !d.LastStepAt.Equal(digestAt(20)) || !d.Head.StatusSince.Equal(digestAt(5)) {
		t.Fatalf("LastStepAt = %s, StatusSince = %s; want +0:20 and +0:05", d.LastStepAt, d.Head.StatusSince)
	}
}

// auditRefused is the reason the audit's row records when the gateway
// refused its call.
const auditRefused = "the audit call was refused by the gateway (refused_budget): the card's budget is spent"

// The activity the workflow calls when a review or an audit failed for
// good writes the row itself: the flag fires on it by stage and reason,
// and a later row that ran on the same head resets it.
func TestTheNotRunActivitysRowFiresTheFlag(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.Status, c.Gate = "in-review", GateChecks })
	g.event(10, EventSessionPushed, FactoryDecider, map[string]any{"session": "s-1", "attempt": 1})
	if err := g.r.RecordNotRun(context.Background(), "t1410", policy.StageReview, 9, "c1", 1, "litellm down"); err != nil {
		t.Fatal(err)
	}
	want := []DigestFlag{{"review_refused_not_run", "review: litellm down"}}
	if got := g.digest(t).Flags; !slices.Equal(got, want) {
		t.Fatalf("Flags = %v, want %v", got, want)
	}
	if err := g.r.RecordNotRun(context.Background(), "t1410", policy.StageAudit, 9, "c1", 2, "the audit failed: sandbox down"); err != nil {
		t.Fatal(err)
	}
	want = []DigestFlag{{"review_refused_not_run", "audit: the audit failed: sandbox down"},
		{"review_refused_not_run", "review: litellm down"}}
	if got := g.digest(t).Flags; !slices.Equal(got, want) {
		t.Fatalf("Flags after the audit's row = %v, want %v", got, want)
	}
	g.event(30, EventModelReview, FactoryDecider, map[string]any{"commit": "c1", "round": 2, "verdict": "approve", "reviewed": true})
	if got, wantReset := g.digest(t).Flags, want[:1]; !slices.Equal(got, wantReset) {
		t.Fatalf("Flags after a reviewed row on the same head = %v, want %v", got, wantReset)
	}
	g.event(31, EventGateAudit, FactoryDecider, map[string]any{"commit": "c1", "ran": true, "verdict": AuditClear})
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags after an audit that ran = %v, want none", got)
	}
}

// The newest model_review and gate_audit rows since the newest push each
// flag the in-review card when they did not run, by their stage and their
// reason. A later row that ran clears its stage's, a push clears both,
// and a card out of review carries neither.
func TestReviewRefusedNotRunReadsBothRows(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.Status, c.Gate = "in-review", GateChecks })
	g.event(10, EventSessionPushed, FactoryDecider, map[string]any{"session": "s-1", "attempt": 1})
	g.event(20, EventModelReview, FactoryDecider, map[string]any{"commit": "c1", "round": 1, "verdict": "", "reviewed": false,
		"reason": "model review not run: the gateway refused the call"})
	g.event(21, EventGateAudit, FactoryDecider, map[string]any{"commit": "c1", "ran": false, "verdict": "",
		"reason": auditRefused})
	both := []DigestFlag{{"review_refused_not_run", "audit: " + auditRefused},
		{"review_refused_not_run", "review: the gateway refused the call"}}
	if got := g.digest(t).Flags; !slices.Equal(got, both) {
		t.Fatalf("Flags = %v, want %v", got, both)
	}
	g.store.tasks["t1410"] = func() Task { c := g.store.tasks["t1410"]; c.Status = "running"; return c }()
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags of the running card = %v, want none", got)
	}
	g.store.tasks["t1410"] = func() Task { c := g.store.tasks["t1410"]; c.Status = "in-review"; return c }()
	g.event(30, EventModelReview, FactoryDecider, map[string]any{"commit": "c1", "round": 2, "verdict": "approve", "reviewed": true})
	if got, want := g.digest(t).Flags, both[:1]; !slices.Equal(got, want) {
		t.Fatalf("Flags after a reviewed row = %v, want %v", got, want)
	}
	g.event(31, EventGateAudit, FactoryDecider, map[string]any{"commit": "c1", "ran": true, "verdict": AuditClear})
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags after an audit that ran = %v, want none", got)
	}
	g.event(32, EventGateAudit, FactoryDecider, map[string]any{"commit": "c1", "ran": false, "verdict": "", "reason": auditRefused})
	if got, want := g.digest(t).Flags, both[:1]; !slices.Equal(got, want) {
		t.Fatalf("Flags after a second audit that did not run = %v, want %v", got, want)
	}
	g.event(40, EventSessionPushed, FactoryDecider, map[string]any{"session": "s-2", "attempt": 2})
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags after a new push = %v, want none", got)
	}
}

// No wait passed its bound: each is inside it or at it.
func TestNoFlagInsideTheBound(t *testing.T) {
	queued := digestAt(60)
	for _, tc := range []struct {
		name   string
		change func(*Task)
		seed   func(t *testing.T, g digestRig)
		at     int
	}{
		{"queued 1 h of 2 h", func(c *Task) { c.Status, c.QueueReason, c.QueueSince = "queued", "waits on #7", &queued }, nil, 120},
		{"queued 2 h of 2 h", func(c *Task) { c.Status, c.QueueReason, c.QueueSince = "queued", "waits on #7", &queued }, nil, 180},
		{"a run quiet 20 m of 30 m", nil, func(t *testing.T, g digestRig) {
			g.line(t, 40, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
		}, 60},
		{"a dependency wait of 47 h of 48 h", func(c *Task) { c.Status = "waiting-deps" }, nil, 47 * 60},
		{"a gate of 5 h of 6 h", func(c *Task) { c.Status, c.Gate = "in-review", GateChecks }, nil, 5 * 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDigestRig(tc.change)
			if tc.seed != nil {
				tc.seed(t, g)
			}
			g.at(tc.at)
			if got := g.digest(t).Flags; len(got) != 0 {
				t.Fatalf("Flags = %v, want none", got)
			}
		})
	}
}

// planRig is the digest rig with t1410 a done plan task, p its revision
// 1, and rows its plan_proofs rows.
func planRig(t *testing.T, p Plan, rows ...PlanProof) digestRig {
	t.Helper()
	g := newDigestRig(func(c *Task) { c.Track, c.Status, c.PlanName = policy.TrackPlan, "done", "size-doors" })
	if _, err := g.store.AddPlanRevision(context.Background(), "t1410", mustJSON(p), "r1"); err != nil {
		t.Fatal(err)
	}
	g.store.proofs = map[string][]PlanProof{"t1410": rows}
	return g
}

// jsonDetail is an event's detail as its row reads back: s decoded.
func jsonDetail(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("detail %s: %v", s, err)
	}
	return m
}

// proofRow is a criterion's row of state, its test's code beside it.
func proofRow(idx int, id, state string) PlanProof {
	return PlanProof{Idx: idx, Round: 1, ID: id, Criterion: id, Origin: ProofOriginCriterion, State: state, Path: "proofs/" + id + "/" + id + "_test.go",
		Code: "package " + id}
}

// storedKeyLog is keyLog stored with its test.
func storedKeyLog() PlanProof {
	h := keyLog()
	h.State, h.Path, h.Suite, h.Name, h.Code = ProofStored, "proofs/key_log/key_log_test.go", "./proofs/key_log", "TestKeyLog", "package key_log"
	return h
}

// digestPlan is blindPlan's plan with the human criterion pages_ok, an
// integration card integ and a feature card sizes.
func digestPlan() Plan {
	p := blindPlan()
	p.Criteria = append(p.Criteria, PlanCriterion{ID: "pages_ok", Kind: CriterionHuman, Claim: "The help page names every panel",
		Inputs: "the help page", Expected: "every panel"})
	p.Cards = []PlanCard{{Key: "integ", Role: RoleIntegration, Title: "integrate", Brief: "b"},
		{Key: "sizes", Role: RoleFeature, Title: "sizes", Brief: "b"}}
	return p
}

// digestPlanRig is the digest rig with t1410 plan1's card under key and
// plan1's revision digestPlan's.
func digestPlanRig(t *testing.T, key string) digestRig {
	t.Helper()
	g := newDigestRig(func(c *Task) { c.Source, c.Parent, c.PlanKey = SourcePlan, "plan1", key })
	g.store.tasks["plan1"] = Task{ID: "plan1", Repository: "sf/app", IssueNumber: 1340, Kind: "agent", Track: policy.TrackPlan,
		Status: "done", PlanName: "blind proofs"}
	if _, err := g.store.AddPlanRevision(context.Background(), "plan1", mustJSON(digestPlan()), "r1"); err != nil {
		t.Fatal(err)
	}
	return g
}

// The gate's line with states counts blind and open, a proof's opening is
// a step with its failure, and a met gate row names the state it met in.
func TestCardDigestShowsTheBlindTryAndTheOpening(t *testing.T) {
	g := digestPlanRig(t, "integ")
	blindMet := func(id string) gateProof {
		return gateProof{ID: id, Kind: CriterionTest, Suite: "./" + id, Name: "Test" + id, Outcome: proofPassed, Met: true,
			State: proofBlind, Line: "criterion " + id + ": Test" + id + " in ./" + id + " passed"}
	}
	g.line(t, 30, "gate_criteria", map[string]any{"pr": 12, "commit": "8e3a1b0c9d",
		"proofs": []gateProof{blindMet("t_a"), blindMet("t_b"), blindMet("t_c"), blindMet("t_d"), blindMet("t_e"), blindMet("t_f"),
			{ID: "size_bound", Kind: CriterionTest, Suite: "./proofs/size_bound", Name: "TestSizeBound", Outcome: proofFailed,
				State: proofBlind, Line: "criterion size_bound: TestSizeBound in ./proofs/size_bound failed: want 3.75, got 3.5",
				Message: sizeBoundMsg},
			{ID: "door", Kind: CriterionTest, Suite: "./proofs/door", Name: "TestDoor", Outcome: proofFailed, State: proofBlind,
				Line: "criterion door: TestDoor in ./proofs/door failed", Message: "Failed\n--- FAIL: TestDoor (0.00s)"},
			{ID: "group_open", Kind: CriterionTest, Suite: "./acceptance", Name: "TestGroupOpen", Outcome: proofPassed, Met: true,
				State: proofOpen, Changed: true, Path: "acceptance/group_open_test.go",
				Line: "criterion group_open: TestGroupOpen in ./acceptance passed"},
			{ID: "all_green", Kind: CriterionInvariant, Suite: "./...", Outcome: proofPassed, Met: true,
				Line: "criterion all_green: suite ./... passed"}},
		"opened": []string{"size_bound", "door"}})
	g.event(31, EventProofOpened, FactoryDecider, map[string]any{"criterion": "size_bound", "head": "8e3a1b0c9d", "outcome": proofFailed,
		"message": sizeBoundMsg, "path": "proofs/size_bound/size_bound_test.go", "suite": "./proofs/size_bound",
		"name": "TestSizeBound", "code": sizeBoundCode})
	g.event(32, EventProofOpened, FactoryDecider, map[string]any{"criterion": "reads_calm", "head": "8e3a1b0c9d",
		"outcome": proofNoArtifact, "message": "Failed\npanic: no page", "path": "proofs/reads_calm/reads_calm_test.go",
		"suite": "./proofs/reads_calm", "name": "TestReadsCalm", "code": "package reads_calm"})
	g.store.criteriaVerdicts = map[string][]CriterionVerdict{"t1410": {
		{Criterion: "size_bound", Verdict: "unmet", Evidence: "blind: criterion size_bound: TestSizeBound in ./proofs/size_bound failed: want 3.75, got 3.5",
			Kind: CriterionTest, Source: CriteriaSourceGate, DecidedBy: FactoryDecider, CreatedAt: digestAt(31)},
		{Criterion: "group_open", Verdict: "met", Evidence: "open: criterion group_open: TestGroupOpen in ./acceptance passed",
			Kind: CriterionTest, Source: CriteriaSourceGate, DecidedBy: FactoryDecider, CreatedAt: digestAt(31)},
		{Criterion: "t_a", Verdict: "met", Evidence: "blind: criterion t_a: Testt_a in ./t_a passed",
			Kind: CriterionTest, Source: CriteriaSourceGate, DecidedBy: FactoryDecider, CreatedAt: digestAt(31)},
	}}
	d := g.digest(t)
	if got, want := d.Steps[1].Outcome, "at 8e3a1b0: blind 6 met 2 opened, open 1 met, invariant ./... green"; got != want {
		t.Fatalf("proofs outcome = %q, want %q", got, want)
	}
	if d.Steps[1].commit != "8e3a1b0c9d" {
		t.Fatalf("proofs commit = %q, want the line's head 8e3a1b0c9d", d.Steps[1].commit)
	}
	for i, w := range []DigestStep{
		{Kind: "opened", Actor: "gate", Outcome: "size_bound: red at 8e3a1b0: want 3.75, got 3.5"},
		{Kind: "criteria", Actor: "gate", Outcome: "size_bound unmet, group_open met (open), t_a met (blind)"},
		{Kind: "opened", Actor: "gate", Outcome: "reads_calm: no artifact at 8e3a1b0: panic: no page"},
	} {
		if s := d.Steps[i+2]; s.Kind != w.Kind || s.Actor != w.Actor || s.Outcome != w.Outcome {
			t.Fatalf("step %d = %+v, want %+v", i+3, s, w)
		}
	}
	for _, w := range []string{"size_bound unmet", "group_open met (open)", "t_a met (blind)"} {
		if !strings.Contains(d.Steps[3].Outcome, w) {
			t.Fatalf("criteria outcome %q wants %q", d.Steps[3].Outcome, w)
		}
	}
	opened, err := g.r.CardStep(context.Background(), "t1410", 3, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(3): %v", err)
	}
	for _, w := range []string{"size_bound opened at 8e3a1b0c9d: failed\n" +
		"TestSizeBound in ./proofs/size_bound at proofs/size_bound/size_bound_test.go\ncode:\n  package size_bound\n",
		"output:\n  Failed\n  === RUN   TestSizeBound\n"} {
		if !strings.Contains(opened, w) {
			t.Fatalf("CardStep(3) =\n%s\nwant it to hold\n%s", opened, w)
		}
	}
	proofs, err := g.r.CardStep(context.Background(), "t1410", 2, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(2): %v", err)
	}
	if w := "group_open met (open) changed: criterion group_open: TestGroupOpen in ./acceptance passed"; !strings.Contains(proofs, w) {
		t.Fatalf("CardStep(2) =\n%s\nwant it to hold\n%s", proofs, w)
	}
}

// A failed proof's package output can name a passing sibling before the
// failing test. The failure line is the failing test's line.
func TestFailureLineNamesTheFailingTestNotAPassingSibling(t *testing.T) {
	message := "Failed\n--- PASS: TestOther (0.01s)\n--- FAIL: TestDoor (0.00s)\n    door_test.go:12: the door did not open"
	if got, want := failureLine(message), "the door did not open"; got != want {
		t.Fatalf("failureLine = %q, want %q", got, want)
	}
}

// The judgement call, the proofs at main and a human's not met are steps;
// a met row and its event make one step alone.
func TestCardDigestShowsTheJudgementTheProofsAtMainAndTheHumansNotMet(t *testing.T) {
	g := digestPlanRig(t, "integ")
	g.line(t, 40, "judgement", map[string]any{"criterion": "reads_calm", "artifact": "reads_calm.html", "model": "factory/big",
		"prompt_hash": "6b2355d78a19", "verdict": "met", "reason": "the page names every panel's proof", "cost": 0.04})
	ids := []string{"size_bound", "group_open", "door", "no_proof", "all_green", "judged", "pages_ok", "t_a", "t_b"}
	atMain := make([]mainOutcome, len(ids))
	for i, id := range ids {
		atMain[i] = mainOutcome{ID: id, Outcome: proofPassed}
	}
	g.line(t, 41, "proofs_at_main", map[string]any{"head": "29b0d33aa", "criteria": atMain})
	g.event(42, EventCriterionDecided, "sebastian", map[string]any{"approval": 31, "criterion": "pages_ok", "verb": "not_met",
		"reason": "The help page has no links yet."})
	g.event(43, EventCriterionDecided, "sebastian", map[string]any{"approval": 32, "criterion": "pages_ok", "verb": "met"})
	g.store.criteriaVerdicts = map[string][]CriterionVerdict{"t1410": {
		{Criterion: "pages_ok", Verdict: "met", Evidence: "met (pages_ok.txt at 8e3a1b0)", Kind: CriterionHuman,
			Source: CriteriaSourceApprover, DecidedBy: "sebastian", CreatedAt: digestAt(43)},
	}}
	d := g.digest(t)
	for i, w := range []DigestStep{
		{Kind: "judgement", Actor: "big", Outcome: "met: the page names every panel's proof", CostEUR: 0.04},
		{Kind: "proofs", Actor: "runner", Outcome: "at main 29b0d33: 9 of 9 met"},
		{Kind: "criteria", Actor: "sebastian", Outcome: "pages_ok not met"},
		{Kind: "criteria", Actor: "sebastian", Outcome: "pages_ok met"},
	} {
		if s := d.Steps[i+1]; s.Kind != w.Kind || s.Actor != w.Actor || s.Outcome != w.Outcome || s.CostEUR != w.CostEUR {
			t.Fatalf("step %d = %+v, want %+v", i+2, s, w)
		}
	}
	if d.Steps[2].commit != "29b0d33aa" {
		t.Fatalf("proofs at main commit = %q, want the line's head 29b0d33aa", d.Steps[2].commit)
	}
	pages, met := 0, 0
	for _, s := range d.Steps {
		if s.Kind != "criteria" || !strings.HasPrefix(s.Outcome, "pages_ok") {
			continue
		}
		pages++
		if s.Outcome == "pages_ok met" {
			met++
		}
	}
	if pages != 2 || met != 1 {
		t.Fatalf("pages_ok made %d criteria steps, %d of them met, want 2 with 1 met: the row and its event make one step", pages, met)
	}
	judged, err := g.r.CardStep(context.Background(), "t1410", 2, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(2): %v", err)
	}
	if w := "criterion reads_calm, artifact reads_calm.html\nmodel big, prompt 6b2355d78a19, cost 0.04 EUR\n" +
		"reason: the page names every panel's proof\n"; !strings.Contains(judged, w) {
		t.Fatalf("CardStep(2) =\n%s\nwant it to hold\n%s", judged, w)
	}
	atMainStep, err := g.r.CardStep(context.Background(), "t1410", 3, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(3): %v", err)
	}
	for _, w := range []string{"main at 29b0d33aa\n", "size_bound passed\n"} {
		if !strings.Contains(atMainStep, w) {
			t.Fatalf("CardStep(3) =\n%s\nwant it to hold\n%s", atMainStep, w)
		}
	}
	notMet, err := g.r.CardStep(context.Background(), "t1410", 4, "card", readPointer)
	if err != nil {
		t.Fatalf("CardStep(4): %v", err)
	}
	if w := "reason: The help page has no links yet."; !strings.Contains(notMet, w) {
		t.Fatalf("CardStep(4) =\n%s\nwant it to hold\n%s", notMet, w)
	}
}

// An opened proof red at two heads flags; one the builder changed in the
// newest run flags with its path.
func TestCardDigestFlagsTheOpenedProofs(t *testing.T) {
	open := func(changed bool) gateProof {
		p := gateProof{ID: "group_open", Kind: CriterionTest, Suite: "./acceptance", Name: "TestGroupOpen", Outcome: proofFailed,
			State: proofOpen, Path: "acceptance/group_open_test.go", Line: "criterion group_open: TestGroupOpen in ./acceptance failed"}
		if changed {
			p.Changed, p.Met, p.Outcome, p.Line = true, true, proofPassed, "criterion group_open: TestGroupOpen in ./acceptance passed"
		}
		return p
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, g digestRig)
		want []DigestFlag
	}{
		{"red at two heads", func(t *testing.T, g digestRig) {
			g.line(t, 30, "gate_criteria", map[string]any{"pr": 12, "commit": "c1c1c1c1c1", "proofs": []gateProof{open(false)}})
			g.line(t, 35, "gate_criteria", map[string]any{"pr": 12, "commit": "c2c2c2c2c2", "proofs": []gateProof{open(false)}})
		}, []DigestFlag{{"opened_proof_red_twice", "group_open"}}},
		{"red at one head", func(t *testing.T, g digestRig) {
			g.line(t, 30, "gate_criteria", map[string]any{"pr": 12, "commit": "c1c1c1c1c1", "proofs": []gateProof{open(false)}})
		}, nil},
		{"red twice at one head", func(t *testing.T, g digestRig) {
			g.line(t, 30, "gate_criteria", map[string]any{"pr": 12, "commit": "c1c1c1c1c1", "proofs": []gateProof{open(false)}})
			g.line(t, 35, "gate_criteria", map[string]any{"pr": 12, "commit": "c1c1c1c1c1", "proofs": []gateProof{open(false)}})
		}, nil},
		{"changed in the newest run", func(t *testing.T, g digestRig) {
			g.line(t, 30, "gate_criteria", map[string]any{"pr": 12, "commit": "c1c1c1c1c1", "proofs": []gateProof{open(true)}})
		}, []DigestFlag{{"opened_proof_changed", "group_open acceptance/group_open_test.go"}}},
		{"changed only in an older run", func(t *testing.T, g digestRig) {
			g.line(t, 30, "gate_criteria", map[string]any{"pr": 12, "commit": "c1c1c1c1c1", "proofs": []gateProof{open(true)}})
			g.line(t, 35, "gate_criteria", map[string]any{"pr": 12, "commit": "c2c2c2c2c2", "proofs": []gateProof{open(false)}})
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDigestRig(nil)
			tc.run(t, g)
			if got := g.digest(t).Flags; !slices.Equal(got, tc.want) {
				t.Fatalf("flags = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// At the approval a criterion no row holds flags; not at another gate,
// not on a card that is not the plan's integration card.
func TestCardDigestFlagsACriterionNoRowHolds(t *testing.T) {
	var rows []CriterionVerdict
	for _, c := range digestPlan().Criteria {
		if c.ID == "pages_ok" {
			continue
		}
		rows = append(rows, CriterionVerdict{Criterion: c.ID, Verdict: "met", Kind: c.Kind, Source: CriteriaSourceGate,
			DecidedBy: FactoryDecider, CreatedAt: digestAt(30)})
	}
	for _, tc := range []struct {
		key, gate string
		want      []DigestFlag
	}{
		{"integ", GateApproval, []DigestFlag{{"criterion_unproven", "pages_ok"}}},
		{"integ", GateReview, nil},
		{"sizes", GateApproval, nil},
	} {
		t.Run(tc.key+" at "+tc.gate, func(t *testing.T) {
			g := digestPlanRig(t, tc.key)
			c := g.store.tasks["t1410"]
			c.Status, c.Gate = "in-review", tc.gate
			g.store.tasks["t1410"] = c
			g.store.criteriaVerdicts = map[string][]CriterionVerdict{"t1410": rows}
			if got := g.digest(t).Flags; !slices.Equal(got, tc.want) {
				t.Fatalf("flags = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCardDigestShowsTheProversFilingsAndTheStore(t *testing.T) {
	g := planRig(t, proverPlan(), proofRow(1, "size_bound", ProofStored), proofRow(2, "door", ProofStored), proofRow(3, "reads_calm", ProofStored),
		storedKeyLog())
	g.event(40, EventProofsFiled, FactoryDecider, jsonDetail(t, `{"prover": "t-prove", "filing": 1,
		"proofs": [{"criterion": "size_bound", "path": "proofs/size_bound/size_bound_test.go", "suite": "./proofs/size_bound", "name": "TestSizeBound", "outcome": "red"},
			{"criterion": "holdout-4", "outcome": "red"}],
		"refusals": ["file_proofs: no proof for reads_calm", "file_proofs: no proof for door"]}`))
	g.event(45, EventProofsFiled, FactoryDecider, jsonDetail(t, `{"prover": "t-prove", "filing": 2,
		"proofs": [{"criterion": "size_bound", "path": "proofs/size_bound/size_bound_test.go", "suite": "./proofs/size_bound", "name": "TestSizeBound", "outcome": "red"},
			{"criterion": "door", "path": "proofs/door/door_test.go", "suite": "./proofs/door", "name": "TestDoor", "outcome": "red"},
			{"criterion": "reads_calm", "path": "proofs/reads_calm/reads_calm_test.go", "suite": "./proofs/reads_calm", "name": "TestReadsCalm", "outcome": "green"},
			{"criterion": "holdout-4", "outcome": "red"}],
		"refusals": []}`))
	g.event(60, EventProofsStored, FactoryDecider, jsonDetail(t, `{"prover": "t-prove", "commit": "m0", "stored": ["size_bound", "door", "reads_calm", "holdout-4"],
		"flagged": [{"criterion": "door", "flag": "green on main"}]}`))
	d := g.digest(t)
	want := []DigestStep{
		{Kind: "proofs", Actor: "prover", Outcome: "filing 1: 2 red on main, 2 refused"},
		{Kind: "proofs", Actor: "prover", Outcome: "filing 2: 3 red on main, 1 green on main"},
		{Kind: "proofs", Actor: "factoryd", Outcome: "4 stored, 1 flagged green on main"},
	}
	if len(d.Steps) != 4 || d.Steps[0].Kind != "filed" {
		t.Fatalf("Steps = %+v, want filed and three proofs steps", d.Steps)
	}
	for i, w := range want {
		if s := d.Steps[i+1]; s.Kind != w.Kind || s.Actor != w.Actor || s.Outcome != w.Outcome {
			t.Fatalf("step %d = %+v, want %+v", i+2, s, w)
		}
	}
	render := d.Render()
	for _, w := range []string{"#1410 · plan #1410 size-doors · proofs 4 of 4 stored · budget", "   2 +0:40   proofs by prover       → filing 1: 2 red on main, 2 refused\n",
		"   4 +1:00   proofs by factoryd     → 4 stored, 1 flagged green on main\n"} {
		if !strings.Contains(render, w) {
			t.Fatalf("Render =\n%s\nwant it to hold %q", render, w)
		}
	}
	// surfaces[n] is CardStep(n)
	surfaces := []string{render}
	for n := range len(d.Steps) {
		got, err := g.r.CardStep(context.Background(), "t1410", n+1, "card", readPointer)
		if err != nil {
			t.Fatalf("CardStep(%d): %v", n+1, err)
		}
		surfaces = append(surfaces, got)
	}
	for n, wants := range map[int][]string{
		2: {"\nprover t-prove, filing 1\n", "\nsize_bound red on main: TestSizeBound in ./proofs/size_bound at proofs/size_bound/size_bound_test.go\n",
			"\nholdout-4 red on main\n", "\nrefused: file_proofs: no proof for reads_calm\n", "\nrefused: file_proofs: no proof for door\n"},
		3: {"\nprover t-prove, filing 2\n", "\nreads_calm green on main: TestReadsCalm in ./proofs/reads_calm at proofs/reads_calm/reads_calm_test.go\n"},
		4: {"\nprover t-prove at m0\n", "\nsize_bound stored\n", "\ndoor stored, flagged green on main\n", "\nholdout-4 stored\n"},
	} {
		for _, w := range wants {
			if !strings.Contains(surfaces[n], w) {
				t.Fatalf("CardStep(%d) =\n%s\nwant it to hold %q", n, surfaces[n], w)
			}
		}
	}
	if strings.Contains(surfaces[3], "refused:") || strings.Contains(surfaces[4], "{") {
		t.Fatalf("CardStep(3) =\n%s\nCardStep(4) =\n%s\nwant no refusal in a clean filing and no raw detail", surfaces[3], surfaces[4])
	}
	for _, surface := range surfaces {
		for _, hidden := range []string{"key_log", "Every key turn", "package "} {
			if strings.Contains(surface, hidden) {
				t.Fatalf("a digest surface holds %q:\n%s", hidden, surface)
			}
		}
	}
}

// The head counts the due proofs whose row is stored or opened against all
// the prover owes: a row the newest revision dropped, or a dropped
// hold-out, is no proof owed.
func TestCardDigestHeadCountsThePlansProofs(t *testing.T) {
	dropped := keyLog()
	dropped.Idx, dropped.ID, dropped.State = 5, "slow_door", ProofDropped
	rows := []PlanProof{proofRow(1, "size_bound", ProofStored), proofRow(2, "door", ProofFiled), keyLog(), proofRow(6, "old_bound", ProofStored), dropped}
	d := planRig(t, proverPlan(), rows...).digest(t)
	if d.Head.ProofsStored != 1 || d.Head.ProofsWanted != 4 {
		t.Fatalf("Head = %+v, want proofs 1 of 4", d.Head)
	}
	head, _, _ := strings.Cut(d.Render(), "\n")
	if want := "#1410 · plan #1410 size-doors · proofs 1 of 4 stored · budget 5.00 spent 0.00 holds 0.00 EUR · done"; head != want {
		t.Fatalf("head line = %q, want %q", head, want)
	}
	rows[1].State = ProofOpened
	if h := planRig(t, proverPlan(), rows...).digest(t).Head; h.ProofsStored != 2 || h.ProofsWanted != 4 {
		t.Fatalf("Head with door opened = %+v, want proofs 2 of 4", h)
	}
	// a stored hold-out the criterion reads_calm shadows is holdout-7's
	// proof, not the criterion's
	shadowed := storedKeyLog()
	shadowed.Idx, shadowed.ID = 7, "reads_calm"
	if h := planRig(t, proverPlan(), append(rows, shadowed)...).digest(t).Head; h.ProofsStored != 3 || h.ProofsWanted != 5 {
		t.Fatalf("Head with a shadowed hold-out stored = %+v, want proofs 3 of 5", h)
	}
	g := newDigestRig(nil)
	g.store.proofs = map[string][]PlanProof{"t1410": rows}
	off := g.digest(t)
	if head, _, _ := strings.Cut(off.Render(), "\n"); off.Head.ProofsWanted != 0 || strings.Contains(head, "proofs") {
		t.Fatalf("a card off the plan track: Head = %+v, head line %q, want no proofs", off.Head, head)
	}
}

// A plan's proofs are missing an hour after its prover is due, while no
// row is stored: from its foundation card's end done, merged or not, or
// from its first card's filing, at its confirmation, when it has no
// foundation card; never from its revision. Once rows are stored, the flag
// counts the proofs still owed. The proofs_stored event alone stores
// nothing.
func TestCardDigestFlagsAMissingProof(t *testing.T) {
	foundation := func(key, status, merge string) func(g digestRig) {
		return func(g digestRig) {
			at := digestAt(0)
			g.store.tasks["t1411"] = Task{ID: "t1411", Kind: "agent", Repository: "sf/app", IssueNumber: 1411, Source: SourcePlan, Parent: "t1410",
				PlanKey: key, Status: status, MergeCommit: merge, FinishedAt: &at}
		}
	}
	revised := func(g digestRig) { g.store.plans["t1410"][0].CreatedAt = digestAt(0) }
	filed := func(m int) func(g digestRig) {
		return func(g digestRig) {
			g.store.tasks["t1412"] = Task{ID: "t1412", Kind: "agent", Repository: "sf/app", IssueNumber: 1412, Source: SourcePlan, Parent: "t1410",
				PlanKey: "sizes", Status: "queued", CreatedAt: digestAt(m)}
		}
	}
	stored := func(holdout PlanProof) func(g digestRig) {
		return func(g digestRig) {
			g.store.proofs["t1410"] = []PlanProof{proofRow(1, "size_bound", ProofStored), proofRow(2, "door", ProofStored), proofRow(3, "reads_calm", ProofStored), holdout}
		}
	}
	noFoundation := proverPlan()
	noFoundation.Cards = noFoundation.Cards[1:]
	invariants := proverPlan()
	invariants.Criteria = invariants.Criteria[3:]
	for _, tc := range []struct {
		name string
		plan Plan
		at   int
		seed []func(g digestRig)
		want []DigestFlag
	}{
		{"an hour and a minute after the merge", proverPlan(), 61, []func(digestRig){foundation("foundation", "done", "abc")}, []DigestFlag{{"proofs_missing", "1 hour"}}},
		{"three hours and a minute after", proverPlan(), 181, []func(digestRig){foundation("foundation", "done", "abc")}, []DigestFlag{{"proofs_missing", "3 hours"}}},
		{"exactly an hour after", proverPlan(), 60, []func(digestRig){foundation("foundation", "done", "abc")}, nil},
		{"under an hour after", proverPlan(), 59, []func(digestRig){foundation("foundation", "done", "abc")}, nil},
		{"a foundation done without a merge", proverPlan(), 61, []func(digestRig){foundation("foundation", "done", "")}, []DigestFlag{{"proofs_missing", "1 hour"}}},
		{"every proof stored", proverPlan(), 181, []func(digestRig){foundation("foundation", "done", "abc"), stored(storedKeyLog())}, nil},
		{"3 of 4 stored", proverPlan(), 181, []func(digestRig){foundation("foundation", "done", "abc"), stored(keyLog())},
			[]DigestFlag{{"proofs_missing", "1 of 4 wanted proofs without a store"}}},
		{"the store's event with no row stored", proverPlan(), 181, []func(digestRig){foundation("foundation", "done", "abc"), func(g digestRig) {
			g.event(30, EventProofsStored, FactoryDecider, map[string]any{"prover": "t-prove", "commit": "m0", "stored": []any{"size_bound"}, "flagged": []any{}})
		}}, []DigestFlag{{"proofs_missing", "3 hours"}}},
		{"a foundation not done", proverPlan(), 181, []func(digestRig){foundation("foundation", "in-review", "")}, nil},
		{"a plan with no foundation card, its first card filed 61 minutes ago", noFoundation, 181, []func(digestRig){revised, filed(120)},
			[]DigestFlag{{"proofs_missing", "1 hour"}}},
		{"a plan with no foundation card, its first card filed 59 minutes ago", noFoundation, 181, []func(digestRig){revised, filed(122)}, nil},
		{"a plan with no foundation card and no card filed", noFoundation, 181, []func(digestRig){revised}, nil},
		{"a plan that owes no proof", invariants, 181, []func(digestRig){foundation("foundation", "done", "abc")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := planRig(t, tc.plan)
			g.r.now = func() time.Time { return digestAt(tc.at) }
			for _, seed := range tc.seed {
				seed(g)
			}
			if got := g.digest(t).Flags; !slices.Equal(got, tc.want) {
				t.Fatalf("Flags = %v, want %v", got, tc.want)
			}
		})
	}
}

// Each row of the owned wait, read at a minute just past its bound where
// the wait flags: the first row that holds, a failed merge's retry before
// the gate it sits in, and a rung's step only on a card no earlier row
// holds.
func TestOwnedWaitTakesTheFirstRowThatHolds(t *testing.T) {
	queued, gateStart := digestAt(60), digestAt(40)
	inReview := func(gate string) func(*Task) { return func(c *Task) { c.Status, c.Gate = "in-review", gate } }
	moved := func(m int, status, gate string) func(t *testing.T, g digestRig) {
		return func(t *testing.T, g digestRig) {
			g.event(m, EventTaskStatus, FactoryDecider, map[string]any{"status": status, "gate": gate})
		}
	}
	rungStep := func(status string, rung, due int) func(t *testing.T, g digestRig) {
		return func(t *testing.T, g digestRig) {
			at := digestAt(due)
			g.line(t, 40, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
			g.store.cases = []Case{{ID: 1, TaskID: "t1410", Kind: CaseMonitorFlag, StateKey: "k1", Status: status}}
			g.store.caseSteps = map[int64][]CaseStep{1: {{ID: 1, CaseID: 1, Rung: rung, StartedAt: digestAt(10), Outcome: StepInProgress, DueAt: &at}}}
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*Task)
		seed   func(t *testing.T, g digestRig)
		at     int
		want   ownedWait
		held   bool
		flags  []DigestFlag
	}{
		{"queued with a reason", func(c *Task) { c.Status, c.QueueReason, c.QueueSince = "queued", "waits on #7", &queued }, nil, 90,
			ownedWait{"queued", 0, queued}, true, nil},
		{"queued with no reason", func(c *Task) { c.Status = "queued" }, nil, 90, ownedWait{}, false, nil},
		{"waiting on dependencies", func(c *Task) { c.Status = "waiting-deps" }, moved(10, "waiting-deps", GateDependencies), 48*60 + 11,
			ownedWait{"dependency", 0, digestAt(10)}, true, []DigestFlag{{"wait_over_bound", "dependency since +0:10"}}},
		{"in a gate", inReview(GateReview), moved(30, "in-review", GateReview), 6*60 + 31,
			ownedWait{"gate", 0, digestAt(30)}, true, []DigestFlag{{"wait_over_bound", "gate since +0:30"}}},
		{"in a gate since its own start", func(c *Task) {
			c.Status, c.Gate, c.GateSince = "in-review", GateReview, &gateStart
		}, moved(30, "in-review", GateChecks), 6*60 + 41,
			ownedWait{"gate", 0, gateStart}, true, []DigestFlag{{"wait_over_bound", "gate since +0:40"}}},
		{"at the approval", inReview(GateApproval), moved(30, "in-review", GateApproval), 6*60 + 31, ownedWait{}, false, nil},
		{"a failed merge with its retry pending", inReview(GateMerge), func(t *testing.T, g digestRig) {
			moved(30, "in-review", GateMerge)(t, g)
			g.line(t, 50, "merge_failed", map[string]any{"pr": 12, "commit": "c1", "error": "forgejo is down"})
		}, 111, ownedWait{"merge_retry", 0, digestAt(50)}, true, []DigestFlag{{"wait_over_bound", "merge_retry since +0:50"}}},
		{"a failed merge a later step followed", inReview(GateMerge), func(t *testing.T, g digestRig) {
			moved(30, "in-review", GateMerge)(t, g)
			g.line(t, 50, "merge_failed", map[string]any{"pr": 12, "commit": "c1", "error": "forgejo is down"})
			g.line(t, 55, "queue_turn", map[string]any{"pr": 12})
		}, 111, ownedWait{"gate", 0, digestAt(30)}, true, nil},
		{"a provider refusal", nil, func(t *testing.T, g digestRig) {
			g.session(t, 10, 12, 1, string(OutcomeProviderRefused))
			g.line(t, 12, StepProviderRefused, map[string]any{"status": 529, "reason": "overloaded", "backoff_s": 120})
		}, 45, ownedWait{"provider_backoff", 0, digestAt(14)}, true, []DigestFlag{{"wait_over_bound", "provider_backoff since +0:14"}}},
		{"a quiet run", nil, func(t *testing.T, g digestRig) {
			g.line(t, 20, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
		}, 51, ownedWait{"run_quiet", 0, digestAt(20)}, true, []DigestFlag{{"wait_over_bound", "run_quiet since +0:20"}}},
		{"a run that moves", nil, func(t *testing.T, g digestRig) {
			g.line(t, 40, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
		}, 60, ownedWait{}, false, nil},
		{"a run with no step that answers", nil, func(t *testing.T, g digestRig) {
			g.line(t, 20, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
			g.spend(digestAt(45), 0.01)
		}, 51, ownedWait{}, false, nil},
		{"a run whose last answer is older than the bound", nil, func(t *testing.T, g digestRig) {
			moved(5, "running", GateSession)(t, g)
			g.line(t, 10, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
			g.spend(digestAt(4*60+29), 0.01)
		}, 5 * 60, ownedWait{"run_quiet", 0, digestAt(4*60 + 29)}, true, []DigestFlag{{"wait_over_bound", "run_quiet since +4:29"}}},
		{"a rung's step past its due", nil, rungStep(CaseOpen, 1, 45), 46,
			ownedWait{"rung_answer", 1, digestAt(10)}, true, []DigestFlag{{"wait_over_bound", "rung_answer since +0:10"}}},
		{"a rung's step past a due inside the bound", nil, rungStep(CaseOpen, 1, 20), 21,
			ownedWait{"rung_answer", 1, digestAt(10)}, true, []DigestFlag{{"wait_over_bound", "rung_answer since +0:10"}}},
		{"a rung's step not yet due", nil, rungStep(CaseOpen, 1, 50), 46, ownedWait{}, false, nil},
		{"a rung's step on a case that ended", nil, rungStep(CaseResolved, 1, 45), 46, ownedWait{}, false, nil},
		{"the human's step past its due", nil, rungStep(CaseAtHuman, 3, 45), 46, ownedWait{}, false, nil},
		{"a rung's step past its due on a queued card", func(c *Task) { c.Status, c.QueueReason, c.QueueSince = "queued", "waits on #7", &queued },
			rungStep(CaseAtHuman, 1, 45), 46, ownedWait{"queued", 0, queued}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDigestRig(tc.change)
			if tc.seed != nil {
				tc.seed(t, g)
			}
			g.at(tc.at)
			d := g.digest(t)
			if got, held := d.ownedWait(); got != tc.want || held != tc.held {
				t.Fatalf("ownedWait = %+v, %t; want %+v, %t", got, held, tc.want, tc.held)
			}
			if !slices.Equal(d.Flags, tc.flags) {
				t.Fatalf("Flags = %v, want %v", d.Flags, tc.flags)
			}
		})
	}
}

func TestCardDigestFlagsAProofTheDryRunFlagged(t *testing.T) {
	g := planRig(t, proverPlan())
	g.event(30, EventProofsStored, FactoryDecider, jsonDetail(t, `{"prover": "t-prove", "commit": "m0", "stored": ["size_bound", "door", "reads_calm", "holdout-4"],
		"flagged": [{"criterion": "door", "flag": "green on main"}, {"criterion": "holdout-4", "flag": "unrunnable"}]}`))
	d := g.digest(t)
	if len(d.Steps) != 2 {
		t.Fatalf("Steps = %+v, want filed and the store", d.Steps)
	}
	if want := []DigestFlag{{"prover_refused_thrice", "door"}, {"prover_refused_thrice", "holdout-4"}}; !slices.Equal(d.Flags, want) {
		t.Fatalf("Flags = %v, want %v", d.Flags, want)
	}
	if got, want := d.Steps[1].Outcome, "4 stored, 1 flagged green on main, 1 flagged unrunnable"; got != want {
		t.Fatalf("step 2 outcome = %q, want %q", got, want)
	}
	clean := planRig(t, proverPlan())
	clean.event(30, EventProofsStored, FactoryDecider, jsonDetail(t, `{"prover": "t-prove", "commit": "m0", "stored": ["size_bound"], "flagged": []}`))
	if d := clean.digest(t); len(d.Flags) != 0 || len(d.Steps) != 2 || d.Steps[1].Outcome != "1 stored" {
		t.Fatalf("a store with no flag: Flags = %v, step 2 %+v, want none and 1 stored", d.Flags, d.Steps[1])
	}
}

// A proofs event whose detail does not decode is a step that says so,
// its filing's number kept, and its detail a line that says so too.
func TestCardDigestReadsAnUnreadableProofsEvent(t *testing.T) {
	g := planRig(t, proverPlan())
	g.event(30, EventProofsFiled, FactoryDecider, map[string]any{"prover": "t-prove", "filing": 2, "proofs": "not a list"})
	g.event(40, EventProofsStored, FactoryDecider, map[string]any{"prover": "t-prove", "stored": 4})
	d := g.digest(t)
	if len(d.Steps) != 3 {
		t.Fatalf("Steps = %+v, want filed and the two proofs steps", d.Steps)
	}
	for i, want := range []string{"filing 2: unreadable detail", "unreadable detail"} {
		if got := d.Steps[i+1].Outcome; got != want {
			t.Fatalf("step %d outcome = %q, want %q", i+2, got, want)
		}
	}
	for n := 2; n <= 3; n++ {
		got, err := g.r.CardStep(context.Background(), "t1410", n, "card", readPointer)
		if err != nil || !strings.Contains(got, "\nunreadable detail\n") {
			t.Fatalf("CardStep(%d) = %q, %v; want the detail named unreadable", n, got, err)
		}
	}
}

// A report session records no attempt: the prover's reads by its role and
// its model; a session without a role keeps the attempt.
func TestCardDigestNamesTheProversSession(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		role       map[string]any
	}{
		{"the prover", "prover big", map[string]any{"role": "prover"}},
		{"a session without a role", "0 big", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDigestRig(func(c *Task) { c.Source, c.Parent, c.IssueNumber = SourceProver, "t-plan", 0 })
			line := func(detail map[string]any) map[string]any { maps.Copy(detail, tc.role); return detail }
			g.line(t, 1, "session_start", line(map[string]any{"session": "s-p", "model": "factory/big", "mode": "report"}))
			g.line(t, 5, "session_end", line(map[string]any{"session": "s-p", "model": "factory/big", "mode": "report", "outcome": "reported",
				"summary": "filed 4 proofs, filing 1", "started": rfc3339(digestAt(1))}))
			d := g.digest(t)
			if len(d.Steps) != 2 {
				t.Fatalf("Steps = %+v, want filed and the session", d.Steps)
			}
			if s := d.Steps[1]; s.Kind != "session" || s.Actor != tc.want || s.Outcome != "reported" {
				t.Fatalf("step 2 = %+v, want session %s, reported", s, tc.want)
			}
			if w := "session " + tc.want + " "; !strings.Contains(d.Render(), w) {
				t.Fatalf("Render =\n%s\nwant it to hold %q", d.Render(), w)
			}
		})
	}
}

// The policy's default bound is three sessions in a row without a push:
// two do not flag, three flag at the run's first.
func TestSessionNoPushReadsThePolicyBound(t *testing.T) {
	g := newDigestRig(nil)
	g.session(t, 5, 10, 1, "no_push")
	g.session(t, 15, 20, 2, "no_push")
	if got := g.digest(t).Flags; len(got) != 0 {
		t.Fatalf("Flags after two sessions without a push = %v, want none", got)
	}
	g.session(t, 25, 30, 3, "no_push")
	if got, want := g.digest(t).Flags, []DigestFlag{{"session_no_push", "session 1"}}; !slices.Equal(got, want) {
		t.Fatalf("Flags after three = %v, want %v", got, want)
	}
}

// A fifth refusal backs off 30 m. The wait ages from the retry's due time,
// the refusal's line at +0:12 plus its 1800 s: 31 m after the line it is
// 1 m past due, inside the 30 m bound. 61 m after, it is 31 m past.
func TestProviderBackoffAgesFromTheDueRetry(t *testing.T) {
	g := newDigestRig(nil)
	g.session(t, 10, 12, 5, string(OutcomeProviderRefused))
	g.line(t, 12, StepProviderRefused, map[string]any{"status": 529, "reason": "overloaded", "backoff_s": 1800})
	due := ownedWait{"provider_backoff", 0, digestAt(42)}
	g.at(43)
	d := g.digest(t)
	if w, ok := d.ownedWait(); !ok || w != due || len(d.Flags) != 0 {
		t.Fatalf("at +0:43: ownedWait = %+v, %t, Flags = %v; want %+v and none", w, ok, d.Flags, due)
	}
	g.at(73)
	d = g.digest(t)
	if w, ok := d.ownedWait(); !ok || w != due {
		t.Fatalf("at +1:13: ownedWait = %+v, %t; want %+v", w, ok, due)
	}
	if want := []DigestFlag{{"wait_over_bound", "provider_backoff since +0:42"}}; !slices.Equal(d.Flags, want) {
		t.Fatalf("at +1:13: Flags = %v, want %v", d.Flags, want)
	}
}

// A skipped stage's timeline line also writes its events row, which the
// approval rule reads to leave the stage out.
func TestSkippedStageWritesItsEventRow(t *testing.T) {
	g := newDigestRig(nil)
	if err := g.r.timeline.Append(context.Background(), "t1410", StepStageSkipped, map[string]any{"stage": "audit", "reason": "off"}); err != nil {
		t.Fatal(err)
	}
	if len(g.store.events) != 1 || g.store.events[0].Kind != EventStageSkipped || g.store.events[0].Detail["stage"] != "audit" {
		t.Fatalf("events = %+v, want one stage_skipped row for audit", g.store.events)
	}
}

// A resumed card the surge bound holds back flags its wait by its own
// reason, which names it as resumed.
func TestQueuedOverBoundNamesAResumedWait(t *testing.T) {
	since := digestAt(60)
	g := newDigestRig(func(c *Task) {
		c.Status, c.QueueReason, c.QueueSince, c.ResumedAt = "queued", ResumedWaitReason+"7 of 7 live, 1 ahead", &since, &since
	})
	g.at(240)
	want := []DigestFlag{{"queued_over_bound", "resumed: work bound 7 of 7 live, 1 ahead for 3h"}, {"wait_over_bound", "queued since +1:00"}}
	if got := g.digest(t).Flags; !slices.Equal(got, want) {
		t.Fatalf("Flags = %v, want %v", got, want)
	}
}
