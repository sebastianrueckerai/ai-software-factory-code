package factoryd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BranchRetention is how long a card's branch stays on the fork after the
// card ended without merging, for a refile to build on.
const BranchRetention = 30 * 24 * time.Hour

// SweepBranches deletes the factory/<id> branches on the bot's forks whose
// card ended BranchRetention ago or longer. Finish keeps the branch of a card
// whose work never merged. One project's error does not stop the others.
func (r *Runner) SweepBranches(ctx context.Context, now time.Time) error {
	var errs []error
	for name, p := range r.cfg.Policy().Tree.Projects {
		if err := r.sweepProjectBranches(ctx, p.Repository, now); err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) sweepProjectBranches(ctx context.Context, repository string, now time.Time) error {
	repo, err := SplitRepo(repository)
	if err != nil {
		return err
	}
	fork := r.forgejo.Fork(repo)
	branches, err := GetPages[struct{ Name string }](ctx, r.forgejo.call, fmt.Sprintf("/api/v1/repos/%s/%s/branches?", fork.Owner, fork.Name))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range branches {
		id, ok := strings.CutPrefix(b.Name, "factory/")
		if !ok {
			continue
		}
		t, found, err := r.store.Task(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found || !Final(t.Status) || t.FinishedAt == nil || now.Sub(*t.FinishedAt) < BranchRetention {
			continue
		}
		if err := r.forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, b.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
