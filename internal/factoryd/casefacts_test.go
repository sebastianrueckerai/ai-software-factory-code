package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// factsRig is a runner on a fake store, a fake object store and a Forgejo
// whose head h2 is green, at noon on 2026-11-03, under a policy where card
// t1, a large card of demo-app/demo-app, runs the direct track's coder
// class: 3 sessions, 10 minutes of agent time, task_max_eur 5, floors of
// 0.30 EUR cheap and 0.60 EUR frontier, and the default pool and card
// draw.
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
	store, objects := newFakeStore(), &fakeObjectStore{}
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
// has not read whose review has concerns, and a fund at 10:04 with no
// push after it. A draw of t1 last month and one of t2 this month count
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
	g.event("t1", factsAt(10, 4), EventCardFunded, map[string]any{"pool": true, "rung": 3, "amount_eur": 0.5, "head": "h2", "checks": "green"})
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
//     raised = 1200 s, 480 s used
//   - spent 4.50 + 0.25 held = 4.75, left 0.25; the floor of small, the
//     cheap rung, 0.30; the ceiling task_max_eur 5
//   - the pool 30, 30h, 60 less November's draws: 1 session and 5 minutes
//     at 09:00, 0.50 at 10:04 and t2's 1.00 = 29, 29h55m, 58.50
//   - the card draw 2, 1h, 4 of a medium card is 3, 1h30m, 6 for a large
//     one, less t1's draws of rungs 0 and 1: 1 session, 5 minutes, and
//     October's 2.00 = 2, 1h25m, 4.00
func TestTripFactsReadsTheCard(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	got, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"])
	if err != nil {
		t.Fatal(err)
	}
	drawn := factsAt(10, 4)
	want := CaseFacts{Head: "h2", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Stage: GateApproval,
		Attempts: 3, SessionCap: 4, AgentTimeS: 480, AgentTimeCapS: 1200, Model: "small",
		BudgetEUR: 5, SpentEUR: 4.75, LeftEUR: 0.25, FloorEUR: 0.3, CeilingEUR: 5,
		Findings:   []string{idA, idB},
		LastDrawAt: &drawn, DrawHead: "h2", DrawChecks: "green",
		Dependencies: []DepFact{
			{TaskID: "d1", Issue: 20, Status: "failed", Why: "session error: the pod is gone", Replaced: RetryID("d1")},
			{TaskID: "d2", Issue: 21, Status: "failed", Why: "policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml"},
			{TaskID: "d3", Issue: 22, Status: "running"},
			{TaskID: "d5", Issue: 24, Status: "closed", Why: ReasonIssueClosed},
		},
		Autonomy:     policy.AutonomyManual,
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
	if again, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"]); err != nil || !again.RerunOnHead {
		t.Errorf("after a case on h2 ran its checks again: rerun on head %v (%v), want true", again.RerunOnHead, err)
	}
}

// A card with no timeline object reads every other fact and names the
// timeline as what it could not read.
func TestTripFactsNamesWhatItCouldNotRead(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	got, err := g.r.tripFacts(context.Background(), g.store.tasks["t1"])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Partial, []string{"timeline"}) || got.Findings != nil || got.Head != "h2" || got.Checks != "green" || got.SessionCap != 4 {
		t.Fatalf("facts = %+v, want the timeline partial, no findings, the rest read", got)
	}
}
