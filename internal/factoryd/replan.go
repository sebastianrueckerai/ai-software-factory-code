package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// stepReplan is the timeline step of a replan, on the plan card or on the
// direct card that goes to the planner: it is pending until the planner
// files its revision (replanned), the switch is refused (to_plan_refused),
// or the revert it waits for with off_main cannot start (off_main_failed).
const stepReplan = "replan"

// stepOffMainFailed ends a replan whose revert could not start.
const stepOffMainFailed = "off_main_failed"

// replanClosers are the steps that end a pending replan.
var replanClosers = []string{"replanned", "to_plan_refused", stepOffMainFailed}

// The origins of a replan: a person's, the cut of a running card, or the
// next round of a plan whose round is done.
const (
	ReplanPerson = "person"
	ReplanCut    = "cut"
	ReplanRound  = "round"
)

// ReplanStep is the replan step's detail. Card is the card whose work goes
// back (the cut card, or a direct card replanned as itself), Stops the
// cards the replan stops, OffMain that the plan's merged cards come off
// main first (the revert's end sends the plan to its planner), Revision
// the plan's newest revision at the ask (0: none), Round the planning
// round the replan files (0: the newest revision's), Time when the step
// was written.
type ReplanStep struct {
	Origin   string    `json:"origin"`
	By       string    `json:"by,omitempty"`
	Comment  string    `json:"comment,omitempty"`
	Card     string    `json:"card,omitempty"`
	Work     string    `json:"work,omitempty"`
	Stops    []string  `json:"stops,omitempty"`
	OffMain  bool      `json:"off_main,omitempty"`
	Revision int       `json:"revision"`
	Round    int       `json:"round,omitempty"`
	Time     time.Time `json:"time"`
}

// replanRuns are the statuses of a plan whose replan runs.
var replanRuns = []string{"queued", "running", "waiting-human"}

// pendingReplan is the last replan step on p's timeline while neither
// closer follows it.
func (r *Runner) pendingReplan(ctx context.Context, p Task) (ReplanStep, bool, error) {
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return ReplanStep{}, false, fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	return pendingIn(tl, p.ID)
}

// pendingIn is pendingReplan of plan id's timeline tl. Its Stops hold
// those of each group revert's end since: an off-main revert's end stops
// the cards that started while it ran too.
func pendingIn(tl []byte, id string) (ReplanStep, bool, error) {
	line := openStep(tl, stepReplan, replanClosers...)
	if line == nil {
		return ReplanStep{}, false, nil
	}
	var step ReplanStep
	if err := json.Unmarshal(line, &step); err != nil {
		return ReplanStep{}, false, fmt.Errorf("the replan step of %s: %w", id, err)
	}
	for l := range bytes.SplitSeq(tl[bytes.LastIndex(tl, line)+len(line):], []byte("\n")) {
		var end struct {
			Step  string   `json:"step"`
			Stops []string `json:"stops"`
		}
		if json.Unmarshal(l, &end) != nil || end.Step != stepGroupReverted && end.Step != stepGroupRevertEnded {
			continue
		}
		for _, k := range end.Stops {
			if !slices.Contains(step.Stops, k) {
				step.Stops = append(step.Stops, k)
			}
		}
	}
	return step, true, nil
}

// revertWaits tells whether pending replan step of plan timeline tl waits
// for its revert: it has off_main, and no group revert ended since.
func revertWaits(tl []byte, step ReplanStep, pending bool) bool {
	return pending && step.OffMain && openStep(tl, stepReplan, stepGroupReverted, stepGroupRevertEnded) != nil
}

// fundPlanner tops card id up to the default card budget left for the
// planner's session; a card with that much left gets nothing. It reads
// the row itself: a caller's copy may predate another replan's funding.
func (r *Runner) fundPlanner(ctx context.Context, id string) error {
	t, _, err := r.task(ctx, id)
	if err != nil {
		return err
	}
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	short := Cents(policy.CardBudget(v.Document.Budget, project.Budget, "", false, 0)) - (Cents(t.BudgetEUR) - Cents(t.SpentEUR))
	if short <= 0 {
		return nil
	}
	_, _, err = r.store.FundCard(ctx, t.ID, float64(short)/100, nil, Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber,
		Kind: EventCardFunded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"reason": "the planner's session"}})
	return err
}

// planReplannable says plan p may go back to its planner: done, or failed
// with a revision stored (a replan whose planner failed leaves its cards
// on the board), and not closed; closed tells such a plan whose criteria
// closed it.
func (r *Runner) planReplannable(ctx context.Context, p Task) (replannable, closed bool, err error) {
	switch p.Status {
	case "done":
	case "failed":
		if _, ok, err := r.store.PlanRevision(ctx, p.ID, 0); err != nil || !ok {
			return false, false, err
		}
	default:
		return false, false, nil
	}
	_, closed, err = r.store.PlanClosed(ctx, p.ID)
	return !closed, closed, err
}

// reopenFrom are the statuses reopenPlan moves a plan from.
var reopenFrom = []string{"done", "failed", "queued"}

// reopenPlan sends plan p back to its planner: queued with its counters at
// zero, money for a session, its issue open and labelled queued, and a
// new run of its workflow. Each step is safe to repeat.
func (r *Runner) reopenPlan(ctx context.Context, p Task) error {
	return r.requeuePlan(ctx, p, true)
}

// requeuePlan is reopenPlan; without openIssue it leaves p's issue as it
// is, as a round's start does: it never opens an issue a person closed.
func (r *Runner) requeuePlan(ctx context.Context, p Task, openIssue bool) error {
	if err := r.store.TransitionTask(ctx, p.ID, reopenFrom, "queued"); err != nil {
		return err
	}
	if err := r.store.SetTaskUsage(ctx, p.ID, TaskUsage{}); err != nil {
		return err
	}
	if err := r.fundPlanner(ctx, p.ID); err != nil {
		return err
	}
	if openIssue {
		if err := r.reopenIssue(ctx, p); err != nil {
			return err
		}
	}
	mirrorTaskState(ctx, r.forgejo, p, "queued")
	return r.cfg.Workflows.Reopen(ctx, p.ID, "", TaskSignal{})
}

// reopenIssue opens card t's issue again when it is closed.
func (r *Runner) reopenIssue(ctx context.Context, t Task) error {
	if t.IssueNumber == 0 {
		return nil
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return terminalError("%v", err)
	}
	closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil || !closed {
		return err
	}
	return r.forgejo.ReopenIssue(ctx, repo.Owner, repo.Name, t.IssueNumber)
}

// ReplanChildResult is ReplanChild's answer: the child's closing reason,
// or Refused, which names the plan's state and the fitting action.
type ReplanChildResult struct{ Reason, Refused string }

// ReplanChild sends the work of plan child childID, cut at its session,
// back to its plan's planner: the plan's cards that wait on the child wait
// on the plan, and the plan replans, or its running replan takes the
// child in. A plan that can take no replan refuses, and nothing is
// recorded or moved. Safe to repeat: the join of a queued plan reopens it
// again, so a reopen that failed before its run started is healed.
func (r *Runner) ReplanChild(ctx context.Context, childID, work string) (ReplanChildResult, error) {
	k, _, err := r.task(ctx, childID)
	if err != nil {
		return ReplanChildResult{}, err
	}
	if k.Source != SourcePlan || k.Parent == "" {
		return ReplanChildResult{}, terminalError("task %s belongs to no plan", childID)
	}
	p, _, err := r.task(ctx, k.Parent)
	if err != nil {
		return ReplanChildResult{}, err
	}
	work = sanitize.Inbound(work)
	// a card the newest revision drops stops for that revision, whose
	// plan may have ended since: it closes, and no second round follows
	if newest, n, err := r.planAt(ctx, p.ID, 0); err != nil {
		return ReplanChildResult{}, err
	} else if newest != nil {
		if drop, err := r.dropped(ctx, p.ID, k, *newest, map[int]*Plan{}); err != nil {
			return ReplanChildResult{}, err
		} else if drop {
			if _, err := r.timeline.AppendOnce(ctx, k.ID, "replan_stop", map[string]any{"work": work}); err != nil {
				return ReplanChildResult{}, fmt.Errorf("record replan_stop: %w", err)
			}
			if err := r.moveDependents(ctx, k, p); err != nil {
				return ReplanChildResult{}, err
			}
			return ReplanChildResult{Reason: droppedReason(n)}, nil
		}
	}
	replannable, closed, err := r.planReplannable(ctx, p)
	if err != nil {
		return ReplanChildResult{}, err
	}
	if !replannable && !slices.Contains(replanRuns, p.Status) {
		status := p.Status
		if closed {
			status = "closed"
		}
		return ReplanChildResult{Refused: fmt.Sprintf("Plan #%d is %s, so this card's work cannot go back to its planner: answer to go on with the card, or close it.",
			p.IssueNumber, status)}, nil
	}
	switch wait, err := r.revertCard(ctx, p); {
	case err != nil:
		return ReplanChildResult{}, err
	case wait != "":
		return ReplanChildResult{Refused: wait}, nil
	}
	if replannable {
		tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
		if err != nil {
			return ReplanChildResult{}, fmt.Errorf("the timeline of %s: %w", p.ID, err)
		}
		step, pending, err := pendingIn(tl, p.ID)
		if err != nil {
			return ReplanChildResult{}, err
		}
		if revertWaits(tl, step, pending) {
			return ReplanChildResult{Refused: fmt.Sprintf("Plan #%d's merged cards are coming off main, so this card's work cannot go back to its planner until the revert ends: "+
				"answer to go on with the card, or close it.", p.IssueNumber)}, nil
		}
	}
	if _, err := r.timeline.AppendOnce(ctx, k.ID, "replan_stop", map[string]any{"work": work}); err != nil {
		return ReplanChildResult{}, fmt.Errorf("record replan_stop: %w", err)
	}
	answer := ReplanChildResult{Reason: fmt.Sprintf("stopped for the replan of plan #%d", p.IssueNumber)}
	if replannable {
		newest, _, err := r.store.PlanRevision(ctx, p.ID, 0)
		if err != nil {
			return ReplanChildResult{}, err
		}
		wrote, err := r.timeline.AppendUnlessPending(ctx, p.ID, stepReplan, map[string]any{"origin": ReplanCut, "card": k.ID, "work": work,
			"stops": []string{k.ID}, "revision": newest.Revision}, replanClosers...)
		if err != nil {
			return ReplanChildResult{}, fmt.Errorf("record %s: %w", stepReplan, err)
		}
		if wrote {
			answer.Reason = fmt.Sprintf("cut: plan #%d replans its work", p.IssueNumber)
		}
	}
	// the plan is open again before the waits move onto it, or they read
	// as met; a queued plan may be a reopen that failed before its run
	// started
	if replannable || p.Status == "queued" {
		if err := r.reopenPlan(ctx, p); err != nil {
			return ReplanChildResult{}, err
		}
	}
	if err := r.moveDependents(ctx, k, p); err != nil {
		return ReplanChildResult{}, err
	}
	return answer, nil
}

// ReplanWait is ReplanStopped's answer: Stopped when the planner may run,
// else Open the stopped cards whose work still runs (stoppable); By and
// Comment are the person's name and note of their replan, or of the one a
// cut took.
type ReplanWait struct {
	Stopped     bool
	Open        []string
	By, Comment string
}

// ReplanStopped tells whether the cards plan planID's pending replan stops,
// and its started cards its newest revision drops (dropped), have ended,
// so its planner may run on what they left and a revision may file on
// their last pushes.
func (r *Runner) ReplanStopped(ctx context.Context, planID string) (ReplanWait, error) {
	p, _, err := r.task(ctx, planID)
	if err != nil {
		return ReplanWait{}, err
	}
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return ReplanWait{}, err
	}
	var w ReplanWait
	if pending {
		w.By, w.Comment = step.By, step.Comment
	}
	for _, id := range step.Stops {
		k, ok, err := r.store.Task(ctx, id)
		if err != nil {
			return ReplanWait{}, err
		}
		if ok && stoppable(k) {
			w.Open = append(w.Open, id)
		}
	}
	newest, _, err := r.planAt(ctx, p.ID, 0)
	if err != nil {
		return ReplanWait{}, err
	}
	if newest != nil {
		cards, err := r.store.PlanCards(ctx, p.ID)
		if err != nil {
			return ReplanWait{}, err
		}
		revisions := map[int]*Plan{}
		for _, k := range cards {
			if slices.Contains(w.Open, k.ID) {
				continue
			}
			if drop, err := r.dropped(ctx, p.ID, k, *newest, revisions); err != nil {
				return ReplanWait{}, err
			} else if drop {
				w.Open = append(w.Open, k.ID)
			}
		}
	}
	w.Stopped = len(w.Open) == 0
	return w, nil
}

// dropped tells whether plan card k started, still runs (stoppable), and
// has its key in the revision it started against but not in next, as
// RevisionDrops judges a stale card: a delta card, in no revision, is
// never dropped. revisions caches the plan's revisions read, by number.
func (r *Runner) dropped(ctx context.Context, planID string, k Task, next Plan, revisions map[int]*Plan) (bool, error) {
	holds := func(p *Plan) bool {
		return p != nil && slices.ContainsFunc(p.Cards, func(c PlanCard) bool { return c.Key == k.PlanKey })
	}
	if k.PlanKey == "" || k.PlanRevision == 0 || !stoppable(k) || holds(&next) {
		return false, nil
	}
	ran, read := revisions[k.PlanRevision]
	if !read {
		var err error
		if ran, _, err = r.planAt(ctx, planID, k.PlanRevision); err != nil {
			return false, err
		}
		revisions[k.PlanRevision] = ran
	}
	return holds(ran), nil
}

// droppedReason closes a card the plan's revision drops.
func droppedReason(revision int) string {
	return fmt.Sprintf("the plan's revision %d drops it", revision)
}

// stopForReplan stops card k for plan p's replan: a live session is asked
// to stop (its workflow then cuts the card through ReplanChild); a card
// with none hands its dependents to p and ends closed with why. stopped
// is false for a session that took a replan already: it stops for it.
func (r *Runner) stopForReplan(ctx context.Context, k, p Task, why string) (stopped bool, err error) {
	err = r.EscalateSession(ctx, k.ID, AgentAsk{Kind: AskReplan, Reason: why})
	switch {
	case errors.Is(err, errReplanAsked):
		return false, nil
	case !errors.Is(err, ErrNoLiveSession):
		return err == nil, err
	}
	if err := r.moveDependents(ctx, k, p); err != nil {
		return false, err
	}
	return true, r.Finish(ctx, k.ID, "closed", why)
}

// stoppable: card k's work still runs, so a replan stops it; a merged
// card's work is on main, and a closing card is ending.
func stoppable(k Task) bool {
	return !Final(k.Status) && k.Status != "merged" && k.Status != "closing"
}

// moveDependents hands the waits of plan p's cards on card k to p: the
// store's edges, then their issues' dependencies, which only mirror them,
// so a Forgejo failure there is logged.
func (r *Runner) moveDependents(ctx context.Context, k, p Task) error {
	moved, err := r.store.MoveDependents(ctx, k.ID, p.ID)
	if err != nil || len(moved) == 0 {
		return err
	}
	repo, err := SplitRepo(p.Repository)
	if err != nil {
		log.Printf("plan %s: the issue dependencies on #%d: %v", p.ID, k.IssueNumber, err)
		return nil
	}
	for _, id := range moved {
		s, ok, err := r.store.Task(ctx, id)
		if err != nil || !ok || s.IssueNumber == 0 {
			if err != nil {
				log.Printf("plan %s: the issue dependencies of %s: %v", p.ID, id, err)
			}
			continue
		}
		for _, dep := range []struct {
			issue  int64
			remove bool
		}{{p.IssueNumber, false}, {k.IssueNumber, true}} {
			if dep.issue == 0 {
				continue
			}
			if err := r.forgejo.SetIssueDependency(ctx, repo, s.IssueNumber, dep.issue, dep.remove); err != nil {
				log.Printf("plan %s: issue #%d on #%d (remove %t): %v", p.ID, s.IssueNumber, dep.issue, dep.remove, err)
			}
		}
	}
	return nil
}

// ReplanAsk is a person's Replan: who asks, and why, for the planner, and
// whether a plan's merged cards come off main first.
type ReplanAsk struct {
	By, Comment string
	OffMain     bool
}

// ErrNoReplan refuses a person's Replan: it names the state of the card or
// its plan and the fitting action.
type ErrNoReplan string

func (e ErrNoReplan) Error() string { return string(e) }

// MaxReplanComment bounds a person's replan comment, in bytes.
const MaxReplanComment = maxHandoverMessage

// ReplanComment is a person's replan comment as the replan step holds it,
// sanitised and cut. The console posts these words on the issue, so the
// plan loop knows the comment for the replan's own note.
func ReplanComment(s string) string {
	return textutil.Truncate(sanitize.Inbound(s), MaxReplanComment)
}

// MayReplan tells whether a person may send card t back to the planner: an
// agent card in the work bound that is a plan, a plan's child, or a direct
// card that did not merge, where its project's process has a plan track
// (planTrack).
func MayReplan(t Task, planTrack bool) bool {
	return t.Kind == "agent" && !t.Unbound() && (InPlan(t) || planTrack && t.MergeCommit == "" && t.Status != "done" && t.Status != "merged")
}

// Replan sends card taskID's work back to the planner at a person's ask,
// to plan again on top of what was built: a plan, or a plan's child,
// replans its plan, and the plan's started open cards stop; a direct card
// goes to the planner as itself; with OffMain a plan's merged cards come
// off main first. The answer is the note for the person. A card or plan
// that takes no replan answers ErrNoReplan and changes nothing, except
// that a plan with a replan pending has its reopen healed, or the revert
// its off-main replan waits for started.
func (r *Runner) Replan(ctx context.Context, taskID string, ask ReplanAsk) (string, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	ask.Comment = ReplanComment(ask.Comment)
	switch {
	case t.Kind != "agent":
		return "", ErrNoReplan(personCardRefusal(t))
	case t.Unbound():
		return "", ErrNoReplan(unboundRefusal(t))
	case InPlan(t):
		return r.replanPlan(ctx, GroupOf(t), ask)
	}
	return r.replanDirect(ctx, t, ask)
}

// replanPlan sends plan planID back to its planner: the replan step with
// the plan's started open cards as its stops, and the next round when
// the plan's round is complete (askRound), then completeReplan. With off
// main the step waits for the revert of the plan's merged cards still on
// main (takeOffMain), and the revert's end completes it.
func (r *Runner) replanPlan(ctx context.Context, planID string, ask ReplanAsk) (string, error) {
	p, _, err := r.task(ctx, planID)
	if err != nil {
		return "", err
	}
	plan := fmt.Sprintf("Plan #%d", p.IssueNumber)
	switch p.Status {
	case "closed":
		return "", ErrNoReplan(plan + " is closed: a person closed it. File a new card or plan for the change.")
	case "closing":
		return "", ErrNoReplan(plan + " is closing: replan it once it has ended.")
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return "", fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	step, pending, err := pendingIn(tl, p.ID)
	if err != nil {
		return "", err
	}
	switch wait, err := r.revertCard(ctx, p); {
	case err != nil:
		return "", err
	case wait != "":
		return "", ErrNoReplan(wait)
	}
	if revertWaits(tl, step, pending) {
		return r.awaitOffMain(ctx, p, step)
	}
	if pending {
		return r.completeReplan(ctx, p, step)
	}
	replannable, closed, err := r.planReplannable(ctx, p)
	switch {
	case err != nil:
		return "", err
	case closed:
		return "", ErrNoReplan(plan + " is closed: its criteria are met. File a new card or plan for the change.")
	case !replannable:
		return "", ErrNoReplan(plan + " is " + p.Status + ": it has not filed its cards yet. Comment on its issue to change the plan.")
	}
	newest, revision, err := r.planAt(ctx, p.ID, 0)
	if err != nil {
		return "", err
	}
	cards, err := r.store.PlanCards(ctx, p.ID)
	if err != nil {
		return "", err
	}
	step = ReplanStep{Origin: ReplanPerson, By: ask.By, Comment: ask.Comment, OffMain: ask.OffMain, Revision: revision}
	for _, k := range cards {
		if k.PlanRevision > 0 && stoppable(k) {
			step.Stops = append(step.Stops, k.ID)
		}
	}
	detail := map[string]any{"origin": step.Origin, "by": step.By, "comment": step.Comment, "stops": step.Stops, "revision": step.Revision}
	if newest != nil {
		step.Round = askRound(*newest, cards)
	}
	if step.Round > 0 {
		detail["round"] = step.Round
	}
	var merged []Task
	if step.OffMain {
		if merged = onMain(cards); len(merged) == 0 {
			return "", ErrNoReplan(plan + " has no card on main: replan without taking work off main.")
		}
		switch pause, paused, err := r.store.QueuePause(ctx, p.Repository); {
		case err != nil:
			return "", err
		case paused:
			return "", r.pauseRefusal(ctx, p, pause)
		}
		detail["off_main"] = true
	}
	wrote, err := r.timeline.AppendUnlessPending(ctx, p.ID, stepReplan, detail, replanClosers...)
	if err != nil {
		return "", fmt.Errorf("record %s: %w", stepReplan, err)
	}
	if !wrote {
		return "", beingReplanned(p)
	}
	if step.OffMain {
		return r.takeOffMain(ctx, p, step, merged)
	}
	return r.completeReplan(ctx, p, step)
}

// onMain are the merged cards of cards still on main.
func onMain(cards []Task) []Task {
	var out []Task
	for _, k := range cards {
		if k.MergeCommit != "" && k.RevertedAt == nil {
			out = append(out, k)
		}
	}
	return out
}

// takeOffMain starts the revert of plan p's merged cards, which its
// pending replan step waits for; the plan stays done, and the revert's end
// completes the step (endGroupRevert). The note is the revert's, then
// what follows its end. A revert another pause refuses ends the step. The
// revert runs past ctx's end: a press's request may end mid-clone.
func (r *Runner) takeOffMain(ctx context.Context, p Task, step ReplanStep, cards []Task) (string, error) {
	note, err := r.RevertGroup(context.WithoutCancel(ctx), p, cards, "asked by "+cmp.Or(step.By, "a person")+": "+step.Comment)
	var no ErrNoReplan
	if errors.As(err, &no) {
		// a pause that took the project after the press read none: the
		// person may replan without taking work off main
		if err := r.timeline.Append(ctx, p.ID, stepOffMainFailed, map[string]any{"error": no.Error()}); err != nil {
			return "", fmt.Errorf("record %s: %w", stepOffMainFailed, err)
		}
	}
	if err != nil {
		return "", err
	}
	// no count: the end also stops the cards that start meanwhile
	return fmt.Sprintf("%s Once the revert lands or ends, plan #%d goes back to the planner, and the plan's started cards stop. Their work stays on their branches.",
		note, p.IssueNumber), nil
}

// revertCard is the refusal of a replan of plan p while its project's
// pause names p with a filed revert card, a person's off-main revert or
// one the planner's revision started: it names the card while the card
// runs, and says so of a card that ended before the plan's timeline
// recorded its end (endGroupRevert). "" for none, and for a card whose end
// is recorded: the cards that end stops may cut themselves before the
// pause lifts.
func (r *Runner) revertCard(ctx context.Context, p Task) (string, error) {
	pause, paused, err := r.store.QueuePause(ctx, p.Repository)
	if err != nil || !paused || pause.Plan != p.ID {
		return "", err
	}
	card, filed, err := r.store.Task(ctx, pause.RevertTask)
	if err != nil || !filed {
		return "", err
	}
	if !Final(card.Status) && card.Status != "merged" {
		return fmt.Sprintf("Plan #%d's merged cards are coming off main: wait for the revert, card #%d.", p.IssueNumber, card.IssueNumber), nil
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return "", fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	if lastStep(tl, stepGroupReverted)["card"] == card.ID || lastStep(tl, stepGroupRevertEnded)["card"] == card.ID {
		return "", nil
	}
	return fmt.Sprintf("Plan #%d's revert, card #%d, has ended, and factoryd is still recording its end: replan again in two minutes.", p.IssueNumber, card.IssueNumber), nil
}

// awaitOffMain answers a press on plan p while its pending replan step
// waits for the revert of its merged cards and no revert card is filed
// (revertCard): its press stopped after the step, so the press starts
// the revert; with no card left on main the step completes as any replan
// does.
func (r *Runner) awaitOffMain(ctx context.Context, p Task, step ReplanStep) (string, error) {
	cards, err := r.store.PlanCards(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if merged := onMain(cards); len(merged) > 0 {
		return r.takeOffMain(ctx, p, step, merged)
	}
	return r.completeReplan(ctx, p, step)
}

// beingReplanned refuses a press on plan p, whose replan is pending.
func beingReplanned(p Task) ErrNoReplan {
	return ErrNoReplan(fmt.Sprintf("Plan #%d is being replanned: wait for its revision.", p.IssueNumber))
}

// completeReplan carries out plan p's pending replan step, for the press
// that wrote it and for any later one, so a press heals one that failed
// partway: the plan reopened, then each of the step's stops that still
// runs stopped, so the waits they hand over land on an open plan. A press
// that finds nothing left to do is refused: the plan is being replanned.
func (r *Runner) completeReplan(ctx context.Context, p Task, step ReplanStep) (string, error) {
	acted := p.Status == "done" || p.Status == "failed"
	var err error
	if slices.Contains(reopenFrom, p.Status) {
		err = r.reopenPlan(ctx, p)
	} else {
		// a run that runs answers it as a no-op
		err = r.cfg.Workflows.Reopen(ctx, p.ID, "", TaskSignal{})
	}
	if err != nil {
		return "", err
	}
	why := fmt.Sprintf("stopped for the replan of plan #%d", p.IssueNumber)
	for _, id := range step.Stops {
		k, ok, err := r.store.Task(ctx, id)
		if err != nil {
			return "", err
		}
		if !ok || !stoppable(k) {
			continue
		}
		stopped, err := r.stopForReplan(ctx, k, p, why)
		if err != nil {
			return "", err
		}
		acted = acted || stopped
	}
	if !acted {
		return "", beingReplanned(p)
	}
	return stopsNote(p, len(step.Stops)), nil
}

// stopsNote tells the person that plan p goes back to its planner and n
// cards stop.
func stopsNote(p Task, n int) string {
	plan := fmt.Sprintf("Plan #%d", p.IssueNumber)
	switch n {
	case 0:
		return plan + " goes back to the planner: no card stops."
	case 1:
		return plan + " goes back to the planner: 1 card stops, and its work stays on its branch."
	}
	return fmt.Sprintf("%s goes back to the planner: %d cards stop, and their work stays on their branches.", plan, n)
}

// replanDirect sends direct card t to the planner. The person's words go
// first, as the to_plan signal, which the workflow takes at its next step
// and records as the card's replan step; then a running session is asked
// to stop. A failed or closed card is queued again for a new run. A card
// that waits on its screening refuses: the note posted on its issue would
// answer the screening's question.
func (r *Runner) replanDirect(ctx context.Context, t Task, ask ReplanAsk) (string, error) {
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	switch {
	case t.MergeCommit != "" || t.Status == "done" || t.Status == "merged":
		return "", ErrNoReplan(mergedRefusal(t))
	case t.Status == "closing":
		return "", ErrNoReplan(card + " is closing: replan it once it has ended.")
	case t.Status == "waiting-human":
		events, err := r.store.IntakeEvents(ctx, t.ID)
		if err != nil {
			return "", err
		}
		if waitsOnScreening(events) {
			return "", ErrNoReplan(card + " waits for a person to judge its screening: let it run or close its issue, then replan it.")
		}
	}
	project, _, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return "", err
	}
	if _, planTrack := trackClass(project, policy.TrackPlan); !planTrack {
		return "", ErrNoReplan(noPlanTrackRefusal(t))
	}
	sig := TaskSignal{Author: ask.By, Body: ask.Comment}
	if Final(t.Status) {
		if err := r.store.TransitionTask(ctx, t.ID, []string{"failed", "closed"}, "queued"); err != nil {
			return "", err
		}
		if err := r.reopenIssue(ctx, t); err != nil {
			return "", err
		}
		mirrorTaskState(ctx, r.forgejo, t, "queued")
		if err := r.cfg.Workflows.Reopen(ctx, t.ID, SignalToPlan, sig); err != nil {
			return "", err
		}
		return card + " goes to the planner, which plans on top of its branch.", nil
	}
	if err := r.cfg.Workflows.Reopen(ctx, t.ID, SignalToPlan, sig); err != nil {
		return "", err
	}
	switch err := r.EscalateSession(ctx, t.ID, AgentAsk{Kind: AskReplan, Reason: "replanned by " + ask.By + ": " + ask.Comment}); {
	case err == nil || errors.Is(err, errReplanAsked):
		return card + " stops its session and goes to the planner.", nil
	case errors.Is(err, ErrNoLiveSession):
		return card + " goes to the planner at its next step.", nil
	default:
		return "", err
	}
}

// oldCards are plan p's record of cards a revision keeps, changes or
// drops: its cards, and p itself while its pending replan is its own (a
// direct card's replan or cut), whose work the findings name by issue.
func (r *Runner) oldCards(ctx context.Context, p Task) ([]Task, error) {
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return nil, err
	}
	return r.oldCardsOf(ctx, p, step, pending)
}

// oldCardsOf is oldCards with p's pending replan step already read.
func (r *Runner) oldCardsOf(ctx context.Context, p Task, step ReplanStep, pending bool) ([]Task, error) {
	cards, err := r.store.PlanCards(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if pending && step.Card == p.ID {
		cards = append(cards, p)
	}
	return cards, nil
}

// replanState is what a revision of plan is judged against: the revision
// before (nil: none), the newest stored and its number, the pending
// replan step (zero: none), the old cards, each started open card's entry
// in the revision it started against by key, the pending cut's card and
// whether its branch holds a commit past its base, and the old cards with
// a merge commit, those of them taken off main, and those that are the
// integration card of the revision they ran against, by id.
type replanState struct {
	prev        *Plan
	newest      *Plan
	revision    int
	step        ReplanStep
	old         []Task
	started     map[string]PlanCard
	plan        Task
	cut         string
	cutWork     bool
	merged      map[string]bool
	reverted    map[string]bool
	integration map[string]bool
}

// replanState reads plan p's record for replanFindings of its revision
// of, 0 for the one to store next, from its timeline tl and each revision
// once. The revision before is the newest that filed, the last filing
// step's: a revision refused at its filing holds nothing a later one is
// judged by. A plan that never wrote one has the newest stored before of.
func (r *Runner) replanState(ctx context.Context, p Task, tl []byte, of int) (replanState, error) {
	st := replanState{plan: p, started: map[string]PlanCard{}, merged: map[string]bool{}, reverted: map[string]bool{}, integration: map[string]bool{}}
	var err error
	if st.newest, st.revision, err = r.planAt(ctx, p.ID, 0); err != nil {
		return replanState{}, err
	}
	revisions := map[int]*Plan{0: nil, st.revision: st.newest}
	at := func(n int) (*Plan, error) {
		plan, read := revisions[n]
		if !read {
			var err error
			if plan, _, err = r.planAt(ctx, p.ID, n); err != nil {
				return nil, err
			}
			revisions[n] = plan
		}
		return plan, nil
	}
	before, filed := openRevision(tl, stepFiling)
	switch {
	case filed:
	case of == 0:
		before = st.revision
	default:
		before = of - 1
	}
	if st.prev, err = at(before); err != nil {
		return replanState{}, err
	}
	step, pending, err := pendingIn(tl, p.ID)
	if err != nil {
		return replanState{}, err
	}
	st.step = step
	if st.old, err = r.oldCardsOf(ctx, p, step, pending); err != nil {
		return replanState{}, err
	}
	for _, k := range st.old {
		if k.MergeCommit != "" {
			st.merged[k.ID] = true
			ran, err := at(k.PlanRevision)
			if err != nil {
				return replanState{}, err
			}
			if ran != nil && roleOf(*ran, k.PlanKey) == RoleIntegration {
				st.integration[k.ID] = true
			}
		}
		if k.RevertedAt != nil {
			st.reverted[k.ID] = true
		}
		if k.PlanKey == "" || k.PlanRevision == 0 || !stoppable(k) {
			continue
		}
		ran, err := at(k.PlanRevision)
		if err != nil {
			return replanState{}, err
		}
		if ran == nil {
			continue
		}
		if i := slices.IndexFunc(ran.Cards, func(c PlanCard) bool { return c.Key == k.PlanKey }); i >= 0 {
			st.started[k.PlanKey] = ran.Cards[i]
		}
	}
	if pending && step.Origin == ReplanCut {
		st.cut = step.Card
		head, err := r.BranchHead(ctx, step.Card)
		if err != nil {
			return replanState{}, err
		}
		st.cutWork = head != ""
	}
	return st, nil
}

// planAt is plan task id's revision n decoded, the newest for 0, and its
// number; nil and 0 when there is none.
func (r *Runner) planAt(ctx context.Context, id string, n int) (*Plan, int, error) {
	rev, ok, err := r.store.PlanRevision(ctx, id, n)
	if err != nil || !ok {
		return nil, 0, err
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return nil, 0, terminalError("plan %s revision %d: %v", id, rev.Revision, err)
	}
	return &p, rev.Revision, nil
}

// fromRef is how a revision's from names old card k: its key, or
// #<issue> for a card without one.
func fromRef(k Task) string {
	if k.PlanKey != "" {
		return k.PlanKey
	}
	return fmt.Sprintf("#%d", k.IssueNumber)
}

// replanFindings judges revision next against the plan's record st: a
// card is kept by its key, changed by a new card whose from names it, and
// dropped by leaving its key out. A merged card on main stays unless
// take_off_main names it, which takes its work off main after the filing;
// one taken off main lands again from salvage. An old key is a key with a
// card row. A from the revision before held for the same key was judged
// at its filing.
func replanFindings(st replanState, next Plan) []string {
	old := map[string]Task{}
	for _, k := range st.old {
		old[fromRef(k)] = k
	}
	prev := map[string]PlanCard{}
	if st.prev != nil {
		for _, c := range st.prev.Cards {
			prev[c.Key] = c
		}
	}
	kept := map[string]PlanCard{}
	adds := false
	for _, c := range next.Cards {
		kept[c.Key] = c
		_, row := old[c.Key]
		adds = adds || !row
	}
	off := map[string]bool{}
	for _, x := range next.TakeOffMain {
		off[x] = true
	}
	var found []string
	first := map[string]string{}
	for _, c := range next.Cards {
		if c.From == "" {
			continue
		}
		if a, ok := first[c.From]; ok {
			found = append(found, fmt.Sprintf("cards %s and %s: both start from %s; one card carries an old card's work: keep the from on %s and start %s from main",
				a, c.Key, c.From, a, c.Key))
		} else {
			first[c.From] = c.Key
		}
		k, known := old[c.From]
		// a kept card that ended or merged has its own finding
		if _, keeps := kept[c.From]; known && keeps && !st.merged[k.ID] && k.Status != "closed" && k.Status != "failed" {
			found = append(found, fmt.Sprintf("card %s: starts from %s, which this revision keeps; drop %s's key or start %s from main", c.Key, c.From, c.From, c.Key))
		}
		if was, held := prev[c.Key]; held && was.From == c.From {
			continue
		}
		if _, row := old[c.Key]; row {
			found = append(found, fmt.Sprintf("card %s: keeps an old key and names from; give %s a new key, since a card that starts from another card's work takes a new key", c.Key, c.Key))
		}
		switch {
		case !known:
			found = append(found, fmt.Sprintf("card %s: from %s names no card of this plan's record; name an old card's key, or #<issue> of a card the findings list without a key", c.Key, c.From))
		case off[c.From]:
			found = append(found, fmt.Sprintf("card %s: from %s, which this revision takes off main: its work leaves main only after this filing, so no card can land it yet; "+
				"leave %s out of take_off_main and start %s from main to change it there, or leave from out", c.Key, c.From, c.From, c.Key))
		case st.merged[k.ID] && !st.reverted[k.ID]:
			found = append(found, fmt.Sprintf("card %s: from %s, which merged: its work is on main, so start from main and leave from out", c.Key, c.From))
		}
	}
	named := map[string]bool{}
	for _, x := range next.TakeOffMain {
		if named[x] {
			continue
		}
		named[x] = true
		k, known := old[x]
		_, keeps := kept[x]
		switch {
		case !known || k.PlanKey == "":
			found = append(found, fmt.Sprintf("take_off_main names %s, which is no card of this plan's record; name the key of a merged card", x))
		case !st.merged[k.ID]:
			found = append(found, fmt.Sprintf("take_off_main names %s, which has not merged: its work is not on main; leave it out of take_off_main", x))
		case st.reverted[k.ID]:
			found = append(found, fmt.Sprintf("take_off_main names %s, which was taken off main already; leave it out of take_off_main", x))
		case st.integration[k.ID]:
			found = append(found, fmt.Sprintf("take_off_main names %s, the integration card, whose code stays on main; leave it out of take_off_main", x))
		case keeps:
			found = append(found, fmt.Sprintf("take_off_main names %s, which this revision keeps; leave its key out to take its work off main, or leave it out of take_off_main", x))
		}
	}
	for _, k := range st.old {
		if k.PlanKey == "" {
			continue
		}
		c, keeps := kept[k.PlanKey]
		_, held := prev[k.PlanKey]
		integration := st.prev != nil && roleOf(*st.prev, k.PlanKey) == RoleIntegration
		switch {
		case keeps && (k.Status == "closed" || k.Status == "failed") && k.PlanRevision == 0:
			found = append(found, fmt.Sprintf("card %s ended %s before it started: leave its key out, and add its work under a new key", k.PlanKey, k.Status))
		case keeps && (k.Status == "closed" || k.Status == "failed"):
			found = append(found, fmt.Sprintf(`card %s ended %s: leave its key out, and add a card with "from": "%s" to go on from its work`, k.PlanKey, k.Status, k.PlanKey))
		case keeps && st.reverted[k.ID]:
			found = append(found, fmt.Sprintf(`card %s was taken off main: leave its key out, and add a card with "from": "%s" to land it again from salvage`, k.PlanKey, k.PlanKey))
		case st.merged[k.ID] && integration && keeps && adds:
			found = append(found, "card "+k.PlanKey+" is the integration card and merged: leave its key out; the revision's new integration card checks the whole feature again")
		case st.merged[k.ID] && !st.reverted[k.ID] && !integration && held && !keeps && !off[k.PlanKey]:
			found = append(found, "card "+k.PlanKey+" is merged on main: keep its key, or name it in take_off_main to take its work off main")
		}
		if entry, ok := st.started[k.PlanKey]; ok && keeps && !sameEntry(c, entry) {
			found = append(found, fmt.Sprintf("card %s started against revision %d: keep its entry as revision %d has it, or leave its key out to stop it",
				k.PlanKey, k.PlanRevision, k.PlanRevision))
		}
	}
	if st.cutWork {
		if i := slices.IndexFunc(st.old, func(k Task) bool { return k.ID == st.cut }); i >= 0 {
			ref := fromRef(st.old[i])
			if !slices.ContainsFunc(next.Cards, func(c PlanCard) bool { return c.From == ref }) {
				found = append(found, fmt.Sprintf(`the cut card #%d holds work on its branch: name "%s" in the "from" of the card that lands it`, st.old[i].IssueNumber, ref))
			}
		}
	}
	return found
}

// sameEntry tells whether a and b are one card entry; a list left out is
// an empty one.
func sameEntry(a, b PlanCard) bool {
	if !slices.Equal(a.Directories, b.Directories) || !slices.Equal(a.DependsOn, b.DependsOn) || !slices.Equal(a.Acceptance, b.Acceptance) {
		return false
	}
	a.Directories, a.DependsOn, a.Acceptance = nil, nil, nil
	b.Directories, b.DependsOn, b.Acceptance = nil, nil, nil
	return reflect.DeepEqual(a, b)
}

// recordReplan writes the record of plan p's pending replan step, whose
// revision filed next, the cards' issues by key: each old card kept by
// its key, changed into the new card whose from names it, or dropped,
// and the new cards added. The old cards are oldCards that existed at
// the step, without those closed before it; the cards the replan itself
// stops count whatever the clocks say. It comments the counts and who
// asked on the plan's issue and steps each old card; a cut says on its
// card's issue what it was cut into, at which session and spend, and
// steps it. The plan's replanned step comes last, as it ends the pending
// replan: a failure before it fails the filing, which runs again, and
// writes the comments, the cut and each old card's step once. A round's
// step records its round alone (recordRound).
func (r *Runner) recordReplan(ctx context.Context, p Task, repo Repo, revision int, next Plan, step ReplanStep, issues map[string]int64) error {
	if step.Origin == ReplanRound {
		return r.recordRound(ctx, p, repo, revision, next, issues)
	}
	cards, err := r.oldCardsOf(ctx, p, step, true)
	if err != nil {
		return err
	}
	keys, into := map[string]bool{}, map[string]string{}
	for _, c := range next.Cards {
		keys[c.Key] = true
		if c.From != "" {
			into[c.From] = c.Key
		}
	}
	type fate struct {
		k       Task
		verdict string
		into    int64
	}
	var fates []fate
	count, held, named := map[string]int{}, map[string]bool{}, map[string]bool{}
	for _, k := range cards {
		stopped := k.ID == step.Card || slices.Contains(step.Stops, k.ID)
		if k.CreatedAt.After(step.Time) || k.Status == "closed" && k.FinishedAt != nil && k.FinishedAt.Before(step.Time) && !stopped {
			continue
		}
		f := fate{k: k, verdict: "dropped"}
		switch ref := fromRef(k); {
		case k.PlanKey != "" && keys[k.PlanKey]:
			f.verdict = "kept"
		case into[ref] != "":
			f.verdict, f.into, named[ref] = "changed", issues[into[ref]], true
		}
		held[k.PlanKey] = k.PlanKey != ""
		count[f.verdict]++
		fates = append(fates, f)
	}
	added := 0
	var cutInto []int64
	for _, c := range planOrder(next, planEdges(next)) {
		if held[c.Key] {
			continue
		}
		cutInto = append(cutInto, issues[c.Key])
		if !named[c.From] {
			added++
		}
	}
	note := fmt.Sprintf("replanned at revision %d: kept %d, changed %d, dropped %d, added %d.", revision, count["kept"], count["changed"], count["dropped"], added)
	var off []string
	for _, k := range cards {
		if k.PlanKey != "" && slices.Contains(next.TakeOffMain, k.PlanKey) {
			off = append(off, fmt.Sprintf("#%d (%s)", k.IssueNumber, k.PlanKey))
		}
	}
	if len(off) > 0 {
		note += fmt.Sprintf(" It takes %s off main.", strings.Join(off, ", "))
	}
	i := slices.IndexFunc(cards, func(k Task) bool { return k.ID == step.Card })
	switch {
	case step.Origin == ReplanPerson:
		comment := sanitize.Inbound(step.Comment)
		note += fmt.Sprintf("\nAsked by %s: %s", cmp.Or(step.By, "a person"), comment)
		if step.OffMain && slices.ContainsFunc(cards, func(k Task) bool { return k.RevertedAt != nil && !k.RevertedAt.Before(step.Time) }) {
			if c := strings.TrimSpace(comment); c != "" && !strings.ContainsAny(c[len(c)-1:], ".!?") {
				note += "."
			}
			note += " Its merged cards came off main."
		}
	case step.Origin == ReplanCut && i >= 0:
		note += fmt.Sprintf("\nThe cut of #%d: %s", cards[i].IssueNumber, sanitize.Inbound(step.Work))
	}
	if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, p.IssueNumber, "factoryd: "+r.untrusted(p, note)); err != nil {
		return fmt.Errorf("comment the replan on #%d: %w", p.IssueNumber, err)
	}
	if step.Origin == ReplanCut && i >= 0 {
		cut := cards[i]
		session, spent, err := r.cutAt(ctx, p, cut)
		if err != nil {
			return err
		}
		refs := make([]string, len(cutInto))
		for j, n := range cutInto {
			refs[j] = fmt.Sprintf("#%d", n)
		}
		body := fmt.Sprintf("factoryd: cut into %s at session %d, %.2f EUR spent.", strings.Join(refs, ", "), session, spent)
		if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, cut.IssueNumber, body); err != nil {
			return fmt.Errorf("comment the cut on #%d: %w", cut.IssueNumber, err)
		}
		if _, err := r.timeline.AppendOnce(ctx, cut.ID, "cut", map[string]any{"into": cutInto, "session": session, "spent": spent}); err != nil {
			return fmt.Errorf("record cut: %w", err)
		}
	}
	for _, f := range fates {
		if f.k.ID == p.ID {
			continue // the plan's own step records it
		}
		d := map[string]any{"revision": revision, "verdict": f.verdict}
		if f.into != 0 {
			d["into"] = f.into
		}
		if _, err := r.timeline.AppendOnceFor(ctx, f.k.ID, "replanned", revision, d); err != nil {
			return fmt.Errorf("record replanned on %s: %w", f.k.ID, err)
		}
	}
	detail := map[string]any{"revision": revision, "kept": count["kept"], "changed": count["changed"], "dropped": count["dropped"], "added": added,
		"origin": step.Origin, "by": step.By, "comment": step.Comment}
	if len(next.TakeOffMain) > 0 {
		detail["take_off_main"] = next.TakeOffMain
	}
	if err := r.timeline.Append(ctx, p.ID, "replanned", detail); err != nil {
		return fmt.Errorf("record replanned: %w", err)
	}
	r.timeline.RecordRow(ctx, p.ID, EventPlanReplanned, FactoryDecider, ViaFactoryd, detail)
	return nil
}

const (
	// recordFileBytes bounds one old card's record file.
	recordFileBytes = 16 << 10
	// recordSessions is how many of an old card's newest sessions its
	// record reads for the working state of its last session.
	recordSessions = 3
)

// replanRecord is a replan's record for its planner. findings are
// factoryd's lines: a person's or a round's opener and one line per old
// card that is not settled; work are the agent's words, the cut's opener,
// which stand in the brief's fence; branches are the fork's branches of
// the old cards that hold work and did not merge, and the plan's salvage
// branch, each <fork branch>:<local branch>; files are one record per old
// card that is not settled.
type replanRecord struct {
	findings, branches []string
	files              []harness.ReportFile
	work               []string
}

// replanRecord is plan p's record for the planner session of its pending
// replan, and whether one is pending, from its timeline ptl. A merged card
// a group revert was to take off main, and that revert ended without
// landing, says so: a person stopped that take-off. The merged cards of
// earlier rounds that the revision keeps as they are (settledCards) have
// no line, file or branch: the planner never sees them.
func (r *Runner) replanRecord(ctx context.Context, p Task, ptl []byte) (replanRecord, bool, error) {
	step, pending, err := pendingIn(ptl, p.ID)
	if err != nil || !pending {
		return replanRecord{}, false, err
	}
	unlanded := map[string]string{}
	for line := range bytes.SplitSeq(ptl, []byte("\n")) {
		var end struct {
			Step, State string
			Cards       []string
		}
		if json.Unmarshal(line, &end) == nil && end.Step == stepGroupRevertEnded {
			for _, id := range end.Cards {
				unlanded[id] = end.State
			}
		}
	}
	cards, err := r.oldCardsOf(ctx, p, step, true)
	if err != nil {
		return replanRecord{}, false, err
	}
	var rec replanRecord
	switch step.Origin {
	case ReplanPerson:
		opener := fmt.Sprintf("This is a replan of plan #%d, asked by %s", p.IssueNumber, cmp.Or(step.By, "a person"))
		if comment := sanitize.Inbound(step.Comment); comment != "" {
			opener += ": " + comment
		}
		rec.findings = append(rec.findings, opener)
		if step.Round > 0 {
			rec.findings = append(rec.findings, fmt.Sprintf("It files round %d: every card of round %d ended.", step.Round, step.Round-1))
		}
	case ReplanRound:
		rec.findings = append(rec.findings, fmt.Sprintf("This is round %d of plan #%d: the cards of round %d are done. Plan round %d from your outline, on top of what the earlier rounds built.",
			step.Round, p.IssueNumber, step.Round-1, step.Round))
	}
	if i := slices.IndexFunc(cards, func(k Task) bool { return k.ID == step.Card }); step.Origin == ReplanCut && i >= 0 {
		session, spent, err := r.cutAt(ctx, p, cards[i])
		if err != nil {
			return replanRecord{}, false, err
		}
		rec.work = append(rec.work, fmt.Sprintf("This is the cut of #%d at session %d, %.2f EUR spent: %s", cards[i].IssueNumber, session, spent, sanitize.Inbound(step.Work)))
	}
	roles, settled := map[string]string{}, map[string]bool{}
	if newest, _, err := r.planAt(ctx, p.ID, 0); err != nil {
		return replanRecord{}, false, err
	} else if newest != nil {
		for _, c := range newest.Cards {
			roles[c.Key] = c.Role
		}
		for _, c := range settledCards(*newest, cards, filingRound(step, newest)) {
			settled[c.Key] = true
		}
	}
	revisions := map[int]*Plan{}
	for _, k := range cards {
		if settled[k.PlanKey] {
			continue
		}
		var head string
		if k.MergeCommit == "" {
			if head, err = r.BranchHead(ctx, k.ID); err != nil {
				return replanRecord{}, false, err
			}
		}
		branch := fmt.Sprintf("card/%d", k.IssueNumber)
		work, onBranch := "no commit past its base", ""
		if head != "" {
			work, onBranch = "its work on branch "+branch, ", its work on branch "+branch
			rec.branches = append(rec.branches, taskBranch(k)+":"+branch)
		}
		tl, err := r.objects.Get(ctx, "sessions/"+k.ID+"/timeline.jsonl")
		if err != nil {
			return replanRecord{}, false, fmt.Errorf("the timeline of %s: %w", k.ID, err)
		}
		// said is the file's state line where it differs: a final reason
		// may be the agent's words, which the fenced file holds and no
		// finding does
		var state, said string
		switch {
		case k.RevertedAt != nil:
			state = "taken off main (it was merged as " + k.MergeCommit[:min(8, len(k.MergeCommit))] + ")"
		case k.MergeCommit != "":
			state = "merged as " + k.MergeCommit[:min(8, len(k.MergeCommit))]
			if final, ok := unlanded[k.ID]; ok {
				state += "; the revert that was to take it off main ended " + final + " without landing"
			}
		case k.ID == p.ID:
			state = work
		case k.ID == step.Card || slices.Contains(step.Stops, k.ID):
			state = "stopped for this replan, " + work
		case Final(k.Status):
			state = "ended " + k.Status + ", " + work
			if reason, _ := lastStep(tl, "final")["reason"].(string); stopLine(reason) != "" {
				said = "ended " + k.Status + ": " + stopLine(reason) + ", " + work
			}
		case k.PlanRevision == 0:
			state = "never started" + onBranch
		default:
			state = "running" + onBranch
		}
		// the title is planner text drawn from an issue, and a finding is
		// one line
		title, _, _ := strings.Cut(sanitize.Inbound(k.Objective), "\n")
		title = oneLine(title)
		name := fmt.Sprintf("card-%d.md", k.IssueNumber)
		role, spend := roles[k.PlanKey], ""
		if k.ID == p.ID {
			rec.findings = append(rec.findings, fmt.Sprintf("#%d (this card, no key): %s; its record is %s", k.IssueNumber, state, name))
		} else {
			if _, held := roles[k.PlanKey]; !held && k.PlanRevision > 0 {
				at, read := revisions[k.PlanRevision]
				if !read {
					if at, _, err = r.planAt(ctx, p.ID, k.PlanRevision); err != nil {
						return replanRecord{}, false, err
					}
					revisions[k.PlanRevision] = at
				}
				if at != nil {
					role = roleOf(*at, k.PlanKey)
				}
			}
			role = cmp.Or(role, RoleFeature)
			spend = fmt.Sprintf("; spent %.2f of %.2f EUR in %d sessions", k.SpentEUR, k.BudgetEUR, k.Usage.Attempts)
			rec.findings = append(rec.findings, fmt.Sprintf("#%d %s (%s, key %s): %s%s; its record is %s", k.IssueNumber, title, role, k.PlanKey, state, spend, name))
		}
		// its last session is one from before the replan: a direct card's
		// later ones are its planner's
		text, err := r.oldCardText(ctx, k, tl, fmt.Sprintf("# #%d %s", k.IssueNumber, title), cmp.Or(said, state)+spend, role, cmp.Or(head, k.MergeCommit, k.SHA), step.Time)
		if err != nil {
			return replanRecord{}, false, err
		}
		rec.files = append(rec.files, harness.ReportFile{Name: name, Text: textutil.Truncate(sanitize.Inbound(text), recordFileBytes)})
	}
	repo, err := SplitRepo(p.Repository)
	if err != nil {
		return replanRecord{}, false, err
	}
	salvage, err := r.salvagePair(ctx, repo, p.ID)
	if err != nil {
		return replanRecord{}, false, err
	}
	if salvage != "" {
		rec.branches = append(rec.branches, salvage)
	}
	return rec, true, nil
}

// oldCardText is old card k's record file under title: its state line,
// then each section that has lines: the reviews' findings and the stages'
// from its timeline tl, an integration card's criteria verdicts, the
// check-in's notes, and the working state of its newest session created
// before before that left one, at branch head commit, with how that
// session ended.
func (r *Runner) oldCardText(ctx context.Context, k Task, tl []byte, title, state, role, commit string, before time.Time) (string, error) {
	parts := []string{title, state}
	section := func(heading string, lines []string) {
		if len(lines) > 0 {
			parts = append(parts, "## "+heading+"\n"+strings.Join(lines, "\n"))
		}
	}
	var st RunState
	stageMemoryOf(tl, &st)
	var lines []string
	for i, rv := range st.Reviews {
		for _, f := range rv.Findings {
			lines = append(lines, fmt.Sprintf("- round %d, %s: %s", i+1, rv.Verdict, f))
		}
	}
	section("Review findings", lines)
	lines = nil
	for _, h := range st.History {
		lines = append(lines, "- "+h)
	}
	section("Stage findings", lines)
	if role == RoleIntegration {
		rows, err := r.store.CriteriaVerdicts(ctx, k.ID)
		if err != nil {
			return "", err
		}
		lines = nil
		for _, v := range rows {
			line := "- " + v.Criterion
			for _, s := range []string{v.Verdict, v.Evidence} {
				if s != "" {
					line += ": " + s
				}
			}
			lines = append(lines, line)
		}
		section("Criteria verdicts", lines)
	}
	lines = nil
	ends := map[string]carryFrom{}
	for line := range strings.SplitSeq(string(tl), "\n") {
		var s struct{ Step, Reason, Handover, Session, Outcome, Summary string }
		if json.Unmarshal([]byte(line), &s) != nil {
			continue
		}
		switch {
		case s.Step == "checkin" && s.Reason != "":
			lines = append(lines, "- check-in: "+s.Reason)
		case s.Step == "handover" && s.Handover != "":
			lines = append(lines, "- handover: "+s.Handover)
		case s.Step == "session_end":
			ends[s.Session] = carryFrom{Outcome: SessionOutcome(s.Outcome), Stop: s.Summary}
		}
	}
	section("Check-in notes", lines)
	sessions, err := r.store.RecentSessions(ctx, k.ID, before, recordSessions)
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		rec, err := r.objects.Get(ctx, "sessions/"+k.ID+"/"+s.ID+".jsonl")
		if err != nil {
			return "", fmt.Errorf("the record of session %s: %w", s.ID, err)
		}
		from := ends[s.ID]
		from.Session, from.Commit = s.ID, commit
		if last := workingState(rec, from); last != nil {
			section("Its last session", []string{last.Prior.Heading + "\n\n" + last.Prior.Body})
			break
		}
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}

// cutAt is the session and the spend at which old card cut of plan p was
// cut. A direct card's switch set its counters to zero; its to_plan step
// kept them.
func (r *Runner) cutAt(ctx context.Context, p, cut Task) (session int, spent float64, err error) {
	if cut.ID != p.ID {
		return cut.Usage.Attempts, cut.SpentEUR, nil
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return 0, 0, fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	sw := lastStep(tl, stepToPlan)
	n, _ := sw["session"].(float64)
	spent, _ = sw["spent"].(float64)
	return int(n), spent, nil
}
