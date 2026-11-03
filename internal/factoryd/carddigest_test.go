package factoryd

import (
	"context"
	"fmt"
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
	return digestRig{r: &Runner{store: store, objects: objects, cfg: testRunnerConfig()}, store: store, tl: &Timeline{Objects: objects}}
}

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
func skeletonRig(t *testing.T) digestRig {
	g := newDigestRig(func(c *Task) {
		c.Source, c.Parent, c.PlanRevision = SourcePlan, "t1344", 2
		c.Status, c.Gate, c.SpentEUR = "in-review", GateApproval, 4.92
	})
	g.store.tasks["t1344"] = Task{ID: "t1344", Kind: "agent", Repository: "sf/app", IssueNumber: 1344, Track: policy.TrackPlan, PlanName: "session-metrics"}
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
	want := "#1410 · plan #1344 session-metrics · budget 5.00 spent 4.92 holds 0.00 EUR · slot held · in-review, approval\n" +
		"   1 +0:00   filed by planner       → revision 2, depends on #1402 (done)\n" +
		"   2 +0:41   session 1 small        → pushed a1b2c3d, 0.41 EUR, 23 calls\n" +
		"   3 +0:58   review                 → concerns, findings " + idA + " " + idB + "\n" +
		"   4 +1:02   checkin gate           → confirmed " + idA + " " + idB + ", handover\n" +
		"   5 +2:10   ask                    → session_cap: Card 1410 asks: grant one more session?\n" +
		"   6 +9:40   answer by sebastian    → Grant one session\n" +
		"   7 +9:41   session 4 small        → no push ×2\n" +
		"flags: grant_bought_nothing(session 4), money_ask_hides_findings, session_no_push(session 4), slot_held_by_wait(approval)\n"
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

func TestCardDigestFlagsAFindingConfirmedTwice(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 10, "model_review", map[string]any{"verdict": "concerns", "findings": []string{findingA, findingB}, "round": 1})
	g.line(t, 12, "checkin", map[string]any{"round": 1, "decision": CheckHandover,
		"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}, findingB: {State: CheckConfirmed}},
		"record":   CheckInRecord{Moment: CheckInGate, Decision: CheckHandover}})
	g.line(t, 30, "checkin", map[string]any{"round": 1, "decision": CheckHuman,
		"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}, findingB: {State: CheckDropped}},
		"record":   CheckInRecord{Moment: CheckInGate, Decision: CheckHuman}})
	d := g.digest(t)
	if want := []DigestFlag{{"finding_confirmed_twice", idA}}; !slices.Equal(d.Flags, want) {
		t.Fatalf("Flags = %v, want %v", d.Flags, want)
	}
	if s := d.Steps[3]; s.Outcome != "confirmed "+idA+", dropped "+idB+", human" || !slices.Equal(s.Findings, []string{idA}) {
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
		{"two sessions in a row without a push", nil, func(t *testing.T, g digestRig) {
			g.session(t, 5, 10, 1, "no_push")
			g.session(t, 15, 20, 2, "no_push")
		}, []DigestFlag{{"session_no_push", "session 1"}}},
		{"a push between them", nil, func(t *testing.T, g digestRig) {
			g.session(t, 5, 10, 1, "no_push")
			g.session(t, 15, 20, 2, "pushed")
			g.session(t, 25, 30, 3, "no_push")
		}, nil},
		{"waiting on dependencies with its slot", func(c *Task) {
			c.Status, c.AdmittedAt = "waiting-deps", &digestStart
		}, nil, []DigestFlag{{"slot_held_by_wait", "waiting-deps"}}},
		{"waiting on dependencies before its admission", func(c *Task) { c.Status = "waiting-deps" }, nil, nil},
		{"at the merge approval", func(c *Task) { c.Status, c.Gate = "in-review", GateApproval }, nil,
			[]DigestFlag{{"slot_held_by_wait", "approval"}}},
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
		"  18 +0:19   criteria by factoryd   → C1 met, C2 undecided\n" +
		"  19 +0:20   criteria by sebastian  → C2 met\n" +
		"flags: none\n"
	if got := g.digest(t).Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}
