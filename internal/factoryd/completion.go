package factoryd

import (
	"context"
	"errors"
	"fmt"
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
	return c.Store.TransitionTask(ctx, taskID, []string{"closing"}, finalStatus)
}

// Requeue hands an interrupted task back to the queue with its credentials
// revoked; the next claim mints fresh ones.
func (c *Completion) Requeue(ctx context.Context, taskID string) error {
	return c.Complete(ctx, taskID, "queued")
}

// SpendSettleDelay is how long after a task finishes its spend is settled
// against LiteLLM. LiteLLM writes key spend to its database in batches,
// observed 60 to 80 s after a call; the task's keys are blocked at
// completion, so after this delay their reported spend is final.
const SpendSettleDelay = 3 * time.Minute

// SettleSpend raises each task finished before now-SpendSettleDelay to at
// least the spend LiteLLM reports for its keys, so a call whose cost the
// gateway could not book (an abandoned stream) still counts. A task stays
// unsettled while LiteLLM is unreachable and is retried on the next call.
func (c *Completion) SettleSpend(ctx context.Context, now time.Time) error {
	ids, err := c.Store.UnsettledTasks(ctx, now.Add(-SpendSettleDelay))
	if err != nil {
		return err
	}
	errs := []error{}
	for _, id := range ids {
		spend, err := c.LiteLLM.TaskSpend(ctx, id)
		if err == nil {
			err = c.Store.SettleSpend(ctx, id, spend)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
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
