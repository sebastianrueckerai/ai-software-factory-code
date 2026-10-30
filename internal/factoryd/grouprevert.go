package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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

// The group revert's steps. On its plan's timeline group_revert records
// card's git work, its merges' shas newest first, and group_reverted or
// group_revert_ended the revert's end, landed or not. On a plain revert
// card's own timeline group_revert_handover names the agent card that took
// it over, its checks red.
const (
	stepGroupRevert         = "group_revert"
	stepGroupRevertHandover = "group_revert_handover"
	stepGroupReverted       = "group_reverted"
	stepGroupRevertEnded    = "group_revert_ended"
)

// revertRefileAfter is how long a plan's pause may name a revert card with
// no row before RevertPausePass files the card again: RevertGroup files
// it within minutes of its pause, so a card still missing was lost.
const revertRefileAfter = 10 * time.Minute

// revertHookOwns is how long after a revert card's end RevertPausePass
// leaves its pause to the end's own hook (revertEnded in finish).
const revertHookOwns = time.Minute

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
// it. Any other pause refuses (ErrNoReplan). An error lifts the pause,
// and closes the pull request the call opened, unless the card exists.
// The answer is the note for the person.
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
	card, filed, err := r.store.Task(ctx, id)
	issue, opened := card.IssueNumber, int64(0)
	if err == nil && !filed {
		issue, opened, err = r.fileGroupRevert(ctx, p, repo, cards, why, id)
	}
	if err != nil {
		if unfiled(ctx, r.store, id) {
			closeOpened(ctx, r.forgejo, repo, opened)
			if cerr := r.store.ClearRevertPause(context.WithoutCancel(ctx), p.Repository, id); cerr != nil {
				log.Printf("plan %s: lift the merge queue's pause of %s: %v", p.ID, p.Repository, cerr)
			}
		}
		return "", err
	}
	verb := "come"
	if len(cards) == 1 {
		verb = "comes"
	}
	return fmt.Sprintf("Plan #%d's %s %s off main: the merge queue waits for the revert, card #%d.", p.IssueNumber, plural(len(cards), "merged card"), verb, issue), nil
}

// unfiled tells that card id has no row, read past ctx's end: a call that
// failed before it filed the card undoes what it began. A read that fails
// tells nothing, and nothing is undone.
func unfiled(ctx context.Context, store interface {
	Task(ctx context.Context, id string) (Task, bool, error)
}, id string) bool {
	_, ok, err := store.Task(context.WithoutCancel(ctx), id)
	if err != nil {
		log.Printf("card %s: read its row: %v: nothing is undone", id, err)
	}
	return err == nil && !ok
}

// closeOpened closes pull request pr of repo, which a call that failed
// before it filed the pull request's card opened, past ctx's end; 0 is
// none. A failure is logged.
func closeOpened(ctx context.Context, f *Forgejo, repo Repo, pr int64) {
	if pr == 0 {
		return
	}
	if err := f.ClosePullRequest(context.WithoutCancel(ctx), repo.Owner, repo.Name, pr); err != nil {
		log.Printf("close pull request #%d of %s: %v", pr, repo, err)
	}
}

// revertPause pauses p's project's merge queue for the revert of cards and
// answers the id of the revert's card: a new one, or the card of a pause
// that names p and the same cards already, while that card has not ended.
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
	set := func(ids []string) []string { return slices.Compact(slices.Sorted(slices.Values(ids))) }
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", fmt.Errorf("the merge queue's pause of %s ended while plan #%d's revert began", p.Repository, p.IssueNumber)
	case standing.Plan == p.ID && slices.Equal(set(standing.Reverts), set(pause.Reverts)):
		card, filed, err := r.store.Task(ctx, standing.RevertTask)
		if err != nil {
			return "", err
		}
		if !filed || !Final(card.Status) {
			return standing.RevertTask, nil
		}
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
// group_revert step on plan p right after git, so a retry may record it
// again; a step that fails to write fails the call, as a red plain
// revert's hand-over reads the merges' order from it. It answers the
// card's issue, and the pull request it opened, 0 for none, also with an
// error.
func (r *Runner) fileGroupRevert(ctx context.Context, p Task, repo Repo, cards []Task, why, id string) (issue, opened int64, err error) {
	project, ok, err := r.store.ProjectByRepository(ctx, p.Repository)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, fmt.Errorf("no project has the repository %s", p.Repository)
	}
	fork, salvage, branch := r.forgejo.Fork(repo), salvageBranch(p.ID), "factory/"+id
	_, err = r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, salvage)
	salvageExists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, 0, err
	}
	main, err := r.forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return 0, 0, err
	}
	bySHA := map[string]Task{}
	shas := make([]string, len(cards))
	for i, c := range cards {
		shas[i], bySHA[c.MergeCommit] = c.MergeCommit, c
	}
	conflict, err := groupRevertBranch(ctx, r.forgejo, repo, shas, salvage, salvageExists, branch)
	if err != nil {
		return 0, 0, fmt.Errorf("revert plan #%d's merges: %w", p.IssueNumber, err)
	}
	if err := r.timeline.Append(ctx, p.ID, stepGroupRevert, map[string]any{"card": id, "shas": shas, "salvage": salvage, "conflict": conflict}); err != nil {
		return 0, 0, fmt.Errorf("record %s: %w", stepGroupRevert, err)
	}
	merges := make([]Task, len(shas))
	for i, sha := range shas {
		merges[i] = bySHA[sha]
	}
	title := groupRevertTitle(p, len(merges))
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
			return 0, 0, err
		}
		if pr == nil {
			var created PullRequestRef
			if created, err = r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, title, body); err != nil {
				return 0, 0, err
			}
			pr, opened = &created, created.Number
		}
		_, ref, err = FileFactoryPRTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, r.cfg.Policy().Document.Budget, project, title, body, nil, Task{
			ID: id, RiskClass: "standard", Objective: title + "\n\n" + body, SHA: main, Source: SourceRevert,
		}, pr.Number)
	} else {
		_, ref, err = r.fileRevertAgent(ctx, p, project, merges,
			fmt.Sprintf("The plain revert stopped at %s: resolve each conflict so the later changes on main stay and main builds green.", conflict), id, main)
	}
	if err != nil {
		return 0, opened, err
	}
	return ref.Number, opened, nil
}

// fileRevertAgent files agent card id, based on base, that takes plan p's
// merges off main, newest first; situation is what the card starts from.
func (r *Runner) fileRevertAgent(ctx context.Context, p Task, project Project, merges []Task, situation, id, base string) (Task, IssueRef, error) {
	title, body := groupRevertTitle(p, len(merges)), groupRevertObjective(p, merges, situation)
	return FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, project, title, body, nil, Task{
		ID: id, RiskClass: "standard", BudgetEUR: policy.CardBudget(r.cfg.Policy().Document.Budget, project.Budget, "", false, 0),
		Objective: title + "\n\n" + body, SHA: base, Source: SourceGroupRevert,
	})
}

// groupRevertTitle is the title of the revert of plan p's n merges.
func groupRevertTitle(p Task, n int) string {
	return fmt.Sprintf("Revert plan #%d: %s, newest first", p.IssueNumber, plural(n, "merge"))
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

// revertEnded ends the group revert whose card t, the plain revert's or
// its agent card's, ended final; merged says its change landed on main, as
// a merge commit on t does. It acts only while t's project's pause names a
// plan and t: a main-red pause, or a pause that moved on to another card,
// passes untouched. A revert that landed marks the plan's cards reverted.
// Any other end (a person closed the card, the agent card ended, a hard
// failure such as the wall clock or an error) tells the plan once that its
// cards stay on main. At the session cap the agent card asks a person
// first, as spec H wants; a hard failure lifts the pause with no person's
// decision, since nothing else could lift it. Then a replan that waits
// with off_main for the revert sends the plan to its planner, and the
// pause goes last, the queue's waiting heads woken.
// A step that fails leaves the pause for RevertPausePass, which runs it
// all again; a comment or a reopen Forgejo refuses is logged and passed
// instead, as no retry changes it and the queue must go on. Safe to
// repeat: MarkReverted sets only nulls, the plan comment goes once, the
// reopen takes only a plan that waits, and the clear of t's pause is
// idempotent. finish runs it twice for a merged card with a chain (merged,
// then EndCovered): the first call clears the pause, so the second finds
// no pause naming the card.
func (r *Runner) revertEnded(ctx context.Context, t Task, final string, merged bool) {
	if t.Source != SourceRevert && t.Source != SourceGroupRevert {
		return
	}
	pause, paused, err := r.store.QueuePause(ctx, t.Repository)
	switch {
	case err != nil:
		log.Printf("task %s: the revert's end: the pause of %s: %v", t.ID, t.Repository, err)
	case paused && pause.Plan != "" && pause.RevertTask == t.ID:
		if err := r.endGroupRevert(ctx, t, pause, final, merged || t.MergeCommit != ""); err != nil {
			log.Printf("task %s: the end of plan %s's revert: %v: the minute pass tries again", t.ID, pause.Plan, err)
		}
	}
}

// endGroupRevert is revertEnded's work on pause, which names t.
func (r *Runner) endGroupRevert(ctx context.Context, t Task, pause QueuePause, final string, landed bool) error {
	p, repo, err := r.task(ctx, pause.Plan)
	if err != nil {
		return err
	}
	step, detail := stepGroupReverted, map[string]any{"card": t.ID, "cards": pause.Reverts}
	if landed {
		now := time.Now()
		for _, id := range pause.Reverts {
			if _, err := r.store.MarkReverted(ctx, id, now); err != nil {
				return err
			}
		}
	} else {
		step, detail = stepGroupRevertEnded, map[string]any{"card": t.ID, "state": final}
		msg := fmt.Sprintf("factoryd: the revert of plan #%d did not land (card #%d ended %s): its cards stay on main.", p.IssueNumber, t.IssueNumber, final)
		if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, p.IssueNumber, msg); forgejoTerminal(err) {
			log.Printf("plan %s: Forgejo refused the comment that its revert did not land: %v", p.ID, err)
		} else if err != nil {
			return err
		}
	}
	replan, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return err
	}
	if pending && replan.OffMain && slices.Contains(reopenFrom, p.Status) {
		if err := r.reopenPlan(ctx, p); forgejoTerminal(err) {
			log.Printf("plan %s: Forgejo refused a step of its reopen after the revert: %v", p.ID, err)
		} else if err != nil {
			return err
		}
	}
	r.record(ctx, p.ID, step, detail)
	if err := r.store.ClearRevertPause(ctx, pause.Project, pause.RevertTask); err != nil {
		return err
	}
	signalQueueHeads(ctx, r.store, r.cfg.Workflows)
	return nil
}

// RevertPausePass heals, each minute, every pause that names a plan. One
// whose revert card ended (final, or merged) revertHookOwns ago or more
// runs revertEnded with the card's state: a lost hook (a store error, a
// factoryd that stopped between the card's final write and the hook, a
// retried finish that returns early on a final card) heals within two
// minutes, and the hook runs alone in the minute it owns. One whose card
// has no row revertRefileAfter after the pause began lost its filing:
// RevertGroup, called again with the pause's plan and cards, resumes the
// pause and files the card. It runs in the Runner, not in the Roller's
// mainState: the Roller polls only the factory's own repository, and a
// plan's pause can stand in any project.
func (r *Runner) RevertPausePass(ctx context.Context) {
	pauses, err := r.store.PlanQueuePauses(ctx)
	if err != nil {
		log.Printf("revert pause pass: %v", err)
		return
	}
	for _, pause := range pauses {
		card, filed, err := r.store.Task(ctx, pause.RevertTask)
		switch {
		case err != nil:
			log.Printf("revert pause pass: the revert card %s of %s: %v", pause.RevertTask, pause.Project, err)
		case filed && (Final(card.Status) || card.Status == "merged"):
			if card.FinishedAt == nil || time.Since(*card.FinishedAt) >= revertHookOwns {
				r.revertEnded(ctx, card, card.Status, card.Status == "merged")
			}
		case !filed && time.Since(pause.Since) >= revertRefileAfter:
			if note, err := r.refileRevert(ctx, pause); err != nil {
				log.Printf("revert pause pass: file plan %s's revert card %s again: %v", pause.Plan, pause.RevertTask, err)
			} else {
				log.Printf("revert pause pass: %s", note)
			}
		}
	}
}

// refileRevert calls RevertGroup again for pause, whose card never filed.
func (r *Runner) refileRevert(ctx context.Context, pause QueuePause) (string, error) {
	p, _, err := r.task(ctx, pause.Plan)
	if err != nil {
		return "", err
	}
	cards := make([]Task, len(pause.Reverts))
	for i, id := range pause.Reverts {
		if cards[i], _, err = r.task(ctx, id); err != nil {
			return "", err
		}
	}
	return r.RevertGroup(ctx, p, cards, "factoryd files the revert's card again: its first filing did not finish")
}

// handOverRevert hands plain revert card t, whose checks are red, to an
// agent card when its project's pause names a plan and t: the agent card
// continues t's branch on t's base, the pause names it, and t ends closed
// with no person asked. The agent card's id goes on t's timeline before
// the card files, so a retry files that card and no other, and the pause
// names t until the card exists. A retry that finds the card filed only
// links the pause if it still names t, and finishes t whatever the pause
// says: the agent card may have ended and its end cleared the pause. With
// no card filed, any other pause, or none, leaves t to a person: false.
func (r *Runner) handOverRevert(ctx context.Context, t Task) (bool, error) {
	own, err := r.timeline.Objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
	if err != nil {
		return false, fmt.Errorf("the timeline of %s: %w", t.ID, err)
	}
	id, _ := lastStep(own, stepGroupRevertHandover)["card"].(string)
	pause, paused, err := r.store.QueuePause(ctx, t.Repository)
	if err != nil {
		return false, err
	}
	held := paused && pause.Plan != "" && pause.RevertTask == t.ID
	card, filed := Task{}, false
	if id != "" {
		if card, filed, err = r.store.Task(ctx, id); err != nil {
			return false, err
		}
	}
	switch {
	case filed:
	case !held:
		return false, nil
	default:
		if id == "" {
			id = NewTaskID()
			if err := r.timeline.Append(ctx, t.ID, stepGroupRevertHandover, map[string]any{"card": id}); err != nil {
				return false, fmt.Errorf("record %s: %w", stepGroupRevertHandover, err)
			}
		}
		if card, err = r.fileHandover(ctx, t, pause, id); err != nil {
			return false, err
		}
	}
	if held {
		if err := r.store.SetQueuePauseRevert(ctx, t.Repository, id); err != nil {
			return false, err
		}
	}
	return true, r.Finish(ctx, t.ID, "closed", fmt.Sprintf("the plain revert's checks failed: card #%d takes it over", card.IssueNumber))
}

// fileHandover files agent card id that takes over plain revert card t,
// whose pause names its plan and the cards it reverted: the merges come
// newest first from the plan's group_revert step of t, and the card's
// branch first continues t's, as the card's issue can start it. Merges it
// cannot name, or t's branch gone, are terminal: no retry changes them,
// and t's failure lifts the pause.
func (r *Runner) fileHandover(ctx context.Context, t Task, pause QueuePause, id string) (Task, error) {
	p, _, err := r.task(ctx, pause.Plan)
	if err != nil {
		return Task{}, err
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return Task{}, fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	var shas []string
	for line := range bytes.SplitSeq(tl, []byte("\n")) {
		var s struct {
			Step, Card string
			Shas       []string
		}
		if json.Unmarshal(line, &s) == nil && s.Step == stepGroupRevert && s.Card == t.ID {
			shas = s.Shas
		}
	}
	project, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return Task{}, err
	}
	if !ok {
		return Task{}, fmt.Errorf("no project has the repository %s", t.Repository)
	}
	reverts := pause.Reverts
	bySHA := map[string]Task{}
	for _, c := range reverts {
		card, _, err := r.task(ctx, c)
		if err != nil {
			return Task{}, err
		}
		bySHA[card.MergeCommit] = card
	}
	merges := make([]Task, len(shas))
	for i, sha := range shas {
		m, ok := bySHA[sha]
		if !ok {
			return Task{}, terminalError("plan #%d's revert %s reverted %s, a merge of none of its cards %q", p.IssueNumber, t.ID, sha, reverts)
		}
		merges[i] = m
	}
	if len(merges) != len(reverts) {
		return Task{}, terminalError("plan #%d's timeline records the merges of revert %s for %d of its %d cards", p.IssueNumber, t.ID, len(merges), len(reverts))
	}
	if _, err := r.ContinueBranch(ctx, t, Task{ID: id}); errors.Is(err, ErrNoBranch) {
		return Task{}, terminalError("continue the branch of %s: %v", t.ID, err)
	} else if err != nil {
		return Task{}, fmt.Errorf("continue the branch of %s: %w", t.ID, err)
	}
	card, _, err := r.fileRevertAgent(ctx, p, project, merges, "The plain revert is on this card's branch; its checks failed: make main build green.", id, t.SHA)
	return card, err
}
