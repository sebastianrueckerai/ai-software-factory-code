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
// card ended BranchRetention ago or longer, and the salvage/<plan> branches
// of plans that closed (plan_closed) and finished as long ago. Finish keeps
// the branch of a card whose work never merged. One project's error does
// not stop the others.
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
		outlived, err := r.branchOutlived(ctx, b.Name, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !outlived {
			continue
		}
		if err := r.forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, b.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// branchOutlived tells whether fork branch name has outlived
// BranchRetention: a card's factory/<id> since the card ended, a plan's
// salvage/<plan> since the plan, closed, finished.
func (r *Runner) branchOutlived(ctx context.Context, name string, now time.Time) (bool, error) {
	id, card := strings.CutPrefix(name, "factory/")
	plan, salvage := strings.CutPrefix(name, "salvage/")
	switch {
	case salvage:
		if closed, err := r.store.PlanClosed(ctx, plan); err != nil || !closed {
			return false, err
		}
		id = plan
	case !card:
		return false, nil
	}
	t, found, err := r.store.Task(ctx, id)
	if err != nil || !found || !salvage && !Final(t.Status) {
		return false, err
	}
	return t.FinishedAt != nil && now.Sub(*t.FinishedAt) >= BranchRetention, nil
}
