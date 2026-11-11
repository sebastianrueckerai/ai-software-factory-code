package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"go.temporal.io/sdk/temporal"
)

// ActivityEndCase is EndCase's activity.
const ActivityEndCase = "EndCase"

// ActivityRecordGrant is RecordGrant's activity.
const ActivityRecordGrant = "RecordGrant"

// ActivityAuditDraw is AuditDraw's activity.
const ActivityAuditDraw = "AuditDraw"

// ActInput is one answer: by whom, how it reached the factory (ViaConsole,
// ViaChat, "" for ViaFactoryd), on which rung, and on which case of the
// card (0 for an action on a card that waits on no case, such as a fund
// while it runs).
type ActInput struct {
	TaskID string
	CaseID int64
	By     string
	Via    string
	Rung   int
	Res    Resolution
	// Read, Found and CostEUR are what the rung's step read, found and
	// cost; they land on the in-progress step of Rung that Act ends.
	Read    []CaseRead
	Found   string
	CostEUR float64
	// Did are the actions the rung took before its answer, the verdicts,
	// which lead the step's own.
	Did []CaseAction
	// Post posts the answer's issue comment as By and answers its id: the
	// console's, as the human, is the comment a run from before the cases
	// gate wakes on. Act calls it once the answer passed its checks and
	// its outside actions ran (a close included), before the case ends,
	// and only for an answer that resolves its case. Nil: Act posts factoryd's record
	// after the case ends, when the card has an issue, except for a human
	// card's choice, whose work the console does, and for factoryd's own
	// answers, rung 0's and rung 1's, whose steps are the case's record.
	Post func(ctx context.Context) (int64, error)
	// Work is what a human card's choice does outside factoryd: the
	// console's close, replan, re-run or hand-over, and the work of a board
	// action (boardActions). Act runs it under the case's lock once every
	// refusal has passed and the answer does not propose, so no work runs
	// for an answer Act refuses or leaves to the human. The comment it
	// posted (0 for none) is the answer's.
	Work func(ctx context.Context) (posted int64, err error)
	// Detail are keys every row of the answer carries beside Act's own,
	// which they never replace: the chat token behind a token's turn.
	Detail map[string]any
}

// ActResult is what an answer did: the case as it stands after it, what
// ran outside the run and what it drew, each with its result, what it drew
// from the pool, the comment that carries it, and one line for the human
// or the chat.
type ActResult struct {
	Case    Case
	Did     []CaseAction
	Drawn   policy.Draw
	Comment int64
	Text    string
	// Proposed: the answer ran nothing and drew nothing; the human confirms
	// the action on the card
	Proposed bool
}

// ErrActRefused is Act's refusal: the state that refuses the answer and
// the actions that fit.
type ErrActRefused string

func (e ErrActRefused) Error() string { return string(e) }

// outsideActions are the actions Act runs outside the run, before the
// case ends.
var outsideActions = []string{ActRetry, ActReplan, ActClose, ActDeployNow}

// endedCardActions are the actions a case of an ended card still takes: they
// run or draw nothing for the card's work.
var endedCardActions = []string{ActClose, ActNote, ActSettle, ActWithdraw}

// Act is the one way a human, the chat or a rung answers a case. It reads
// the facts and refuses an action the table does not offer rung on them,
// or a draw past the rung's bounds. It runs the outside actions first, so
// a refusal of theirs leaves the case open, then has the caller post its
// comment. It ends the case and writes the draw and the step in one
// transaction, which weighs the draw against its rung's bounds again
// under the project's lock. Then it posts factoryd's comment when the
// caller posted none, records the answer and signals the run. A note
// records a step and ends nothing. A human card's case takes a value of its question's
// choices, which Act records and runs nothing for, deploy_now excepted, which
// runs outside the run. Rung 0 is factoryd's,
// and factoryd answers on rung 0 alone. An answer holds its case from its
// first read to its end, so a second answer on it waits, then finds it
// ended, with nothing of its own run.
func (r *Runner) Act(ctx context.Context, in ActInput) (ActResult, error) {
	switch {
	case in.Rung == 0 && in.By != FactoryDecider:
		return ActResult{}, ErrActRefused("rung 0 is factoryd's own: answer on rung 1, 2 or 3")
	case in.Rung != 0 && in.By == FactoryDecider:
		return ActResult{}, ErrActRefused(fmt.Sprintf("rung 0 is factoryd's own, and factoryd answers on rung 0 alone: answer on rung 0, not rung %d", in.Rung))
	case in.Rung != 1 && in.By == CheckinDecider:
		return ActResult{}, ErrActRefused(fmt.Sprintf("the check-in answers on rung 1 alone, not rung %d", in.Rung))
	}
	if in.CaseID != 0 {
		lock, _ := r.caseLocks.LoadOrStore(in.CaseID, &sync.Mutex{})
		lock.(*sync.Mutex).Lock()
		defer lock.(*sync.Mutex).Unlock()
	}
	t, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return ActResult{}, err
	}
	c, err := r.actCase(ctx, t, in)
	if err != nil {
		return ActResult{Case: c}, err
	}
	if c.ID != 0 && Final(t.Status) && !slices.Contains(endedCardActions, in.Res.Action) {
		if _, known := ActionByName(in.Res.Action); !known {
			return ActResult{Case: c}, ErrActRefused(fmt.Sprintf("card %s ended %s: its case closes on its own, so %s changes nothing; a new card carries the work on", cardName(t), t.Status, in.Res.Action))
		}
		return ActResult{Case: c}, ErrActRefused(fmt.Sprintf("card %s ended %s, so its case takes no %s: its case closes on its own, and a new card carries the work on", cardName(t), t.Status, in.Res.Action))
	}
	own, err := r.ownStep(ctx, c, in)
	if err != nil {
		return ActResult{}, err
	}
	if in.By == CheckinDecider && own == 0 {
		return ActResult{Case: c}, ErrActRefused("rung 1's step on this case ended before its answer came: the answer is too late")
	}
	if _, ok := in.Res.Args["comment"]; ok {
		in.Res.Args = maps.Clone(in.Res.Args)
		delete(in.Res.Args, "comment") // the comment is the one the answer posts
	}
	res := in.Res
	rungNote := in.Res.Note
	choice := slices.Contains(choiceKinds, c.Kind) && res.Action != ActNote
	var f CaseFacts
	proposing := in.Rung == 2 && c.ID != 0 && res.Action != ActNote && Proposes(r.autonomyOf(t), res.Action)
	if choice {
		if res.Action == ActDeployNow {
			deploy, _ := ActionByName(ActDeployNow)
			if why := deploy.Applies(CaseFacts{Kind: c.Kind}, res); why != "" {
				return ActResult{}, ErrActRefused(why)
			}
		}
		why, err := r.humanChoice(ctx, t, in)
		if err != nil {
			return ActResult{}, err
		}
		if why != "" {
			return ActResult{}, ErrActRefused(why)
		}
	} else {
		head, _ := c.Facts["head"].(string)
		model, _ := c.Facts["model"].(string)
		if f, err = r.tripFacts(ctx, t, head, model); err != nil {
			return ActResult{}, err
		}
		f.atCase(c)
		if human, why := humanOnly(c, f); human && in.Rung == 2 {
			return ActResult{}, ErrActRefused(fmt.Sprintf("this case is the human's by its kind (%s): record your analysis and recommended option with attach_analysis", why))
		}
		if why := actRefusal(f, in, cardName(t)); why != "" {
			return ActResult{}, ErrActRefused(why)
		}
		in.Res.Note = r.sessionNote(f, in)
		res = in.Res
	}
	if proposing {
		steps, err := r.store.CaseSteps(ctx, c.ID)
		if err != nil {
			return ActResult{}, err
		}
		if i := slices.IndexFunc(steps, func(st CaseStep) bool {
			return st.Rung == 2 && st.Outcome == StepHandedUp && slices.ContainsFunc(st.Did, func(a CaseAction) bool { return a.Result == resultProposed })
		}); i >= 0 {
			return ActResult{}, ErrActRefused(fmt.Sprintf("case %d already carries your proposal of %s: the human decides it on the card", c.ID, steps[i].Decision))
		}
		return r.propose(ctx, t, c, in, own, rungNote)
	}
	if IsBoardAction(res.Action) && in.Work == nil {
		return ActResult{}, ErrActRefused(res.Action + " is the console's work: take it from the board or the chat")
	}
	via := cmp.Or(in.Via, ViaFactoryd)
	var out ActResult
	var draw *CaseDraw
	if !choice || res.Action == ActDeployNow {
		if out.Did, err = r.actOutside(ctx, t, f, in); err != nil {
			return out, err
		}
	}
	if !choice {
		grants, err := r.runGrants(ctx, c, res)
		if err != nil {
			return out, err
		}
		if draw = r.caseDraw(t, f, c, in, via); draw != nil {
			draw.RunGrants = grants
		}
	}
	// a merge decision's request ended the case already, and a criterion's
	// verdict or a filed card leaves it at the human
	ends := c.ID != 0 && res.Action != ActNote && !slices.Contains(keptCaseActions, res.Action) && !slices.Contains(mergeDecisions, res.Action)
	answers := ends && res.Action != ActWithdraw
	var worked int64
	if in.Work != nil {
		if worked, err = in.Work(ctx); err != nil {
			return out, ranErr(out.Did, err)
		}
		if IsBoardAction(res.Action) {
			out.Did = append(out.Did, CaseAction{Action: res.Action, Args: res.Args, Result: res.Sentence()})
		}
	}
	posts := answers && in.Post != nil && worked == 0
	out.Comment = worked
	if res.Action == ActClose && !choice {
		closed, err := r.closeCard(ctx, t, in, posts)
		if err != nil {
			return out, ranErr(out.Did, err)
		}
		out.Did = append(out.Did, closed)
	}
	if posts {
		if out.Comment, err = in.Post(ctx); err != nil {
			return out, ranErr(out.Did, err)
		}
	}
	var drew []CaseAction
	if draw != nil {
		drew = drawActions(res, *draw)
	}
	if err := r.actWrite(ctx, t, c, in, own, slices.Concat(in.Did, out.Did, drew), out.Comment, draw, ends); err != nil {
		return out, ranErr(out.Did, err)
	}
	out.Did = append(out.Did, drew...)
	if draw != nil {
		out.Drawn = draw.Extend.Plus(policy.Draw{EUR: draw.FundEUR})
	}
	out.Case = c
	if c.ID != 0 {
		if out.Case, err = r.store.CaseByID(ctx, c.ID); err != nil {
			log.Printf("task %s: case %d after %s's %s: %v", t.ID, c.ID, in.By, res.Action, err)
			out.Case = c
		}
	}
	out.Text = actText(in, t, c, res, out.Did)
	if !answers {
		return out, nil
	}
	if out.Comment == 0 && t.IssueNumber != 0 && res.Action != ActClose && !choice && !slices.Contains(boardActions, res.Action) && in.By != FactoryDecider && in.By != CheckinDecider {
		if out.Comment, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, r.answerComment(t, in.By, res)); err != nil {
			log.Printf("task %s: factoryd's comment of %s's %s on case %d: %v", t.ID, in.By, res.Action, c.ID, err)
		}
	}
	kind := EventQuestionAnswered
	if draw != nil && draw.Extend.Sessions > 0 {
		kind = EventSessionGranted
	}
	detail := map[string]any{"action": res.Action, "case_id": c.ID, "answer": cmp.Or(strings.TrimSpace(res.Note), res.Sentence())}
	if out.Comment != 0 {
		detail["comment"] = out.Comment
	}
	if choice {
		detail["value"] = res.Action
	}
	beside(detail, in.Detail)
	r.timeline.RecordRow(ctx, t.ID, kind, in.By, via, detail)
	if t.Kind == "human" || Final(t.Status) || c.Kind == CaseMonitorFlag {
		// a human card runs no workflow, an ended card's run is over and no
		// run waits on a monitor case: a signal would start one
		return out, nil
	}
	r.signalRun(ctx, t.ID, SignalCaseResolved, TaskSignal{Author: in.By, Comment: out.Comment,
		Case: &CaseSignal{CaseID: c.ID, Rung: in.Rung, By: in.By, Res: res, Comment: out.Comment}})
	if out.Comment != 0 {
		r.signalRun(ctx, t.ID, SignalAnswerWhole, TaskSignal{Author: in.By, Comment: out.Comment})
	}
	return out, nil
}

// Proposes says rung 2's answer action at autonomy state is a proposal the
// human confirms, not an action that runs: never at full auto, at autonomous
// for a merge approval alone, at assisted and manual for every action a
// human may take. An action only the rungs take (route_approval) runs at
// every level: it asks nobody for anything the human could confirm.
func Proposes(state, action string) bool {
	if a, ok := ActionByName(action); ok && !a.Rungs[3] {
		return false
	}
	switch state {
	case policy.AutonomyFullAuto:
		return false
	case policy.AutonomyAutonomous:
		return action == ActApproveMerge
	}
	return true
}

// autonomyOf is the autonomy state of card t's project.
func (r *Runner) autonomyOf(t Task) string {
	_, project, _ := r.cfg.Policy().ProjectOf(t.Repository)
	return project.AutonomyState()
}

// propose ends rung 2's step on case c handed up with the action it
// proposes, which nothing runs and nothing draws for, and puts it before
// the human preselected with note, the rung's own words: a case still open
// climbs and faces once, a case already at the human has its face updated
// and one comment posted. The case's lock is held.
func (r *Runner) propose(ctx context.Context, t Task, c Case, in ActInput, own int64, note string) (ActResult, error) {
	res := in.Res
	grounds := textutil.Truncate(r.scrub(strings.TrimSpace(note)), 1200)
	found := textutil.Truncate(r.scrub(in.Found), 1200)
	step := CaseStep{CaseID: c.ID, Rung: 2, Actor: in.By, Outcome: StepHandedUp, Found: found, Decision: res.Action, Grounds: grounds,
		Did: []CaseAction{{Action: res.Action, Args: res.Args, Fund: res.Fund, Result: resultProposed}}, Read: nonNil(in.Read), CostEUR: in.CostEUR}
	var err error
	if own != 0 {
		step.ID = own
		err = r.store.EndCaseStep(ctx, step)
	} else {
		_, err = r.store.AppendCaseStep(ctx, step)
	}
	if err != nil {
		return ActResult{}, err
	}
	if c.Status == CaseAtHuman {
		err = r.proposeAtHuman(ctx, t, c, handUp{Action: res.Action, Grounds: grounds, Rung: 2, ActionRung: 2, Preselected: &step.Did[0]})
	} else {
		var f CaseFacts
		head, _ := c.Facts["head"].(string)
		model, _ := c.Facts["model"].(string)
		if f, err = r.tripFacts(ctx, t, head, model); err == nil {
			f.atCase(c)
			_, err = r.climb(ctx, t, c, f, 2, found)
		}
	}
	if err != nil {
		return ActResult{}, err
	}
	out := ActResult{Case: c, Did: step.Did, Proposed: true, Text: fmt.Sprintf("Proposed %s on %s: the human confirms it on the card.", res.Action, cardName(t))}
	if live, err := r.store.CaseByID(ctx, c.ID); err == nil {
		out.Case = live
	}
	return out, nil
}

// proposeAtHuman puts rung 2's proposal on the face card t's case already
// has at the human: the face of that case carries the recommendation and
// the preselected action, and one comment on the issue names the proposal.
func (r *Runner) proposeAtHuman(ctx context.Context, t Task, c Case, up handUp) error {
	if err := r.store.SetQuestionProposal(ctx, t.ID, c.ID, c.Kind == CaseMonitorFlag, r.recommendation(t, &up), up.Preselected.preselected()); err != nil {
		return err
	}
	if t.IssueNumber == 0 {
		return nil
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("The factory agent proposes %s: %s", up.Action, up.Grounds)
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, r.untrusted(t, r.scrub(body))); err != nil {
		log.Printf("task %s: the comment of rung 2's proposal: %v", t.ID, err)
	}
	return nil
}

// actCase is the case in answers on card t: case id when it is live and
// t's, else none for 0. An answer with no case on a card that waits on a
// live ask case is refused: it would draw and leave the case waiting.
func (r *Runner) actCase(ctx context.Context, t Task, in ActInput) (Case, error) {
	if in.CaseID == 0 {
		c, err := r.store.NewestAskCase(ctx, t.ID)
		if err == nil && c.ID != 0 {
			err = ErrActRefused(fmt.Sprintf("card %s waits on %s: answer that case, not the card", cardName(t), caseRef(in.Via, "", c)))
		}
		return Case{}, err
	}
	c, err := r.store.CaseByID(ctx, in.CaseID)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.TaskID != t.ID:
		other, _, _ := r.store.Task(ctx, c.TaskID)
		other.ID = c.TaskID
		return Case{}, ErrActRefused(fmt.Sprintf("%s is card %s's, not card %s's: answer it on its own card", caseRef(in.Via, "", c), cardName(other), cardName(t)))
	case c.Status != CaseOpen && c.Status != CaseAtHuman:
		return c, ErrCaseEnded{Case: c, Card: cardName(t)}
	}
	return c, nil
}

// actWrite writes answer in on card t, with what it did and the comment
// that carries it: with its case c ending, the resolution or the
// withdrawal with the draw and the step in one transaction; with none, the
// draw alone, and a note's step, which stays in progress. A draw past its
// rung's bounds is refused.
func (r *Runner) actWrite(ctx context.Context, t Task, c Case, in ActInput, own int64, did []CaseAction, comment int64, draw *CaseDraw, ends bool) error {
	res := in.Res
	step := CaseStep{CaseID: c.ID, Rung: in.Rung, Actor: in.By, Did: did, Decision: res.Action, Grounds: res.Note, Outcome: StepResolved,
		Read: in.Read, Found: in.Found, CostEUR: in.CostEUR}
	if ends {
		step.ID = own
	}
	var err error
	switch {
	case ends:
		resolution := CaseResolution{By: in.By, Rung: in.Rung, Action: res.Action, Args: maps.Clone(res.Args), Reason: res.Note}
		if comment != 0 {
			if resolution.Args == nil {
				resolution.Args = map[string]any{}
			}
			resolution.Args["comment"] = comment
		}
		if res.Action == ActWithdraw {
			step.Outcome = StepWithdrawn
			resolution.Reason = cmp.Or(strings.TrimSpace(res.Note), "withdrawn by "+in.By)
		}
		err = r.store.ResolveCase(ctx, c.ID, resolution, draw, &step)
	case draw != nil:
		err = r.store.DrawCard(ctx, t.ID, *draw)
	}
	if err == nil && c.ID != 0 && !ends {
		if res.Action == ActNote {
			step.Outcome = StepInProgress
		}
		_, err = r.store.AppendCaseStep(ctx, step)
	}
	var bound *DrawBoundError
	var ended ErrCaseEnded
	switch {
	case errors.As(err, &bound):
		return ErrActRefused(bound.Error())
	case errors.As(err, &ended) && ended.Card == "":
		ended.Card = cardName(t)
		return ended
	}
	return err
}

// ownStep is the id of the in-progress step of in.Rung on case c, the
// newest, 0 for none: the step Act ends with its answer.
func (r *Runner) ownStep(ctx context.Context, c Case, in ActInput) (int64, error) {
	if c.ID == 0 {
		return 0, nil
	}
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return 0, err
	}
	for _, st := range slices.Backward(steps) {
		if st.Rung == in.Rung && st.Outcome == StepInProgress {
			return st.ID, nil
		}
	}
	return 0, nil
}

// runGrants reports whether the run waiting on case c grants the sessions
// and agent time res draws itself: a run from before the cases gate, which
// wakes on the human's comment and grants its own. A trip under the gate
// marks the case it opens with its trip fact, which rung 0 acts on before
// any step, and writes a step of rung 0 on every case it waits on, the
// old run's case a restarted run takes among them. No older face does
// either.
func (r *Runner) runGrants(ctx context.Context, c Case, res Resolution) (bool, error) {
	if d, _ := res.draw(); c.ID == 0 || c.Facts["trip"] != nil || d.Sessions == 0 && d.AgentTime == 0 {
		return false, nil
	}
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 0 }), nil
}

// ranErr is err after the outside actions of did ran: their results lead
// it, so the human reads what already happened.
func ranErr(did []CaseAction, err error) error {
	var ran []string
	for _, a := range did {
		if slices.Contains(outsideActions, a.Action) {
			ran = append(ran, a.Result)
		}
	}
	if len(ran) == 0 {
		return err
	}
	return fmt.Errorf("%s The answer stopped there: %w", strings.Join(ran, " "), err)
}

// signalRun signals card taskID's run. A failed signal is logged: the
// answer stands in the store.
func (r *Runner) signalRun(ctx context.Context, taskID, name string, sig TaskSignal) {
	if err := r.cfg.Workflows.Signal(ctx, taskID, name, sig); err != nil {
		log.Printf("task %s: signal %s of %s's answer: %v", taskID, name, sig.Author, err)
	}
}

// EndCase withdraws case caseID for reason: the run's end of a wait that
// ended on its own (a cap turned, an image came). A case that ended
// already, and no case (0), are no error.
func (r *Runner) EndCase(ctx context.Context, caseID int64, reason string) error {
	if caseID == 0 {
		return nil
	}
	return caseEnded(r.store.WithdrawCase(ctx, caseID, reason))
}

// answersACase reports whether a answers a case and so needs one named:
// every action the run carries out but the draws, a withdrawal, a note
// and a settle.
func answersACase(a Action) bool {
	return a.InRun && a.Name != ActExtend && a.Name != ActFund || a.Name == ActWithdraw || a.Name == ActNote || a.Name == ActSettle
}

// actRefusal is why in may not answer the facts f of card, "" when it
// may: no such rung, an action the table does not hold, an action that
// answers a case on a card with none open, or the table's refusal, with
// the actions the rung may take there.
func actRefusal(f CaseFacts, in ActInput, card string) string {
	if in.Rung < 0 || in.Rung > 3 {
		return fmt.Sprintf("there is no rung %d", in.Rung)
	}
	a, ok := ActionByName(in.Res.Action)
	var why string
	switch {
	case !ok:
		why = "there is no action " + in.Res.Action
	case in.CaseID == 0 && answersACase(a):
		why = fmt.Sprintf("no case is open on card %s, so %s has nothing to answer", card, a.Name)
	default:
		why = a.refusal(f, in.Rung, in.Res)
	}
	if why == "" {
		return ""
	}
	var names []string
	for _, o := range Offered(f, in.Rung) {
		if in.CaseID != 0 || !answersACase(o) {
			names = append(names, o.Name)
		}
	}
	if len(names) == 0 {
		return why + ", and " + who(in.Rung) + " may take no action on a card with no open case"
	}
	return why + ", and " + who(in.Rung) + " may take " + textutil.AndList(names)
}

// askOf is the question the newest escalation of card taskID put, nil for none.
func (r *Runner) askOf(ctx context.Context, taskID string) (*HumanQuestion, error) {
	events, err := r.store.IntakeEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var q *HumanQuestion
	for _, e := range events {
		if e.Kind == IntakeEscalation {
			q = e.Question
		}
	}
	return q, nil
}

// humanChoice is why human card t's case does not take in, "" when it
// does: rung 2 or the human picks a value its question offers, with no
// draw beside it.
func (r *Runner) humanChoice(ctx context.Context, t Task, in ActInput) (string, error) {
	switch {
	case in.Rung != 2 && in.Rung != 3:
		return fmt.Sprintf("%s may not answer a human card: rung 2 and the human may", who(in.Rung)), nil
	case in.Res.Fund != nil:
		return fmt.Sprintf("card %s is a human card, so its answer draws nothing: answer it without the fund", cardName(t)), nil
	case in.Res.Action == ActClose:
		return "", nil // a human card closes whatever it asks, as its Close button does
	}
	q, err := r.askOf(ctx, t.ID)
	if err != nil {
		return "", err
	}
	switch {
	case q == nil || len(q.Choices) == 0:
		return fmt.Sprintf("card %s asks no question with choices: answer it with a note", cardName(t)), nil
	case q.Offers(in.Res.Action):
		return "", nil
	}
	choices := make([]string, 0, len(q.Choices))
	for _, c := range q.Choices {
		choices = append(choices, fmt.Sprintf("%s (%s)", c.Label, c.Value))
	}
	return fmt.Sprintf("card %s takes one of its choices or a note: %s", cardName(t), strings.Join(choices, ", ")), nil
}

// actOutside runs what in.Res does outside the run before the case ends:
// a retry files the failed dependency again, or finishes a retry of it
// that stopped part way, and names the replacement of one retried already;
// a replan sends the card to the planner, or a failed dependency's work to
// its plan's planner, which the card then waits on. It answers what ran,
// with its result.
func (r *Runner) actOutside(ctx context.Context, t Task, f CaseFacts, in ActInput) ([]CaseAction, error) {
	switch in.Res.Action {
	case ActDeployNow:
		return r.deployNow(ctx, t, in)
	case ActRetry:
		id, _ := in.Res.Args["task_id"].(string)
		if id == "" {
			i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return retryRefusal(d) == "" })
			if i < 0 {
				return nil, ErrActRefused("no failed dependency is open to a retry")
			}
			id = f.Dependencies[i].TaskID
		}
		old, _, err := r.task(ctx, id)
		if err != nil {
			return nil, err
		}
		issue, err := r.RetryCard(ctx, id, in.By)
		var no ErrNoRetry
		if as, ok := retriedAs(f, id); errors.As(err, &no) && ok {
			return []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": id}, Result: fmt.Sprintf("%s was retried as %s already.", cardName(old), as)}}, nil
		}
		if err != nil {
			return nil, err
		}
		return []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": id, "replacement": issue},
			Result: fmt.Sprintf("Filed #%d in place of #%d.", issue, old.IssueNumber)}}, nil
	case ActReplan:
		if id, _ := in.Res.Args["task_id"].(string); id != "" {
			return r.replanDependency(ctx, t, id)
		}
		said, err := r.Replan(ctx, t.ID, ReplanAsk{By: in.By, Comment: in.Res.Note})
		if err != nil {
			return nil, err
		}
		return []CaseAction{{Action: ActReplan, Result: said}}, nil
	}
	return nil, nil
}

// deployNow releases held production of card t's repository for in.By: the
// agent decides for rung 2, the stored member for the human. Its refusals
// come back as Act's, the secrets of any other failure masked.
func (r *Runner) deployNow(ctx context.Context, t Task, in ActInput) ([]CaseAction, error) {
	if r.DeployNow == nil {
		return nil, errors.New("no release keeper is wired to deploy")
	}
	decider := "member"
	if in.Rung == 2 {
		decider = "agent"
	}
	commit, err := r.DeployNow(ctx, t.Repository, in.By, cmp.Or(in.Via, ViaFactoryd), decider)
	var red ErrMainRed
	switch {
	case errors.Is(err, ErrNotHeld):
		return nil, ErrActRefused("production of " + t.Repository + " is not held: every merge goes to production on its own")
	case errors.As(err, &red):
		return nil, ErrActRefused(fmt.Sprintf("main of %s at %s is red: deploy_now waits for a green run on main", t.Repository, red.Commit[:min(7, len(red.Commit))]))
	case err != nil:
		return nil, ErrActRefused("production of " + t.Repository + " could not be released: " + r.scrub(err.Error()))
	}
	return []CaseAction{{Action: ActDeployNow, Result: fmt.Sprintf("Deploy now: main of %s at %s goes to production. A merge after it waits for the open plans again.",
		t.Repository, commit[:min(7, len(commit))])}}, nil
}

// replanDependency sends failed dependency id of card t to its plan's
// planner (impasse) and answers what ran. It is refused when the card
// still waits on it after, as when its plan takes no replan.
func (r *Runner) replanDependency(ctx context.Context, t Task, id string) ([]CaseAction, error) {
	dep, _, err := r.task(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := r.impasse(ctx, dep); err != nil {
		return nil, err
	}
	deps, err := r.store.OpenDependencies(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(deps, func(d Dependency) bool { return d.TaskID == id }) {
		return nil, ErrActRefused(fmt.Sprintf("the planner did not take the work of #%d, and card %s still waits on it: retry it or close the card", dep.IssueNumber, cardName(t)))
	}
	return []CaseAction{{Action: ActReplan, Args: map[string]any{"task_id": id},
		Result: fmt.Sprintf("Sent the work of #%d to its plan's planner.", dep.IssueNumber)}}, nil
}

// closeCard closes card t for in.By: its issue as factoryd's recorded
// close, which the webhook then ends the card on, with factoryd's comment
// unless the caller posts its own after the close, or with no issue a
// signal to its run. An ended card's close is closeEnded's.
func (r *Runner) closeCard(ctx context.Context, t Task, in ActInput, posted bool) (CaseAction, error) {
	if Final(t.Status) {
		return r.closeEnded(ctx, t, in, posted)
	}
	if t.IssueNumber == 0 {
		if err := r.cfg.Workflows.Signal(ctx, t.ID, SignalIssueClosed, TaskSignal{Author: in.By}); err != nil {
			return CaseAction{}, err
		}
		return CaseAction{Action: ActClose, Result: fmt.Sprintf("Card %s was told to close.", t.ID)}, nil
	}
	comment := ""
	if !posted {
		comment = r.answerComment(t, in.By, in.Res)
	}
	if err := closeIssueAsFactory(ctx, r.store, r.forgejo, t, comment); err != nil {
		return CaseAction{}, err
	}
	return CaseAction{Action: ActClose, Result: fmt.Sprintf("Closed #%d.", t.IssueNumber)}, nil
}

// closeEnded closes what ended card t left open for in.By: its issue, when
// Forgejo reads it open, as the heal closes an unconfirmed close. It
// signals no run. The comment is the answer's record, as a live card's
// close posts it, or factoryd's own words for rung 0, and none when the
// caller posts its own. On an issue closed already a person's answer is
// posted alone.
func (r *Runner) closeEnded(ctx context.Context, t Task, in ActInput, posted bool) (CaseAction, error) {
	if t.IssueNumber == 0 {
		return CaseAction{Action: ActClose, Result: fmt.Sprintf("Card %s has no issue to close.", cardName(t))}, nil
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return CaseAction{}, err
	}
	comment := ""
	switch {
	case posted:
	case in.By == FactoryDecider:
		comment = "factoryd: this card ended " + t.Status + ", so its issue closes."
	default:
		comment = r.answerComment(t, in.By, in.Res)
	}
	closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil {
		return CaseAction{}, err
	}
	if !closed {
		if err := closeIssueAsFactory(ctx, r.store, r.forgejo, t, comment); err != nil {
			return CaseAction{}, err
		}
		return CaseAction{Action: ActClose, Result: fmt.Sprintf("Closed #%d.", t.IssueNumber)}, nil
	}
	if comment != "" && in.By != FactoryDecider {
		if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, comment); err != nil {
			return CaseAction{}, err
		}
	}
	return CaseAction{Action: ActClose, Result: fmt.Sprintf("#%d was closed already.", t.IssueNumber)}, nil
}

// caseDraw is what in draws for card t on facts f, nil for nothing: a raise
// of the ceiling, an extend's sessions, agent time and lines, and the
// money for the budget, with the card's draw bound and the pool its rung
// draws within. Its rows are
// pool draws of in's rung for case c, with the head, the checks and, when
// the timeline was read, the findings they were weighed on (boughtNothing).
func (r *Runner) caseDraw(t Task, f CaseFacts, c Case, in ActInput, via string) *CaseDraw {
	d, budget := in.Res.draw()
	lines, shares := in.Res.bounds()
	if d.IsZero() && lines <= 0 && shares <= 0 {
		return nil
	}
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	detail := map[string]any{"pool": true, "rung": in.Rung, "case_id": c.ID, "head": f.Head, "checks": f.Checks}
	if !slices.Contains(f.Partial, "timeline") {
		detail["findings"] = nonNil(f.Findings)
	}
	beside(detail, in.Detail)
	esc := v.EscalationOf(t.Repository)
	draw := &CaseDraw{Rung: in.Rung, Extend: policy.Draw{Sessions: d.Sessions, AgentTime: d.AgentTime}, FundEUR: budget,
		PolicyCeiling: CardCeiling(v.Document, project), CardBound: esc.CardDrawOf(t.Size), Pool: esc.Pool,
		Event: Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Actor: in.By, Via: via, Detail: detail}}
	switch in.Res.Action {
	case ActRaiseCeiling:
		draw.Raise, _ = in.Res.Args["eur"].(float64)
	case ActExtend:
		draw.Lines, draw.Shares = lines, shares
	}
	return draw
}

// RecordGrant records grant d, which card taskID's run made itself on
// by's answer (a writer's change request at the cap), as by's draw
// against the pool: a row of rung 3, never refused, that raises nothing
// on the card's row (RunGrants). key is the grant's identity: a retry
// after the commit drops its row on it and writes nothing.
func (r *Runner) RecordGrant(ctx context.Context, taskID, by string, d policy.Draw, key string) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	esc := v.EscalationOf(t.Repository)
	return r.store.DrawCard(ctx, t.ID, CaseDraw{Rung: 3, Extend: d, PolicyCeiling: CardCeiling(v.Document, project), CardBound: esc.CardDrawOf(t.Size),
		Pool: esc.Pool, RunGrants: true, Event: Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Actor: by, Via: ViaFactoryd,
			Detail: map[string]any{"pool": true, "rung": 3}, IdentityKey: key}})
}

// AuditDraw records grant d, which card taskID's run made itself on no
// writer's answer (the audit's gap verdict at a cap), as the factory's
// own draw against the pool: a row of rung 0, refused past the pool or
// the card's bound (draw-refused), that raises nothing on the card's row
// (RunGrants). key is the grant's identity: a retry after the commit
// drops its row on it and writes nothing.
func (r *Runner) AuditDraw(ctx context.Context, taskID string, d policy.Draw, key string) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	esc := v.EscalationOf(t.Repository)
	if err := r.store.DrawCard(ctx, t.ID, CaseDraw{Rung: 0, Extend: d, PolicyCeiling: CardCeiling(v.Document, project), CardBound: esc.CardDrawOf(t.Size),
		Pool: esc.Pool, RunGrants: true, Event: Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"pool": true, "rung": 0}, IdentityKey: key}}); err != nil {
		var refused *DrawBoundError
		if errors.As(err, &refused) {
			return temporal.NewNonRetryableApplicationError(err.Error(), errDrawRefused, nil)
		}
		return err
	}
	return nil
}

// beside adds the keys of extra that detail does not hold.
func beside(detail, extra map[string]any) {
	for k, v := range extra {
		if _, set := detail[k]; !set {
			detail[k] = v
		}
	}
}

// drawActions are the steps of draw d that res makes: its action's own
// draw, and the fund beside it.
func drawActions(res Resolution, d CaseDraw) []CaseAction {
	var out []CaseAction
	own, _ := Resolution{Action: res.Action, Args: res.Args}.draw()
	switch {
	case res.Action == ActRaiseCeiling:
		out = append(out, CaseAction{Action: res.Action, Args: res.Args, Result: fmt.Sprintf("Raised the ceiling by %.2f EUR.", d.Raise)})
	case !own.IsZero() || d.Lines > 0 || d.Shares > 0:
		out = append(out, CaseAction{Action: res.Action, Args: res.Args, Result: "Added " + raisedWords(own, d.Lines, d.Shares) + "."})
	}
	if res.Fund != nil && !res.Fund.IsZero() {
		out = append(out, CaseAction{Action: ActFund, Result: "Added " + raisedWords(*res.Fund, 0, 0) + "."})
	}
	return out
}

// raisedWords says every part an answer raises: draw d from the pool,
// lines on the line bound and check-in shares.
func raisedWords(d policy.Draw, lines, shares int) string {
	var parts []string
	if !d.IsZero() {
		parts = append(parts, drawWords(d)+" from the pool")
	}
	if lines > 0 {
		parts = append(parts, textutil.Plural(lines, "line", "lines")+" to its line bound")
	}
	if shares > 0 {
		parts = append(parts, textutil.Plural(shares, "more check-in share", "more check-in shares"))
	}
	return textutil.AndList(parts)
}

// drawWords says the parts of d that are not zero: "1 session and 1.00
// EUR".
func drawWords(d policy.Draw) string {
	var parts []string
	if d.Sessions != 0 {
		parts = append(parts, textutil.Plural(d.Sessions, "session", "sessions"))
	}
	if d.AgentTime != 0 {
		parts = append(parts, minutesWord(d.AgentTime)+" of agent time")
	}
	if d.EUR != 0 {
		parts = append(parts, fmt.Sprintf("%.2f EUR", d.EUR))
	}
	return textutil.AndList(parts)
}

// Sentence says r in a few words: its action and every part it raises.
func (r Resolution) Sentence() string {
	d, _ := r.draw()
	lines, shares := r.bounds()
	if said := raisedWords(d, lines, shares); said != "" {
		return r.Action + ": " + said
	}
	return r.Action
}

// answerComment is factoryd's record on t's issue of by's answer res: the
// action in words, and the note fenced below it.
func (r *Runner) answerComment(t Task, by string, res Resolution) string {
	body := fmt.Sprintf("factoryd: %s answered with %s.", by, res.Sentence())
	if note := strings.TrimSpace(res.Note); note != "" {
		body += "\n\n" + fenced(r.untrusted(t, note))
	}
	return body
}

// caseRef is case c of card as the reader of via takes it: the chat's
// tools answer by the id, so it keeps the id beside the words.
func caseRef(via, card string, c Case) string {
	words := caseWords(card, c)
	if via == ViaChat {
		words += fmt.Sprintf(" (case %d)", c.ID)
	}
	return words
}

// actText is the line an answer res on card t's case c leaves the human
// or the chat, with what ran outside the run.
func actText(in ActInput, t Task, c Case, res Resolution, did []CaseAction) string {
	var text string
	ref := caseRef(in.Via, cardName(t), c)
	switch {
	case c.ID == 0:
		text = fmt.Sprintf("Card %s took %s.", cardName(t), res.Sentence())
	case res.Action == ActNote:
		text = fmt.Sprintf("The note is on %s, which stays open.", ref)
	case res.Action == ActWithdraw:
		text = fmt.Sprintf("%s is withdrawn.", upperFirst(ref))
	default:
		text = fmt.Sprintf("%s is resolved with %s.", upperFirst(ref), res.Sentence())
	}
	for _, a := range did {
		if slices.Contains(outsideActions, a.Action) {
			text += " " + a.Result
		}
	}
	return text
}

func upperFirst(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// cardName is how a refusal names card t: its issue, else its task id.
func cardName(t Task) string { return cardRef(Event{TaskID: t.ID}, t) }
