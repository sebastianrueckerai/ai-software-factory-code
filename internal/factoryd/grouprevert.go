package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// groupRevertSessions caps a group revert's agent card: after its second
// session a person decides, with the queue still paused.
const groupRevertSessions = 2

// salvageBranch is the fork's branch that keeps main as it stood before
// plan's group revert, the plan's work on it.
func salvageBranch(plan string) string { return "salvage/" + plan }

// groupRevertBranch reverts shas on a full clone of main as the bot,
// newest first by their place in main's first-parent history; it sorts
// shas in place in that order. A merge commit reverts with -m 1. Before
// it, main's head goes to the fork's salvage branch unless salvage is ""
// or salvageExists. A revert that fails is aborted and answered as
// conflict, and the reverts are not pushed; else they go to the fork's
// branch. It makes no API call; a test substitutes it.
var groupRevertBranch = func(ctx context.Context, f *Forgejo, repo Repo, shas []string, salvage string, salvageExists bool, branch string) (conflict string, err error) {
	dir, err := os.MkdirTemp("", "revert-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	login := basicLogin(f.BotUser, f.BotPassword)
	src := filepath.Join(dir, "src")
	if _, err := git(ctx, dir, login, "clone", "-q", "--single-branch", "--branch", prBase, f.BaseURL+"/"+repo.String()+".git", src); err != nil {
		return "", err
	}
	fork := f.BaseURL + "/" + f.Fork(repo).String() + ".git"
	if salvage != "" && !salvageExists {
		if _, err := git(ctx, src, login, "push", "-q", fork, "HEAD:refs/heads/"+salvage); err != nil {
			return "", err
		}
	}
	history, err := git(ctx, src, "", "rev-list", "--first-parent", "HEAD")
	if err != nil {
		return "", err
	}
	place := map[string]int{}
	for i, sha := range strings.Fields(history) {
		place[sha] = i
	}
	for _, sha := range shas {
		if _, ok := place[sha]; !ok {
			return "", fmt.Errorf("commit %s is not on %s's first-parent history", sha, prBase)
		}
	}
	slices.SortFunc(shas, func(a, b string) int { return cmp.Compare(place[a], place[b]) })
	for _, sha := range shas {
		parents, err := git(ctx, src, "", "show", "-s", "--format=%P", sha)
		if err != nil {
			return "", err
		}
		args := []string{"-c", "user.name=" + f.BotUser, "-c", "user.email=" + f.BotUser + "@factory", "revert", "--no-edit"}
		if len(strings.Fields(parents)) > 1 {
			args = append(args, "-m", "1")
		}
		if _, err := git(ctx, src, "", append(args, sha)...); err != nil {
			if _, err := git(ctx, src, "", "revert", "--abort"); err != nil {
				return "", err
			}
			return sha, nil
		}
	}
	// forced: a retry before the revert's card exists finds its branch
	_, err = git(ctx, src, login, "push", "-q", fork, "+HEAD:refs/heads/"+branch)
	return "", err
}

// RevertGroup takes plan p's merged cards off main while its project's
// merge queue is paused for them; why says what asks for it. The plain
// git revert goes to a pull request whose factory pull request card merges
// on green checks; a revert that conflicts files an agent card that
// resolves it. The card holds the pause. A call that finds p's own pause,
// a retry's, goes on with its card, and once the card exists answers from
// it. Any other pause refuses (ErrNoReplan). An error before the card
// exists lifts the pause. The answer is the note for the person.
func (r *Runner) RevertGroup(ctx context.Context, p Task, cards []Task, why string) (string, error) {
	repo, err := SplitRepo(p.Repository)
	if err != nil {
		return "", err
	}
	if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
		return "", err
	}
	id, err := r.revertPause(ctx, p, cards)
	if err != nil {
		return "", err
	}
	card, ok, err := r.store.Task(ctx, id)
	if err != nil {
		return "", err
	}
	issue := card.IssueNumber
	if !ok {
		if issue, err = r.fileGroupRevert(ctx, p, repo, cards, why, id); err != nil {
			if cerr := r.store.ClearQueuePause(context.WithoutCancel(ctx), p.Repository); cerr != nil {
				log.Printf("plan %s: lift the merge queue's pause of %s: %v", p.ID, p.Repository, cerr)
			}
			return "", err
		}
	}
	return fmt.Sprintf("Plan #%d's %d merged cards come off main: the merge queue waits for the revert, card #%d.", p.IssueNumber, len(cards), issue), nil
}

// revertPause pauses p's project's merge queue for the revert of cards and
// answers the id of the revert's card: a new one, or the card of a pause
// that names p already.
func (r *Runner) revertPause(ctx context.Context, p Task, cards []Task) (string, error) {
	pause := QueuePause{Project: p.Repository, Since: time.Now(), Plan: p.ID, RevertTask: NewTaskID()}
	for _, c := range cards {
		pause.Reverts = append(pause.Reverts, c.ID)
	}
	paused, err := r.store.PauseQueueForRevert(ctx, pause)
	if err != nil {
		return "", err
	}
	if paused {
		return pause.RevertTask, nil
	}
	standing, ok, err := r.store.QueuePause(ctx, p.Repository)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", fmt.Errorf("the merge queue's pause of %s ended while plan #%d's revert began", p.Repository, p.IssueNumber)
	case standing.Plan == p.ID:
		return standing.RevertTask, nil
	}
	cause := "main is red since " + standing.Since.UTC().Format(time.RFC3339)
	if standing.Plan != "" {
		other, _, err := r.task(ctx, standing.Plan)
		if err != nil {
			return "", err
		}
		cause = fmt.Sprintf("plan #%d is being taken off main", other.IssueNumber)
	}
	return "", ErrNoReplan(fmt.Sprintf("The merge queue of %s is paused: %s. Replan without taking work off main, or wait until the queue goes on.", p.Repository, cause))
}

// fileGroupRevert reverts cards' merges on the branch of card id and files
// the card on main's head: a factory pull request card for the plain
// revert, an agent card for one that conflicts. It records the
// group_revert step on plan p and answers the card's issue.
func (r *Runner) fileGroupRevert(ctx context.Context, p Task, repo Repo, cards []Task, why, id string) (int64, error) {
	project, ok, err := r.store.ProjectByRepository(ctx, p.Repository)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("no project has the repository %s", p.Repository)
	}
	fork, salvage, branch := r.forgejo.Fork(repo), salvageBranch(p.ID), "factory/"+id
	_, err = r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, salvage)
	salvageExists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	main, err := r.forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return 0, err
	}
	bySHA := map[string]Task{}
	shas := make([]string, len(cards))
	for i, c := range cards {
		shas[i], bySHA[c.MergeCommit] = c.MergeCommit, c
	}
	conflict, err := groupRevertBranch(ctx, r.forgejo, repo, shas, salvage, salvageExists, branch)
	if err != nil {
		return 0, fmt.Errorf("revert plan #%d's merges: %w", p.IssueNumber, err)
	}
	merges := make([]Task, len(shas))
	for i, sha := range shas {
		merges[i] = bySHA[sha]
	}
	title := fmt.Sprintf("Revert plan #%d: %d merges, newest first", p.IssueNumber, len(merges))
	budget := r.cfg.Policy().Document.Budget
	var ref IssueRef
	if conflict == "" {
		named := make([]string, len(merges))
		for i, m := range merges {
			named[i] = fmt.Sprintf("#%d %s", m.IssueNumber, m.MergeCommit)
		}
		body := fmt.Sprintf("Takes plan #%d's merged cards off main, newest first: %s.\n\nWhy: %s\n\n"+
			"The merge queue of %s waits for this revert. Branch %s of the bot's fork keeps main as it was before it.",
			p.IssueNumber, strings.Join(named, ", "), why, p.Repository, salvage)
		var pr *PullRequestRef
		if pr, err = r.forgejo.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, fork, branch); err != nil {
			return 0, err
		}
		if pr == nil {
			created, err := r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, title, body)
			if err != nil {
				return 0, err
			}
			pr = &created
		}
		_, ref, err = FileFactoryPRTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, budget, project, title, body, nil, Task{
			ID: id, RiskClass: "standard", Objective: title + "\n\n" + body, SHA: main, Source: SourceRevert,
		}, pr.Number)
	} else {
		body := groupRevertObjective(p, merges, fmt.Sprintf("The plain revert stopped at %s: resolve each conflict so the later changes on main stay and main builds green.", conflict))
		_, ref, err = FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, project, title, body, nil, Task{
			ID: id, RiskClass: "standard", BudgetEUR: policy.CardBudget(budget, project.Budget, "", false, 0), Objective: title + "\n\n" + body, SHA: main, Source: SourceGroupRevert,
		})
	}
	if err != nil {
		return 0, err
	}
	r.record(ctx, p.ID, "group_revert", map[string]any{"card": id, "shas": shas, "salvage": salvage, "conflict": conflict})
	return ref.Number, nil
}

// groupRevertObjective is the body of the agent card that takes plan p's
// merges off main, newest first; situation is what the card starts from.
func groupRevertObjective(p Task, merges []Task, situation string) string {
	named := make([]string, len(merges))
	for i, m := range merges {
		named[i] = fmt.Sprintf("%s (#%d)", m.MergeCommit, m.IssueNumber)
	}
	return fmt.Sprintf("Take plan #%d's cards off main: revert these merge commits, newest first, with git revert -m 1 for a merge commit: %s. %s Change nothing else.",
		p.IssueNumber, strings.Join(named, ", "), situation)
}
