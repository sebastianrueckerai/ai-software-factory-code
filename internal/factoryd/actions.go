package factoryd

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The actions a case is answered with: what every rung picks from, and
// what the human's buttons and the chat's tools do.
const (
	ActResume        = "resume"
	ActContinue      = "continue"
	ActHandover      = "handover" // args: model = same | stronger | cheaper
	ActReverify      = "reverify"
	ActRouteApproval = "route_approval"
	ActRerunChecks   = "rerun_checks"
	ActWaitChecks    = "wait_checks"   // runs a stopped checks wait again: no session, no agent time
	ActExtend        = "extend"        // args: sessions, agent_time_s, lines
	ActFund          = "fund"          // args: eur
	ActRaiseCeiling  = "raise_ceiling" // args: eur
	ActSuccessor     = "successor"
	ActRetry         = "retry"  // args: task_id of the failed dependency
	ActReplan        = "replan" // args: task_id of a failed dependency, whose work its plan's planner takes (impasse)
	ActClose         = "close"
	ActDeployNow     = "deploy_now" // releases a held production: the roll-held case's own
	ActSalvage       = "salvage"    // ends a run that neither moves nor answers and files its successor: the human's alone
	ActWithdraw      = "withdraw"
	ActNote          = "note"   // records a step and resolves nothing: a park, or keep waiting
	ActSettle        = "settle" // ends a monitor flag's case alone; the card goes on

	// the board's actions: the console's code does their work (ActInput.Work)
	ActApproveMerge     = "approve_merge"
	ActDenyMerge        = "deny_merge"        // note: the reason; args: action = request_changes | reject
	ActSetDependency    = "set_dependency"    // args: depends_on, task_id (the card, else the case's)
	ActRemoveDependency = "remove_dependency" // args: as set_dependency
	ActFileCard         = "file_card"         // args: title, body, size
	ActRestartPlan      = "restart_plan"      // note: the shape the new plan takes
	ActAnswerCriteria   = "answer_criteria"   // args: criterion, verdict = met | not_met | waive; note: the reason
	ActGrantCriteria    = "grant_criteria"    // one more bounded card for the open criteria of a criteria request
	ActConfirmCriteria  = "confirm_criteria"  // the delivered cards meet the criteria: every open one met, the note the evidence
	ActDenyCriteria     = "deny_criteria"     // note: the reason; waive a criterion instead if it should not hold
)

// Resolution is one answer on a case: the action with its arguments, an
// optional draw that rides beside it, and a note.
type Resolution struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
	Fund   *policy.Draw   `json:"fund,omitempty"`
	Note   string         `json:"note,omitempty"`
	// Verdicts are a rung's verdicts on the findings it judged, keyed by
	// the finding's text.
	Verdicts map[string]CheckVerdict `json:"verdicts,omitempty"`
}

// Action is one row of the action table.
type Action struct {
	Name  string
	Rungs [4]bool // which rungs may pick it; index 3 is the human
	// Applies is "" when the action applies to the facts, else why not, in
	// a sentence. The bounds of the draw an answer makes are drawWithin's,
	// by rung.
	Applies func(f CaseFacts, r Resolution) string
	InRun   bool // the run carries it out on case_resolved; false: Act runs it
}

// extendKinds are the cases an extend answers: the caps, and the stops
// whose questions offer extending the card as an owner exit.
var extendKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseCheckinShare, CaseGateStop, CaseFindingsRepeated}

// handoverModels are the rungs a handover names, against the session's.
var handoverModels = []string{"same", "stronger", "cheaper"}

// Actions is the action table, in the order Offered lists it.
var Actions = []Action{
	{Name: ActResume, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string { return sessionRoom(f.withDraw(r)) }},
	{Name: ActContinue, Rungs: rungs(1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case strings.TrimSpace(r.Note) == "":
			return "the answer carries no note for the session"
		case f.Running:
			return ""
		}
		return sessionRoom(f.withDraw(r))
	}},
	{Name: ActHandover, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		if why := sessionRoom(f.withDraw(r)); why != "" {
			return why
		}
		if model, _ := r.Args["model"].(string); !slices.Contains(handoverModels, model) {
			return fmt.Sprintf("a handover names its model as same, stronger or cheaper, not %q", model)
		}
		return ""
	}},
	{Name: ActRouteApproval, Rungs: rungs(0, 1, 2), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Kind == CaseGateStop && f.Gate == ActivityWaitChecks:
			return "a checks wait's stop goes on with wait_checks, not route_approval"
		case !f.PROpen:
			return "no pull request is open"
		case f.Checks != "green":
			return fmt.Sprintf("the checks on the head are %s, not green", cmp.Or(f.Checks, "unread"))
		case !f.ReviewApproved && f.Kind != CasePolicyClass:
			return "the newest review of the head did not approve it"
		}
		return ""
	}},
	{Name: ActReverify, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case !f.PROpen:
			return "no pull request is open"
		case f.Head == "":
			return "no head reached the gates"
		case f.Running:
			return "a session is running"
		}
		return ""
	}},
	{Name: ActRerunChecks, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case !f.PROpen:
			return "no pull request is open"
		case f.Checks != "red":
			return fmt.Sprintf("the checks on the head are %s, not red", cmp.Or(f.Checks, "unread"))
		case f.RerunOnHead:
			return "the checks on this head were run again once already"
		}
		return ""
	}},
	{Name: ActWaitChecks, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Kind != CaseGateStop || f.Gate != ActivityWaitChecks:
			return "wait_checks runs a stopped checks wait again, and this case is no checks wait's stop"
		case !f.PROpen:
			return "no pull request is open"
		}
		return ""
	}},
	{Name: ActExtend, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		d := extendDraw(r.Args)
		lines, _ := toInt(r.Args["lines"])
		shares, _ := toInt(r.Args["checkin_shares"])
		switch {
		case d.Sessions < 0 || d.AgentTime < 0 || lines < 0 || shares < 0:
			return "an extend takes no part below 0"
		case shares > 1:
			return "an extend grants one more check-in share at a time"
		case f.Final != "":
			return "this card ended " + f.Final + ": an extend buys nothing, file a new card"
		case f.Kind == CaseBudgetFloor:
			return "this card waits on money, not a session: fund it"
		case f.Kind == CaseMonthCap:
			return "this card waits on the month cap, not a session: raise budget.month_eur or wait for the month to turn, then fund or retry"
		case f.Kind != "" && !slices.Contains(extendKinds, f.Kind):
			return "this card asked a question, not for a session: answer it, or extend at its cap"
		case d.Sessions == 0 && d.AgentTime == 0 && lines == 0 && shares == 0:
			return "an extend needs sessions, agent time, lines or check-in shares above 0"
		case d.AgentTime > MaxExtendMinutes*time.Minute:
			return fmt.Sprintf("an extend raises at most %d minutes of agent time at once", MaxExtendMinutes)
		case f.Kind == CaseAgentTimeCap && d.AgentTime == 0:
			return "this card stopped for agent time: an extend of its agent time starts the next session; sessions, lines or check-in shares alone buy none"
		case !d.IsZero() && boughtNothing(f):
			return boughtNothingText(f)
		}
		return ""
	}},
	{Name: ActFund, Rungs: rungs(1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		switch eur, _ := r.Args["eur"].(float64); {
		case f.Final != "":
			return "this card ended " + f.Final + ": a fund buys nothing, file a new card"
		case !(eur > 0) || math.IsInf(eur, 1):
			return "a fund needs an amount above 0 EUR"
		case boughtNothing(f):
			return boughtNothingText(f)
		}
		return ""
	}},
	{Name: ActRaiseCeiling, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		if f.Final != "" {
			return "this card ended " + f.Final + ": a raise of its ceiling buys nothing, file a new card"
		}
		eur, _ := r.Args["eur"].(float64)
		return raiseRefusal(eur)
	}},
	{Name: ActSuccessor, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		if !f.PROpen && !f.Pushed && f.Head == "" {
			return "no pull request is open and no session pushed a branch"
		}
		return ""
	}},
	{Name: ActRetry, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, r Resolution) string {
		id, _ := r.Args["task_id"].(string)
		if id == "" {
			if slices.ContainsFunc(f.Dependencies, func(d DepFact) bool { return retryRefusal(d) == "" }) {
				return ""
			}
			return "no failed dependency is open to a retry"
		}
		if _, ok := retriedAs(f, id); ok {
			return ""
		}
		i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return d.TaskID == id })
		if i < 0 {
			return "the card does not depend on " + id
		}
		return retryRefusal(f.Dependencies[i])
	}},
	{Name: ActReplan, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, r Resolution) string {
		if id, _ := r.Args["task_id"].(string); id != "" {
			i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return d.TaskID == id })
			switch {
			case f.Plan == "":
				return "the card is in no plan, so no planner takes the work of " + id + ": retry it instead"
			case i < 0:
				return "the card does not depend on " + id + ": name a failed dependency's task id, or replan the card itself with no task id"
			case f.Dependencies[i].Status != "failed":
				return fmt.Sprintf("dependency #%d is %s, not failed, so the planner has none of its work to take: replan the card itself with no task id",
					f.Dependencies[i].Issue, f.Dependencies[i].Status)
			}
			return ""
		}
		if f.Plan == "" && f.Running {
			return "a session is running on this direct card"
		}
		return ""
	}},
	{Name: ActClose, Rungs: rungs(2, 3), Applies: applyAlways},
	{Name: ActApproveMerge, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Kind != CaseMergeApproval:
			return "approve_merge decides a merge request, and this case is no merge approval"
		case !f.PROpen:
			return "the pull request is not open, so there is nothing to approve"
		case f.Checks != "green":
			return fmt.Sprintf("the checks on %s are %s, not green: approve_merge waits for green checks on the head", f.Head[:min(7, len(f.Head))], cmp.Or(f.Checks, "unread"))
		}
		return ""
	}},
	{Name: ActDenyMerge, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch a, _ := r.Args["action"].(string); {
		case f.Kind != CaseMergeApproval:
			return "deny_merge decides a merge request, and this case is no merge approval"
		case !f.PROpen:
			return "the pull request is not open, so there is nothing to deny"
		case strings.TrimSpace(r.Note) == "":
			return "a denial carries its reason: the note"
		case a != "" && a != "request_changes" && a != "reject":
			return "deny_merge takes the action request_changes or reject"
		}
		return ""
	}},
	{Name: ActSetDependency, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case f.Kind != "" && !slices.Contains(dependencyKinds, f.Kind), f.Final != "" || f.Running || f.Attempts > 0:
			return "set_dependency makes a card that has not started wait for another"
		case strings.TrimSpace(argString(r.Args, "depends_on")) == "":
			return "set_dependency names the card to wait for: depends_on"
		}
		return ""
	}},
	{Name: ActRemoveDependency, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case f.Kind != "" && !slices.Contains(dependencyKinds, f.Kind), f.Final != "" || f.Running || f.Attempts > 0:
			return "remove_dependency frees a card that has not started"
		case len(f.Dependencies) == 0 && argString(r.Args, "task_id") == "":
			return "the card waits on no dependency, so there is no edge to remove"
		case strings.TrimSpace(argString(r.Args, "depends_on")) == "":
			return "remove_dependency names the dependency to remove: depends_on"
		}
		return ""
	}},
	{Name: ActFileCard, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case !slices.Contains(fileCardKinds, f.Kind):
			return "file_card files the work of a root cause outside the card, and this case names none"
		case strings.TrimSpace(argString(r.Args, "title")) == "":
			return "file_card needs a title"
		}
		return ""
	}},
	{Name: ActRestartPlan, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case f.Plan == "":
			return "restart_plan ends a plan and files it anew, and this card is neither a plan nor a plan's card"
		case !ClosablePlan(Task{Track: policy.TrackPlan, Status: f.PlanStatus}):
			return "the plan is not at rest, so it cannot be closed to restart it: wait for its cards to end, or replan it"
		case strings.TrimSpace(r.Note) == "":
			return "restart_plan carries the shape the new plan takes: the note"
		}
		return ""
	}},
	{Name: ActAnswerCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch verdict := argString(r.Args, "verdict"); {
		case f.Kind != CasePlanCriteria && !(f.Kind == CaseMergeApproval && f.Criteria):
			return "answer_criteria judges a plan's criteria, and this case holds none"
		case verdict != "met" && verdict != "not_met" && verdict != "waive":
			return "answer_criteria takes the verdict met, not_met or waive"
		case argString(r.Args, "criterion") == "":
			return "answer_criteria names the criterion"
		case verdict != "met" && strings.TrimSpace(r.Note) == "":
			return "not_met and waive carry their reason: the note"
		}
		return ""
	}},
	{Name: ActGrantCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		if f.ApprovalKind == ApprovalCriteriaRerun {
			return "this plan's ask is criteria never judged: grant_criteria grants no card and runs no review; waive a criterion with your reason, or confirm the delivered cards"
		}
		return criteriaRequestRefusal(f, ActGrantCriteria)
	}},
	{Name: ActConfirmCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		return criteriaRequestRefusal(f, ActConfirmCriteria)
	}},
	{Name: ActDenyCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		if why := criteriaRequestRefusal(f, ActDenyCriteria); why != "" {
			return why
		}
		if strings.TrimSpace(r.Note) == "" {
			return "a denial carries its reason: the note"
		}
		return ""
	}},
	{Name: ActDeployNow, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		if f.Kind != CaseRollHeld {
			return "deploy_now releases a held production, and this case is no roll hold's"
		}
		return ""
	}},
	// settle is a monitor flag's own: it ends the flag's case and leaves
	// the card in its run, where close would end the card itself
	{Name: ActSettle, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		if f.Kind != CaseMonitorFlag {
			return "settle ends a monitor flag's case alone, and this case is no monitor flag's"
		}
		return ""
	}},
	{Name: ActSalvage, Rungs: rungs(3), Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Final != "":
			return "this card ended " + f.Final + " and no run of it is left to end"
		case f.Kind == CaseMonitorFlag && f.Flag == "wait_over_bound" && f.Wait == "run_quiet", f.RunGone:
			return ""
		}
		return "salvage ends a run that neither moves nor answers: the monitor's run_quiet flag, or a live card whose workflow is gone, opens it"
	}},
	// withdraw is the factory's own: Act signals no run for it, so neither
	// the chat's agent nor the human takes it
	{Name: ActWithdraw, Rungs: rungs(0, 1), Applies: applyAlways},
	{Name: ActNote, Rungs: rungs(2, 3), Applies: func(_ CaseFacts, r Resolution) string {
		if strings.TrimSpace(r.Note) == "" {
			return "the answer carries no note"
		}
		return ""
	}},
}

// monitorActions are the actions a monitor flag's case takes: those Act carries
// out itself, since the card's run waits on no such case and drops its
// resolution.
var monitorActions = []string{ActNote, ActSettle, ActSalvage, ActRetry, ActReplan, ActFund, ActRaiseCeiling, ActWithdraw}

// rungs marks the rungs of in a row's Rungs.
func rungs(of ...int) [4]bool {
	var out [4]bool
	for _, n := range of {
		out[n] = true
	}
	return out
}

func applyAlways(CaseFacts, Resolution) string { return "" }

// ActionByName is the row of the action table named name.
func ActionByName(name string) (Action, bool) {
	i := slices.IndexFunc(Actions, func(a Action) bool { return a.Name == name })
	if i < 0 {
		return Action{}, false
	}
	return Actions[i], true
}

// probes are the smallest answers Offered tries the actions that need
// arguments or a note with: one session, one cent, the same model, one
// note. At an agent time cap the extend's probe buys agent time, the
// part its stop names.
var probes = map[string]Resolution{
	ActHandover: {Args: map[string]any{"model": "same"}}, ActExtend: {Args: map[string]any{"sessions": 1}},
	ActFund: {Args: map[string]any{"eur": 0.01}}, ActRaiseCeiling: {Args: map[string]any{"eur": 0.01}},
	ActContinue: {Note: "probe"}, ActNote: {Note: "probe"},
	ActDenyMerge:        {Note: "probe"},
	ActSetDependency:    {Args: map[string]any{"depends_on": "probe"}},
	ActRemoveDependency: {Args: map[string]any{"depends_on": "probe"}},
	ActFileCard:         {Args: map[string]any{"title": "probe"}},
	ActRestartPlan:      {Note: "probe"},
	ActAnswerCriteria:   {Args: map[string]any{"criterion": "probe", "verdict": "met"}},
}

// criteriaRequestRefusal is why action decides no criteria request on f: only
// a plan_criteria case holds one.
func criteriaRequestRefusal(f CaseFacts, action string) string {
	if f.Kind != CasePlanCriteria {
		return action + " decides a plan's criteria request, and this case is none"
	}
	return ""
}

// fileCardKinds are the cases whose cause may lie outside the card: the
// work is filed as a card of its own.
var fileCardKinds = []CaseKind{CaseDependencyFailed, CaseDependencyClosed, CaseFindingsRepeated, CaseAgentEscalate, CaseAgentRequestInfo}

// dependencyKinds are the cases a dependency edge answers; an edge is also
// set on a card that waits on no case.
var dependencyKinds = []CaseKind{CaseDependencyFailed, CaseDependencyClosed}

// argString is args' key as a string, "" for none.
func argString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// boardActions are the actions whose work the console's code does
// (ActInput.Work), outside the run.
var boardActions = []string{ActApproveMerge, ActDenyMerge, ActSetDependency, ActRemoveDependency, ActFileCard, ActRestartPlan, ActAnswerCriteria,
	ActGrantCriteria, ActConfirmCriteria, ActDenyCriteria}

// IsBoardAction reports whether name is one of the actions the console's
// code does (ActInput.Work).
func IsBoardAction(name string) bool { return slices.Contains(boardActions, name) }

// keptCaseActions are the board actions that leave their case at the human:
// a verdict on one criterion and a filed card settle none of it.
var keptCaseActions = []string{ActAnswerCriteria, ActFileCard}

// requestEnds are the actions whose request's decision ends the case, by
// the decision's status: a merge's approval and denial, a criteria request's
// denial. A grant decides its request too, but its work leaves the case to Act.
var requestEnds = []string{ActApproveMerge, ActDenyMerge, ActDenyCriteria}

// mergeDecisions are the actions that decide an approval request alone.
var mergeDecisions = append(slices.Clone(requestEnds), ActGrantCriteria)

// probe is action a's smallest answer on facts f: its probe, an extend of
// one minute of agent time at an agent time cap, where a raise of the
// sessions alone is refused with the stop's own words.
func probe(a Action, f CaseFacts) Resolution {
	p := probes[a.Name]
	if a.Name == ActExtend && f.Kind == CaseAgentTimeCap {
		p.Args = map[string]any{"agent_time_s": 60}
	}
	return p
}

// fittingActions is the action a stop's own kind asks for, which leads
// every listing of the actions on a case of that kind.
var fittingActions = map[CaseKind]string{CaseAgentTimeCap: ActExtend, CaseSessionCap: ActExtend, CaseBudgetFloor: ActFund}

// orderedActions is the action table in order with the action the case's
// kind asks for first.
func orderedActions(f CaseFacts) []Action {
	fit, ok := fittingActions[f.Kind]
	if !ok {
		return Actions
	}
	i := slices.IndexFunc(Actions, func(a Action) bool { return a.Name == fit })
	if i < 0 {
		return Actions
	}
	return slices.Concat(Actions[i:i+1], Actions[:i], Actions[i+1:])
}

// Offered lists the actions rung may take on facts f, in table order: its
// rows whose Applies holds and whose draw fits rung's bounds, tried with
// the smallest answers (probes) where an action needs arguments or a note.
func Offered(f CaseFacts, rung int) []Action {
	var out []Action
	for _, a := range orderedActions(f) {
		if a.offerRefusal(f, rung) == "" {
			out = append(out, a)
		}
	}
	return out
}

// offerRefusal is why a is not on offer to rung on f, "" when it is: the
// action tried with the smallest answer (its probe). A handover at the cap is
// on offer to rung 1 alone, the one rung whose answer carries a session draw,
// while the card's draw bound holds a session (handoverNeedsDraw).
func (a Action) offerRefusal(f CaseFacts, rung int) string {
	p := probe(a, f)
	res := Resolution{Action: a.Name, Args: p.Args, Note: p.Note}
	why := a.refusal(f, rung, res)
	if why != "" && a.handoverNeedsDraw(f, rung) {
		return ""
	}
	return why
}

// handoverNeedsDraw says a is the handover, refused at the cap for want of a
// session, and rung 1's answer may draw the one session that makes room.
func (a Action) handoverNeedsDraw(f CaseFacts, rung int) bool {
	if rung != 1 || a.Name != ActHandover {
		return false
	}
	p := probe(a, f)
	res := Resolution{Action: a.Name, Args: p.Args, Note: p.Note}
	if a.refusal(f, rung, res) == "" {
		return false
	}
	res.Fund = &policy.Draw{Sessions: 1}
	return a.refusal(f, rung, res) == ""
}

// refusal is why rung may not answer f with r by a, "" when it may: a's
// rungs, its Applies, and the draw r makes. Money is drawn only on the
// rungs that may fund. A draw that buys sessions, agent time or budget is
// refused while the reads it is weighed on failed, and after a draw that
// bought nothing. drawWithin bounds it by rung: every part against the
// pool and the card's bound, the money for the budget against the
// ceiling, which a raise beside it lifts first.
func (a Action) refusal(f CaseFacts, rung int, r Resolution) string {
	if rung < 0 || rung > 3 {
		return fmt.Sprintf("there is no rung %d", rung)
	}
	// rung 0 closes what an ended card left open, and no live card
	ended := rung == 0 && a.Name == ActClose && f.Final != ""
	if !a.Rungs[rung] && !ended {
		return fmt.Sprintf("%s may not pick %s: %s may", who(rung), a.Name, rungsText(a.Rungs))
	}
	if rung == 1 && a.Name == ActWithdraw && f.Kind != CaseMonitorFlag {
		return "withdraw ends a monitor flag's case alone: the card's run waits on this case, so rung 1 answers it or hands it up"
	}
	if f.Kind == CaseMonitorFlag {
		switch {
		case a.Name == ActClose && f.Final == "":
			return "close ends the card, not the flag's case: settle it with settle, which ends the case and the card goes on"
		case a.Name != ActClose && !slices.Contains(monitorActions, a.Name):
			return a.Name + " would do nothing on a monitor flag's case: the card's run waits on none"
		}
	}
	if why := a.Applies(f, r); why != "" {
		return why
	}
	if rung == 2 && a.Name == ActApproveMerge && len(f.UnreadGates) > 0 {
		return (&UnreviewedMergeError{Stages: f.UnreadGates}).Error()
	}
	if slices.Contains(mergeDecisions, a.Name) && (r.Fund != nil || !r.drawIsZero()) {
		return a.Name + " decides the merge request alone: it draws nothing"
	}
	if a.Name == ActWaitChecks && rung < 3 && f.WaitedOnHead {
		return "the checks wait on this head was run again once already"
	}
	d, budget := r.draw()
	if d.IsZero() {
		return ""
	}
	fund, _ := ActionByName(ActFund)
	unread := slices.IndexFunc(f.Partial, func(p string) bool { return p == "events" || p == "checks" })
	buys := d.Sessions > 0 || d.AgentTime > 0 || budget > 0
	switch {
	case badDraw(d) || r.Fund != nil && badDraw(*r.Fund):
		return "the draw has a part below 0 or an endless one"
	case d.EUR > 0 && !fund.Rungs[rung]:
		return fmt.Sprintf("%s may not draw money: %s may", who(rung), rungsText(fund.Rungs))
	case buys && unread >= 0:
		return fmt.Sprintf("the card's %s could not be read, so a draw cannot be weighed against the last one: try again in a minute, or close the card", f.Partial[unread])
	case buys && boughtNothing(f):
		return boughtNothingText(f)
	}
	if why := drawWithin(f, rung, d, false); why != "" || budget == 0 {
		return why
	}
	if r.Action == ActRaiseCeiling {
		raise, _ := r.Args["eur"].(float64)
		f.CeilingEUR += raise
	}
	return drawWithin(f, rung, policy.Draw{EUR: budget}, true)
}

// draw is everything r draws from the pool: an extend's sessions and
// agent time, a fund's or a ceiling raise's money, and the draw beside
// the action; and of its money the part that goes to the card's budget,
// which the ceiling bounds.
func (r Resolution) draw() (policy.Draw, float64) {
	var d policy.Draw
	var budget float64
	switch r.Action {
	case ActExtend:
		d = extendDraw(r.Args)
	case ActFund:
		budget, _ = r.Args["eur"].(float64)
		d.EUR = budget
	case ActRaiseCeiling:
		d.EUR, _ = r.Args["eur"].(float64)
	}
	if r.Fund != nil {
		d, budget = d.Plus(*r.Fund), budget+r.Fund.EUR
	}
	return d, budget
}

// drawIsZero reports a resolution that draws nothing from the pool.
func (r Resolution) drawIsZero() bool {
	d, budget := r.draw()
	return d.IsZero() && budget == 0
}

// bounds are the lines and check-in shares an extend r raises its card's
// bounds by, 0 for any other action.
func (r Resolution) bounds() (lines, shares int) {
	if r.Action != ActExtend {
		return 0, 0
	}
	lines, _ = toInt(r.Args["lines"])
	shares, _ = toInt(r.Args["checkin_shares"])
	return lines, shares
}

// badDraw reports a draw with a part below 0 or an endless one.
func badDraw(d policy.Draw) bool {
	return d.Sessions < 0 || d.AgentTime < 0 || !(d.EUR >= 0) || math.IsInf(d.EUR, 1)
}

// extendDraw is the draw of an extend's args: its sessions and its agent
// time in seconds. Its lines ride free.
func extendDraw(args map[string]any) policy.Draw {
	sessions, _ := toInt(args["sessions"])
	seconds, _ := toInt(args["agent_time_s"])
	return policy.Draw{Sessions: sessions, AgentTime: time.Duration(min(seconds, math.MaxInt32)) * time.Second}
}

// raiseRefusal is why a raise of the ceiling by eur is refused, "" when it
// is a whole cent or more: the store keeps cents.
func raiseRefusal(eur float64) string {
	if math.IsNaN(eur) || math.IsInf(eur, 0) || Cents(eur) <= 0 {
		return fmt.Sprintf("a raise of the ceiling by %v EUR is no amount above zero: raise it by at least 0.01 EUR", eur)
	}
	return ""
}

// withDraw is f with what r draws beside its action counted: the sessions
// and agent time raise the caps, the money the room left to spend, so a
// decision that rides a draw is weighed as one record.
func (f CaseFacts) withDraw(r Resolution) CaseFacts {
	d, _ := r.draw()
	f.SessionCap += d.Sessions
	f.AgentTimeCapS += d.AgentTime.Seconds()
	f.LeftEUR += d.EUR
	return f
}

// sessionRoom is why f leaves no room to open a session, "" when it has
// it: none running, an attempt under the cap, the agent time a session
// needs (SessionLeastS), and the money an opening needs, with the action
// that makes the room.
func sessionRoom(f CaseFacts) string {
	limit, left, need := f.agentTime()
	switch {
	case f.Running:
		return "a session is running"
	case !f.Planner && capWaitDecision(f.CapWait) != "":
		return "a cap binds before a session can start: " + f.CapWait
	case f.Attempts >= f.SessionCap:
		return fmt.Sprintf("the session cap of %d is reached: extend or close", f.SessionCap)
	case slices.Contains(f.Partial, "config"):
		return "the agent time cap could not be read: try again in a minute, or close the card"
	case left <= 0:
		return fmt.Sprintf("the agent time cap of %s is reached: extend", minutesWord(limit))
	case left < need:
		return fmt.Sprintf("the agent time cap of %s leaves %s and a session needs %s: extend", minutesWord(limit), minutesWord(left), minutesWord(need))
	case slices.Contains(f.Partial, "floor"):
		return "what an opening needs could not be read: try again in a minute, or close the card"
	case f.unfunded():
		return fmt.Sprintf("%.2f EUR is left for a session and an opening needs %.2f EUR: fund", f.SessionLeft(), f.FloorEUR)
	}
	return ""
}

// retriedAs names the card that replaced failed dependency id of f's card
// when a retry filed one: the dependency the retry moved the card onto, or
// the replacement the failed dependency names.
func retriedAs(f CaseFacts, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	for _, d := range f.Dependencies {
		switch {
		case d.TaskID == RetryID(id) || d.RetryOf == id:
			if d.Issue != 0 {
				return fmt.Sprintf("#%d", d.Issue), true
			}
			return d.TaskID, true
		case d.TaskID == id && d.Replaced != "":
			return d.Replaced, true
		}
	}
	return "", false
}

// retryRefusal is why a retry does not take dependency d, "" when it does.
func retryRefusal(d DepFact) string {
	switch {
	case d.Status != "failed":
		return fmt.Sprintf("dependency #%d is %s, not failed", d.Issue, d.Status)
	case d.Replaced != "":
		return fmt.Sprintf("dependency #%d was retried as %s already", d.Issue, d.Replaced)
	case !retryableEnd(d.Why):
		return fmt.Sprintf("dependency #%d ended for good: %s", d.Issue, firstLineCut(d.Why))
	}
	return ""
}

// boughtNothing reports the design's refusal of a draw: a draw happened, no push
// and no filed revision since, the head, the checks and the findings
// unchanged since it. A draw whose row recorded no findings, or one on
// facts whose timeline could not be read, is weighed on the head and the
// checks. At an agent time stop the draw weighed is the newest that
// raised agent time: one that raised nothing the stop names is no draw.
func boughtNothing(f CaseFacts) bool {
	d, ok := weighedDraw(f)
	return ok && !d.Pushed && !d.Revised && d.Head == f.Head && d.Checks == f.Checks &&
		(d.Findings == nil || slices.Contains(f.Partial, "timeline") || slices.Equal(d.Findings, f.Findings))
}

// weighedDraw is the draw the guard weighs on f.
func weighedDraw(f CaseFacts) (DrawFacts, bool) {
	if f.Kind == CaseAgentTimeCap {
		if f.TimeDraw == nil {
			return DrawFacts{}, false
		}
		return *f.TimeDraw, true
	}
	if f.LastDrawAt == nil {
		return DrawFacts{}, false
	}
	return DrawFacts{At: *f.LastDrawAt, Head: f.DrawHead, Checks: f.DrawChecks, Findings: f.DrawFindings, Pushed: f.PushedSinceDraw, Revised: f.RevisedSinceDraw}, true
}

// boughtNothingText is boughtNothing's refusal, with the actions that
// draw nothing.
func boughtNothingText(f CaseFacts) string {
	d, _ := weighedDraw(f)
	what, since := "draw", "push"
	if f.Kind == CaseAgentTimeCap {
		what = "agent-time draw"
	}
	if f.Planner {
		since = "plan revision"
	}
	return "the last " + what + " at " + d.At.UTC().Format("15:04") + " UTC bought nothing, with no " + since + " since and the head, " +
		"the checks and the findings unchanged: hand over, start a successor, replan or close"
}

// drawWithin reports why draw d does not fit rung's bounds on f: the pool for
// rungs 0 to 2, the card bound for rungs 0 and 1, the ceiling for fund.
func drawWithin(f CaseFacts, rung int, d policy.Draw, toBudget bool) string {
	switch {
	case rung < 0 || rung > 3:
		return fmt.Sprintf("there is no rung %d", rung)
	case rung <= 1 && !fitsIn(d, f.CardDrawLeft):
		return (&DrawBoundError{Card: true, Left: f.CardDrawLeft}).Error()
	case rung <= 2 && !fitsIn(d, f.PoolLeft):
		return (&DrawBoundError{Left: f.PoolLeft}).Error()
	case toBudget && Cents(f.BudgetEUR)+Cents(d.EUR) > Cents(f.CeilingEUR):
		return fmt.Sprintf("%.2f EUR more takes the budget of %.2f EUR past the card's ceiling of %.2f EUR: raise the ceiling first",
			d.EUR, f.BudgetEUR, f.CeilingEUR)
	}
	return ""
}

// fitsIn is policy.Draw.Fits with money compared in cents, as the store
// keeps it.
func fitsIn(d, in policy.Draw) bool {
	return d.Sessions <= in.Sessions && d.AgentTime <= in.AgentTime && Cents(d.EUR) <= Cents(in.EUR)
}

// drawText says draw d's three parts.
func drawText(d policy.Draw) string {
	return fmt.Sprintf("%s, %s of agent time and %.2f EUR", textutil.Plural(d.Sessions, "session", "sessions"), minutesWord(d.AgentTime), d.EUR)
}

// who is rung as a refusal names it.
func who(rung int) string {
	if rung == 3 {
		return "the human"
	}
	return fmt.Sprintf("rung %d", rung)
}

// rungsText names the rungs of: "rungs 0, 1 and 2", "rung 2 and the
// human".
func rungsText(of [4]bool) string {
	var items []string
	for n, ok := range of[:3] {
		if ok {
			items = append(items, strconv.Itoa(n))
		}
	}
	switch len(items) {
	case 0:
	case 1:
		items[0] = "rung " + items[0]
	default:
		items[0] = "rungs " + items[0]
	}
	if of[3] {
		items = append(items, "the human")
	}
	return textutil.AndList(items)
}

// alertNoLongerFires is the reason an alert card ends with when its alert
// stopped firing before its first session (the alert pre-check).
const alertNoLongerFires = "alert no longer fires"

// terminalEnds mark the end reasons no retry can fix: the policy gate's
// class forbidden to agents, the issue a human closed and the alert that
// stopped firing.
var terminalEnds = []string{refusedToAgents, ReasonIssueClosed, alertNoLongerFires}

// retryableEnd reports whether a card that ended for why may be retried:
// why names none of terminalEnds.
func retryableEnd(why string) bool {
	return !slices.ContainsFunc(terminalEnds, func(end string) bool { return strings.Contains(why, end) })
}
