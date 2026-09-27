package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Completion is the task completion path: flip the task to closing (the
// gateway refuses model spend for anything but "running"), revoke the
// repo-scoped Forgejo token, block the task's LiteLLM virtual keys, and only
// then mark the task done. Tokens are found by name (task-<id>), so a
// completion retry after a crash — or the boot sweep — revokes exactly what
// the mint created even though the mint's ID is gone. Any failure leaves the
// task in closing: spend is already impossible, so a retry can finish the
// cleanup (Stage 4 gate item).
type Completion struct {
	Store   Store
	Forgejo *Forgejo
	LiteLLM *LiteLLMAdmin
}

func TokenName(taskID string) string { return "task-" + taskID }

func (c *Completion) Complete(ctx context.Context, taskID string, finalStatus string) error {
	if err := c.Store.TransitionTask(ctx, taskID, []string{"running", "closing"}, "closing"); err != nil {
		return err
	}
	if err := c.reconcile(ctx, taskID); err != nil {
		return err
	}
	if err := c.Store.TransitionTask(ctx, taskID, []string{"closing"}, finalStatus); err != nil {
		return err
	}
	c.mirrorState(ctx, taskID, finalStatus)
	return nil
}

// mirrorState looks up a completed task and swaps its linked Forgejo issue
// onto exactly one state/* label, via the shared mirrorTaskState helper.
// Mirroring failures are logged, never returned: Complete's own result must
// never depend on Forgejo being reachable.
func (c *Completion) mirrorState(ctx context.Context, taskID, state string) {
	t, ok, err := c.Store.Task(ctx, taskID)
	if err != nil {
		log.Printf("task %s: mirror state %s: %v", taskID, state, err)
		return
	}
	if !ok {
		return
	}
	mirrorTaskState(ctx, c.Forgejo, t, state)
}

// Requeue hands an interrupted task back to the queue with its credentials
// revoked; the next claim mints fresh ones.
func (c *Completion) Requeue(ctx context.Context, taskID string) error {
	return c.Complete(ctx, taskID, "queued")
}

// SpendSettleWindow is how long after a task finishes its spend keeps being
// settled against LiteLLM. LiteLLM writes key spend to its database in
// batches, observed 60 to 80 s after a call, so an early pass can see part
// of it; every pass inside the window raises spend again, never lowers it.
const SpendSettleWindow = time.Hour

// SettleSpend raises each task finished within SpendSettleWindow to at
// least the spend LiteLLM reports for its keys, so a call whose cost the
// gateway could not book (an abandoned stream) still counts. It lists
// LiteLLM's keys once per pass and returns how many tasks it settled.
func (c *Completion) SettleSpend(ctx context.Context) (int, error) {
	ids, err := c.Store.RecentlyFinishedTasks(ctx, SpendSettleWindow)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	spend, err := c.LiteLLM.SpendByTask(ctx)
	if err != nil {
		return 0, err
	}
	errs := []error{}
	for _, id := range ids {
		if err := c.Store.SettleSpend(ctx, id, spend[id]); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
		}
	}
	return len(ids) - len(errs), errors.Join(errs...)
}

// SettleLoop runs a SettleSpend pass every interval until ctx ends. It has
// its own goroutine, so a slow LiteLLM never delays task claims.
func (c *Completion) SettleLoop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := c.SettleSpend(ctx)
			if err != nil {
				log.Printf("settle spend: %v", err)
			}
			if n > 0 {
				log.Printf("settle spend: pass settled %d tasks finished within %s", n, SpendSettleWindow)
			}
		}
	}
}

// reconcile revokes every token named for the task and blocks its keys.
// Calling it on an already-clean task is a no-op, which is what makes
// completion idempotent.
func (c *Completion) reconcile(ctx context.Context, taskID string) error {
	refs, err := c.Forgejo.ListTokens(ctx)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	errs := []error{}
	for _, ref := range refs {
		if ref.Name != TokenName(taskID) {
			continue
		}
		if err := c.Forgejo.RevokeToken(ctx, ref.ID); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := c.LiteLLM.BlockTaskKeys(ctx, taskID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
