package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A card holds no work slot while its approval waits for a human or it
// waits on a dependency, and takes it again as it resumes: the approved
// card back in a gate of its turn, the other through MarkStarted. The
// slot reads the status, so the admission stamp keeps the first admission
// through both waits, and a waiting card, admitted, takes no queue
// reason. holdsSlot reads the same rule card by card, and the queue cap
// counts only the queued cards not yet admitted: t-q and the fix-forward
// card t-fix, whose queued and running rows hold no slot, two at every step.
func TestCountAdmittedFreesTheApprovalAndDependencyWaits(t *testing.T) {
	for name, open := range bothStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			cards := []struct{ id, status, source string }{
				{"t-run", "running", ""}, {"t-close", "closing", ""}, {"t-review", "in-review", ""}, {"t-appr", "in-review", ""},
				{"t-deps", "queued", ""}, {"t-qa", "queued", ""}, {"t-q", "queued", ""}, {"t-human", "waiting-human", ""},
				{"t-roll", "running", SourceRoll}, {"t-fix", "queued", SourceMainFix}, {"t-fixrun", "running", SourceMainFix},
			}
			var ids []string
			for i, c := range cards {
				ids = append(ids, c.id)
				if err := s.CreateTask(ctx, Task{ID: c.id, Repository: "sf/r", Kind: "agent", Track: "direct", Status: c.status,
					Source: c.source, Objective: "b", IssueNumber: int64(i + 1)}); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"t-run", "t-close", "t-review", "t-appr", "t-deps", "t-qa", "t-human"} {
				if err := s.SetAdmitted(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			for id, gate := range map[string]string{"t-review": GateReview, "t-appr": GateChecks} {
				if err := s.SetGate(ctx, id, gate, now); err != nil {
					t.Fatal(err)
				}
			}
			counts := func(step string, admitted, queue int, holding ...string) {
				t.Helper()
				if n, err := s.CountAdmitted(ctx, "sf/r"); err != nil || n != admitted {
					t.Fatalf("%s: CountAdmitted = %d (err %v), want %d", step, n, err, admitted)
				}
				if n, err := s.CountQueue(ctx, "sf/r"); err != nil || n != queue {
					t.Fatalf("%s: CountQueue = %d (err %v), want %d", step, n, err, queue)
				}
				var held []string
				for _, id := range ids {
					task, _, err := s.Task(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if holdsSlot(task) {
						held = append(held, id)
					}
				}
				if !slices.Equal(held, holding) {
					t.Fatalf("%s: holdsSlot holds %v, want %v", step, held, holding)
				}
			}
			stamps := map[string]time.Time{}
			for _, id := range []string{"t-appr", "t-deps"} {
				task, _, err := s.Task(ctx, id)
				if err != nil || task.AdmittedAt == nil {
					t.Fatalf("%s admitted at %v (err %v), want stamped", id, task.AdmittedAt, err)
				}
				stamps[id] = *task.AdmittedAt
			}
			admitted := func(id string) {
				t.Helper()
				task, _, err := s.Task(ctx, id)
				if err != nil || task.AdmittedAt == nil || !task.AdmittedAt.Equal(stamps[id]) {
					t.Fatalf("%s admitted at %v (err %v), want its first admission %v", id, task.AdmittedAt, err, stamps[id])
				}
			}
			counts("at the start", 6, 2, "t-run", "t-close", "t-review", "t-appr", "t-deps", "t-qa")

			if err := s.SetGate(ctx, "t-appr", GateApproval, now); err != nil {
				t.Fatal(err)
			}
			admitted("t-appr")
			counts("the approval waits", 5, 2, "t-run", "t-close", "t-review", "t-deps", "t-qa")

			if err := s.TransitionTask(ctx, "t-deps", []string{"queued"}, "waiting-deps"); err != nil {
				t.Fatal(err)
			}
			admitted("t-deps")
			counts("the dependency waits", 4, 2, "t-run", "t-close", "t-review", "t-qa")

			if err := s.SetGate(ctx, "t-appr", GateMergeQueue, now); err != nil {
				t.Fatal(err)
			}
			counts("the approved card in its turn", 5, 2, "t-run", "t-close", "t-review", "t-appr", "t-qa")

			if err := s.SetQueueReason(ctx, "t-deps", "waits on #3", now); err != nil {
				t.Fatal(err)
			}
			if task, _, _ := s.Task(ctx, "t-deps"); task.QueueReason != "" || task.QueueSince != nil {
				t.Fatalf("the waiting card's queue reason = %q since %v, want none: it is admitted", task.QueueReason, task.QueueSince)
			}
			if err := s.MarkStarted(ctx, "t-deps"); err != nil {
				t.Fatal(err)
			}
			admitted("t-deps")
			counts("the dependency done", 6, 2, "t-run", "t-close", "t-review", "t-appr", "t-deps", "t-qa")
		})
	}
}

// A re-filed issue can hold two cards, one final one live, since the
// live-card index binds (repository, issue_number) to live cards alone:
// the read prefers the live card, and once both are final the newest
// answers.
func TestTaskByIssuePrefersTheLiveCard(t *testing.T) {
	for name, open := range bothStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
			card := func(id, status string, created time.Time) Task {
				return Task{ID: id, Repository: "sf/r", Kind: "agent", Track: "direct", Status: status,
					Objective: "b", IssueNumber: 7, CreatedAt: created}
			}
			if err := s.CreateTask(ctx, card("t-old", "done", at)); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateTask(ctx, card("t-live", "in-review", at.Add(time.Hour))); err != nil {
				t.Fatal(err)
			}
			read := func(step, want string) {
				t.Helper()
				got, ok, err := s.TaskByIssue(ctx, "sf/r", 7)
				if err != nil || !ok || got.ID != want {
					t.Fatalf("%s: TaskByIssue = %q ok %v (err %v), want %q", step, got.ID, ok, err, want)
				}
			}
			read("a final and a live card", "t-live")
			if err := s.TransitionTask(ctx, "t-live", []string{"in-review"}, "done"); err != nil {
				t.Fatal(err)
			}
			read("two final cards", "t-live")
		})
	}
}

// A parked card whose resumption the surge bound holds back waits queued:
// it holds no slot, is no never-started card (the pull never lists it),
// keeps the time its wait began through every refresh, and lists oldest
// resumption first. MarkStarted ends the wait.
func TestResumeWaitHoldsNoSlotAndListsOldestFirst(t *testing.T) {
	for name, open := range bothStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			for i, c := range []struct{ id, status string }{{"t-run", "running"}, {"t-a", "waiting-human"}, {"t-b", "waiting-deps"}, {"t-new", "queued"}} {
				if err := s.CreateTask(ctx, Task{ID: c.id, Repository: "sf/r", Kind: "agent", Track: "direct", Status: c.status, Objective: "b", IssueNumber: int64(i + 1)}); err != nil {
					t.Fatal(err)
				}
				if c.id != "t-new" {
					if err := s.SetAdmitted(ctx, c.id); err != nil {
						t.Fatal(err)
					}
				}
			}
			at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			for _, w := range []struct {
				id     string
				at     time.Time
				reason string
			}{{"t-b", at, "resumed: first"}, {"t-a", at.Add(time.Minute), "resumed: second"}, {"t-b", at.Add(2 * time.Minute), "resumed: refreshed"}} {
				if err := s.SetResumeWait(ctx, w.id, w.reason, w.at); err != nil {
					t.Fatal(err)
				}
			}
			waiters, err := s.ResumeWaiters(ctx, "sf/r")
			if err != nil || len(waiters) != 2 || waiters[0].ID != "t-b" || waiters[1].ID != "t-a" {
				t.Fatalf("waiters = %+v (err %v), want t-b then t-a", waiters, err)
			}
			b := waiters[0]
			if b.Status != "queued" || b.QueueReason != "resumed: refreshed" || !b.ResumedAt.Equal(at) || !b.QueueSince.Equal(at) {
				t.Fatalf("t-b = %s, %q, resumed %v, since %v; want queued, the refreshed reason, the first wait's time", b.Status, b.QueueReason, b.ResumedAt, b.QueueSince)
			}
			if n, err := s.CountAdmitted(ctx, "sf/r"); err != nil || n != 1 {
				t.Fatalf("CountAdmitted = %d (err %v), want 1: the waiters hold no slot", n, err)
			}
			if queued, err := s.ListQueuedByRepository(ctx, "sf/r"); err != nil || len(queued) != 1 || queued[0].ID != "t-new" {
				t.Fatalf("queued = %+v (err %v), want only the never-started t-new", queued, err)
			}
			if err := s.SetResumeWait(ctx, "t-new", "resumed: no", at); err != nil {
				t.Fatal(err)
			}
			if n, _ := s.ResumeWaiters(ctx, "sf/r"); len(n) != 2 {
				t.Fatalf("a never-started card became a resumed waiter: %+v", n)
			}
			if err := s.SetResumeWait(ctx, "t-run", "resumed: no", at); err != nil {
				t.Fatal(err)
			}
			if run, _, _ := s.Task(ctx, "t-run"); run.Status != "running" || run.ResumedAt != nil {
				t.Fatalf("a running card became a waiter: %s, %v", run.Status, run.ResumedAt)
			}
			if err := s.TransitionTask(ctx, "t-a", []string{"queued"}, "closed"); err != nil {
				t.Fatal(err)
			}
			closed, _, _ := s.Task(ctx, "t-a")
			if closed.ResumedAt != nil || closed.QueueReason != "" || closed.QueueSince != nil {
				t.Fatalf("t-a closed while it waited keeps resumed %v, reason %q, since %v", closed.ResumedAt, closed.QueueReason, closed.QueueSince)
			}
			if err := s.TransitionTask(ctx, "t-a", []string{"closed"}, "queued"); err != nil {
				t.Fatal(err)
			}
			if n, _ := s.ResumeWaiters(ctx, "sf/r"); len(n) != 1 || n[0].ID != "t-b" {
				t.Fatalf("a reopened card is a resumed waiter: %+v", n)
			}
			if err := s.MarkStarted(ctx, "t-b"); err != nil {
				t.Fatal(err)
			}
			started, _, _ := s.Task(ctx, "t-b")
			if started.Status != "running" || started.ResumedAt != nil || started.QueueReason != "" || started.QueueSince != nil {
				t.Fatalf("t-b after MarkStarted = %s, resumed %v, reason %q, since %v", started.Status, started.ResumedAt, started.QueueReason, started.QueueSince)
			}
			if n, _ := s.CountAdmitted(ctx, "sf/r"); n != 3 {
				t.Fatalf("CountAdmitted = %d, want 3: t-run, t-b running and the reopened, admitted t-a queued", n)
			}
		})
	}
}
