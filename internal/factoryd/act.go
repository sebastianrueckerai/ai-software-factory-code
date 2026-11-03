package factoryd

import (
	"cmp"
	"context"
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
// ViaChat, "" for ViaFactoryd), on which rung, on which case of the card (0
// for an action on a card with no open case, such as a fund while it
// runs), and the issue comment the caller posted as By (0 for none: Act
// posts factoryd's).
type ActInput struct {
	TaskID  string
	CaseID  int64
	By      string
	Via     string
	Rung    int
	Res     Resolution
	Comment int64
}

// ActResult is what an answer did: the case as it stands after it, what
// ran outside the run with its result, what it drew from the pool, the
// comment that carries it, and one line for the human or the chat.
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

// Act is the one way a human, the chat or a rung answers a case. It reads
// the facts and refuses an action the table does not offer rung on them,
// or a draw past the rung's bounds. It runs the actions outside the run
// first, so a refusal of theirs leaves the case open. Then it resolves the
// case and writes the draw and the step in one transaction, posts the
// comment, records the answer and signals the run. A note records a step
// and resolves nothing. A human card's case takes a value of its
// question's choices, which Act records and runs nothing for.
func (r *Runner) Act(ctx context.Context, in ActInput) (ActResult, error) {
	t, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return ActResult{}, err
	}
	var c Case
	if in.CaseID != 0 {
		if c, err = r.store.CaseByID(ctx, in.CaseID); err != nil {
			return ActResult{}, err
		}
		switch {
		case c.TaskID != t.ID:
			return ActResult{}, ErrActRefused(fmt.Sprintf("case %d is card %s's, not %s's", c.ID, c.TaskID, t.ID))
		case c.Status != CaseOpen && c.Status != CaseAtHuman && !answeredBy(c, in.Comment):
			return ActResult{Case: c}, ErrCaseEnded{c}
		}
	}
	f, err := r.tripFacts(ctx, t)
	if err != nil {
		return ActResult{}, err
	}
	res := in.Res
	choice := c.Kind == CaseHumanCard && res.Action != ActNote
	var why string
	if choice {
		if why, err = r.humanChoice(ctx, t, res); err != nil {
			return ActResult{}, err
		}
	} else {
		why = actRefusal(f, in)
	}
	if why != "" {
		return ActResult{}, ErrActRefused(why)
	}
	via := cmp.Or(in.Via, ViaFactoryd)
	comment := ""
	if in.Comment == 0 {
		comment = r.answerComment(t, in.By, res)
	}
	var did []CaseAction
	var draw *CaseDraw
	var drawn policy.Draw
	if !choice {
		if did, err = r.actOutside(ctx, t, f, in, comment); err != nil {
			return ActResult{}, err
		}
		if draw = r.caseDraw(t, f, c, in, via); draw != nil {
			drawn = draw.Extend
			drawn.EUR = draw.FundEUR
			did = append(did, drawActions(res, *draw)...)
		}
	}
	step := CaseStep{Rung: in.Rung, Actor: in.By, Did: did, Decision: res.Action, Grounds: res.Note, Outcome: StepResolved}
	out := ActResult{Did: did, Drawn: drawn, Comment: in.Comment}
	resolves := c.ID != 0 && res.Action != ActNote && res.Action != ActWithdraw
	if resolves {
		resolution := CaseResolution{By: in.By, Rung: in.Rung, Action: res.Action, Args: maps.Clone(res.Args), Reason: res.Note}
		if in.Comment != 0 {
			if resolution.Args == nil {
				resolution.Args = map[string]any{}
			}
			resolution.Args["comment"] = in.Comment
		}
		if err := r.store.ResolveCase(ctx, c.ID, resolution, draw, &step); err != nil {
			return ActResult{}, err
		}
	} else if err := r.actUnresolved(ctx, t, c, in, draw, step); err != nil {
		return ActResult{}, err
	}
	if c.ID != 0 {
		if out.Case, err = r.store.CaseByID(ctx, c.ID); err != nil {
			log.Printf("task %s: case %d after %s's %s: %v", t.ID, c.ID, in.By, res.Action, err)
			out.Case = c
		}
	}
	out.Text = actText(t, c, res, did)
	if !resolves {
		return out, nil
	}
	if out.Comment == 0 && c.CommentID != 0 && t.IssueNumber != 0 && res.Action != ActClose {
		if out.Comment, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, comment); err != nil {
			log.Printf("task %s: the comment of %s's %s on case %d: %v", t.ID, in.By, res.Action, c.ID, err)
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

// actUnresolved writes an answer that resolves no case: its draw alone,
// and for a case a note's step, which stays in progress, or a withdrawal.
func (r *Runner) actUnresolved(ctx context.Context, t Task, c Case, in ActInput, draw *CaseDraw, step CaseStep) error {
	if draw != nil {
		if err := r.drawAlone(ctx, t.ID, *draw); err != nil {
			return err
		}
	}
	if c.ID == 0 {
		return nil
	}
	step.CaseID = c.ID
	if in.Res.Action == ActWithdraw {
		if err := r.store.WithdrawCase(ctx, c.ID, cmp.Or(strings.TrimSpace(in.Res.Note), "withdrawn by "+in.By)); err != nil {
			return err
		}
		step.Outcome = StepWithdrawn
	} else {
		step.Outcome = StepInProgress
	}
	_, err := r.store.AppendCaseStep(ctx, step)
	return err
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

// answeredBy reports whether case c ended as a plain answer by the
// webhook's copy of answer comment comment, which an answer on that
// comment resolves again (ResolveCase).
func answeredBy(c Case, comment int64) bool {
	if comment == 0 || c.Status != CaseResolved || c.Resolution == nil || c.Resolution.Action != caseAnswer {
		return false
	}
	n, _ := toInt(c.Resolution.Args["comment"])
	return int64(n) == comment
}

// actRefusal is why in may not answer card facts f, "" when it may: no
// such rung, an action the table does not hold, an action that answers a
// case asked with none, or the table's refusal, with the actions the rung
// may take.
func actRefusal(f CaseFacts, in ActInput) string {
	if in.Rung < 0 || in.Rung > 3 {
		return fmt.Sprintf("there is no rung %d", in.Rung)
	}
	a, ok := ActionByName(in.Res.Action)
	var why string
	switch {
	case !ok:
		why = "there is no action " + in.Res.Action
	case in.CaseID == 0 && (a.InRun && a.Name != ActExtend && a.Name != ActFund || a.Name == ActWithdraw || a.Name == ActNote):
		return a.Name + " answers a case and none is named: answer the card's open case"
	default:
		why = a.refusal(f, in.Rung, in.Res)
	}
	if why == "" {
		return ""
	}
	var names []string
	for _, o := range Offered(f, in.Rung) {
		names = append(names, o.Name)
	}
	return why + ", and " + who(in.Rung) + " may take " + andList(names)
}

// humanChoice is why human card t's case does not take res, "" when its
// question offers res's action as a value and res draws nothing.
func (r *Runner) humanChoice(ctx context.Context, t Task, res Resolution) (string, error) {
	if res.Fund != nil {
		return fmt.Sprintf("card #%d is a human card: its answer draws nothing", t.IssueNumber), nil
	}
	events, err := r.store.IntakeEvents(ctx, t.ID)
	if err != nil {
		return "", err
	}
	q := &HumanQuestion{}
	for _, e := range events {
		if e.Kind == IntakeEscalation && e.Question != nil {
			q = e.Question
		}
	}
	if q.Offers(res.Action) {
		return "", nil
	}
	values := make([]string, 0, len(q.Choices))
	for _, c := range q.Choices {
		values = append(values, c.Value)
	}
	return fmt.Sprintf("card #%d takes one of its choices or a note: %s", t.IssueNumber, strings.Join(values, ", ")), nil
}

// actOutside runs what in.Res does outside the run, before its case ends:
// a retry files the failed dependency again, a replan sends the card to
// the planner, a close closes the card's issue as factoryd with comment
// ("" for none), or with no issue tells the run. It answers what ran,
// with its result.
func (r *Runner) actOutside(ctx context.Context, t Task, f CaseFacts, in ActInput, comment string) ([]CaseAction, error) {
	switch in.Res.Action {
	case ActRetry:
		dep, _ := in.Res.Args["task_id"].(string)
		if dep == "" {
			i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return retryRefusal(d) == "" })
			if i < 0 {
				return nil, ErrActRefused("no failed dependency is open to a retry")
			}
			dep = f.Dependencies[i].TaskID
		}
		issue, err := r.RetryCard(ctx, dep, in.By)
		if err != nil {
			return nil, err
		}
		return []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": dep}, Result: fmt.Sprintf("Filed #%d in place of %s.", issue, dep)}}, nil
	case ActReplan:
		said, err := r.Replan(ctx, t.ID, ReplanAsk{By: in.By, Comment: in.Res.Note})
		if err != nil {
			return nil, err
		}
		return []CaseAction{{Action: ActReplan, Result: said}}, nil
	case ActClose:
		if t.IssueNumber == 0 {
			if err := r.cfg.Workflows.Signal(ctx, t.ID, SignalIssueClosed, TaskSignal{Author: in.By}); err != nil {
				return nil, err
			}
			return []CaseAction{{Action: ActClose, Result: "The run was told to close the card."}}, nil
		}
		if err := closeIssueAsFactory(ctx, r.store, r.forgejo, t, comment); err != nil {
			return nil, err
		}
		return []CaseAction{{Action: ActClose, Result: fmt.Sprintf("Closed #%d.", t.IssueNumber)}}, nil
	}
	return nil, nil
}

// caseDraw is what in draws for card t on facts f, nil for nothing: a
// ceiling raise on the card's ceiling, an extend's sessions, agent time
// and lines, and the money for the budget. Its rows are pool draws of
// in's rung for case c, with the head, the checks and, when the timeline
// was read, the findings they were weighed on (boughtNothing).
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
	draw := &CaseDraw{Extend: policy.Draw{Sessions: d.Sessions, AgentTime: d.AgentTime}, FundEUR: budget, PolicyCeiling: CardCeiling(v.Document, project),
		Event: Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Actor: in.By, Via: via, Detail: detail}}
	switch in.Res.Action {
	case ActRaiseCeiling:
		raise, _ := in.Res.Args["eur"].(float64)
		draw.Ceiling = f.CeilingEUR + raise
	case ActExtend:
		draw.Lines, _ = toInt(in.Res.Args["lines"])
	}
	return draw
}

// drawActions are the steps of draw d that res makes: its action's own
// draw, and the fund beside it.
func drawActions(res Resolution, d CaseDraw) []CaseAction {
	var out []CaseAction
	own, _ := Resolution{Action: res.Action, Args: res.Args}.draw()
	switch {
	case res.Action == ActRaiseCeiling:
		out = append(out, CaseAction{Action: res.Action, Args: res.Args, Result: fmt.Sprintf("the ceiling to %.2f EUR", d.Ceiling)})
	case !own.IsZero():
		out = append(out, CaseAction{Action: res.Action, Args: res.Args, Result: drawWords(own) + " from the pool"})
	}
	if res.Fund != nil && !res.Fund.IsZero() {
		out = append(out, CaseAction{Action: ActFund, Result: drawWords(*res.Fund) + " from the pool"})
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
	return andList(parts)
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
		text = fmt.Sprintf("Card %s took %s.", t.ID, res.sentence())
	case res.Action == ActNote:
		text = fmt.Sprintf("The note is on case %d, which stays open.", c.ID)
	case res.Action == ActWithdraw:
		text = fmt.Sprintf("Case %d is withdrawn.", c.ID)
	default:
		text = fmt.Sprintf("Case %d is resolved with %s.", c.ID, res.sentence())
	}
	for _, a := range did {
		if a.Action == ActRetry || a.Action == ActReplan || a.Action == ActClose {
			text += " " + a.Result
		}
	}
	return text
}

// drawAlone writes d for card taskID when no case resolves with it: each
// part in its own transaction, the raise first.
func (r *Runner) drawAlone(ctx context.Context, taskID string, d CaseDraw) error {
	ceiling, extend, fund := d.rows()
	if d.Ceiling > 0 {
		if err := r.store.SetCeiling(ctx, taskID, d.Ceiling, ceiling); err != nil {
			return err
		}
	}
	if !d.Extend.IsZero() || d.Lines > 0 {
		if _, _, err := r.store.ExtendCard(ctx, taskID, d.Extend, d.Lines, extend); err != nil {
			return err
		}
	}
	if d.FundEUR > 0 {
		if _, _, err := r.store.FundCard(ctx, taskID, d.FundEUR, d.PolicyCeiling, fund); err != nil {
			return err
		}
	}
	return nil
}
