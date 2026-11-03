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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// ActivityEndCase is EndCase's activity.
const ActivityEndCase = "EndCase"

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
	// Post posts the answer's issue comment as By and answers its id: the
	// console's, as the human, is the comment a run from before the cases
	// gate wakes on. Act calls it once the answer passed its checks and
	// its outside actions ran (a close included), before the case ends,
	// and only for an answer that resolves its case. Nil: Act posts factoryd's record
	// after the case ends, when the card has an issue, except for a human
	// card's choice, whose work the console does, and for factoryd's own
	// answer, whose steps are the case's record.
	Post func(ctx context.Context) (int64, error)
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
}

// ErrActRefused is Act's refusal: the state that refuses the answer and
// the actions that fit.
type ErrActRefused string

func (e ErrActRefused) Error() string { return string(e) }

// outsideActions are the actions Act runs outside the run, before the
// case ends.
var outsideActions = []string{ActRetry, ActReplan, ActClose}

// Act is the one way a human, the chat or a rung answers a case. It reads
// the facts and refuses an action the table does not offer rung on them,
// or a draw past the rung's bounds. It runs the outside actions first, so
// a refusal of theirs leaves the case open, then has the caller post its
// comment. It ends the case and writes the draw and the step in one
// transaction, which weighs the draw against its rung's bounds again
// under the project's lock. Then it posts factoryd's comment when the
// caller posted none, records the answer and signals the run. A note
// records a step and ends nothing. A human card's case takes a value of its question's
// choices, which Act records and runs nothing for. Rung 0 is factoryd's,
// and factoryd answers on rung 0 alone.
func (r *Runner) Act(ctx context.Context, in ActInput) (ActResult, error) {
	switch {
	case in.Rung == 0 && in.By != FactoryDecider:
		return ActResult{}, ErrActRefused("rung 0 is factoryd's own: answer on rung 1, 2 or 3")
	case in.Rung != 0 && in.By == FactoryDecider:
		return ActResult{}, ErrActRefused(fmt.Sprintf("rung 0 is factoryd's own, and factoryd answers on rung 0 alone: answer on rung 0, not rung %d", in.Rung))
	}
	t, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return ActResult{}, err
	}
	c, err := r.actCase(ctx, t, in.CaseID)
	if err != nil {
		return ActResult{Case: c}, err
	}
	res := in.Res
	choice := c.Kind == CaseHumanCard && res.Action != ActNote
	var f CaseFacts
	if choice {
		why, err := r.humanChoice(ctx, t, in)
		if err != nil {
			return ActResult{}, err
		}
		if why != "" {
			return ActResult{}, ErrActRefused(why)
		}
	} else {
		head, _ := c.Facts["head"].(string)
		if f, err = r.tripFacts(ctx, t, head); err != nil {
			return ActResult{}, err
		}
		if why := actRefusal(f, in, cardName(t)); why != "" {
			return ActResult{}, ErrActRefused(why)
		}
	}
	via := cmp.Or(in.Via, ViaFactoryd)
	var out ActResult
	var draw *CaseDraw
	if !choice {
		if out.Did, err = r.actOutside(ctx, t, f, in); err != nil {
			return out, err
		}
		draw = r.caseDraw(t, f, c, in, via)
	}
	ends := c.ID != 0 && res.Action != ActNote
	answers := ends && res.Action != ActWithdraw
	posts := answers && in.Post != nil
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
	if err := r.actWrite(ctx, t, c, in, slices.Concat(out.Did, drew), out.Comment, draw, ends); err != nil {
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
	out.Text = actText(t, c, res, out.Did)
	if !answers {
		return out, nil
	}
	if out.Comment == 0 && t.IssueNumber != 0 && res.Action != ActClose && !choice && in.By != FactoryDecider {
		if out.Comment, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, r.answerComment(t, in.By, res)); err != nil {
			log.Printf("task %s: factoryd's comment of %s's %s on case %d: %v", t.ID, in.By, res.Action, c.ID, err)
		}
	}
	kind := EventQuestionAnswered
	if draw != nil && draw.Extend.Sessions > 0 {
		kind = EventSessionGranted
	}
	detail := map[string]any{"action": res.Action, "case_id": c.ID, "answer": cmp.Or(strings.TrimSpace(res.Note), res.sentence())}
	if out.Comment != 0 {
		detail["comment"] = out.Comment
	}
	if choice {
		detail["value"] = res.Action
	}
	beside(detail, in.Detail)
	r.timeline.RecordRow(ctx, t.ID, kind, in.By, via, detail)
	if t.Kind == "human" {
		return out, nil // a human card runs no workflow: a signal would start one
	}
	r.signalRun(ctx, t.ID, SignalCaseResolved, TaskSignal{Author: in.By, Comment: out.Comment,
		Case: &CaseSignal{CaseID: c.ID, Rung: in.Rung, By: in.By, Res: res, Comment: out.Comment}})
	if out.Comment != 0 {
		r.signalRun(ctx, t.ID, SignalAnswerWhole, TaskSignal{Author: in.By, Comment: out.Comment})
	}
	return out, nil
}

// actCase is the case in answers on card t: case id when it is live and
// t's, else none for 0. An answer with no case on a card that waits on a
// live ask case is refused: it would draw and leave the case waiting.
func (r *Runner) actCase(ctx context.Context, t Task, id int64) (Case, error) {
	if id == 0 {
		open, err := r.store.OpenCasesOf(ctx, t.ID)
		if err != nil {
			return Case{}, err
		}
		for _, c := range slices.Backward(open) {
			if !slices.Contains(ownAnswerKinds, c.Kind) {
				return Case{}, ErrActRefused(fmt.Sprintf("card %s waits on its case %d: answer that case by its id", cardName(t), c.ID))
			}
		}
		return Case{}, nil
	}
	c, err := r.store.CaseByID(ctx, id)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.TaskID != t.ID:
		other, _, _ := r.store.Task(ctx, c.TaskID)
		other.ID = c.TaskID
		return Case{}, ErrActRefused(fmt.Sprintf("case %d is card %s's, not card %s's: answer it on its own card", c.ID, cardName(other), cardName(t)))
	case c.Status != CaseOpen && c.Status != CaseAtHuman:
		return c, ErrCaseEnded{c}
	}
	return c, nil
}

// actWrite writes answer in on card t, with what it did and the comment
// that carries it: with its case c ending, the resolution or the
// withdrawal with the draw and the step in one transaction; with none, the
// draw alone, and a note's step, which stays in progress. A draw past its
// rung's bounds is refused.
func (r *Runner) actWrite(ctx context.Context, t Task, c Case, in ActInput, did []CaseAction, comment int64, draw *CaseDraw, ends bool) error {
	res := in.Res
	step := CaseStep{CaseID: c.ID, Rung: in.Rung, Actor: in.By, Did: did, Decision: res.Action, Grounds: res.Note, Outcome: StepResolved}
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
		step.Outcome = StepInProgress
		_, err = r.store.AppendCaseStep(ctx, step)
	}
	var bound *DrawBoundError
	if errors.As(err, &bound) {
		return ErrActRefused(bound.Error())
	}
	return err
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
// every action the run carries out but the draws, and a withdrawal and a
// note.
func answersACase(a Action) bool {
	return a.InRun && a.Name != ActExtend && a.Name != ActFund || a.Name == ActWithdraw || a.Name == ActNote
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

// humanChoice is why human card t's case does not take in, "" when it
// does: rung 2 or the human picks a value its question offers, with no
// draw beside it.
func (r *Runner) humanChoice(ctx context.Context, t Task, in ActInput) (string, error) {
	switch {
	case in.Rung != 2 && in.Rung != 3:
		return fmt.Sprintf("%s may not answer a human card: rung 2 and the human may", who(in.Rung)), nil
	case in.Res.Fund != nil:
		return fmt.Sprintf("card %s is a human card, so its answer draws nothing: answer it without the fund", cardName(t)), nil
	}
	events, err := r.store.IntakeEvents(ctx, t.ID)
	if err != nil {
		return "", err
	}
	var q *HumanQuestion
	for _, e := range events {
		if e.Kind == IntakeEscalation {
			q = e.Question
		}
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
// a retry files the failed dependency again, a replan sends the card to
// the planner, or a failed dependency's work to its plan's planner, which
// the card then waits on. It answers what ran, with its result.
func (r *Runner) actOutside(ctx context.Context, t Task, f CaseFacts, in ActInput) ([]CaseAction, error) {
	switch in.Res.Action {
	case ActRetry:
		id, _ := in.Res.Args["task_id"].(string)
		i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return d.TaskID == id || id == "" && retryRefusal(d) == "" })
		if i < 0 {
			return nil, ErrActRefused("no failed dependency is open to a retry")
		}
		dep := f.Dependencies[i]
		issue, err := r.RetryCard(ctx, dep.TaskID, in.By)
		if err != nil {
			return nil, err
		}
		return []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": dep.TaskID},
			Result: fmt.Sprintf("Filed #%d in place of #%d.", issue, dep.Issue)}}, nil
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
// signal to its run.
func (r *Runner) closeCard(ctx context.Context, t Task, in ActInput, posted bool) (CaseAction, error) {
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

// caseDraw is what in draws for card t on facts f, nil for nothing: a raise
// of the ceiling, an extend's sessions, agent time and lines, and the
// money for the budget, with the card's draw bound and the pool its rung
// draws within. Its rows are
// pool draws of in's rung for case c, with the head, the checks and, when
// the timeline was read, the findings they were weighed on (boughtNothing).
func (r *Runner) caseDraw(t Task, f CaseFacts, c Case, in ActInput, via string) *CaseDraw {
	d, budget := in.Res.draw()
	if d.IsZero() {
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
		draw.Lines, _ = toInt(in.Res.Args["lines"])
	}
	return draw
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
	case !own.IsZero():
		out = append(out, CaseAction{Action: res.Action, Args: res.Args, Result: "Drew " + drawWords(own) + " from the pool."})
	}
	if res.Fund != nil && !res.Fund.IsZero() {
		out = append(out, CaseAction{Action: ActFund, Result: "Drew " + drawWords(*res.Fund) + " from the pool."})
	}
	return out
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

// sentence says r in a few words: its action and what it draws from the
// pool.
func (r Resolution) sentence() string {
	if d, _ := r.draw(); !d.IsZero() {
		return r.Action + ": " + drawWords(d) + " from the pool"
	}
	return r.Action
}

// answerComment is factoryd's record on t's issue of by's answer res: the
// action in words, and the note fenced below it.
func (r *Runner) answerComment(t Task, by string, res Resolution) string {
	body := fmt.Sprintf("factoryd: %s answered with %s.", by, res.sentence())
	if note := strings.TrimSpace(res.Note); note != "" {
		body += "\n\n" + fenced(r.untrusted(t, note))
	}
	return body
}

// actText is the line an answer res on card t's case c leaves the human
// or the chat, with what ran outside the run.
func actText(t Task, c Case, res Resolution, did []CaseAction) string {
	var text string
	switch {
	case c.ID == 0:
		text = fmt.Sprintf("Card %s took %s.", cardName(t), res.sentence())
	case res.Action == ActNote:
		text = fmt.Sprintf("The note is on case %d, which stays open.", c.ID)
	case res.Action == ActWithdraw:
		text = fmt.Sprintf("Case %d is withdrawn.", c.ID)
	default:
		text = fmt.Sprintf("Case %d is resolved with %s.", c.ID, res.sentence())
	}
	for _, a := range did {
		if slices.Contains(outsideActions, a.Action) {
			text += " " + a.Result
		}
	}
	return text
}

// cardName is how a refusal names card t: its issue, else its task id.
func cardName(t Task) string { return cardRef(Event{TaskID: t.ID}, t) }
