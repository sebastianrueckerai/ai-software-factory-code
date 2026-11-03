package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// ActivityOpenTrip is OpenTrip's activity.
const ActivityOpenTrip = "OpenTrip"

// TripInput is what the run knows when it stops: the reason as today's
// Escalate took it, the choices, and the head and stage when it has them.
// The facts are read on the head. The kind comes from the reason as phase
// 1's caseKindOf derives it, the state key from the kind: the head for a
// cap, a floor, a gate stop or a merge matter, the dependency's task id
// for a dependency kind, phase 1's text key for a plan kind and for a kind
// with no head.
type TripInput struct {
	TaskID  string
	Reason  string
	Choices []string
	Head    string
	Stage   string
}

// tripAttempt is one attempt of a trip's activity: its id, the run's and
// the activity's, and its number, 1 for the first.
type tripAttempt struct {
	ID string
	N  int32
}

// TripOutcome is what the run gets back: the case, the comment the human
// face posted (0 when rung 0 resolved it and nobody was asked), and the
// resolution when rung 0 resolved it in place. Rung is the rung that holds
// the case: 0 when it resolved it, 3 when the case waits on the human.
type TripOutcome struct {
	CaseID     int64
	Comment    int64
	Resolution *Resolution
	Rung       int
}

// resolver is one row of rung 0: it fires on a case's kind and facts and
// returns the resolution it took, or nil.
type resolver struct {
	Name string
	Fire func(ctx context.Context, r *Runner, c Case, f CaseFacts) (*Resolution, error)
}

// headKinds are the stops keyed on the card's head, whose way on rung 0
// may find in the facts: the caps, the floor, the check-in's share and a
// gate stop.
var headKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor, CaseCheckinShare, CaseGateStop}

// rung0 is rung 0's table, in the order its rows are tried. Act carries a
// row's resolution out on rung 0: it checks the action table again, runs
// what lives outside the run and records the step.
var rung0 = []resolver{
	// an open pull request whose head is green and approved goes to the
	// approval again: no session is asked for
	{Name: "route_approval", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		return rungZeroTakes(c, f, headKinds, Resolution{Action: ActRouteApproval}), nil
	}},
	// a red head's checks run again, once per head (RerunOnHead)
	{Name: "rerun_checks", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		return rungZeroTakes(c, f, headKinds, Resolution{Action: ActRerunChecks}), nil
	}},
	// a day's, a month's or a project's cap: the run's own wait (capWait)
	// ends it when the cap turns, and the case waits on the human until then
	{Name: "cap_wait", Fire: func(context.Context, *Runner, Case, CaseFacts) (*Resolution, error) { return nil, nil }},
	// a failed dependency outside a plan is filed again once, and a retry
	// that failed in turn goes to the human
	{Name: "retry_dependency", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		d := caseDep(c, f)
		if f.Plan != "" || d == nil || d.RetryOf != "" {
			return nil, nil
		}
		return rungZeroTakes(c, f, []CaseKind{CaseDependencyFailed}, Resolution{Action: ActRetry, Args: map[string]any{"task_id": d.TaskID}}), nil
	}},
	// inside a plan, the failed dependency's work goes to the plan's planner
	{Name: "impasse", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		d := caseDep(c, f)
		if f.Plan == "" || d == nil {
			return nil, nil
		}
		return rungZeroTakes(c, f, []CaseKind{CaseDependencyFailed}, Resolution{Action: ActReplan, Args: map[string]any{"task_id": d.TaskID}}), nil
	}},
}

// rungZeroTakes is res when case c is of kinds and the action table lets
// rung 0 take res on facts f, else nil.
func rungZeroTakes(c Case, f CaseFacts, kinds []CaseKind, res Resolution) *Resolution {
	a, ok := ActionByName(res.Action)
	if !ok || !slices.Contains(kinds, c.Kind) || a.refusal(f, 0, res) != "" {
		return nil
	}
	return &res
}

// caseDep is the dependency a case of a dependency kind is about, the one
// its state key names, and nil for any other case.
func caseDep(c Case, f CaseFacts) *DepFact {
	if c.Kind != CaseDependencyFailed && c.Kind != CaseDependencyClosed {
		return nil
	}
	i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return d.TaskID == c.StateKey })
	if i < 0 {
		return nil
	}
	return &f.Dependencies[i]
}

// OpenTrip is the one function every trip calls. It reads the card's
// facts and opens the stop's case. A live case with the stop's key at the
// human is the repeat guard: the stop came again, a step says so, and the
// card waits on that case's ask, with no new face. On a fresh case rung 0
// runs, and the first row that fires resolves the case through Act. With
// none, the human is asked (askHuman) and the case waits at the human. A
// restart's trip (healedReason) waits on the card's newest live ask. An
// attempt after the first takes what the attempts before left
// (retriedTrip).
func (r *Runner) OpenTrip(ctx context.Context, in TripInput, at tripAttempt) (TripOutcome, error) {
	t, _, err := r.task(ctx, in.TaskID)
	if err != nil {
		return TripOutcome{}, err
	}
	if in.Reason == healedReason {
		return r.healedTrip(ctx, t, in)
	}
	f, err := r.tripFacts(ctx, t, in.Head)
	if err != nil {
		return TripOutcome{}, err
	}
	f.Stage = cmp.Or(in.Stage, f.Stage)
	c, cases, err := r.tripCase(ctx, t, f, in, CaseOpen)
	if err != nil {
		return TripOutcome{}, err
	}
	if at.N > 1 {
		if out, ok, err := r.retriedTrip(ctx, t, c.Kind, cases, at.ID); ok || err != nil {
			return out, err
		}
	}
	if at.ID != "" {
		c.Facts["trip"] = at.ID
	}
	c, opened, err := r.store.OpenCase(ctx, c)
	if err != nil {
		return TripOutcome{}, err
	}
	if !opened && c.Status == CaseAtHuman {
		return r.waitsAtHuman(ctx, t, c, "the same stop again: "+c.Problem)
	}
	res, found, err := r.rungZero(ctx, t, c, f, cases)
	if err != nil || res != nil {
		return TripOutcome{CaseID: c.ID, Resolution: res}, err
	}
	if err := r.tripStep(ctx, c.ID, found); err != nil {
		return TripOutcome{}, err
	}
	comment, err := r.askHuman(ctx, t, c, in.Reason, in.Choices)
	if err != nil {
		return TripOutcome{}, err
	}
	if err := r.store.SetCaseStatus(ctx, c.ID, CaseAtHuman); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: comment, Rung: 3}, nil
}

// rungZero runs rung 0 on fresh case c of card t: no row when rung 0
// resolved a case of c's kind and key before, for the stop came back,
// else the first row of rung0 that fires, which Act carries out on rung
// 0. It answers the resolution taken, or nil and what rung 0 found: a row
// Act refused or failed hands the case up with why, so rung 0 never keeps
// the human from being asked.
func (r *Runner) rungZero(ctx context.Context, t Task, c Case, f CaseFacts, cases []Case) (*Resolution, string, error) {
	if i := slices.IndexFunc(cases, func(e Case) bool {
		return e.ID != c.ID && e.Kind == c.Kind && e.StateKey == c.StateKey && e.Status == CaseResolved && e.Resolution != nil && e.Resolution.Rung == 0
	}); i >= 0 {
		return nil, "rung 0 took this stop with " + cases[i].Resolution.Action + " before, and it came back", nil
	}
	for _, row := range rung0 {
		res, err := row.Fire(ctx, r, c, f)
		if err != nil {
			return nil, "", err
		}
		if res == nil {
			continue
		}
		_, err = r.Act(ctx, ActInput{TaskID: t.ID, CaseID: c.ID, By: FactoryDecider, Via: ViaFactoryd, Rung: 0, Res: *res})
		var refused ErrActRefused
		var noRetry ErrNoRetry
		switch {
		case ctx.Err() != nil:
			return nil, "", ctx.Err()
		case errors.As(err, &refused), errors.As(err, &noRetry):
			return nil, row.Name + " was refused: " + err.Error(), nil
		case err != nil:
			return nil, row.Name + " failed: " + err.Error(), nil
		}
		return res, "", nil
	}
	return nil, "no resolver fits", nil
}

// healedTrip is the trip of a run that restarted while its card waited on
// a human: the card's newest live ask, as askCase finds it, waits on with
// a step and no new face. With none, the question is asked again as
// Escalate asked it, and its words open its case.
func (r *Runner) healedTrip(ctx context.Context, t Task, in TripInput) (TripOutcome, error) {
	c, err := r.liveAsk(ctx, t.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	if c.ID != 0 {
		return r.waitsAtHuman(ctx, t, c, "asked again after a restart")
	}
	comment, err := r.askHuman(ctx, t, Case{}, in.Reason, in.Choices)
	if err != nil {
		return TripOutcome{}, err
	}
	if c, err = r.liveAsk(ctx, t.ID); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: comment, Rung: 3}, nil
}

// retriedTrip is what the attempts before trip's left on a case of kind,
// ok false for nothing to take: the case they opened, when rung 0
// resolved it, with its resolution, or when it waits at the human, on
// which the card waits with no new step and no new face. It goes by the
// trip and the kind, not the key: rung 0's retry of a dependency moves
// the key the trip reads.
func (r *Runner) retriedTrip(ctx context.Context, t Task, kind CaseKind, cases []Case, trip string) (TripOutcome, bool, error) {
	i := slices.IndexFunc(cases, func(c Case) bool { return c.Kind == kind && c.Facts["trip"] == trip })
	if i < 0 {
		return TripOutcome{}, false, nil
	}
	switch c := cases[i]; {
	case c.Status == CaseResolved && c.Resolution != nil && c.Resolution.Rung == 0:
		return TripOutcome{CaseID: c.ID, Resolution: &Resolution{Action: c.Resolution.Action, Args: c.Resolution.Args, Note: c.Resolution.Reason}}, true, nil
	case c.Status == CaseAtHuman:
		return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, true, r.waitOnHuman(ctx, t)
	}
	return TripOutcome{}, false, nil
}

// tripStep is rung 0's step on case id that found found and handed the
// case up, appended once: not again when the case's last step is rung
// 0's with the same words, as a retried trip would write it.
func (r *Runner) tripStep(ctx context.Context, id int64, found string) error {
	steps, err := r.store.CaseSteps(ctx, id)
	if err != nil {
		return err
	}
	if n := len(steps); n > 0 && steps[n-1].Rung == 0 && steps[n-1].Found == found {
		return nil
	}
	_, err = r.store.AppendCaseStep(ctx, CaseStep{CaseID: id, Rung: 0, Actor: FactoryDecider, Found: found, Outcome: StepHandedUp})
	return err
}

// waitsAtHuman is the trip that finds case c at the human already: a step
// on rung 0 that found found and handed up, the card waiting on the human,
// and the comment of the case's newest ask, which no new face follows.
func (r *Runner) waitsAtHuman(ctx context.Context, t Task, c Case, found string) (TripOutcome, error) {
	if err := r.tripStep(ctx, c.ID, found); err != nil {
		return TripOutcome{}, err
	}
	if err := r.waitOnHuman(ctx, t); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, nil
}

// liveAsk is card taskID's newest live case an answer on the card ends
// (newestAskCase), the zero Case for none.
func (r *Runner) liveAsk(ctx context.Context, taskID string) (Case, error) {
	open, err := r.store.OpenCasesOf(ctx, taskID)
	if err != nil {
		return Case{}, err
	}
	for _, c := range slices.Backward(open) {
		if !slices.Contains(ownAnswerKinds, c.Kind) {
			return c, nil
		}
	}
	return Case{}, nil
}

// tripCase is the case trip in makes on card t with facts f, as status,
// with the card's cases, for OpenCase to open. The kind is caseKindOf's
// of the reason. The problem is the shaped question's ask where a
// template names the decision, else the reason's first line. The facts
// are f with the reason, its source and what the reason names, the money
// the card's for a stop on its own budget, with what the next opening
// needs at the floor. The case follows the card's newest ended one.
func (r *Runner) tripCase(ctx context.Context, t Task, f CaseFacts, in TripInput, status string) (Case, []Case, error) {
	cases, err := r.store.CasesOf(ctx, t.ID)
	if err != nil {
		return Case{}, nil, err
	}
	source := escalationSource(in.Reason)
	kind, named := caseKindOf(source, in.Reason)
	if kind == CaseOther && t.Kind == "human" {
		kind = CaseHumanCard
	}
	problem := ShapedQuestion(in.Reason, r.questionFacts(ctx, t, in.Reason), in.Choices).Ask
	if problem == plainAsk {
		problem = escalationQuestion(in.Reason)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return Case{}, nil, err
	}
	var facts map[string]any
	if err := json.Unmarshal(b, &facts); err != nil {
		return Case{}, nil, err
	}
	maps.Copy(facts, named)
	facts["source"], facts["reason"] = source, textutil.Truncate(in.Reason, 300)
	c := Case{TaskID: t.ID, Repository: t.Repository, Kind: kind, StateKey: tripKey(kind, problem, f.Head, f),
		Status: status, Problem: problem, Facts: facts, Money: tripMoney(kind, f)}
	for _, e := range cases {
		if e.Status != CaseOpen && e.Status != CaseAtHuman {
			c.Follows = max(c.Follows, e.ID)
		}
	}
	return c, cases, nil
}

// tripKey is the state key of a trip's case of kind: head for a stop at
// the head, the dependency's task id for a dependency's, else the
// problem's words (caseStateKey), as for a kind with no head.
func tripKey(kind CaseKind, problem, head string, f CaseFacts) string {
	switch {
	case slices.Contains(headKinds, kind) && head != "":
		return head
	case kind == CaseDependencyFailed || kind == CaseDependencyClosed:
		if d := tripDep(kind, f); d != nil {
			return d.TaskID
		}
	}
	return caseStateKey(kind, problem, "")
}

// tripDep is the dependency a trip of a dependency kind stops on, as the
// Dependencies activity names it: the first failed one with no
// replacement, the first closed one.
func tripDep(kind CaseKind, f CaseFacts) *DepFact {
	i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool {
		if kind == CaseDependencyFailed {
			return d.Status == "failed" && d.Replaced == ""
		}
		return d.Status == "closed"
	})
	if i < 0 {
		return nil
	}
	return &f.Dependencies[i]
}

// tripMoney is the money fact of a case of kind on facts f, nil for none:
// a budget floor's names what the next opening needs past what is left, in
// cents as the store keeps them, and what that buys. The other stops on
// the card's own budget name the budget and what is spent.
func tripMoney(kind CaseKind, f CaseFacts) *CaseMoney {
	switch {
	case kind == CaseBudgetFloor:
		buys := "one session"
		if f.Model != "" {
			buys += " on " + f.Model
		}
		need := float64(max(0, Cents(f.FloorEUR)-Cents(f.LeftEUR))) / 100
		return &CaseMoney{Kind: "budget", BudgetEUR: f.BudgetEUR, SpentEUR: f.SpentEUR, NeedEUR: need, Buys: buys}
	case slices.Contains(cardMoneyKinds, kind):
		return &CaseMoney{Kind: string(kind), BudgetEUR: f.BudgetEUR, SpentEUR: f.SpentEUR}
	}
	return nil
}

// askHuman is a case's human face (architecture 6.6): state
// waiting-human, mirrored on the issue, a comment with the reason and a
// question_asked row with its question and case c (none for the zero
// Case); the agent's choices, if any, ride on that row, the timeline's step
// and the intake event the card reads. It returns the comment's id: only
// later comments answer it.
func (r *Runner) askHuman(ctx context.Context, t Task, c Case, reason string, choices []string) (int64, error) {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return 0, terminalError("%v", err)
	}
	// the reason in a code block: a mention the agent wrote rings no bell
	next := "Reply on this issue to start a new attempt with your comment as guidance, or close the issue to end the task."
	if floorRefused(reason) {
		// no answer can run: the card names why and the way out
		next = "An answer starts an attempt only once the budget can fund a call, as above; close this issue to end the task."
	}
	if capWaitDecision(reason) != "" {
		next = "The task resumes on its own as above. Reply on this issue to start a new attempt sooner with your comment as guidance, or close the issue to end the task."
	}
	if strings.HasPrefix(reason, leftQueue) {
		next = "Reply on this issue to put the pull request back at the end of the merge queue, request changes on it to send the task back to the agent, or close the issue to end the task."
	}
	safe := r.untrusted(t, reason)
	body := "factoryd: task " + t.ID + " needs a human.\n\n" + fenced(safe)
	if links := sanitize.Links(safe, r.linkHosts(t)); len(links) > 0 {
		// the fence renders no link: the ones the sanitiser keeps follow it
		body += "\n\nLinks:\n- " + strings.Join(links, "\n- ")
	}
	body += "\n\n" + next
	if strings.HasPrefix(reason, screeningAsks) {
		// the reasons go to the card only: they would teach the author
		body = screenedLine
	}
	var id int64
	if t.IssueNumber != 0 {
		// a task without one (a report task) has its question on the card alone
		if id, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body); err != nil {
			return 0, err
		}
	}
	// a clicked choice is posted as the member's comment: agent text,
	// sanitised like the reason
	choices = slices.Clone(choices)
	for i, c := range choices {
		choices[i] = r.untrusted(t, c)
	}
	question := escalationQuestion(reason)
	var shaped *HumanQuestion
	if reason != healedReason && !strings.HasPrefix(reason, screeningAsks) {
		// the deterministic stops shape their five-part question from the
		// stop's own numbers; every other ask gets the same five parts
		// from its reason, so no card asks with words alone
		shaped = ShapedQuestion(reason, r.questionFacts(ctx, t, reason), choices)
		question = shaped.Ask
		// the refusal the newest session ended on rides with the ask,
		// verbatim: the blocker reaches the human with no agent-side
		// reporting to lose it (card #1345)
		if tool, refusal, ok := r.lastToolRefusal(ctx, t.ID); ok {
			shaped.Info += "\n\nThe session's last tool call (" + tool + ") was refused:\n\n" + fenced(refusal)
		}
	}
	if reason == healedReason {
		// the restart's sentence asks nothing: the row names the question it asks again
		events, err := r.store.IntakeEvents(ctx, t.ID)
		if err != nil {
			log.Printf("task %s: the question a healed run asks again: %v", t.ID, err)
		}
		_, again, _ := answersFrom(events)
		question = cmp.Or(again, question)
	}
	// the durable anchor Reconcile rebuilds the answers from; a retry
	// with the same comment records nothing twice. It is written before
	// the wait: a card in waiting-human without its row has no ask
	if _, err := r.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeEscalation, Author: "factoryd", Body: reason, CommentID: id,
		Choices: choices, Question: shaped}); err != nil {
		return 0, fmt.Errorf("task %s: record escalation comment %d: %w", t.ID, id, err)
	}
	if err := r.waitOnHuman(ctx, t); err != nil {
		return 0, err
	}
	// the shaped question's buttons are not these choices: they carry
	// values, and park and keep waiting record an answer without posting
	// one, so the console renders them from the question itself
	step := map[string]any{"reason": reason, "comment": id}
	asked := map[string]any{"question": textutil.Truncate(question, 300), "comment": id, "source": escalationSource(reason),
		"reason": textutil.Truncate(reason, 300)}
	if c.ID != 0 {
		asked["case_id"] = c.ID
	}
	if shaped != nil {
		asked["ask"], asked["why"], asked["info"] = shaped.Ask, shaped.Why, shaped.Info
		if shaped.Recommendation != "" {
			asked["recommendation"] = shaped.Recommendation
		}
		asked["choices"] = shaped.Labels()
	}
	if len(choices) > 0 {
		step["choices"] = choices
		if shaped == nil {
			asked["choices"] = choices
		}
	}
	r.record(ctx, t.ID, "escalated", step)
	r.timeline.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd, asked)
	if reason == healedReason || strings.HasPrefix(reason, screeningAsks) {
		return id, nil // the question asked again, or the screening's: its alert is open already
	}
	if _, err := r.store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: escalationSource(reason), TaskID: t.ID,
		Detail: map[string]any{"comment": id, "reason": textutil.Truncate(reason, 2000)}}); err != nil {
		log.Printf("task %s: record the escalation alert: %v", t.ID, err)
	}
	return id, nil
}

// waitOnHuman moves card t to waiting-human, mirrored on its issue, unless
// it waits there already.
func (r *Runner) waitOnHuman(ctx context.Context, t Task) error {
	if t.Status == "waiting-human" {
		return nil
	}
	// queued: the screening stage's suspect verdict, before any session;
	// waiting-deps: a dependency ended failed or closed; waiting-poster:
	// the author's answer screened suspect, or a cap reached on it;
	// waiting-image: the image did not come within imageWait
	if err := r.store.TransitionTask(ctx, t.ID, []string{"queued", "waiting-deps", "running", "in-review", "waiting-poster", "waiting-image"}, "waiting-human"); err != nil {
		return terminalError("task %s is %s, cannot wait for a human: %v", t.ID, t.Status, err)
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	// a parked card holds no slot: the next queued card takes it
	pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	return nil
}

// openTrip is OpenTrip's activity, on the activity's attempt.
func (r *Runner) openTrip(ctx context.Context, in TripInput) (TripOutcome, error) {
	info := activity.GetInfo(ctx)
	return r.OpenTrip(ctx, in, tripAttempt{ID: info.WorkflowExecution.RunID + "/" + info.ActivityID, N: info.Attempt})
}
