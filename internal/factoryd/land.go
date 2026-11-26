package factoryd

import (
	"context"
	"fmt"
	"log"
	"regexp"
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
// "" when it passes; nil vets nothing. LandPullRequest answers why a human merges it, "" when no
// human needs to: it merged, or it closed.
func (f *Forgejo) LandPullRequest(ctx context.Context, repo Repo, number int64, a Attribution, subject string, vet func(PullRequestRef) string) string {
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
	if why := f.awaitChecks(ctx, repo, pr.HeadSHA); why != "" {
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
// landChecksBound, or they did not read. "" when none fails.
func (f *Forgejo) awaitChecks(ctx context.Context, repo Repo, head string) string {
	start := time.Now()
	var noChecks *bool // whether the repository has no pull request check, read once
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
		waited := time.Since(start)
		if pending == nil && len(st.Statuses) == 0 && waited < landNoStatusGrace {
			if noChecks == nil {
				none := !f.runsOnPullRequests(ctx, repo)
				noChecks = &none
			}
			if *noChecks {
				return ""
			}
		}
		switch {
		case pending != nil && waited >= landChecksBound:
			return fmt.Sprintf("the check “%s” of its head %s has the state pending", pending.Context, ShortSHA(head))
		case pending == nil && (len(st.Statuses) > 0 || waited >= landNoStatusGrace):
			return ""
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("the checks of its head %s did not read: %v", ShortSHA(head), ctx.Err())
		case <-time.After(landChecksPoll):
		}
	}
}

// pullRequestTrigger finds a pull request trigger in a workflow file.
var pullRequestTrigger = regexp.MustCompile(`\bpull_request(_target)?\b`)

// runsOnPullRequests reports whether any workflow of repo's main, listed as
// the chain poller lists them, names a pull request trigger: only then does
// a head's status come a moment after its pull request opens. A list or a
// file that does not read says yes, so the grace stays.
func (f *Forgejo) runsOnPullRequests(ctx context.Context, repo Repo) bool {
	files, err := f.WorkflowList(ctx, repo)
	if err != nil {
		return true
	}
	for _, name := range files {
		body, err := f.FileAt(ctx, repo.Owner, repo.Name, prBase, name)
		if err != nil || pullRequestTrigger.Match(body) {
			return true
		}
	}
	return false
}
