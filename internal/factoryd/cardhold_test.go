package factoryd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// holdFixture is boundedFixture at bound 3 with plan #1915 (t-pl) and its
// card #2062 (t-r, running), the release card, and card #1960 (t-held,
// the oldest queued) held on it unless release is "".
func holdFixture(t *testing.T, release string, more ...Task) (*fakeStore, *recordingWorkflows, BoundedWorkflows) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	tasks := append([]Task{
		{ID: "t-pl", Repository: "demo/app", Kind: "agent", Track: policy.TrackPlan, Status: "done", IssueNumber: 1915},
		{ID: "t-r", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "running", IssueNumber: 2062, Source: SourcePlan, Parent: "t-pl", PlanKey: "pg-split-prove-3"},
		{ID: "t-held", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "queued", IssueNumber: 1960, CreatedAt: base,
			Hold: &CardHold{Reason: "needs the finished pg split", Release: release, By: "sebastian", At: base}},
	}, more...)
	return boundedFixture(t, 3, tasks...)
}

// A held card is not admitted while the queued cards filed after it are:
// the pull passes it, and admit records its hold's line as its reason.
func TestAHeldCardIsNotAdmittedWhileOtherQueuedCardsAre(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	store, rec, bounded := holdFixture(t, "",
		Task{ID: "t-q2", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "queued", IssueNumber: 2, CreatedAt: base},
		Task{ID: "t-q3", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "queued", IssueNumber: 3, CreatedAt: base.Add(time.Minute)})
	pullQueued(ctx, store, bounded, "demo/app")
	if !slices.Equal(rec.starts, []string{"t-q2", "t-q3"}) {
		t.Fatalf("starts = %v, want the two cards after the held one", rec.starts)
	}
	if h, _, _ := store.Task(ctx, "t-held"); h.AdmittedAt != nil || h.QueueReason != "on hold: needs the finished pg split" {
		t.Fatalf("the held card: admitted at %v, reason %q; want it unadmitted, its hold the reason", h.AdmittedAt, h.QueueReason)
	}
}

// staleHold is a store whose read of card stale answers it as it was
// before its hold landed: admit read the card, then the hold came.
type staleHold struct {
	*fakeStore
	stale Task
}

func (s staleHold) Task(ctx context.Context, id string) (Task, bool, error) {
	if id == s.stale.ID {
		return s.stale, true, nil
	}
	return s.fakeStore.Task(ctx, id)
}

// A hold that lands after admit read the card keeps it: the stamp refuses
// a held card, and nothing starts.
func TestAHoldThatLandsAfterAdmitReadTheCardKeepsIt(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := holdFixture(t, "")
	stale := store.tasks["t-held"]
	stale.Hold = nil
	bounded.Store = staleHold{store, stale}
	if err := bounded.Start(ctx, "t-held"); err != nil || len(rec.starts) != 0 {
		t.Fatalf("Start = %v, starts %v; want nothing started", err, rec.starts)
	}
	if h := store.tasks["t-held"]; h.AdmittedAt != nil || h.Hold == nil {
		t.Fatalf("admitted at %v, hold %+v; want the hold standing and no stamp", h.AdmittedAt, h.Hold)
	}
}

// The release card #2062 of plan #1915 merges: the next pull ends the hold
// as the release card's, with factoryd's row naming it, and starts #1960.
// Until then the card waits with the line that names its release card.
func TestTheReleaseCardsMergeEndsTheHoldAndStartsTheCard(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := holdFixture(t, "t-r")
	pullQueued(ctx, store, bounded, "demo/app")
	want := "on hold until #2062 (plan #1915) is merged or done: needs the finished pg split"
	if h := store.tasks["t-held"]; len(rec.starts) != 0 || h.QueueReason != want {
		t.Fatalf("starts %v, reason %q; want none and %q", rec.starts, h.QueueReason, want)
	}
	r := store.tasks["t-r"]
	r.Status = "merged"
	store.tasks["t-r"] = r
	pullQueued(ctx, store, bounded, "demo/app")
	if h := store.tasks["t-held"]; !slices.Equal(rec.starts, []string{"t-held"}) || h.Hold != nil || h.AdmittedAt == nil {
		t.Fatalf("starts %v, hold %+v, admitted %v; want #1960 started with no hold", rec.starts, h.Hold, h.AdmittedAt)
	}
	if len(store.events) != 1 {
		t.Fatalf("events = %+v, want the hold's end alone", store.events)
	}
	e := store.events[0]
	if e.Kind != EventCardHoldEnded || e.TaskID != "t-held" || e.IssueNumber != 1960 || e.Actor != FactoryDecider ||
		e.Detail["release"] != int64(2062) || e.Detail["status"] != "merged" {
		t.Fatalf("the row = %+v, want card_hold_ended of #1960 by factoryd, release 2062 merged", e)
	}
}

// A release card that ends closed or failed releases nothing: the card
// stays held, the line warns naming #2062, plan #1915, how it ended and
// what a human does, a retry of a failed #2062 first, whose retry takes
// the hold over, and no case opens and no row is written.
func TestAReleaseCardThatEndsClosedOrFailedKeepsTheHold(t *testing.T) {
	for end, fit := range map[string]string{
		"closed": "releases the hold or holds the card on another card",
		"failed": "retries #2062, whose retry takes the hold over, releases the hold or holds the card on another card",
	} {
		t.Run(end, func(t *testing.T) {
			ctx := context.Background()
			store, rec, bounded := holdFixture(t, "t-r")
			r := store.tasks["t-r"]
			r.Status = end
			store.tasks["t-r"] = r
			pullQueued(ctx, store, bounded, "demo/app")
			pullQueued(ctx, store, bounded, "demo/app")
			want := "on hold: its release card #2062 (plan #1915) ended " + end + " and releases nothing, so a human " + fit + ": needs the finished pg split"
			if h := store.tasks["t-held"]; len(rec.starts) != 0 || h.Hold == nil || h.QueueReason != want {
				t.Fatalf("starts %v, hold %+v, reason %q; want none, the hold, and %q", rec.starts, h.Hold, h.QueueReason, want)
			}
			board, err := store.BoardTasks(ctx, "demo/app")
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(board, func(b BoardTask) bool { return b.ID == "t-held" })
			if v := board[i].HoldView; v.Released || v.Line != want {
				t.Fatalf("the board's hold = %+v, want the warning", v)
			}
			if len(store.cases) != 0 || len(store.events) != 0 {
				t.Fatalf("cases %+v, events %+v; want none", store.cases, store.events)
			}
		})
	}
}

// A release card that a cut turned into a plan releases nothing when the
// plan ends done, which it does once it files its cards: the card stays
// held, and the line warns naming #2063.
func TestAReleaseCardThatBecameAPlanKeepsTheHold(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := holdFixture(t, "t-cut", Task{ID: "t-cut", Repository: "demo/app", Kind: "agent", Track: policy.TrackPlan, Status: "done", IssueNumber: 2063})
	pullQueued(ctx, store, bounded, "demo/app")
	want := "on hold: its release card #2063 became a plan, which is done once it files its cards, before their work lands, " +
		"so a human releases the hold or holds the card on the plan's last card: needs the finished pg split"
	if h := store.tasks["t-held"]; len(rec.starts) != 0 || !h.Held() || h.QueueReason != want {
		t.Fatalf("starts %v, hold %+v, reason %q; want none, the hold, and %q", rec.starts, h.Hold, h.QueueReason, want)
	}
	board, err := store.BoardTasks(ctx, "demo/app")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(board, func(b BoardTask) bool { return b.ID == "t-held" })
	if v := board[i].HoldView; v.Released || v.Line != want {
		t.Fatalf("the board's hold = %+v, want the warning", v)
	}
}

// A failed release card counts through its successor: the hold follows
// #2062's work to #2070, which carries the plan, and #2070's end done
// releases #1960.
func TestAFailedReleaseCardsSuccessorReleasesTheHold(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := holdFixture(t, "t-r")
	r := store.tasks["t-r"]
	r.Status = "failed"
	store.tasks["t-r"] = r
	succ := Task{ID: "t-s", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "queued", IssueNumber: 2070, Source: SourceSuccessor,
		Parent: "t-pl", PlanKey: "pg-split-prove-3"}
	if err := store.CreateSuccessor(ctx, "t-r", succ); err != nil {
		t.Fatal(err)
	}
	if h := store.tasks["t-held"].Hold; h == nil || h.Release != "t-s" {
		t.Fatalf("the hold = %+v, want its release card moved to the successor", h)
	}
	s := store.tasks["t-s"]
	s.Status = "running"
	store.tasks["t-s"] = s
	pullQueued(ctx, store, bounded, "demo/app")
	if want := "on hold until #2070 (plan #1915) is merged or done: needs the finished pg split"; store.tasks["t-held"].QueueReason != want || len(rec.starts) != 0 {
		t.Fatalf("reason %q, starts %v; want %q and none", store.tasks["t-held"].QueueReason, rec.starts, want)
	}
	s.Status = "done"
	store.tasks["t-s"] = s
	pullQueued(ctx, store, bounded, "demo/app")
	if !slices.Equal(rec.starts, []string{"t-held"}) || store.tasks["t-held"].Hold != nil {
		t.Fatalf("starts %v, hold %+v; want #1960 started, its hold ended", rec.starts, store.tasks["t-held"].Hold)
	}
}

// A hold with no release card ends only by a human's release: the pulls
// pass it however often they run, and the release ends it and starts it.
func TestAHoldWithNoReleaseCardEndsOnlyByAHumansRelease(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := holdFixture(t, "")
	r := &Runner{store: store}
	r.cfg.Workflows = bounded
	for range 3 {
		pullQueued(ctx, store, bounded, "demo/app")
	}
	if len(rec.starts) != 0 || store.tasks["t-held"].Hold == nil {
		t.Fatalf("starts %v, hold %+v; want the hold standing", rec.starts, store.tasks["t-held"].Hold)
	}
	if ok, err := r.ReleaseCardHold(ctx, "t-held"); err != nil || !ok {
		t.Fatalf("ReleaseCardHold = %v, %v", ok, err)
	}
	if !slices.Equal(rec.starts, []string{"t-held"}) || store.tasks["t-held"].Hold != nil {
		t.Fatalf("starts %v, hold %+v; want the release to start #1960", rec.starts, store.tasks["t-held"].Hold)
	}
	if ok, err := r.ReleaseCardHold(ctx, "t-held"); err != nil || ok {
		t.Fatalf("a second release = %v, %v; want false: no hold stands", ok, err)
	}
}

// A hold lands on a card that has not started, and only there: queued and
// never admitted, or in waiting-deps on a dependency that has not ended.
func TestSetCardHoldTakesOnlyACardThatHasNotStarted(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := newFakeStore()
	for _, k := range []Task{
		{ID: "t-q", Status: "queued", IssueNumber: 1},
		{ID: "t-adm", Status: "queued", IssueNumber: 2, AdmittedAt: &now},
		{ID: "t-res", Status: "queued", IssueNumber: 3, ResumedAt: &now},
		{ID: "t-run", Status: "running", IssueNumber: 4},
		{ID: "t-roll", Status: "queued", IssueNumber: 5, Source: SourceRoll},
		{ID: "t-noissue", Status: "queued"},
		{ID: "t-wd", Status: "waiting-deps", IssueNumber: 6},
		{ID: "t-wd-ended", Status: "waiting-deps", IssueNumber: 7},
		{ID: "t-open", Status: "running", IssueNumber: 8},
		{ID: "t-gone", Status: "closed", IssueNumber: 9},
	} {
		k.Kind, k.Repository, k.Track = "agent", "demo/app", "direct"
		store.tasks[k.ID] = k
	}
	store.AddTaskDeps(ctx, "t-wd", []string{"t-gone", "t-open"})
	store.AddTaskDeps(ctx, "t-wd-ended", []string{"t-gone"})
	var took []string
	for _, id := range []string{"t-q", "t-adm", "t-res", "t-run", "t-roll", "t-noissue", "t-wd", "t-wd-ended"} {
		if ok, err := store.SetCardHold(ctx, id, CardHold{Reason: "x", By: "sebastian"}); err != nil {
			t.Fatal(err)
		} else if ok {
			took = append(took, id)
		}
	}
	if !slices.Equal(took, []string{"t-q", "t-noissue", "t-wd"}) {
		t.Fatalf("held = %v, want the never-admitted queued cards, a half-filed one among them, and the one waiting on a live dependency", took)
	}
}

// The dependency wait of a held card: it waits however its dependencies
// stand, with no question when one ended closed, until the human's
// release; then its dependencies decide again. A hold whose release card
// merged ends at the check.
func TestDependenciesKeepAHeldCardWaiting(t *testing.T) {
	ctx := context.Background()
	card := Task{ID: "t-c", Kind: "agent", Status: "waiting-deps", Gate: GateDependencies, Track: "direct", Process: "spec", Repository: "sf/repo", IssueNumber: 7,
		Hold: &CardHold{Reason: "after the split", By: "sebastian"}}
	fj := &prForgejo{labels: map[int64][]string{7: {"agent", "state/waiting-deps"}}}
	r, store, _ := newActivityRunner(t, card, fj)
	store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "running", Track: "direct", Repository: "sf/repo", IssueNumber: 3})
	store.AddTaskDeps(ctx, "t-c", []string{"t-a"})
	ask := func() DepState {
		t.Helper()
		v, err := execActivity(t, r, ActivityDependencies, "t-c")
		if err != nil {
			t.Fatal(err)
		}
		var d DepState
		if err := v.Get(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := ask(); d.Hold != "on hold: after the split" || !slices.Equal(d.Waiting, []int64{3}) || d.Ended != nil {
		t.Fatalf("with #3 open = %+v, want the hold and #3", d)
	}
	a := store.tasks["t-a"]
	a.Status = "closed"
	store.tasks["t-a"] = a
	if d := ask(); d.Hold == "" || d.Ended != nil || d.Failed != nil {
		t.Fatalf("with #3 closed = %+v, want the hold and no question", d)
	}
	a.Status = "done"
	store.tasks["t-a"] = a
	if d := ask(); d.Hold != "on hold: after the split" || len(d.Waiting) != 0 {
		t.Fatalf("with #3 done = %+v, want the hold alone", d)
	}
	if ok, _ := store.EndCardHold(ctx, "t-c", ""); !ok {
		t.Fatal("the human's release ended nothing")
	}
	if d := ask(); d.Hold != "" || len(d.Waiting) != 0 || d.Ended != nil {
		t.Fatalf("after the release = %+v, want the card free to start", d)
	}

	merged := time.Now()
	store.CreateTask(ctx, Task{ID: "t-rel", Kind: "agent", Status: "merged", Track: "direct", Repository: "sf/repo", IssueNumber: 9, FinishedAt: &merged})
	c := store.tasks["t-c"]
	c.Hold = &CardHold{Reason: "after #9", Release: "t-rel", By: "sebastian"}
	store.tasks["t-c"] = c
	if d := ask(); d.Hold != "" || store.tasks["t-c"].Hold != nil {
		t.Fatalf("with #9 merged = %+v, hold %+v; want the hold ended and the card free", d, store.tasks["t-c"].Hold)
	}
}

// The workflow keeps a held card in the wait though no dependency is open,
// asking again every 10 minutes, and starts it once the hold ends. A run
// from before the change starts it at once.
func TestTheDependencyWaitHoldsAHeldCard(t *testing.T) {
	run := func(t *testing.T, old bool) []time.Duration {
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		if old {
			h.env.OnGetVersion("card-hold", workflow.DefaultVersion, versionCardHold).Return(workflow.DefaultVersion)
		}
		start := h.env.Now()
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
			if h.env.Now().Sub(start) < 15*time.Minute {
				return DepState{Hold: "on hold: after the split"}, nil
			}
			return DepState{}, nil
		})
		var began []time.Duration
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(func(ctx context.Context, in SessionInput) (SessionResult, error) {
			began = append(began, h.env.Now().Sub(start))
			h.sessions = append(h.sessions, in)
			return pushed("s1", "c1"), nil
		})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		h.run(t)
		return began
	}
	if began := run(t, false); !slices.Equal(began, []time.Duration{20 * time.Minute}) {
		t.Fatalf("sessions began at %v, want one at 20m, the first check after the hold ended", began)
	}
	if began := run(t, true); !slices.Equal(began, []time.Duration{0}) {
		t.Fatalf("a run from before began at %v, want at once", began)
	}
}

// The heal pass wakes no held card in its dependency wait, though its
// dependencies are done, and wakes one whose release card merged.
func TestHealWakesNoHeldCard(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	ended := time.Now()
	for id, st := range map[string]string{"t-done": "done", "t-rel": "merged", "t-open": "running"} {
		store.CreateTask(ctx, Task{ID: id, Status: st, Kind: "agent", Repository: "sf/ai-software-factory-code", Track: "direct", IssueNumber: 1, FinishedAt: &ended})
	}
	for id, release := range map[string]string{"tw-held": "", "tw-held-open": "t-open", "tw-released": "t-rel"} {
		store.CreateTask(ctx, Task{ID: id, Status: "waiting-deps", Kind: "agent", Repository: "sf/ai-software-factory-code", Track: "direct",
			Hold: &CardHold{Reason: "x", Release: release}})
		store.AddTaskDeps(ctx, id, []string{"t-done"})
	}
	wf := &fakeWorkflows{}
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	HealPass(ctx, store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent"}, nil, noneCompleted, nil)
	var dep []string
	for _, s := range wf.signals {
		if strings.Contains(s, " "+SignalDepEnded+" ") {
			dep = append(dep, strings.Fields(s)[0])
		}
	}
	if !slices.Equal(dep, []string{"tw-released"}) {
		t.Fatalf("dep_ended to %v, want tw-released alone", dep)
	}
}

// A queued card held past every bound owns no wait: no queued_over_bound
// or wait_over_bound flag, so the monitor opens no case a rung could
// answer with a start or a close. Its head names the hold, and its steps
// the human's hold.
func TestAHeldCardOwnsNoWait(t *testing.T) {
	since := digestAt(60)
	hold := &CardHold{Reason: "after the split", By: "sebastian", At: since}
	for _, status := range []string{"queued", "waiting-deps"} {
		g := newDigestRig(func(c *Task) {
			c.Status, c.QueueReason, c.QueueSince, c.Hold = status, "on hold: after the split", &since, hold
		})
		g.event(60, EventCardHeld, "sebastian", map[string]any{"reason": "after the split", "release": 2062})
		g.at(100 * 60)
		d := g.digest(t)
		if r := d.Render(); !strings.Contains(r, "held by sebastian") || !strings.Contains(r, "→ until #2062 is merged or done: after the split\n") {
			t.Fatalf("%s: the steps =\n%s\nwant the hold's step", status, d.Render())
		}
		if len(d.Flags) != 0 {
			t.Fatalf("%s: flags = %v, want none", status, d.Flags)
		}
		if head, _, _ := strings.Cut(d.Render(), "\n"); !strings.HasSuffix(head, " · on hold: after the split") {
			t.Fatalf("%s: the head = %q, want it to name the hold", status, head)
		}
	}
}

// A hold that its card's close ended reads in the card's digest as the
// card's move, not as a release.
func TestTheDigestNamesAHoldsEndAtTheCardsClose(t *testing.T) {
	g := newDigestRig(func(c *Task) { c.Status = "queued" })
	g.event(60, EventCardHoldEnded, FactoryDecider, map[string]any{"reason": "after the split", "card_status": "closed"})
	g.at(120)
	if r := g.digest(t).Render(); !strings.Contains(r, "hold ended by factoryd → the card moved to closed\n") {
		t.Fatalf("the steps =\n%s\nwant the hold's end at the close", r)
	}
}

// Rungs 0 to 2 neither start nor close a held card: a resume of rung 0, a
// handover of rung 1, and a close and a replan of rung 2 on its case are
// refused naming the hold, what fits the rung on the monitor's case (its
// settle, rung 2's note, the hand-up) and the actions it may take, and so
// is rung 2's close with no case. A restart_plan of rung 2, on the held
// card's case or on its done sibling's, would close the plan and file its
// work again: it is refused naming the held card. Rung 2's note, its room
// for the card and rung 0's retry of the failed dependency start and close
// nothing: they are taken, and the card stays held in waiting-deps.
func TestARungsAnswerOnAHeldCardIsRefusedNamingTheHold(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := newRetryRig(t)
	readable(store, "t-b")
	store.CreateTask(ctx, Task{ID: "t-pl2", Kind: "agent", Status: "done", Track: policy.TrackPlan, Repository: "linkshort/linkshort", IssueNumber: 30})
	store.CreateTask(ctx, Task{ID: "t-rel", Kind: "agent", Status: "running", Track: policy.TrackDirect, Repository: "linkshort/linkshort",
		IssueNumber: 31, Source: SourcePlan, Parent: "t-pl2"})
	b := store.tasks["t-b"]
	hold := CardHold{Reason: "waits for the pg split", Release: "t-rel", By: "sebastian"}
	b.Hold = &hold
	store.tasks["t-b"] = b
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseMonitorFlag, Status: CaseOpen}, {ID: 5, TaskID: "t-f", Kind: CaseMonitorFlag, Status: CaseOpen}}
	if _, err := store.AppendCaseStep(ctx, CaseStep{CaseID: 3, Rung: 1, Actor: CheckinDecider}); err != nil {
		t.Fatal(err)
	}
	const held = "card #6 is on hold until #31 (plan #30) is merged or done: waits for the pg split. Only a human releases a held card, starts it or closes it, so "
	const rung0, rung1 = ": settle it or hand it up to the human, and the factory may take `retry`, `settle` and `withdraw`", ": settle it or hand it up to the human, and the check-in may take `retry`, `settle` and `withdraw`"
	const rung2 = ": note the ask, settle it or hand it up to the human, and the factory agent may take `retry`, `settle` and `note`"
	for _, c := range []struct {
		in   ActInput
		want string
	}{
		{ActInput{TaskID: "t-b", CaseID: 3, By: FactoryDecider, Rung: 0, Res: Resolution{Action: ActResume}}, held + "the factory takes no `resume` on it" + rung0},
		{ActInput{TaskID: "t-b", CaseID: 3, By: CheckinDecider, Rung: 1, Res: Resolution{Action: ActHandover, Args: map[string]any{"model": "stronger"}}}, held + "the check-in takes no `handover` on it" + rung1},
		{ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Rung: 2, Res: Resolution{Action: ActClose, Note: "the human closed its dependency"}}, held + "the factory agent takes no `close` on it" + rung2},
		{ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Rung: 2, Res: Resolution{Action: ActReplan, Note: "the plan is wrong"}}, held + "the factory agent takes no `replan` on it" + rung2},
		{ActInput{TaskID: "t-b", By: "sebastian", Rung: 2, Res: Resolution{Action: ActClose, Note: "the human closed its dependency"}},
			held + "the factory agent takes no `close` on it: ask the human instead, and the factory agent may take `extend`, `retry`, `set_dependency` and `remove_dependency`"},
		{ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Rung: 2, Res: Resolution{Action: ActRestartPlan, Note: "one card"}}, held + "the factory agent takes no `restart_plan` on its plan" + rung2},
		{ActInput{TaskID: "t-f", CaseID: 5, By: "sebastian", Rung: 2, Res: Resolution{Action: ActRestartPlan, Note: "one card"}},
			held + "the factory agent takes no `restart_plan` on its plan: note the ask, settle it or hand it up to the human, and the factory agent may take `close`, `settle` and `note`"},
	} {
		var refused ErrActRefused
		if _, err := r.Act(ctx, c.in); !errors.As(err, &refused) || err.Error() != c.want {
			t.Errorf("rung %d's %s on %s's case %d = %v\nwant %q", c.in.Rung, c.in.Res.Action, c.in.TaskID, c.in.CaseID, err, c.want)
		}
	}
	if store.cases[0].Status != CaseOpen || store.cases[1].Status != CaseOpen || store.tasks["t-b"].Status != "waiting-deps" || store.tasks["t-plan"].Status == "closed" {
		t.Fatalf("cases %+v, card %s, plan %s; want all as they were", store.cases, store.tasks["t-b"].Status, store.tasks["t-plan"].Status)
	}
	if _, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Rung: 2, Res: Resolution{Action: ActNote, Note: "the human holds it"}}); err != nil {
		t.Fatalf("rung 2's note = %v, want it taken", err)
	}
	if _, err := r.Act(ctx, ActInput{TaskID: "t-b", By: "sebastian", Rung: 2, Res: extendBy(map[string]any{"sessions": 1})}); err != nil {
		t.Fatalf("rung 2's room = %v, want it taken", err)
	}
	store.cases = append(store.cases, Case{ID: 4, TaskID: "t-b", Kind: CaseMonitorFlag, Status: CaseOpen})
	if _, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 4, By: FactoryDecider, Rung: 0, Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}}); err != nil {
		t.Fatalf("rung 0's retry of the dependency = %v, want it taken", err)
	}
	if b := store.tasks["t-b"]; b.Status != "waiting-deps" || b.Hold == nil || *b.Hold != hold {
		t.Fatalf("card #6 is %s with hold %+v after the answers, want waiting-deps with %+v", b.Status, b.Hold, hold)
	}
}

// releaseUnread is a store whose read of card id fails.
type releaseUnread struct {
	*fakeStore
	id string
}

func (s releaseUnread) Task(ctx context.Context, id string) (Task, bool, error) {
	if id == s.id {
		return Task{}, false, errors.New("the database is down")
	}
	return s.fakeStore.Task(ctx, id)
}

// The hold is a fact of the case. On held card #6's dependency case rungs 0
// to 2 are offered no resume, continue, handover, successor, close or replan
// of the card, nor rung 2 a restart of its plan, on its own case or on its
// done sibling #4's: each is listed as not offered with the hold's words and
// the actions that fit the rung there, rung 2's note among them. The human
// is offered the close. A hold whose release card cannot be read refuses
// the same answers naming the read.
func TestAHeldCardsCaseOffersTheRungsNoStartOrClose(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := newRetryRig(t)
	b := store.tasks["t-b"]
	b.Hold = &CardHold{Reason: "waits for the pg split", By: "sebastian"}
	store.tasks["t-b"] = b
	setStatus(store, "t-plan", "done")
	dep := Case{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, StateKey: "t-a", Status: CaseOpen}
	f, err := r.FactsAtCase(ctx, b, dep)
	if err != nil {
		t.Fatal(err)
	}
	const held = "card #6 is on hold: waits for the pg split. Only a human releases a held card, starts it or closes it, so "
	fit := map[int]string{0: ": hand it up to the human", 1: ": hand it up to the human", 2: ": note the ask or hand it up to the human"}
	for _, name := range []string{ActResume, ActContinue, ActHandover, ActSuccessor, ActClose, ActReplan} {
		a, _ := ActionByName(name)
		for rung := 0; rung <= 2; rung++ {
			if !a.Rungs[rung] {
				continue
			}
			if got, want := a.offerRefusal(f, rung), held+who(rung)+" takes no `"+name+"` on it"+fit[rung]; got != want {
				t.Errorf("rung %d's %s = %q\nwant %q", rung, name, got, want)
			}
		}
	}
	statement := new(Runner).rungStatement(dep, f, "", nil, 2)
	if line := "- close: not offered: " + held + "the factory agent takes no `close` on it" + fit[2] + "\n"; !strings.Contains(statement, line) {
		t.Errorf("rung 2's statement lacks %q:\n%s", line, statement)
	}
	if !slices.Contains(offeredNames(f, 3), ActClose) {
		t.Errorf("the human is offered %v, want the close among them", offeredNames(f, 3))
	}
	restart, _ := ActionByName(ActRestartPlan)
	sibling, err := r.FactsAtCase(ctx, store.tasks["t-f"], Case{ID: 5, TaskID: "t-f", Kind: CaseAgentEscalate, Status: CaseOpen})
	if err != nil {
		t.Fatal(err)
	}
	for _, facts := range []CaseFacts{f, sibling} {
		if got, want := restart.offerRefusal(facts, 2), held+"the factory agent takes no `restart_plan` on its plan"+fit[2]; got != want {
			t.Errorf("rung 2's restart_plan = %q\nwant %q", got, want)
		}
	}

	b.Hold.Release = "t-f"
	store.tasks["t-b"] = b
	r.store = releaseUnread{store, "t-f"}
	unread, err := r.FactsAtCase(ctx, b, dep)
	if err != nil {
		t.Fatal(err)
	}
	closeAct, _ := ActionByName(ActClose)
	if got, want := closeAct.offerRefusal(unread, 2), "the card's hold could not be read, so the factory agent takes no `close` on it: try again in a minute, or hand it up to the human"; got != want {
		t.Errorf("rung 2's close on an unread hold = %q\nwant %q", got, want)
	}
}

// A plan's held card counts through the successor it was cut into: #5 was
// cut into #9, which a human holds, so rung 2's restart_plan, which would
// close the plan and file its work again, is refused naming #9 on the case
// of the plan's done card #4, and Act runs nothing.
func TestARestartIsRefusedOverAHeldSuccessor(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := newRetryRig(t)
	setStatus(store, "t-plan", "done")
	setStatus(store, "t-a", "closed")
	succ := Task{ID: SuccessorID("t-a"), Kind: "agent", Status: "queued", Track: policy.TrackDirect, Repository: "linkshort/linkshort", IssueNumber: 9,
		Source: SourceSuccessor, Parent: "t-plan", PlanKey: "links-api", Hold: &CardHold{Reason: "after the pg split", By: "sebastian"}}
	if err := store.CreateTask(ctx, succ); err != nil {
		t.Fatal(err)
	}
	store.tasks[succ.ID] = succ
	store.cases = []Case{{ID: 5, TaskID: "t-f", Kind: CaseAgentEscalate, Status: CaseAtHuman}}
	_, err := r.Act(ctx, ActInput{TaskID: "t-f", CaseID: 5, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActRestartPlan, Note: "one card"}})
	const want = "card #9 is on hold: after the pg split. Only a human releases a held card, starts it or closes it, so the factory agent takes no `restart_plan` on its plan: note the ask or hand it up to the human"
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want+", and the factory agent may take ") {
		t.Fatalf("rung 2's restart_plan = %v\nwant the refusal %q and the actions rung 2 may take", err, want)
	}
	if store.cases[0].Status != CaseAtHuman || store.tasks["t-plan"].Status != "done" || !store.tasks[succ.ID].Held() {
		t.Fatalf("case %s, plan %s, #9 held %t: want all as they were", store.cases[0].Status, store.tasks["t-plan"].Status, store.tasks[succ.ID].Held())
	}
}

// heldPlanRig is fileRig with revision 1 filed and card redirects held,
// the plan done.
func heldPlanRig(t *testing.T) (*Runner, *fakeStore, Task, CardHold) {
	t.Helper()
	ctx := context.Background()
	r, store, _, _, _ := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	if ref := requestPlan(t, r); len(ref.Refused) != 0 || len(ref.Filed) != 4 {
		t.Fatalf("the children: %+v", ref)
	}
	cards := cardsByKey(t, store)
	hold := CardHold{Reason: "after the pg split", By: "sebastian"}
	if ok, err := store.SetCardHold(ctx, cards["redirects"].ID, hold); err != nil || !ok {
		t.Fatalf("SetCardHold = %v, %v", ok, err)
	}
	setStatus(store, "t-plan", "done")
	return r, store, store.tasks[cards["redirects"].ID], hold
}

// dropsRedirects is the line that refuses a revision dropping held card
// redirects, #4.
const dropsRedirects = "card redirects (#4) is on hold: after the pg split. Only a human closes a held card, so keep its key: the card takes the entry this revision gives that key. " +
	"If the plan no longer needs the card, keep its key and ask in a question of this revision that the human close it or press “Release the hold” on the opened card"

// Rung 2's replan through Act, sent as the console sends it with the
// woken human's login, is the project agent's: the replan step names
// factory-chat, and the planner's record names it and the hold. The
// revision that drops the held card's key is refused naming the hold, and
// the card stays queued and held; one that keeps the key files, and the
// card keeps its hold under the entry the revision gives it.
func TestARungsReplanKeepsAHeldCardsKey(t *testing.T) {
	ctx := context.Background()
	r, store, held, hold := heldPlanRig(t)
	sibling := cardsByKey(t, store)["links-api"]
	if _, err := r.Act(ctx, ActInput{TaskID: sibling.ID, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActReplan, Note: "links-api is too wide"}}); err != nil {
		t.Fatalf("rung 2's replan of links-api: %v", err)
	}
	setStatus(store, "t-plan", "running")
	tl, err := r.timeline.Objects.Get(ctx, "sessions/t-plan/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if step, pending, err := pendingIn(tl, "t-plan"); err != nil || !pending || step.Origin != ReplanPerson || step.By != ChatUser {
		t.Fatalf("the replan step = %+v, %t, %v; want a pending one by %s", step, pending, err, ChatUser)
	}
	rec, _, err := r.replanRecord(ctx, store.tasks["t-plan"], tl)
	if err != nil {
		t.Fatal(err)
	}
	opener := "This is a replan of plan #1, asked by factory-chat: links-api is too wide"
	line := fmt.Sprintf("#4 Redirects and clicks (feature, key redirects): never started, on hold: after the pg split; spent 0.00 of %.2f EUR in 0 sessions; its record is card-4.md", held.BudgetEUR)
	if len(rec.findings) == 0 || rec.findings[0] != opener || !slices.Contains(rec.findings, line) {
		t.Fatalf("the planner's record = %q\nwant %q first and the line %q", rec.findings, opener, line)
	}

	next := testPlan()
	next.Cards[2].Key, next.Cards[2].From = "redirects-2", "redirects"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, next)); err != nil || res.Revision != 0 || !slices.Equal(res.Findings, []string{dropsRedirects}) {
		t.Fatalf("a revision that drops redirects: %+v, %v; want refused with %q", res, err, dropsRedirects)
	}
	if k := store.tasks[held.ID]; k.Status != "queued" || !k.Held() || *k.Hold != hold {
		t.Fatalf("redirects is %s with hold %+v, want queued and held with %+v", k.Status, k.Hold, hold)
	}

	next = testPlan()
	next.Cards[2].Title = "Redirects after the split"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, next)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("a revision that keeps redirects: %+v, %v", res, err)
	}
	k := store.tasks[held.ID]
	if title, _, _ := strings.Cut(k.Objective, "\n"); k.Status != "queued" || !k.Held() || *k.Hold != hold || title != "Redirects after the split" {
		t.Fatalf("redirects is %s with hold %+v and title %q, want queued, held with %+v, under the new title", k.Status, k.Hold, title, hold)
	}
}

// The human's own replan keeps a held card's key too: the revision that
// drops it is refused with the same line. Once the human closed the card,
// its end records the hold's end, and the replan rules ask for the key to
// be left out; the revision that leaves it out files.
func TestTheHumansReplanKeepsAHeldCardsKeyUntilTheyCloseIt(t *testing.T) {
	ctx := context.Background()
	r, store, held, _ := heldPlanRig(t)
	if _, err := r.Replan(ctx, cardsByKey(t, store)["links-api"].ID, ReplanAsk{By: "sebastian", Comment: "links-api is too wide"}); err != nil {
		t.Fatalf("the human's replan: %v", err)
	}
	setStatus(store, "t-plan", "running")
	dropped := testPlan()
	dropped.Cards[2].Key, dropped.Cards[2].From = "redirects-2", "redirects"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, dropped)); err != nil || res.Revision != 0 || !slices.Equal(res.Findings, []string{dropsRedirects}) {
		t.Fatalf("the human's revision that drops redirects: %+v, %v; want refused with %q", res, err, dropsRedirects)
	}

	if err := r.Finish(ctx, held.ID, "closed", "issue closed by sebastian"); err != nil {
		t.Fatal(err)
	}
	if k := store.tasks[held.ID]; k.Status != "closed" || k.Hold != nil {
		t.Fatalf("redirects is %s with hold %+v, want closed with none", k.Status, k.Hold)
	}
	want := Event{Kind: EventCardHoldEnded, TaskID: held.ID, Repository: held.Repository, IssueNumber: 4, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"reason": "after the pg split", "card_status": "closed"}}
	if i := slices.IndexFunc(store.events, func(e Event) bool { return e.Kind == EventCardHoldEnded }); i < 0 || !reflect.DeepEqual(store.events[i], want) {
		t.Fatalf("the hold's end = %+v, want %+v", store.events, want)
	}
	kept := testPlan()
	kept.Cards[1].Title = "Links API, smaller"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, kept)); err != nil || res.Revision != 0 ||
		!slices.Equal(res.Findings, []string{"card redirects ended closed before it started: leave its key out, and add its work under a new key"}) {
		t.Fatalf("a revision that keeps the closed card's key: %+v, %v", res, err)
	}
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, dropped)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the revision that leaves the closed card out: %+v, %v", res, err)
	}
}

// A plan whose only open card is held stays open: its close check closes
// nothing, asks nothing and comments nothing, with criteria or without,
// and the card stays queued and held, its hold on the board.
func TestAPlanWhoseOnlyOpenCardIsHeldStaysOpen(t *testing.T) {
	ctx := context.Background()
	hold := CardHold{Reason: "after the pg split", By: "sebastian"}
	for name, rig := range map[string]func(*testing.T, ...string) (*Runner, *fakeStore, *planForgejo){
		"criteria": tenCriteriaPlan,
		"no criteria": func(t *testing.T, statuses ...string) (*Runner, *fakeStore, *planForgejo) {
			r, s, _, fj := planWithoutCriteria(t, statuses...)
			return r, s, fj
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, s, fj := rig(t, "done", "queued")
			k := s.tasks["card-2"]
			k.Kind, k.IssueNumber = "agent", 12
			s.tasks["card-2"] = k
			if ok, err := s.SetCardHold(ctx, "card-2", hold); err != nil || !ok {
				t.Fatalf("SetCardHold = %v, %v", ok, err)
			}
			for range 2 {
				r.planCloseCheck(ctx, "t-plan")
			}
			if p := s.tasks["t-plan"]; p.Status != "done" || closeRows(s) != 0 || len(s.approvals) != 0 || len(commentBodies(fj)) != 0 {
				t.Fatalf("plan %s, plan_closed rows %d, approvals %+v, comments %q; want done with none", p.Status, closeRows(s), s.approvals, commentBodies(fj))
			}
			if k := s.tasks["card-2"]; k.Status != "queued" || !k.Held() {
				t.Fatalf("card-2 is %s with hold %+v, want queued and held", k.Status, k.Hold)
			}
			board, err := s.BoardTasks(ctx, "linkshort/linkshort")
			i := slices.IndexFunc(board, func(b BoardTask) bool { return b.ID == "card-2" })
			if err != nil || i < 0 || board[i].HoldView.Line != "on hold: after the pg split" {
				t.Fatalf("the board = %+v, %v; want card-2 with its hold line", board, err)
			}
		})
	}
}
