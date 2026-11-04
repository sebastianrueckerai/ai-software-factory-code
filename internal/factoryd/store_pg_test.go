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
// counts only the queued cards not yet admitted.
func TestCountAdmittedFreesTheApprovalAndDependencyWaits(t *testing.T) {
	for name, open := range bothStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			cards := []struct{ id, status, source string }{
				{"t-run", "running", ""}, {"t-close", "closing", ""}, {"t-review", "in-review", ""}, {"t-appr", "in-review", ""},
				{"t-deps", "queued", ""}, {"t-qa", "queued", ""}, {"t-q", "queued", ""}, {"t-human", "waiting-human", ""},
				{"t-roll", "running", SourceRoll},
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
			counts("at the start", 6, 1, "t-run", "t-close", "t-review", "t-appr", "t-deps", "t-qa")

			if err := s.SetGate(ctx, "t-appr", GateApproval, now); err != nil {
				t.Fatal(err)
			}
			admitted("t-appr")
			counts("the approval waits", 5, 1, "t-run", "t-close", "t-review", "t-deps", "t-qa")

			if err := s.TransitionTask(ctx, "t-deps", []string{"queued"}, "waiting-deps"); err != nil {
				t.Fatal(err)
			}
			admitted("t-deps")
			counts("the dependency waits", 4, 1, "t-run", "t-close", "t-review", "t-qa")

			if err := s.SetGate(ctx, "t-appr", GateMergeQueue, now); err != nil {
				t.Fatal(err)
			}
			counts("the approved card in its turn", 5, 1, "t-run", "t-close", "t-review", "t-appr", "t-qa")

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
			counts("the dependency done", 6, 1, "t-run", "t-close", "t-review", "t-appr", "t-deps", "t-qa")
		})
	}
}
