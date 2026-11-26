package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/follow"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The facts of a failure card's ask: factFollowPart names the part, which
// the card's Try again reads, and factFollowHeld says the repository and
// the cluster hold the old versions, so nothing waits for a human's look.
const (
	factFollowPart = "follow_part"
	factFollowHeld = "follow_held"
)

// FollowFailedQuestion is the failure card of a move: the part and its
// versions, what failed with its text, whether the return held, and what
// the part's maker or the project says about it. Try again opens a new pull
// request for the same versions, and End the card leaves the target tried.
// A card whose move left the old versions in place closes by itself once
// its target is gone (followTargetGone).
func FollowFailedQuestion(in FollowMoveInput, f FollowFailure) HumanQuestion {
	var ret string
	switch {
	case !f.Merged:
		ret = "The pull request did not merge, so nothing was returned."
	case f.Returned.Held:
		ret = fmt.Sprintf("The factory returned the move: pull request #%d reverted the merge %s, and its checks and Flux's statuses are success. The repository and the cluster say the old versions again.", f.Returned.PR, ShortSHA(f.Commit))
	case f.Returned.Why != "":
		ret = fmt.Sprintf("The factory could not return the move: %s.", strings.TrimSuffix(f.Returned.Why, "."))
		if f.Returned.PR != 0 {
			ret += fmt.Sprintf(" Pull request #%d, the revert, stands.", f.Returned.PR)
		}
		ret += " The cluster may still run the new versions: look at it first."
	default:
		ret = fmt.Sprintf("The move merged as %s, and the factory did not return it: the judgment itself did not run. Look at the cluster, and revert the merge by hand if it needs it.", ShortSHA(f.Commit))
	}
	info := fmt.Sprintf("The follow routine moved part %s (lane %s) in pull request #%d: %s.\n\nThe move failed: %s\n\n%s",
		in.Part, in.Lane, in.PR, strings.Join(in.Moves, "; "), f.Why, ret)
	if in.After != "" {
		info += "\n\nAbout this part: " + strings.TrimSpace(in.After)
	}
	if in.Notes != "" {
		info += "\n\nThe maker's release notes: " + in.Notes
	}
	info += "\n\nTry the move again opens a new pull request for the same versions and runs it as before. End the card leaves the part as it is: this target stays tried, and the next version of the lists makes a new target. The routine's runs wait until this card is answered, or until the lists carry another version of the part: the card then closes by itself."
	b := SourceBriefs[SourceFollowFailed](BriefFacts{Name: in.Part})
	return HumanQuestion{
		Ask:      b.Title,
		Why:      b.Why,
		Info:     info,
		Choices:  choices(ActRetry+"/"+variantFollow, ActClose),
		Happened: fmt.Sprintf("Part %s did not move: %s.", in.Part, strings.TrimSuffix(strings.Join(in.Moves, "; "), ".")),
		Waits:    "The follow routine's runs wait for this card.",
		Source:   "the follow routine",
	}
}

// FollowFail files the failure card of a move that failed, and closes its pull
// request when it stands open: a head that failed its checks, or one a human
// never merged, is no one's to merge any more. The one card of the project
// stands: a second failure while one is open files none.
func (r *Runner) FollowFail(ctx context.Context, in FollowMoveInput, f FollowFailure) error {
	p, ok, err := r.Routines.Project(ctx, in.Project)
	if err != nil || !ok {
		return errors.Join(err, terminalError("move %s: no project %s in the store", in.Part, in.Project))
	}
	repo := p.Repo()
	if !f.Merged {
		if pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, in.PR); err == nil && pr.State == "open" {
			if err := r.forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, in.PR); err != nil {
				return err
			}
		}
	}
	if _, standing, err := r.Follow.Cards.StandingQuestion(ctx, p.Repository, followMarker); err != nil || standing {
		return err
	}
	card, err := EscalateToHumanWith(ctx, r.store, r.forgejo, repo, FollowFailedQuestion(in, f), "the follow routine", Task{Source: SourceFollowFailed},
		map[string]any{factFollowPart: in.Part, factFollowHeld: !f.Merged || f.Returned.Held})
	if err != nil {
		return err
	}
	r.followStep(ctx, repo, in, "failed: "+f.Why)
	r.followStep(ctx, repo, in, "filed the failure card "+ShownName(card)+".")
	return nil
}

// followTargetGone closes failure card card once nothing is left to try
// again: its part is not behind the lists any more, or the lists carry
// another version, a target that has no attempt yet. It reports the close,
// after which the routine's runs go on. A card whose move the factory could
// not return stays: a human looks at the cluster first.
func (r *Runner) followTargetGone(ctx context.Context, p Project, card Task) (bool, error) {
	c, err := r.store.NewestAskCase(ctx, card.ID)
	if err != nil {
		return false, err
	}
	part, _ := c.Facts[factFollowPart].(string)
	if held, _ := c.Facts[factFollowHeld].(bool); !held || part == "" {
		return false, nil
	}
	tree, err := r.followBehind(ctx, p)
	if err != nil {
		return false, err
	}
	why := "Part " + part + " is not behind the lists any more"
	if i := slices.IndexFunc(tree.results, func(res follow.Result) bool { return res.Part.Name == part }); i >= 0 && tree.results[i].Behind() > 0 {
		repo := p.Repo()
		pr, err := r.forgejo.FindPullRequestByHead(ctx, repo.Owner, repo.Name, prBase, r.forgejo.Fork(repo), followBranch(part, tree.results[i].Changes, 1))
		if err != nil || pr != nil {
			return false, err
		}
		why = "The lists carry another version of part " + part + ", which the routine's next run moves"
	}
	msg := why + ", so there is nothing left to try again. Closing this issue."
	if err := r.timeline.WithdrawAsk(ctx, card, msg, nil); err != nil {
		return false, err
	}
	return true, closeIssueAsFactory(ctx, r.store, r.forgejo, card, msg)
}

// FollowReturn takes a merged move back: the revert of its merge commit goes
// to a pull request of the bot, which is checked and merged as the move's
// was, and Flux's statuses on the revert must be success. Held says the
// repository and the cluster have the old versions again.
func (r *Runner) FollowReturn(ctx context.Context, in FollowMoveInput, landed FollowLanded) (FollowReturned, error) {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return FollowReturned{}, err
	}
	fork, branch := r.forgejo.Fork(repo), "factory/follow-return-"+ShortSHA(landed.Commit)
	title := "Return the follow routine's move of part " + in.Part
	pr, err := r.forgejo.FindPullRequestByHead(ctx, repo.Owner, repo.Name, prBase, fork, branch)
	if err != nil {
		return FollowReturned{}, err
	}
	var state PullRequestRef
	switch {
	case pr != nil && pr.State != "open" && !pr.Merged:
		return FollowReturned{PR: pr.Number, Why: fmt.Sprintf("pull request #%d was closed without a merge", pr.Number)}, nil
	case pr != nil && pr.Merged:
		state = *pr
	default:
		// the revert is made again and pushed over the branch, so the head
		// that is checked and merged is the one read right after the push
		if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
			return FollowReturned{}, err
		}
		conflict, err := groupRevertBranch(ctx, r.forgejo, repo, []string{landed.Commit}, "", false, branch)
		if err != nil {
			return FollowReturned{}, err
		}
		if conflict != "" {
			return FollowReturned{Why: "the revert of " + ShortSHA(conflict) + " conflicts with main, which moved on since the merge"}, nil
		}
		head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, branch)
		if err != nil {
			return FollowReturned{}, err
		}
		if pr == nil {
			body := fmt.Sprintf("The judgment of the follow routine's move of part %s (pull request #%d) failed, so this pull request reverts its merge %s.", in.Part, in.PR, ShortSHA(landed.Commit))
			created, err := r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, title, body)
			if err != nil {
				return FollowReturned{}, err
			}
			pr = &created
		}
		r.followStep(ctx, repo, in, fmt.Sprintf("the judgment failed: pull request #%d reverts the merge.", pr.Number))
		why := r.forgejo.LandPullRequest(ctx, repo, pr.Number, Attribution{Role: "follow"}, title, headVet(head))
		if state, err = r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
			return FollowReturned{}, err
		}
		if !state.Merged {
			return FollowReturned{PR: pr.Number, Why: cmp.Or(why, fmt.Sprintf("pull request #%d was closed without a merge", pr.Number))}, nil
		}
	}
	if why, err := r.followChain(ctx, repo, FollowLanded{Commit: state.MergeCommit, At: r.Clock()}); err != nil || why != "" {
		return FollowReturned{PR: state.Number, Why: "the revert merged as " + ShortSHA(state.MergeCommit) + ", and " + why}, err
	}
	r.followStep(ctx, repo, in, "the return held: the chain is green on "+ShortSHA(state.MergeCommit)+".")
	return FollowReturned{PR: state.Number, Held: true}, nil
}

// RetryFollow is the failure card's Try again, the work of its answer: a
// new attempt at the card's part, for the versions the lists carry now. The
// attempt rule lets it through by a counter on the branch: the first free
// one of the target. A move that runs, or a part that is not behind any
// more, answers a refusal that names it. The card closes once the new pull
// request opens.
func (r *Runner) RetryFollow(ctx context.Context, t Task) error {
	if r.Follow == nil {
		return errors.New("this factoryd runs no follow routine")
	}
	v := r.cfg.Policy()
	name, cfg, ok := v.ProjectOf(t.Repository)
	if !ok {
		return fmt.Errorf("no project has the repository %s", t.Repository)
	}
	i := slices.IndexFunc(cfg.Routines, func(rt policy.Routine) bool { return rt.Pass == policy.PassFollow })
	if i < 0 {
		return fmt.Errorf("project %s has no follow routine", name)
	}
	c, err := r.store.NewestAskCase(ctx, t.ID)
	if err != nil {
		return err
	}
	part, _ := c.Facts[factFollowPart].(string)
	if part == "" {
		return fmt.Errorf("the card names no part")
	}
	p, ok, err := r.Routines.Project(ctx, name)
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("no project %s in the store", name))
	}
	if mv, running, err := r.Follow.Moves.RunningFollowMove(ctx, name); err != nil || running {
		return errors.Join(err, fmt.Errorf("the move of part %s runs, pull request #%d: wait for it", mv.Part, mv.PR))
	}
	tree, err := r.followBehind(ctx, p)
	if err != nil {
		return err
	}
	for _, res := range tree.results {
		if res.Part.Name != part || res.Behind() == 0 {
			continue
		}
		if line, err := r.storeLacks(ctx, tree, res); err != nil {
			return err
		} else if line != "" {
			return errors.New(line)
		}
		repo, fork := p.Repo(), r.forgejo.Fork(p.Repo())
		attempt := 1
		for ; ; attempt++ {
			pr, err := r.forgejo.FindPullRequestByHead(ctx, repo.Owner, repo.Name, prBase, fork, followBranch(part, res.Changes, attempt))
			if err != nil {
				return err
			}
			if pr == nil {
				break
			}
		}
		if _, err := r.startMove(ctx, p, cfg.Routines[i], 0, res, followBranch(part, res.Changes, attempt), nil); err != nil {
			return err
		}
		return closeIssueAsFactory(ctx, r.store, r.forgejo, t,
			"A human pressed “Try the move again”: attempt "+fmt.Sprint(attempt)+" of part "+part+" opened its pull request. This issue closes.")
	}
	return closeIssueAsFactory(ctx, r.store, r.forgejo, t, "A human pressed “Try the move again”, and part "+part+" is not behind the lists any more: there is nothing to try. This issue closes.")
}
