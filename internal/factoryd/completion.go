package factoryd

import (
	"context"
	"errors"
)

// Completion is the task completion path: flip the task to closing (the
// gateway refuses model spend for anything but "running"), revoke the
// repo-scoped Forgejo token, block the task's LiteLLM virtual keys, and only
// then mark the task done. Any failure leaves the task in closing: spend is
// already impossible, so a retry can finish the cleanup (Stage 4 gate item).
type Completion struct {
	Store   Store
	Forgejo *Forgejo
	BotUser string
	LiteLLM *LiteLLMAdmin
}

func (c *Completion) Complete(ctx context.Context, taskID string, forgejoTokenID int64) error {
	if err := c.Store.SetTaskStatus(ctx, taskID, "closing"); err != nil {
		return err
	}
	errs := []error{}
	if err := c.Forgejo.RevokeToken(ctx, c.BotUser, forgejoTokenID); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.LiteLLM.BlockTaskKeys(ctx, taskID); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return c.Store.SetTaskStatus(ctx, taskID, "done")
}
