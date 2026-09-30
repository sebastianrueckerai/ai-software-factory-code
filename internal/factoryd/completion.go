package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
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
	// Metrics counts the completed task's outcome; nil counts none.
	Metrics *telemetry.Metrics
}

// TokenName names the task's write token on the bot's fork; BaseTokenName
// its read token on the task's repository.
func TokenName(taskID string) string     { return "task-" + taskID }
func BaseTokenName(taskID string) string { return TokenName(taskID) + "-base" }

func (c *Completion) Complete(ctx context.Context, taskID string, finalStatus string) error {
	if err := c.Store.TransitionTask(ctx, taskID, []string{"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster", "closing"}, "closing"); err != nil {
		return err
	}
	if err := c.reconcile(ctx, taskID); err != nil {
		return err
	}
	if err := c.Store.TransitionTask(ctx, taskID, []string{"closing"}, finalStatus); err != nil {
		return err
	}
	c.finished(ctx, taskID, finalStatus)
	return nil
}

// finished counts a completed task's outcome and swaps its linked Forgejo
// issue onto exactly one state/* label, via the shared mirrorTaskState
// helper. Failures are logged, never returned: Complete's own result must
// never depend on Forgejo being reachable.
func (c *Completion) finished(ctx context.Context, taskID, state string) {
	t, ok, err := c.Store.Task(ctx, taskID)
	if err != nil {
		log.Printf("task %s: finished %s: %v", taskID, state, err)
		return
	}
	if !ok {
		return
	}
	countOutcome(ctx, c.Metrics, c.Store, t, state)
	mirrorTaskState(ctx, c.Forgejo, t, state)
}

// SpendSettleWindow is how long after a task finishes its spend keeps being
// settled against LiteLLM. LiteLLM writes key spend to its database in
// batches, observed 60 to 80 s after a call, so an early pass can see part
// of it; every pass inside the window raises spend again, never lowers it.
// It outlasts HoldTTL by an hour: a hold taken just before the task
// finished is swept and booked up to HoldTTL later, and the window must
// still be open on the next pass after that, or the task is never settled.
const SpendSettleWindow = policy.HoldTTL + time.Hour

// SettleSpend raises each task finished within SpendSettleWindow to at
// least the spend LiteLLM reports for its keys, so a call whose cost the
// gateway could not book (an abandoned stream) still counts; a task past
// the window is set to LiteLLM's figure once, down as well as up, since a
// call the gate cut was booked at its hold (ruling 196). It lists LiteLLM's
// keys once per pass and returns how many tasks' spend it changed.
func (c *Completion) SettleSpend(ctx context.Context) (int, error) {
	ids, err := c.Store.RecentlyFinishedTasks(ctx, SpendSettleWindow)
	if err != nil {
		return 0, err
	}
	final, err := c.Store.UnsettledTasks(ctx, SpendSettleWindow)
	if err != nil || len(ids)+len(final) == 0 {
		return 0, err
	}
	spend, err := c.LiteLLM.SpendByTask(ctx)
	if err != nil {
		return 0, err
	}
	changed, errs := 0, []error{}
	for _, id := range ids {
		if ok, err := c.Store.SettleSpend(ctx, id, spend[id]); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
		} else if ok {
			changed++
		}
	}
	for _, id := range final {
		if ok, err := c.Store.SettleSpendFinal(ctx, id, spend[id]); err != nil {
			errs = append(errs, fmt.Errorf("task %s: final: %w", id, err))
		} else if ok {
			changed++
		}
	}
	return changed, errors.Join(errs...)
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
				log.Printf("settle spend: pass changed the spend of %d tasks", n)
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
		if ref.Name != TokenName(taskID) && ref.Name != BaseTokenName(taskID) {
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
