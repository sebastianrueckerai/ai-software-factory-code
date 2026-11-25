package factoryd

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// landChecksBound is how long LandPullRequest waits for the checks of a
// head to leave pending, landChecksPoll how often it reads them, and
// landNoStatusGrace how long it waits for a head that has reported no
// status at all: a check that a pull request triggers reports a moment
// after the pull request opens.
var (
	landChecksBound   = 20 * time.Minute
	landChecksPoll    = 15 * time.Second
	landNoStatusGrace = time.Minute
)

// LandPullRequest merges the factory's own pull request number of repo as
// the bot at the head it checked: Forgejo reports it mergeable (read again
// for up to mergeWaitBound), vet finds nothing to refuse, and no check of
// the head fails. Checks that are pending are waited for, up to
// landChecksBound. vet answers why a human must look at the pull request,
// "" when it passes; nil vets nothing. required names the checks the head
// must show before the merge, each reported and not failed: a check that
// reports a moment after the pull request opens is waited for, up to
// landChecksBound. LandPullRequest answers why a human merges it, "" when no
// human needs to: it merged, or it closed.
func (f *Forgejo) LandPullRequest(ctx context.Context, repo Repo, number int64, a Attribution, subject string, vet func(PullRequestRef) string, required ...string) string {
	pr, err := f.PullRequestState(ctx, repo.Owner, repo.Name, number)
	for waits := 0; err == nil && pr.State == "open" && !pr.Mergeable && waits < maxMergeWaits; waits++ {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(mergeWaitBound / maxMergeWaits):
			pr, err = f.PullRequestState(ctx, repo.Owner, repo.Name, number)
		}
	}
	switch {
	case err != nil:
		return fmt.Sprintf("pull request #%d did not read: %v", number, err)
	case pr.State != "open":
		log.Printf("land %s: pull request #%d is no longer open (merged: %v)", repo, number, pr.Merged)
		return ""
	case !pr.Mergeable:
		return fmt.Sprintf("Forgejo still reported pull request #%d as not mergeable after %s", number, mergeWaitBound)
	}
	if vet != nil {
		if why := vet(pr); why != "" {
			return why
		}
	}
	if why := f.awaitChecks(ctx, repo, pr.HeadSHA, required); why != "" {
		return why
	}
	if err := f.MergePullRequestAsBot(ctx, repo.Owner, repo.Name, number, pr.HeadSHA, a, subject); err != nil {
		return "the bot's merge failed: " + err.Error()
	}
	log.Printf("land %s: merged pull request #%d at %s", repo, number, ShortSHA(pr.HeadSHA))
	return ""
}

// awaitChecks reads the statuses of head until none is pending, and
// answers why they refuse the merge: one failed, one stayed pending past
// landChecksBound, or they did not read. "" when none fails. A check of
// required that has not reported is waited for as a pending one is: it is a
// status whose context is the name or starts with the name and a space
// ("config-check / check (pull_request)" is "config-check").
func (f *Forgejo) awaitChecks(ctx context.Context, repo Repo, head string, required []string) string {
	start := time.Now()
	for {
		st, err := f.CombinedStatus(ctx, repo.Owner, repo.Name, head)
		if err != nil {
			return fmt.Sprintf("the checks of its head %s did not read: %v", ShortSHA(head), err)
		}
		var pending *CommitStatus
		for i, s := range st.Statuses {
			switch s.State {
			case "failure", "error":
				return fmt.Sprintf("the check “%s” of its head %s has the state %s", s.Context, ShortSHA(head), s.State)
			case "pending":
				if pending == nil {
					pending = &st.Statuses[i]
				}
			}
		}
		missing := ""
		for _, name := range required {
			if !slices.ContainsFunc(st.Statuses, func(s CommitStatus) bool { return s.Context == name || strings.HasPrefix(s.Context, name+" ") }) {
				missing = name
				break
			}
		}
		waited := time.Since(start)
		switch {
		case pending != nil && waited >= landChecksBound:
			return fmt.Sprintf("the check “%s” of its head %s has the state pending", pending.Context, ShortSHA(head))
		case missing != "" && waited >= landChecksBound:
			return fmt.Sprintf("the check “%s” of its head %s has not reported within %s", missing, ShortSHA(head), landChecksBound)
		case pending == nil && missing == "" && (len(st.Statuses) > 0 || waited >= landNoStatusGrace):
			return ""
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("the checks of its head %s did not read: %v", ShortSHA(head), ctx.Err())
		case <-time.After(landChecksPoll):
		}
	}
}
