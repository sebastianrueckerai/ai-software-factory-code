package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// factsRig is a runner on a fake store, a fake object store and a Forgejo
// whose head h2 is green, at noon on 2026-11-03, under a policy where card
// t1, a large card of demo-app/demo-app, runs the direct track's coder
// class: 3 sessions, 10 minutes of agent time, task_max_eur 5, floors of
// 0.30 EUR cheap and 0.60 EUR frontier, and the default pool and card
// draw. Its last session ran on small, whose recorded opening is two
// calls at once (TestSessionFundedPricesASessionsOpening's s1): 44,342
// bytes on big and 7,031 on small, each asking 24,576 output tokens, held
// at 0.501666 + 0.105335 = 0.607001 EUR.
type factsRig struct {
	r     *Runner
	store *fakeStore
	tl    *Timeline
}

var factsNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// factsAt is the time hh:mm on the rig's day.
func factsAt(hh, mm int) time.Time { return time.Date(2026, 11, 3, hh, mm, 0, 0, time.UTC) }

func newFactsRig(t *testing.T) factsRig {
	t.Helper()
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/demo-app/demo-app/commits/h2/status" {
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"state":"success","statuses":[{"status":"success","context":"ci / test"}]}`)
	}))
	t.Cleanup(forge.Close)
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	v.Document.Budget.SessionFloorCheapEUR, v.Document.Budget.SessionFloorFrontierEUR = 0.3, 0.6
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	cfg.Prices = aliasPrices{"big": {In: 3e-6, CacheRead: 3e-7, Out: 1.5e-5, MaxOutput: 32768},
		"small": {In: 1e-6, CacheRead: 1e-7, Out: 4e-6, MaxOutput: 32768}}
	store, objects := newFakeStore(), &fakeObjectStore{}
	store.openings = map[string]policy.Opening{"small": {Sessions: [][]policy.OpeningCall{{
		{Ref: "big", Bytes: 44342, WantOut: 24576}, {Ref: "small", Bytes: 7031, WantOut: 24576}}}}}
	r := &Runner{store: store, objects: objects, cfg: cfg, now: func() time.Time { return factsNow },
		forgejo: &Forgejo{BaseURL: forge.URL, BotUser: "factory-agent", BotPassword: "pw"}}
	store.tasks["t1"] = Task{ID: "t1", Kind: "agent", Repository: "demo-app/demo-app", IssueNumber: 12, Status: "waiting-human",
		Gate: GateApproval, Track: policy.TrackDirect, Size: "large", PRNumber: 9, ModelAlias: "small",
		BudgetEUR: 5, SpentEUR: 4.5, HeldEUR: 0.25,
		Usage: TaskUsage{Attempts: 3, AgentTime: 8 * time.Minute, RaisedSessions: 1, RaisedTime: 5 * time.Minute}}
	return factsRig{r: r, store: store, tl: &Timeline{Objects: objects}}
}

func (g factsRig) event(task string, at time.Time, kind string, detail map[string]any) {
	g.store.events = append(g.store.events, Event{ID: int64(len(g.store.events) + 1), At: at, Repository: "demo-app/demo-app",
		TaskID: task, Kind: kind, Detail: detail})
}

// card writes t1's life: a draw at h1 with red checks, a push, h2 reviewed
// twice and approved the second time, green, a push of h3 the merge gate
// has not read whose review has concerns, and a fund at 10:04 on h2 with
// the findings idA and idB and no push after it. A draw of t1 last month and one of t2 this month count
// for the card and for the pool. t1 depends on a failed card already
// retried, one the policy forbade, one whose issue closed, a running and
// a done one. A case on h1 ran its checks again, one on h2 resumed.
func (g factsRig) card() {
	g.event("t1", time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC), EventCardFunded, map[string]any{"pool": true, "rung": 1, "amount_eur": 2.0})
	g.event("t2", factsAt(8, 0), EventCardFunded, map[string]any{"pool": true, "rung": 2, "amount_eur": 1.0})
	g.event("t1", factsAt(9, 0), EventCardExtended, map[string]any{"pool": true, "rung": 1, "sessions": 1, "agent_time_s": 300,
		"amount_eur": 0.0, "head": "h1", "checks": "red"})
	g.event("t1", factsAt(9, 10), EventGateHead, map[string]any{"head": "h1"})
	g.event("t1", factsAt(9, 20), EventSessionPushed, map[string]any{"session": "s2"})
	g.event("t1", factsAt(9, 30), EventModelReview, map[string]any{"commit": "h2", "round": 1, "reviewed": true, "verdict": "concerns"})
	g.event("t1", factsAt(9, 40), EventGateHead, map[string]any{"head": "h2"})
	g.event("t1", factsAt(9, 45), EventModelReview, map[string]any{"commit": "h2", "round": 2, "reviewed": true, "verdict": "approve"})
	g.event("t1", factsAt(9, 48), EventSessionPushed, map[string]any{"session": "s3"})
	g.event("t1", factsAt(9, 50), EventModelReview, map[string]any{"commit": "h3", "round": 1, "reviewed": true, "verdict": "concerns"})
	g.event("t1", factsAt(10, 4), EventCardFunded, map[string]any{"pool": true, "rung": 3, "amount_eur": 0.5, "head": "h2", "checks": "green",
		"findings": []any{idB, idA}})
	for id, d := range map[string]Task{
		"d1": {IssueNumber: 20, Status: "failed"}, "d2": {IssueNumber: 21, Status: "failed"},
		"d3": {IssueNumber: 22, Status: "running"}, "d4": {IssueNumber: 23, Status: "done"}, "d5": {IssueNumber: 24, Status: "closed"},
		RetryID("d1"): {IssueNumber: 30, Status: "queued"},
	} {
		d.ID, d.Repository = id, "demo-app/demo-app"
		g.store.tasks[id] = d
	}
	g.store.deps = map[string][]string{"t1": {"d1", "d2", "d3", "d4", "d5"}}
	g.event("d1", factsAt(8, 30), EventTaskEnded, map[string]any{"state": "failed", "reason": "session error: the pod is gone"})
	g.event("d1", factsAt(8, 31), EventQuestionWithdrawn, map[string]any{"reason": "moved on"})
	g.event("d2", factsAt(8, 40), EventTaskEnded, map[string]any{"state": "failed",
		"reason": "policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml"})
	g.event("d5", factsAt(8, 50), EventTaskEnded, map[string]any{"state": "closed", "reason": ReasonIssueClosed})
	g.store.cases = []Case{
		{ID: 1, TaskID: "t1", Kind: CaseSessionCap, Facts: map[string]any{"head": "h1"}, Resolution: &CaseResolution{Action: ActRerunChecks}},
		{ID: 2, TaskID: "t1", Kind: CaseSessionCap, Facts: map[string]any{"head": "h2"}, Resolution: &CaseResolution{Action: ActResume}},
	}
}

// timeline writes t1's checks, reviews and audit: of the findings on h2,
// the newest review's and the audit's are open.
func (g factsRig) timeline(t *testing.T) {
	t.Helper()
	for _, l := range []struct {
		at     time.Time
		step   string
		detail map[string]any
	}{
		{factsAt(9, 5), "checks", map[string]any{"commit": "h1", "state": "failure", "findings": []string{"check lint failed"}}},
		{factsAt(9, 30), "model_review", map[string]any{"commit": "h2", "verdict": "concerns", "findings": []string{"an older remark"}}},
		{factsAt(9, 45), "model_review", map[string]any{"commit": "h2", "verdict": "approve", "findings": []string{findingA}}},
		{factsAt(9, 46), "audit", map[string]any{"commit": "h2", "verdict": "findings", "findings": []map[string]any{{"item": findingB}}}},
	} {
		g.tl.now = func() time.Time { return l.at }
		if err := g.tl.Append(context.Background(), "t1", l.step, l.detail); err != nil {
			t.Fatalf("append %s: %v", l.step, err)
		}
	}
}

// The facts of a card read from its row, its events, its timeline, the
// checks of its head, its configuration, its dependencies, its cases and
// the draws, by hand:
//   - the checks ran again on h1, not on h2: until a case on h2 does
//   - sessions 3 + 1 raised = 4; agent time 10m, 15m for a large card, + 5m
//     raised = 1200 s, 480 s used; a session needs min(5m, 15m) = 300 s
//   - spent 4.50 + 0.25 held = 4.75, left 0.25; an opening on small needs
//     its recorded opening, 0.607001, above the cheap floor 0.30; the
//     ceiling task_max_eur 5; the gate reserve is the default 0.40
//   - the pool 30, 30h, 60 less November's draws: 1 session and 5 minutes
//     at 09:00, 0.50 at 10:04 and t2's 1.00 = 29, 29h55m, 58.50
//   - the card draw 2, 1h, 4 of a medium card is 3, 1h30m, 6 for a large
//     one, less t1's draws of rungs 0 and 1: 1 session, 5 minutes, and
//     October's 2.00 = 2, 1h25m, 4.00
//   - the findings' texts are the review's at 09:45 and the audit's at 09:46,
//     in that order
//   - the time draw is the newest pool row that raised agent time: the
//     09:00 extend of 300 s on h1, red, no findings recorded, with the
//     pushes of s2 and s3 after it
func TestTripFactsReadsTheCard(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	got, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.FloorEUR-0.607001) > 1e-9 {
		t.Errorf("floor = %v, want the opening's 0.607001 EUR", got.FloorEUR)
	}
	drawn := factsAt(10, 4)
	want := CaseFacts{Head: "h2", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Stage: GateApproval,
		Attempts: 3, SessionCap: 4, AgentTimeS: 480, AgentTimeCapS: 1200, SessionLeastS: 300, Model: "small",
		BudgetEUR: 5, SpentEUR: 4.75, LeftEUR: 0.25, ReserveEUR: 0.4, FloorEUR: got.FloorEUR, CeilingEUR: 5,
		Findings:     []string{idA, idB},
		FindingTexts: map[string]string{idA: findingA, idB: findingB}, FindingOrder: []string{idA, idB},
		LastDrawAt: &drawn, DrawHead: "h2", DrawChecks: "green", DrawFindings: []string{idA, idB}, Pushed: true,
		TimeDraw: &DrawFacts{At: factsAt(9, 0), Head: "h1", Checks: "red", Pushed: true},
		Dependencies: []DepFact{
			{TaskID: "d1", Issue: 20, Status: "failed", Why: "session error: the pod is gone", Replaced: RetryID("d1")},
			{TaskID: "d2", Issue: 21, Status: "failed", Why: "policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml"},
			{TaskID: "d3", Issue: 22, Status: "running"},
			{TaskID: "d5", Issue: 24, Status: "closed", Why: ReasonIssueClosed},
		},
		Autonomy:     policy.AutonomyFullAuto,
		PoolLeft:     policy.Draw{Sessions: 29, AgentTime: 29*time.Hour + 55*time.Minute, EUR: 58.5},
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: 85 * time.Minute, EUR: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("facts =\n%+v\nwant\n%+v", got, want)
	}
	if !boughtNothing(got) {
		t.Error("the fund at 10:04 with no push since, on h2 still green, bought nothing")
	}
	g.store.cases = append(g.store.cases, Case{ID: 3, TaskID: "t1", Kind: CaseSessionCap, Facts: map[string]any{"head": "h2"},
		Resolution: &CaseResolution{Action: ActRerunChecks}})
	if waited, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", ""); err != nil || waited.WaitedOnHead {
		t.Errorf("before a case waited on h2: waited on head %v (%v), want false", waited.WaitedOnHead, err)
	}
	g.store.cases = append(g.store.cases, Case{ID: 4, TaskID: "t1", Kind: CaseGateStop, Facts: map[string]any{"head": "h2", "gate": ActivityWaitChecks},
		Resolution: &CaseResolution{Action: ActWaitChecks}})
	if waited, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", ""); err != nil || !waited.WaitedOnHead {
		t.Errorf("after a case on h2 waited on its checks again: waited on head %v (%v), want true", waited.WaitedOnHead, err)
	}
	if again, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", ""); err != nil || !again.RerunOnHead {
		t.Errorf("after a case on h2 ran its checks again: rerun on head %v (%v), want true", again.RerunOnHead, err)
	}
}

// The criteria gate's run on the head is read with its review and its
// audit: its unmet criterion is one of the findings, its met one none.
func TestTripFactsReadsTheGatesUnmetCriteria(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	unmet := "TestDoorOpens in ./door failed"
	g.tl.now = func() time.Time { return factsAt(9, 47) }
	if err := g.tl.Append(context.Background(), "t1", "gate_criteria", map[string]any{"commit": "h2", "findings": []string{unmet},
		"proofs": []gateProof{
			{ID: "c1", Kind: CriterionTest, Outcome: proofFailed, Line: unmet},
			{ID: "c2", Kind: CriterionTest, Outcome: proofPassed, Met: true, Line: "TestDoorShuts in ./door passed"},
		}}); err != nil {
		t.Fatal(err)
	}
	got, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{idA, idB, findingID(unmet)}
	slices.Sort(want)
	if !slices.Equal(got.Findings, want) {
		t.Fatalf("findings = %v, want the review's, the audit's and the gate's unmet criterion %v", got.Findings, want)
	}
}

// A card with no timeline object has no findings, and no reader failed.
func TestTripFactsTakesAMissingTimelineAsNoFindings(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	got, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Partial != nil || got.Findings != nil || got.Head != "h2" || got.Checks != "green" || got.SessionCap != 4 || boughtNothing(got) {
		t.Fatalf("facts = %+v, want no partial, no findings, the rest read, and the draw's findings idA and idB gone", got)
	}
}

// A timeline S3 cannot read and an opening on a model nobody prices are
// named in Partial, with no findings and no floor; a context that ended
// fails the read.
func TestTripFactsNamesWhatItCouldNotRead(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	g.r.objects = unreadable{ObjectStore: g.r.objects, key: "sessions/t1/timeline.jsonl"}
	g.store.openings["small"] = policy.Opening{Sessions: [][]policy.OpeningCall{{{Ref: "unpriced", Bytes: 7031}}}}
	got, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Partial, []string{"timeline", "floor"}) || got.Findings != nil || got.FloorEUR != 0 || got.Head != "h2" || got.SessionCap != 4 {
		t.Fatalf("facts = %+v, want the timeline and the floor partial, no findings, no floor, the rest read", got)
	}
	if why := sessionRoom(CaseFacts{Attempts: 1, SessionCap: 4, AgentTimeCapS: 1200, Partial: got.Partial}); why != "what an opening needs could not be read: try again in a minute, or close the card" {
		t.Errorf("a resume on an unread floor = %q, want it refused", why)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.r.tripFacts(ctx, g.store.tasks["t1"], "", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("an ended context: err = %v, want context.Canceled", err)
	}
}

// The floor is the one of the model the run's next session opens on. With
// no opening recorded, small's is the cheap floor, 0.30, and big's the
// frontier floor, 0.60. Spent 4.10 of 5 with nothing held leaves 0.90,
// less the 0.40 reserve 0.50 for a session: enough on small, short on big,
// so a card whose next session climbs to big (a retry_stronger's) reads
// short, and its face asks the budget floor.
func TestTripFactsWeighTheFloorOfTheNextSessionsModel(t *testing.T) {
	g := newFactsRig(t)
	g.store.openings = nil
	card := g.store.tasks["t1"]
	card.SpentEUR, card.HeldEUR = 4.1, 0
	for model, want := range map[string]struct {
		floor float64
		short bool
	}{"": {0.3, false}, "factory/small": {0.3, false}, "factory/big": {0.6, true}} {
		f, err := g.r.tripFacts(context.Background(), card, "", model)
		if err != nil {
			t.Fatal(err)
		}
		if f.FloorEUR != want.floor || f.unfunded() != want.short || f.FloorFree {
			t.Errorf("next on %q: floor %v, short %v, free %v; want %v, %v", model, f.FloorEUR, f.unfunded(), f.FloorFree, want.floor, want.short)
		}
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var facts map[string]any
		if err := json.Unmarshal(b, &facts); err != nil {
			t.Fatal(err)
		}
		if face := tripNext(Case{Kind: CaseContentFilter, Facts: facts}, contentFilterRefusal); strings.Contains(face, "the budget floor next") != want.short {
			t.Errorf("next on %q: face %q", model, face)
		}
	}
}

// With no price source the run's floor funds every session, so the facts
// read it funded too, though the card's 0.25 left less the 0.40 reserve
// is under the cheap floor of 0.30 a priced floor would ask. A model the
// facts do not know is no such case: they weigh the policy's floor.
func TestTripFactsFundASessionNoPriceWeighs(t *testing.T) {
	g := newFactsRig(t)
	priced, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
	if err != nil || !priced.unfunded() {
		t.Fatalf("priced: facts %+v, %v; want short", priced, err)
	}
	// a model the facts do not know weighs the policy's cheap floor
	unknown := g.store.tasks["t1"]
	unknown.ModelAlias = ""
	if f, err := g.r.tripFacts(context.Background(), unknown, "", ""); err != nil || f.Model != "" || f.FloorFree || f.FloorEUR != 0.3 || !f.unfunded() {
		t.Fatalf("no model: facts %+v, %v; want the 0.30 floor weighed, short", f, err)
	}
	g.r.cfg.Prices = nil
	f, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !f.FloorFree || f.unfunded() || f.FloorEUR != 0.3 {
		t.Fatalf("unpriced: free %v, short %v, floor %v; want funded beside the policy's 0.30 floor", f.FloorFree, f.unfunded(), f.FloorEUR)
	}
	if why := sessionRoom(f); strings.Contains(why, "fund") {
		t.Errorf("a resume with no price source = %q, want no fund asked", why)
	}
}

// A plan card's agent time is its class's, unsized as the plan loop reads
// it, and its automatic second session is due while it has not run and
// half the class's time is at least what a session needs. The rig's class
// gives 10m: unsized 600 s plus the 300 s raised is 900 s (a large card's
// sized cap is 1200 s), a session needs min(5m, 10m) = 300 s, and half of
// 10m, 5m, is at least that.
func TestTripFactsReadAPlansAutomaticSession(t *testing.T) {
	g := newFactsRig(t)
	card := g.store.tasks["t1"]
	card.Track = policy.TrackPlan
	f, err := g.r.tripFacts(context.Background(), card, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.AgentTimeCapS != 900 || f.SessionLeastS != 300 || !f.Planner || !f.PlannerRetry {
		t.Errorf("cap %v, least %v, planner %v, retry %v; want 900, 300, a plan with its automatic session due", f.AgentTimeCapS, f.SessionLeastS, f.Planner, f.PlannerRetry)
	}
	card.Usage.RetryTime = 4 * time.Minute
	if f, err := g.r.tripFacts(context.Background(), card, "", ""); err != nil || f.PlannerRetry {
		t.Errorf("after the automatic session: retry %v, %v; want none due", f.PlannerRetry, err)
	}
}

// The run's floor weighs the day, month and project caps beside the card's
// budget, its calls in flight netted, and the tighter refuses. The rig's
// card has 0.50 less the 0.40 reserve, 0.10, for a session, under the
// 0.607001 its opening on small needs. With 0.05 EUR of the day free the day
// cap binds first: the facts hold its wait, which lifts at the UTC day's
// turn, 2026-11-04T00:00:00Z, and the face names it before the budget
// floor. With the day ample the budget binds and the face asks the floor.
func TestTripFactsWeighTheCapThatBindsFirst(t *testing.T) {
	g := newFactsRig(t)
	inf := math.Inf(1)
	for name, day := range map[string]float64{"day cap first": 0.05, "budget first": 100} {
		t.Run(name, func(t *testing.T) {
			g.store.capsFree = &policy.Free{Day: day, Month: inf, Project: inf}
			f, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"], "", "")
			if err != nil || !f.unfunded() {
				t.Fatalf("facts %+v, %v; want the budget short", f, err)
			}
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			var facts map[string]any
			if err := json.Unmarshal(b, &facts); err != nil {
				t.Fatal(err)
			}
			face := tripNext(Case{Kind: CaseContentFilter, Facts: facts}, contentFilterRefusal)
			if day == 100 {
				if f.CapWait != "" || !strings.Contains(face, "the task asks the budget floor next") {
					t.Fatalf("cap wait %q, face %q; want no cap and the budget floor", f.CapWait, face)
				}
				return
			}
			if !strings.HasPrefix(f.CapWait, dayCapResumes) || !strings.Contains(f.CapWait, "turns at 2026-11-04T00:00:00Z") || !strings.Contains(f.CapWait, "budget.day_eur in policy.yaml") {
				t.Fatalf("cap wait %q, want the day cap's, lifting at the day's turn", f.CapWait)
			}
			if !strings.HasPrefix(face, "The day cap is reached") || !strings.Contains(face, "Once it lifts, the task asks the budget floor") || strings.Contains(face, "Reply on this issue to start") {
				t.Fatalf("face %q, want the day cap first, then the budget floor", face)
			}
		})
	}
}

// An extend of lines alone and an extend of nothing draw nothing from the
// pool: their rows, written after the fund at 10:04 on h2, are
// no last draw, so the fund stays the last; a card with no other row has
// no last draw at all.
func TestEventFactsPassOverADrawOfNothing(t *testing.T) {
	row := func(at time.Time, kind string, detail map[string]any) Event {
		d := map[string]any{"pool": true, "head": "h2", "checks": "green", "sessions": 0.0, "agent_time_s": 0.0, "amount_eur": 0.0}
		maps.Copy(d, detail)
		return Event{At: at, Kind: kind, Detail: d}
	}
	lines := row(factsAt(10, 30), EventCardExtended, map[string]any{"lines": 40.0})
	nothing := row(factsAt(10, 20), EventCardExtended, nil)
	fund := row(factsAt(10, 4), EventCardFunded, map[string]any{"amount_eur": 0.5})
	var f CaseFacts
	f.eventFacts([]Event{lines, nothing, fund})
	if f.LastDrawAt == nil || !f.LastDrawAt.Equal(factsAt(10, 4)) {
		t.Fatalf("last draw at %v, want the fund's 10:04", f.LastDrawAt)
	}
	var none CaseFacts
	if none.eventFacts([]Event{lines, nothing}); none.LastDrawAt != nil {
		t.Fatalf("last draw at %v, want none", none.LastDrawAt)
	}
}
