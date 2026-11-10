package factoryd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/stretchr/testify/mock"
)

func surgeTree(bound int, surge *int) policy.Tree {
	return policy.Tree{Projects: map[string]policy.ProjectConfig{
		"demo": {Repository: "demo/app", WorkInProgress: bound, Surge: surge}}}
}

func surgeRunner(store *fakeStore, tree policy.Tree) *Runner {
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return policy.Version{Tree: tree} }
	cfg.Workflows = &recordingWorkflows{}
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	n := 0
	return &Runner{store: store, cfg: cfg, now: func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }}
}

func parkedCard(id, status string) Task {
	return Task{ID: id, Repository: "demo/app", Kind: "agent", Track: "direct", Status: status}
}

// Bound 5 and surge 2, five cards running, eight parked cards answered in
// the order p1..p8: two start (live 5 -> 7), six wait queued; the room a
// finished card frees goes to the oldest answer first, whichever card asks
// first.
func TestAdmitResumeStartsTheSurgeAndQueuesTheRestInAnswerOrder(t *testing.T) {
	ctx := context.Background()
	two := 2
	store := newFakeStore()
	for i := 1; i <= 5; i++ {
		store.tasks[fmt.Sprintf("r%d", i)] = parkedCard(fmt.Sprintf("r%d", i), "running")
	}
	for i := 1; i <= 8; i++ {
		store.tasks[fmt.Sprintf("p%d", i)] = parkedCard(fmt.Sprintf("p%d", i), "waiting-human")
	}
	r := surgeRunner(store, surgeTree(5, &two))
	var started, queued []string
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("p%d", i)
		s, err := r.AdmitResume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Granted {
			started = append(started, id)
		} else {
			queued = append(queued, id)
			if s.Ahead != len(queued)-1 {
				t.Fatalf("%s waits with %d ahead, want %d", id, s.Ahead, len(queued)-1)
			}
		}
	}
	if fmt.Sprint(started) != "[p1 p2]" || fmt.Sprint(queued) != "[p3 p4 p5 p6 p7 p8]" {
		t.Fatalf("started %v, queued %v", started, queued)
	}
	if n, _ := store.CountAdmitted(ctx, "demo/app"); n != 7 {
		t.Fatalf("live = %d, want 7", n)
	}
	if p3, _, _ := store.Task(ctx, "p3"); p3.Status != "queued" || p3.ResumedAt == nil || p3.QueueReason != "resumed: work bound 7 of 7 live, 0 ahead" {
		t.Fatalf("p3 = %s, resumed %v, reason %q", p3.Status, p3.ResumedAt, p3.QueueReason)
	}
	// two running cards finish: live 5, room for two. p5 asks first and
	// waits behind p3 and p4; then p3 and p4 start, and p5 waits again.
	for _, id := range []string{"r1", "r2"} {
		d := store.tasks[id]
		d.Status = "done"
		store.tasks[id] = d
	}
	for _, c := range []struct {
		id      string
		granted bool
		ahead   int
	}{{"p5", false, 2}, {"p3", true, 0}, {"p4", true, 0}, {"p5", false, 0}} {
		s, err := r.AdmitResume(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Granted != c.granted || (!c.granted && s.Ahead != c.ahead) {
			t.Fatalf("%s: granted %v, ahead %d; want granted %v, ahead %d", c.id, s.Granted, s.Ahead, c.granted, c.ahead)
		}
	}
}

// A surge of 0 is a bound with no overshoot: a resumed card at the bound
// waits; a card holding a slot already (in review) passes at once.
func TestAdmitResumeSurgeZeroAllowsNoOvershoot(t *testing.T) {
	ctx := context.Background()
	zero := 0
	store := newFakeStore()
	store.tasks["r1"] = parkedCard("r1", "running")
	store.tasks["p1"] = parkedCard("p1", "waiting-deps")
	rev := parkedCard("rev", "in-review")
	rev.Gate = GateReview
	store.tasks["rev"] = rev
	r := surgeRunner(store, surgeTree(2, &zero))
	if s, _ := r.AdmitResume(ctx, "rev"); !s.Granted {
		t.Fatal("a card in review holds its slot: it passes")
	}
	if s, err := r.AdmitResume(ctx, "p1"); err != nil || s.Granted {
		t.Fatalf("p1 at the bound with no surge: granted %v, err %v", s.Granted, err)
	}
}

// A never-started card waits while a resumed card is ahead of it, though
// the bound has room; once the resumed card runs it starts.
func TestBoundedHoldsANeverStartedCardBehindAResumedOne(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := boundedFixture(t, 3,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "running"},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "queued", IssueNumber: 2})
	waiter := parkedCard("t-w", "queued")
	waiter.AdmittedAt, waiter.ResumedAt = &time.Time{}, &time.Time{}
	store.tasks["t-w"] = waiter
	if err := bounded.Start(ctx, "t-q"); err != nil {
		t.Fatal(err)
	}
	if len(rec.starts) != 0 {
		t.Fatalf("starts = %v, want none while a resumed card waits", rec.starts)
	}
	if q, _, _ := store.Task(ctx, "t-q"); q.QueueReason != "work bound 1 of 3, 1 resumed first" {
		t.Fatalf("reason = %q", q.QueueReason)
	}
	if err := store.MarkStarted(ctx, "t-w"); err != nil {
		t.Fatal(err)
	}
	if err := bounded.Start(ctx, "t-q"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rec.starts) != "[t-q]" {
		t.Fatalf("starts = %v, want t-q once the resumed card runs", rec.starts)
	}
}

// The pull wakes the resumed cards, oldest first, before it starts any
// never-started card; the wake reaches a card only while the live count is
// under the bound plus the surge.
func TestPullWakesResumedCardsFirstAndOnlyWithRoom(t *testing.T) {
	ctx := context.Background()
	two := 2
	store, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "running"},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Track: "direct", Status: "queued", IssueNumber: 2})
	bounded.Policy = func() policy.Tree { return surgeTree(1, &two) }
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"t-w1", "t-w2"} {
		w := parkedCard(id, "queued")
		a := at.Add(time.Duration(i) * time.Minute)
		w.AdmittedAt, w.ResumedAt = &at, &a
		store.tasks[id] = w
	}
	for _, id := range []string{"t-run2", "t-run3"} {
		store.tasks[id] = parkedCard(id, "running")
	}
	pullQueued(ctx, store, bounded, "demo/app")
	if len(rec.signals) != 0 || len(rec.starts) != 0 {
		t.Fatalf("live 3 of limit 3: signals %v, starts %v, want none", rec.signals, rec.starts)
	}
	store.tasks["t-run3"] = parkedCard("t-run3", "done")
	pullQueued(ctx, store, bounded, "demo/app")
	if fmt.Sprint(rec.signals) != "[t-w1 t-w2]" || len(rec.starts) != 0 {
		t.Fatalf("signals %v, starts %v, want t-w1 t-w2 and no start", rec.signals, rec.starts)
	}
}

// The face counts the live cards, then the resumed cards by their time,
// then the never-started cards the bound refused, by filing: the board's
// order of admission. A card refused for a dependency is no one's ahead.
func TestSetSlotWaitsOrdersResumedBeforeFresh(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Minute)
	task := func(id string, status string, issue int64, created time.Time, reason string, resumed *time.Time) BoardTask {
		return BoardTask{Task: Task{ID: id, Kind: "agent", Track: "direct", Status: status, IssueNumber: issue, CreatedAt: created,
			QueueReason: reason, ResumedAt: resumed}}
	}
	work := WorkBoundReason + "5 of 5"
	tasks := []BoardTask{
		task("run", "running", 1, at, "", nil),
		task("fresh2", "queued", 3, at.Add(2*time.Second), work, nil),
		task("fresh1", "queued", 2, at.Add(time.Second), work, nil),
		task("dep", "queued", 4, at, "waits on #9", nil),
		task("res2", "queued", 5, at, ResumedWaitReason+"5 of 7 live, 1 ahead", &later),
		task("res1", "queued", 6, at, ResumedWaitReason+"5 of 7 live, 0 ahead", &at),
	}
	tasks[4].AdmittedAt, tasks[5].AdmittedAt = &at, &at
	setSlotWaits(tasks)
	want := map[string]string{"run": "", "dep": "",
		"res1": "waits for a slot, 0 ahead (1 live)", "res2": "waits for a slot, 1 ahead (1 live)",
		"fresh1": "waits for a slot, 2 ahead (1 live)", "fresh2": "waits for a slot, 3 ahead (1 live)"}
	for _, b := range tasks {
		if b.SlotWait != want[b.ID] {
			t.Errorf("%s: %q, want %q", b.ID, b.SlotWait, want[b.ID])
		}
	}
}

// A card the surge bound holds back waits at its session start, asking
// again on each slot_free, and starts its session once AdmitResume grants:
// the session runs after the second ask, none before.
func TestWorkflowWaitsForASlotBeforeItsSession(t *testing.T) {
	h := newWorkflowHarness(t)
	asks := 0
	h.env.OnActivity(ActivityAdmitResume, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (ResumeSlot, error) {
		asks++
		return ResumeSlot{Granted: asks == 2, Ahead: 1}, nil
	})
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalSlotFree, TaskSignal{})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if asks != 2 || len(h.sessions) != 1 {
		t.Fatalf("asks = %d, sessions = %d, want 2 asks and 1 session", asks, len(h.sessions))
	}
}

// An issue closed while the card waits for a slot ends it: no session ran.
func TestWorkflowEndsWhileWaitingForASlot(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityAdmitResume, mock.Anything, "t1").Return(ResumeSlot{Ahead: 3}, nil)
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.sessions) != 0 {
		t.Fatalf("sessions = %d, want none", len(h.sessions))
	}
}

// A card in review holds its slot through its checks, so its CI pod is
// part of the slot: at bound 5 and surge 2, four running cards and three in
// their checks are seven live, and a resumed card waits.
func TestAdmitResumeCountsCardsInTheirChecks(t *testing.T) {
	ctx := context.Background()
	two := 2
	store := newFakeStore()
	for i := 1; i <= 4; i++ {
		store.tasks[fmt.Sprintf("r%d", i)] = parkedCard(fmt.Sprintf("r%d", i), "running")
	}
	for i := 1; i <= 3; i++ {
		c := parkedCard(fmt.Sprintf("c%d", i), "in-review")
		c.Gate = GateChecks
		store.tasks[c.ID] = c
	}
	store.tasks["p1"] = parkedCard("p1", "waiting-human")
	r := surgeRunner(store, surgeTree(5, &two))
	if s, err := r.AdmitResume(ctx, "p1"); err != nil || s.Granted {
		t.Fatalf("granted %v (err %v), want a wait at 7 live", s.Granted, err)
	}
	store.tasks["c3"] = parkedCard("c3", "done")
	if s, err := r.AdmitResume(ctx, "p1"); err != nil || !s.Granted {
		t.Fatalf("granted %v (err %v), want a start at 6 live", s.Granted, err)
	}
}

type countFails struct {
	*fakeStore
	err error
}

func (c countFails) CountAdmitted(context.Context, string) (int, error) { return 0, c.err }

// The error of AdmitResume's count reaches the workflow's reason scrubbed.
func TestAdmitResumeScrubsItsErrors(t *testing.T) {
	store := newFakeStore()
	store.tasks["p1"] = parkedCard("p1", "waiting-human")
	leak := fmt.Errorf("count: https://user:pass@host/x Authorization: Bearer abcdefghijklmnopqrstuvwx")
	r := surgeRunner(store, surgeTree(5, nil))
	r.store = countFails{store, leak}
	_, err := execActivity(t, r, ActivityAdmitResume, "p1")
	if err == nil || !strings.Contains(err.Error(), "count:") || strings.Contains(err.Error(), "pass@") || strings.Contains(err.Error(), "abcdefghijklmnop") {
		t.Fatalf("err = %v, want the count named and both secrets masked", err)
	}
}
