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
	ActConfirmAudit  = "confirm_audit" // args: findings, their ids, one per line; note: the reason
	ActExtend        = "extend"        // args: sessions, 1 to MaxRoomSessions; money, true when left out
	ActSuccessor     = "successor"
	ActRetry         = "retry"  // args: task_id of the failed dependency
	ActReplan        = "replan" // args: task_id of a failed dependency, whose work its plan's planner takes (impasse)
	ActClose         = "close"
	ActDeployNow     = "deploy_now" // releases a held production: the roll-held case's own
	ActSalvage       = "salvage"    // ends a run that neither moves nor answers and files its successor: the human's alone
	ActWithdraw      = "withdraw"
	ActNote          = "note"   // records a step and resolves nothing
	ActSettle        = "settle" // ends a case alone; the card goes on: a monitor flag's own, or a park's

	// the board's actions: the console's code does their work (ActInput.Work)
	ActApproveMerge     = "approve_merge"
	ActDenyMerge        = "deny_merge"        // note: the reason; args: action = request_changes | reject
	ActSetDependency    = "set_dependency"    // args: depends_on, task_id (the card, else the case's)
	ActRemoveDependency = "remove_dependency" // args: as set_dependency
	ActFileCard         = "file_card"         // args: title, why, summary, body, size
	ActRestartPlan      = "restart_plan"      // note: the shape the new plan takes
	ActAnswerCriteria   = "answer_criteria"   // args: criterion, verdict = met | not_met | waive; note: the reason
	ActGrantCriteria    = "grant_criteria"    // one more bounded card for the open criteria of a criteria request
	ActConfirmCriteria  = "confirm_criteria"  // the delivered cards meet the criteria: every open one met, the note the evidence
	ActRunCriteria      = "run_criteria"      // runs the tests of a plan's promises that no test checked yet and records each result
)

// ActHandBack returns a plan's work card for a human to its planner with
// the human's note: the human cannot do the work as it was cut.
const ActHandBack = "hand_back"

// Resolution is one answer on a case: the action with its arguments, an
// optional room that rides beside it, and a note.
type Resolution struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
	Room   *RoomAsk       `json:"room,omitempty"`
	Note   string         `json:"note,omitempty"`
	// Moments are the instants Note names.
	Moments Moments `json:"moments,omitempty"`
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

// questionKinds are the agent's own questions, whose answer runs in a
// session: where no session can run, each is its cap's case first.
var questionKinds = []CaseKind{CaseAgentEscalate, CaseAgentRequestInfo}

// RoomAsk is a room asked for: a count of sessions' worth and whether its
// money is in it.
type RoomAsk struct {
	Sessions int  `json:"sessions"`
	Money    bool `json:"money"`
}

// roomCountRefusal refuses a count of sessions' worth outside 1 to
// MaxRoomSessions.
var roomCountRefusal = fmt.Sprintf("a room counts 1 to %d sessions' worth", MaxRoomSessions)

// roomAsk is the room r asks for: an extend's own count and money, its
// money true when left out, else the room beside its action. why refuses
// a count that is no whole number from 1 to MaxRoomSessions, and a money
// that is neither true nor false.
func (r Resolution) roomAsk() (ask RoomAsk, asked bool, why string) {
	if r.Action != ActExtend {
		if r.Room == nil {
			return RoomAsk{}, false, ""
		}
		if r.Room.Sessions < 1 || r.Room.Sessions > MaxRoomSessions {
			return *r.Room, true, roomCountRefusal
		}
		return *r.Room, true, ""
	}
	n, ok := toInt(r.Args["sessions"])
	if f, fraction := r.Args["sessions"].(float64); fraction && f != math.Trunc(f) || !ok || n < 1 || n > MaxRoomSessions {
		return RoomAsk{}, true, roomCountRefusal
	}
	ask = RoomAsk{Sessions: n, Money: true}
	if m, set := r.Args["money"]; set {
		if ask.Money, ok = m.(bool); !ok {
			return ask, true, "a room's money is true or false"
		}
	}
	return ask, true, ""
}

// RoomRefusal is why the room r asks for is none a decider may ask for, ""
// when it is one or r asks for none: Act's own check of the count and the
// money, which a door runs before it offers the ask to a human.
func (r Resolution) RoomRefusal() string {
	_, _, why := r.roomAsk()
	return why
}

// Said are the instants a refusal on f names (HumanRefusal): the turn of
// the cap it waits on and the time of its last draw.
func (f CaseFacts) Said() Moments {
	var said Moments
	if f.CapWait != "" && !f.CapTurn.IsZero() {
		said.Say(f.CapTurn, LayoutRFC3339)
	}
	if d, _ := weighedDraw(f); !d.At.IsZero() {
		said.Say(d.At, LayoutClock)
	}
	return said
}

// HumanRefusal is why a human's press of r on f is refused, its guard
// aside: what a room's readout and the chat's proposal show in place of
// its figures.
func HumanRefusal(f CaseFacts, r Resolution) string {
	a, ok := ActionByName(r.Action)
	if !ok {
		return "there is no action `" + r.Action + "`"
	}
	return a.refusal(f, 3, r)
}

// room is the room r gives on f: an extend's, or the one beside its
// action; false for none, or for an ask roomAsk refuses.
func (r Resolution) room(f CaseFacts) (Room, bool) {
	ask, asked, why := r.roomAsk()
	if !asked || why != "" {
		return Room{}, false
	}
	return RoomFor(f, ask.Sessions, ask.Money), true
}

// draw is what room r takes from the pool and the card's draw bound: its
// starts, its agent time and its money with the raise of the spend limit.
func (r Room) draw() policy.Draw {
	return policy.Draw{Sessions: r.Starts, AgentTime: r.AgentTime, EUR: r.EUR + r.Limit}
}

// lifts reports whether r's room lifts the cap f stops at: starts the
// session cap, agent time the agent time cap, money the budget floor.
func lifts(f CaseFacts, r Resolution) bool {
	room, ok := r.room(f)
	switch f.Kind {
	case CaseSessionCap:
		return ok && room.Starts > 0
	case CaseAgentTimeCap:
		return ok && room.AgentTime > 0
	case CaseBudgetFloor:
		return ok && room.EUR > 0
	}
	return false
}

// atCap is f as the human meets it: a question of the agent's that no
// session can answer is the case of the cap that keeps the session off,
// in the run's order (noSession), one decision at a time. Any other f is
// itself.
func (f CaseFacts) atCap() CaseFacts {
	if slices.Contains(questionKinds, f.Kind) {
		if k, _ := f.authorGranted().noSession(false, false); k != "" {
			f.Kind = k
		}
	}
	return f
}

// authorGranted is f with what the issue author's answer to an
// information request buys while the run grants it (PosterGrant): one
// more session, and the agent time a session needs.
func (f CaseFacts) authorGranted() CaseFacts {
	if f.Kind != CaseAgentRequestInfo || !f.PosterGrant {
		return f
	}
	if f.SessionCap > 0 {
		f.SessionCap = max(f.SessionCap, f.Attempts+1)
	}
	if f.AgentTimeCapS > 0 {
		f.AgentTimeCapS = max(f.AgentTimeCapS, f.AgentTimeS+f.SessionLeastS)
	}
	return f
}

// extendKinds are the cases an extend answers: the caps, the money stop,
// and the stops whose questions offer more room as an owner exit.
var extendKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor, CaseCheckinShare, CaseGateStop, CaseFindingsRepeated}

// replyAloneKinds are the cases an answer row that names no action ends
// never: every ask whose ladder line reads "A reply alone starts no attempt",
// the five caps and the budget floor, and the factory's month cap card,
// whose choices answer it. Such a row is the webhook's copy of a comment on
// the card's issue; it stands as the recorded comment, the run reads it as
// guidance and the card trips again at the cap or the floor. A row that
// names its action — the console's and chat's Act answers, an extend, a
// fund — and a session grant answer them as any other case.
var replyAloneKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseDayCap, CaseMonthCap, CaseFactoryMonthCap, CaseProjectCap, CaseBudgetFloor}

// reportRuns refuses a handover, an extend and a replan on a report card:
// its run takes none of them, and runs the report again on its own model
// and time.
const reportRuns = "a report card runs its report again on its own model and time, and takes no handover, extend or replan: resume it, continue it with a note as guidance, or close it"

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
		return sessionRoom(f.authorGranted().withDraw(r))
	}},
	{Name: ActHandover, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		if f.Report {
			return reportRuns
		}
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
		case f.MergeConflict != "":
			return conflictRefusal(f, ActRouteApproval)
		case f.Kind == CaseGateStop && f.Gate == ActivityWaitChecks:
			return "a checks wait's stop does not go to the approval (`route_approval`): it goes on with `wait_checks`"
		case !f.PROpen:
			return "no pull request is open"
		case f.Checks != "green":
			return fmt.Sprintf("the checks on the head are %s, not green", cmp.Or(f.Checks, "not known yet"))
		case !f.ReviewApproved && f.Kind != CasePolicyClass:
			return "the newest review of the head did not approve it"
		}
		return ""
	}},
	{Name: ActReverify, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.MergeConflict != "":
			return conflictRefusal(f, ActReverify)
		case !f.PROpen:
			return "no pull request is open"
		case f.Head == "":
			return "no head reached the gates"
		case f.Running:
			return sessionRuns
		}
		return ""
	}},
	{Name: ActRerunChecks, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case !f.PROpen:
			return "no pull request is open"
		case f.Checks != "red":
			return fmt.Sprintf("the checks on the head are %s, so there is nothing to run again", cmp.Or(f.Checks, "not known yet"))
		case f.RerunOnHead:
			return "the checks on this head were run again once already"
		}
		return ""
	}},
	{Name: ActWaitChecks, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Kind != CaseGateStop || f.Gate != ActivityWaitChecks:
			return "`wait_checks` runs a stopped checks wait again, and this ask is no checks wait's stop"
		case !f.PROpen:
			return "no pull request is open"
		}
		return ""
	}},
	// The human's alone, and never a machine's: the audit's checklist says
	// the human decides whether a finding is a fixture.
	{Name: ActConfirmAudit, Rungs: rungs(3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case slices.Contains(f.Partial, partialTimeline):
			return "the card's timeline could not be read, so the head's audit findings and whether the pull request still merges into main are unknown: try again in a minute"
		case f.MergeConflict != "":
			return conflictRefusal(f, ActConfirmAudit)
		case len(f.AuditConfirmable) == 0:
			return "the head's newest audit holds no finding at a file a human could confirm: reverify runs the audit on the head again"
		case strings.TrimSpace(r.Note) == "":
			return "a one-line reason is required"
		case len(confirmAuditFindings(r.Args)) == 0:
			return "no finding is ticked: tick at least one finding to clear"
		}
		for _, id := range confirmAuditFindings(r.Args) {
			switch {
			case !slices.Contains(f.AuditFindings, id) || f.FindingTexts[id] == "":
				return fmt.Sprintf("finding %s is not open on the head's newest audit: open the card again and tick the findings it shows now", id)
			case !slices.Contains(f.AuditConfirmable, id):
				return fmt.Sprintf("finding %s names no file, which no confirmation covers: tick the findings at a file", id)
			}
		}
		return ""
	}},
	{Name: ActExtend, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		ask, _, _ := r.roomAsk()
		switch {
		case f.Final != "":
			return "this card ended " + f.Final + ": more room buys nothing, file a new card"
		case f.Report:
			return reportRuns
		case slices.Contains(capKinds, f.Kind):
			return capLiftsNot
		case f.Kind != "" && !slices.Contains(extendKinds, f.Kind):
			return "this card asked a question, not for room: answer it, or give room at its cap"
		case !ask.Money && f.unfunded():
			return "this card lacks the money to open a session: give the room with its money"
		}
		return ""
	}},
	{Name: ActSuccessor, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		if !f.PROpen && !f.Pushed && f.Head == "" && f.BranchHead == "" {
			return "no pull request is open and the card's branch holds no work"
		}
		return ""
	}},
	{Name: ActRetry, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, r Resolution) string {
		id, _ := r.Args["task_id"].(string)
		if id == "" {
			switch {
			case f.FreezeCard != "" || slices.ContainsFunc(f.Dependencies, func(d DepFact) bool { return retryRefusal(d) == "" }):
				return ""
			case f.NoFreeze != "":
				return f.NoFreeze
			}
			return "no failed dependency is open to a retry, and no provider freeze holds the card: retry files a failed dependency again or ends a freeze, so pick the action the card's wait needs"
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
		if f.Report {
			return reportRuns
		}
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
			return "a session is running on this direct card: wait for it to end, then answer again"
		}
		return ""
	}},
	{Name: ActClose, Rungs: rungs(2, 3), Applies: applyAlways},
	{Name: ActApproveMerge, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Kind != CaseMergeApproval:
			return "`approve_merge` decides a merge request, and this ask is no merge approval"
		case !f.PROpen:
			return "the pull request is not open, so there is nothing to approve"
		case f.Checks != "green":
			return fmt.Sprintf("the checks on %s are %s, not green: `approve_merge` waits for green checks on the head", ShortSHA(f.Head), cmp.Or(f.Checks, "not known yet"))
		}
		return ""
	}},
	{Name: ActDenyMerge, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch a, _ := r.Args["action"].(string); {
		case f.Kind != CaseMergeApproval:
			return "`deny_merge` decides a merge request, and this ask is no merge approval"
		case !f.PROpen:
			return "the pull request is not open, so there is nothing to deny"
		case strings.TrimSpace(r.Note) == "":
			return "a denial carries its reason: the note"
		case a != "" && a != "request_changes" && a != "reject":
			return "`deny_merge` takes its action (`request_changes` or `reject`)"
		}
		return ""
	}},
	{Name: ActSetDependency, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case f.Kind != "" && !slices.Contains(dependencyKinds, f.Kind), f.Final != "" || f.Running || f.Attempts > 0:
			return "`set_dependency` makes a card that has not started wait for another"
		case strings.TrimSpace(argString(r.Args, "depends_on")) == "":
			return "`set_dependency` names the card to wait for (`depends_on`)"
		}
		return ""
	}},
	{Name: ActRemoveDependency, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case f.Kind != "" && !slices.Contains(dependencyKinds, f.Kind), f.Final != "" || f.Running || f.Attempts > 0:
			return "`remove_dependency` frees a card that has not started"
		case len(f.Dependencies) == 0 && argString(r.Args, "task_id") == "":
			return "the card waits on no dependency, so there is no edge to remove"
		case strings.TrimSpace(argString(r.Args, "depends_on")) == "":
			return "`remove_dependency` names the dependency to remove (`depends_on`)"
		}
		return ""
	}},
	{Name: ActFileCard, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case !slices.Contains(fileCardKinds, f.Kind):
			return "`file_card` files the work of a root cause outside the card, and this ask names none"
		case strings.TrimSpace(argString(r.Args, "title")) == "":
			return "`file_card` needs a title"
		}
		return ""
	}},
	{Name: ActRestartPlan, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case f.Plan == "":
			return "`restart_plan` ends a plan and files it anew, and this card is neither a plan nor a plan's card"
		case !ClosablePlan(Task{Track: policy.TrackPlan, Status: f.PlanStatus}):
			return "the plan is not at rest, so it cannot be closed to restart it: wait for its cards to end, or replan it"
		case strings.TrimSpace(r.Note) == "":
			return "`restart_plan` carries the shape the new plan takes: the note"
		}
		return ""
	}},
	{Name: ActAnswerCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch verdict := argString(r.Args, "verdict"); {
		case criteriaCaseRefusal(f, ActAnswerCriteria) != "":
			return criteriaCaseRefusal(f, ActAnswerCriteria)
		case verdict != "met" && verdict != "not_met" && verdict != "waive":
			return "an answer to a promise takes one verdict: `answer_criteria` with `met`, `answer_criteria` with `not_met` or `answer_criteria` with `waive`"
		case argString(r.Args, "criterion") == "":
			return "`answer_criteria` names the criterion"
		case verdict != "met" && strings.TrimSpace(r.Note) == "":
			return "a promise judged not met or waived carries its reason (`not_met` or `waive`): the note"
		}
		return ""
	}},
	{Name: ActGrantCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		return criteriaCaseRefusal(f, ActGrantCriteria)
	}},
	{Name: ActConfirmCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		return cmp.Or(criteriaCaseRefusal(f, ActConfirmCriteria), noEvidenceRefusal(f, r.Note))
	}},
	{Name: ActRunCriteria, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Kind == CaseMonitorFlag && f.Flag == "criterion_unproven":
		case f.Kind != CasePlanCriteria || f.ApprovalKind != ApprovalCriteriaRerun:
			return "Run the tests now runs the tests of a plan's promises no test checked, and this ask asks about none"
		case len(f.Runnable) == 0:
			return "no promise of this plan has a test the factory can run: build the missing part, or drop the promise"
		}
		return ""
	}},
	{Name: ActDeployNow, Rungs: rungs(2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		if f.Kind != CaseRollHeld {
			return "`deploy_now` releases a held production, and this ask is no roll hold's"
		}
		return ""
	}},
	// settle is a monitor flag's own and a park's: it ends the case
	// alone and leaves the card going on, where close would end the
	// card itself. A park's settle carries the choice it settled as its
	// park arg, so the table offers settle on a monitor flag's case
	// alone.
	{Name: ActSettle, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, r Resolution) string {
		if f.Kind != CaseMonitorFlag && f.Kind != CasePlanHeld && argString(r.Args, "park") == "" {
			return "settle ends a monitor flag's ask alone, a held plan's ask, or the ask a park settles, and this ask is none of them"
		}
		return ""
	}},
	{Name: ActSalvage, Rungs: rungs(3), Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case f.Final != "":
			return "this card ended " + f.Final + " and no run of it is left to end"
		case f.Report:
			return "a report card pushes no branch, so no new card can go on from it: resume it or close it"
		case f.Kind == CaseMonitorFlag && (f.Flag == "wait_over_bound" && f.Wait == "run_quiet" || f.Flag == "finding_confirmed_twice"), f.RunGone:
			return ""
		}
		return "`salvage` answers only a run that neither moves nor answers, or one the review keeps failing on the same finding: the monitor's flag of a quiet run or of a finding confirmed twice, or a live card whose run is gone"
	}},
	// withdraw is the factory's own: Act signals no run for it, so neither
	// the chat's agent nor the human takes it
	{Name: ActWithdraw, Rungs: rungs(0, 1), Applies: applyAlways},
	// a note with no words is the human's "Not now": the press is the record
	{Name: ActNote, Rungs: rungs(2, 3), Applies: applyAlways},
	{Name: ActHandBack, Rungs: rungs(3), Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case !ChoiceKind(f.Kind):
			return "`hand_back` returns a human's work card to its planner, and this ask is no human card's: answer the ask"
		case f.Plan == "":
			return "this card is in no plan, so no planner takes it back: have the factory do it, or end the card with your note"
		case strings.TrimSpace(r.Note) == "":
			return "`hand_back` carries what stops you, for the planner: the note"
		}
		return ""
	}},
}

// capKinds are the stops at a spending limit of the factory or the project,
// which no money for the card lifts.
var capKinds = []CaseKind{CaseDayCap, CaseMonthCap, CaseProjectCap}

// capLiftsNot is the refusal of room for a card at a spending limit.
const capLiftsNot = "this card waits on a spending limit of the factory or the project, which more room for the card does not lift: reset the limit's count in the console's status panel, raise the limit by a commit, or wait for it to reset"

// conflictRefusal refuses action, which runs the gates again, on a head
// that no longer merges into main: the merge queue's turn meets the same
// refusal, and only a session that merges main clears it (conflictFit).
func conflictRefusal(f CaseFacts, action string) string {
	return fmt.Sprintf("the pull request no longer merges into main (Forgejo refused to merge main into %s), so `%s` only runs the gates into the same refusal at the merge queue: "+
		"%s, or the card closes as superseded when main already carries its work", short(f.Head), action, conflictFit(f))
}

// rerunGates are the answers that run the gates again, which a head that
// no longer merges into main only runs into the merge queue's refusal.
var rerunGates = []string{ActRouteApproval, ActReverify}

// factsUnread refuses rung the action while what, a read of the case's
// facts that says what action needs to know, failed: whether unknown, a
// merge conflict for rerunGates in the card's timeline, a freeze for a
// retry in the timeline or the class's freeze cards. Try again, or a rung
// below the human hands it up.
func factsUnread(what, unknown, action string, rung int) string {
	why := what + " could not be read, so it is unknown whether " + unknown + ", which `" + action + "` needs: try again in a minute"
	if rung < 3 {
		why += ", or hand it up"
	}
	return why
}

// checksRunRefusal refuses a rung below the human the replan of the card
// itself while the checks on the head of its open pull request run, or
// could not be read: the replan closes the pull request before they judge
// its work.
func checksRunRefusal(f CaseFacts, rung int, action string, args map[string]any) string {
	if action != ActReplan || rung >= 3 || argString(args, "task_id") != "" || !f.PROpen {
		return ""
	}
	switch {
	case slices.Contains(f.Partial, "checks"):
		return factsUnread("the checks on the head", "they still run", action, rung)
	case f.Checks == "pending":
		return fmt.Sprintf("the checks on %s are pending: a replan would close pull request #%d before they judge its work, so %s takes no `replan` until they settle. "+
			"Once they settle red on a test outside the diff, `rerun_checks` runs them again", ShortSHA(f.Head), f.PR, who(rung))
	}
	return ""
}

// conflictFit is what a merge conflict needs, by the stop's kind: a
// session that merges main, with the answers a stop on sessions, agent
// time or money holds, an extend at the other stops an extend answers, the
// cap's own turn at a cap wait, else a continue with a note.
func conflictFit(f CaseFacts) string {
	const session = "the card needs a session that merges main into the branch"
	switch {
	case f.Kind == CaseSessionCap || f.Kind == CaseAgentTimeCap || f.Kind == CaseBudgetFloor:
		return session + stopAnswers(f.Kind, f)
	case slices.Contains(extendKinds, f.Kind):
		return session + ", which `" + ActExtend + "` buys"
	case f.Kind == CaseDayCap || f.Kind == CaseMonthCap || f.Kind == CaseProjectCap:
		return "the card's next session, once the cap turns, merges main into the branch"
	}
	return "continue the card with a note for a session that merges main into the branch"
}

// monitorActions are the actions a monitor flag's case takes: those Act carries
// out itself, since the card's run waits on no such case and drops its
// resolution.
var monitorActions = []string{ActNote, ActSettle, ActSalvage, ActRetry, ActReplan, ActWithdraw, ActRunCriteria}

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
// arguments or a note with: one session's worth, the same model, one note.
var probes = map[string]Resolution{
	ActHandover: {Args: map[string]any{"model": "same"}}, ActExtend: {Args: map[string]any{"sessions": 1}},
	ActContinue:         {Note: "probe"},
	ActDenyMerge:        {Note: "probe"},
	ActSetDependency:    {Args: map[string]any{"depends_on": "probe"}},
	ActRemoveDependency: {Args: map[string]any{"depends_on": "probe"}},
	ActFileCard:         {Args: map[string]any{"title": "probe"}},
	ActRestartPlan:      {Note: "probe"},
	ActAnswerCriteria:   {Args: map[string]any{"criterion": "probe", "verdict": "met"}},
	ActConfirmAudit:     {Note: "probe"},
}

// criteriaCaseRefusal is why action has no criteria to decide on f, "" when
// it has: answer_criteria judges those of a plan_criteria case or of a
// criteria card's merge, and a criteria request's decisions need a
// plan_criteria case, which alone holds one; one more card builds only
// what is open, and the work is enough only once the plan's cards are
// delivered.
func criteriaCaseRefusal(f CaseFacts, action string) string {
	switch {
	case action == ActAnswerCriteria && f.Kind != CasePlanCriteria && !(f.Kind == CaseMergeApproval && f.Criteria):
		return "`answer_criteria` judges a plan's criteria, and this ask holds none"
	case action != ActAnswerCriteria && f.Kind != CasePlanCriteria:
		return "`" + action + "` answers a plan's criteria request, and this ask is none"
	case action == ActGrantCriteria && f.Settled:
		return "every promise is met or dropped, so no card is left to build"
	case action == ActConfirmCriteria && f.Undelivered:
		return "a card of the plan is not delivered yet, so the work is not done"
	}
	return ""
}

// noEvidenceRefusal refuses a confirmation with no note over a plan a
// card of which ended closed.
func noEvidenceRefusal(f CaseFacts, note string) string {
	if f.ClosedCard && strings.TrimSpace(note) == "" {
		return "a card of the plan ended closed, so the work is enough only with your evidence: the note"
	}
	return ""
}

// mergeRuleRefusal is why rung may not take action, a decision of merge
// approval f, "" when it may: below full auto the merge rule is human, and
// rung 2 hands the merge up with its recommendation.
func mergeRuleRefusal(f CaseFacts, rung int, action string) string {
	if rung != 2 || f.Kind != CaseMergeApproval || action != ActApproveMerge && action != ActDenyMerge || f.Autonomy == policy.AutonomyFullAuto {
		return ""
	}
	return "the merge rule is human at `" + f.Autonomy + "`: hand the case up (`hand_up`) with `" + ActApproveMerge + "` or `" + ActDenyMerge + "` as the recommendation"
}

// humanWordActions settle a plan's criteria by word: a verdict on a
// criterion, a waiver among them, a confirmation and a denial. A person
// takes them (ErrNotPerson); rung 2 hands them up (CriteriaWordRefusal).
var humanWordActions = []string{ActAnswerCriteria, ActConfirmCriteria}

// CriteriaWordRefusal is rung 2's refusal of a plan's criteria settled by
// word on a case of kind k, which names what rung 2 may do instead: on a
// plan's criteria case the grant or the hand-up, on a criteria card's
// merge the hand-up.
func CriteriaWordRefusal(k CaseKind) string {
	const human = "settling a plan's criteria by word is the human's: "
	if k == CasePlanCriteria {
		return human + "take `" + ActGrantCriteria + "` for one more bounded card, or hand the case up (`hand_up`) with `" +
			ActAnswerCriteria + "` (waive) or `" + ActConfirmCriteria + "` as the recommendation"
	}
	return human + "hand the case up (`hand_up`) with `" + ActAnswerCriteria + "` as the recommendation"
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

// confirmAuditFindings is confirm_audit's findings argument: the ids of
// the findings a person confirms, one per line.
func confirmAuditFindings(args map[string]any) []string {
	var out []string
	for _, line := range strings.Split(argString(args, "findings"), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// boardActions are the actions whose work the console's code does
// (ActInput.Work), outside the run.
var boardActions = []string{ActApproveMerge, ActDenyMerge, ActSetDependency, ActRemoveDependency, ActFileCard, ActRestartPlan, ActAnswerCriteria,
	ActGrantCriteria, ActConfirmCriteria}

// IsBoardAction reports whether name is one of the actions the console's
// code does (ActInput.Work).
func IsBoardAction(name string) bool { return slices.Contains(boardActions, name) }

// keptCaseActions are the board actions that leave their case at the human:
// a verdict on one criterion and a filed card settle none of it.
var keptCaseActions = []string{ActAnswerCriteria, ActFileCard}

// requestEnds are the actions whose request's decision ends the case, by
// the decision's status: a merge's approval and denial. A grant decides its
// request too, but its work leaves the case to Act.
var requestEnds = []string{ActApproveMerge, ActDenyMerge}

// mergeDecisions are the actions that decide an approval request alone.
var mergeDecisions = append(slices.Clone(requestEnds), ActGrantCriteria)

// probe is action a's smallest answer on facts f: its probe, and the
// clearing of the audit's first finding.
func probe(a Action, f CaseFacts) Resolution {
	p := probes[a.Name]
	if a.Name == ActConfirmAudit && len(f.AuditConfirmable) > 0 {
		p.Args = map[string]any{"findings": f.AuditConfirmable[0]}
	}
	return p
}

// Offered lists the actions rung may take on facts f, in table order: its
// rows whose Applies holds and whose draw fits rung's bounds, tried with
// the smallest answers (probes) where an action needs arguments or a note.
func Offered(f CaseFacts, rung int) []Action {
	var out []Action
	for _, a := range Actions {
		if a.offerRefusal(f, rung) == "" {
			out = append(out, a)
		}
	}
	return out
}

// offerRefusal is why a is not on offer to rung on f, "" when it is: the
// action tried with the smallest answer (its probe). An extend is on offer
// when a room of 1 passes with its money or without it. A handover at the
// cap is on offer to rung 1 alone, the one rung whose answer carries a
// session draw, while the card's draw bound holds a session (handoverDraw).
func (a Action) offerRefusal(f CaseFacts, rung int) string {
	p := probe(a, f)
	res := Resolution{Action: a.Name, Args: p.Args, Note: p.Note}
	why := a.refusal(f, rung, res)
	if why != "" && a.Name == ActExtend && a.refusal(f, rung, Resolution{Action: a.Name, Args: moneylessRoom}) == "" {
		return ""
	}
	if _, ok := a.handoverDraw(f, rung); why != "" && ok {
		return ""
	}
	return why
}

// moneylessRoom is the extend's probe without its money.
var moneylessRoom = map[string]any{"sessions": 1, "money": false}

// handoverDraw is the room of 1 session's worth, with its money where that
// passes and else without it, that rung 1's answer may draw for a, the
// handover refused at the cap for want of a session; false when a is no
// such handover or neither room makes it pass.
func (a Action) handoverDraw(f CaseFacts, rung int) (RoomAsk, bool) {
	if rung != 1 || a.Name != ActHandover {
		return RoomAsk{}, false
	}
	p := probe(a, f)
	res := Resolution{Action: a.Name, Args: p.Args, Note: p.Note}
	if a.refusal(f, rung, res) == "" {
		return RoomAsk{}, false
	}
	for _, money := range []bool{true, false} {
		res.Room = &RoomAsk{Sessions: 1, Money: money}
		if a.refusal(f, rung, res) == "" {
			return *res.Room, true
		}
	}
	return RoomAsk{}, false
}

// refusal is why rung may not answer f with r by a, "" when it may: a's
// rungs, its Applies, and the room r gives. Money is drawn only on the
// rungs of moneyRungs and the spend limit raised only on those of
// limitRungs. A room is refused while the reads it is weighed on failed,
// after a draw that bought nothing, past the project's money left this
// month, and past the bounds of its rung (drawWithin), each refusal with
// the largest count that fits.
func (a Action) refusal(f CaseFacts, rung int, r Resolution) string {
	if rung < 0 || rung > 3 {
		return fmt.Sprintf("there is no decider %d", rung)
	}
	// rung 0 closes what an ended card left open, and no live card
	ended := rung == 0 && a.Name == ActClose && f.Final != ""
	if !a.Rungs[rung] && !ended {
		return fmt.Sprintf("%s may not pick `%s`: %s may", who(rung), a.Name, rungsText(a.Rungs))
	}
	if why := holdRefusal(f, rung, a.Name, r.Args); why != "" {
		return why
	}
	if _, _, why := r.roomAsk(); why != "" {
		return why
	}
	// Act's rule for an ended card's case (endedRefusal), read before a cap
	// stands in for the case's kind; a press with no case is the card's own
	onCase, planWait := f.Kind != "", f.Kind == CaseDependencyFailed && f.Planner
	if rung == 1 && a.Name == ActWithdraw && f.Kind != CaseMonitorFlag {
		return "A withdrawal (`withdraw`) ends a monitor flag's ask alone: the card's run waits on this ask, so the check-in answers it or hands it up"
	}
	if f.Kind == CaseMonitorFlag {
		switch {
		case a.Name == ActClose && f.Final == "" && !f.NoIssue:
			return "`close` ends the card, not the flag's ask: answer it with `settle`, which ends the ask, and the card goes on"
		case a.Name != ActClose && !slices.Contains(monitorActions, a.Name):
			return "`" + a.Name + "` would do nothing on a monitor flag's ask: the card's run waits on none"
		}
	}
	switch {
	case rung < 3 && !f.AtHuman && slices.Contains(questionKinds, f.Kind) && a.Name == ActExtend:
		// a rung working the case by itself: a draw would leave it open
		// with nobody on it
		const waits = ". A human's draw waits until the question reaches the human"
		if lifts(f.atCap(), r) {
			return "this ask is the agent's question, which a draw does not answer: hand it up with the draw as your recommendation" + waits
		}
		return "this ask is the agent's question, which a draw does not answer: answer it, or hand it up" + waits
	case rung == 3 || f.AtHuman:
		// a question at the human is weighed at its cap, whoever presses
		f = f.atCap()
	}
	if slices.Contains(rerunGates, a.Name) && slices.Contains(f.Partial, partialTimeline) {
		return factsUnread("the card's timeline", "the pull request still merges into main", a.Name, rung)
	}
	if id, _ := r.Args["task_id"].(string); a.Name == ActRetry && id == "" && f.Final == "" && a.Applies(f, r) != "" {
		switch {
		case f.Stage == GateProvider && slices.Contains(f.Partial, partialTimeline):
			return factsUnread("the card's timeline", "a provider freeze holds the card", a.Name, rung)
		case slices.Contains(f.Partial, partialFreeze):
			return factsUnread("the class's freeze cards", "a provider freeze holds the card", a.Name, rung)
		}
	}
	if rung == 2 && slices.Contains(humanWordActions, a.Name) && criteriaCaseRefusal(f, a.Name) == "" {
		return CriteriaWordRefusal(f.Kind)
	}
	if why := mergeRuleRefusal(f, rung, a.Name); why != "" {
		return why
	}
	if why := a.Applies(f, r); why != "" {
		return why
	}
	if why := checksRunRefusal(f, rung, a.Name, r.Args); why != "" {
		return why
	}
	if why := endedRefusal(f.Final, planWait, a.Name); onCase && why != "" {
		return "the card " + why
	}
	if a.Name == ActRetry && rung < 2 && endsFreeze(f, r) {
		return who(rung) + " ends no provider freeze, which a human's press or the factory agent's ends: hand it up (`hand_up`) with `retry` as the recommendation"
	}
	if i := slices.IndexFunc(f.Partial, func(p string) bool { return strings.HasPrefix(p, partialHumanClasses) }); rung == 2 && a.Name == ActApproveMerge && i >= 0 {
		return fmt.Sprintf("the classes that keep a human on a merge could not be read (%s), so the factory agent cannot tell whether class %s keeps one: hand it up (`hand_up`) with `approve_merge` as the recommendation",
			strings.TrimPrefix(f.Partial[i], partialHumanClasses+": "), f.RiskClass)
	}
	if rung == 2 && a.Name == ActApproveMerge && slices.Contains(f.HumanClasses, f.RiskClass) {
		return fmt.Sprintf("risk class %s keeps a human on its merge, whatever the autonomy: `approve_merge` is the human's alone, so hand it up (`hand_up`) with `approve_merge` as the recommendation", f.RiskClass)
	}
	if rung == 2 && a.Name == ActApproveMerge && len(f.UnreadGates) > 0 {
		return (&UnreviewedMergeError{Stages: f.UnreadGates}).Error()
	}
	if i := slices.IndexFunc(f.Partial, func(p string) bool { return strings.HasPrefix(p, partialApprovedCases) }); rung == 2 && a.Name == ActApproveMerge && i >= 0 {
		return fmt.Sprintf("the earlier cases of this merge could not be read (%s), so rung 2 cannot tell whether it approved head `%s` before: hand the case up (`hand_up`) with `approve_merge` as the recommendation",
			strings.TrimPrefix(f.Partial[i], partialApprovedCases+": "), f.Head[:min(7, len(f.Head))])
	}
	if rung == 2 && a.Name == ActApproveMerge && f.ApprovedOn != 0 {
		return fmt.Sprintf("rung 2 approved head `%s` of pull request #%d on case %d and its merge did not land: hand the case up (`hand_up`) with what the merge said",
			f.Head[:min(7, len(f.Head))], f.PR, f.ApprovedOn)
	}
	if rung == 2 && a.Name == ActRestartPlan && slices.Contains(f.Partial, "open criteria") {
		return "the plan's open criteria could not be read, so the factory agent cannot tell whether a restart's close waives any, and a waiver is the human's: hand it up (`hand_up`) with `restart_plan` as the recommendation"
	}
	if rung == 2 && a.Name == ActRestartPlan && f.OpenCriteria > 0 {
		return fmt.Sprintf("the plan holds %s, which a restart's close waives, and a waiver drops a requirement of the spec, which is the human's: hand it up (`hand_up`) with `restart_plan` as the recommendation",
			OpenCriteriaPhrase(f.OpenCriteria))
	}
	if slices.Contains(mergeDecisions, a.Name) && r.Room != nil {
		return "`" + a.Name + "` decides the merge request alone: it draws nothing"
	}
	if a.Name == ActWaitChecks && rung < 3 && f.WaitedOnHead {
		return "the checks wait on this head was run again once already"
	}
	room, asked := r.room(f)
	if !asked {
		return ""
	}
	unread := slices.IndexFunc(f.Partial, func(p string) bool { return p == "events" || p == "checks" })
	switch {
	case slices.Contains(f.Partial, "config"):
		return "the card's agent-time cap could not be read, so its room cannot be worked out: try again in a minute"
	case room.Money && slices.Contains(f.Partial, "floor"):
		return "the money a session of the card needs to open could not be read, so the room's money cannot be worked out: try again in a minute, or give the room without its money"
	case room.EUR > 0 && !moneyRungs[rung]:
		return fmt.Sprintf("%s may not draw money: %s may", who(rung), rungsText(moneyRungs))
	case room.Limit > 0 && !limitRungs[rung]:
		return "only the factory agent or a human raises the spend limit: " + fitWords(f, room, "fits under it", func(o Room) bool { return o.Limit == 0 })
	case unread >= 0:
		return fmt.Sprintf("the card's %s could not be read, so a draw cannot be weighed against the last one: try again in a minute, or close the card", f.Partial[unread])
	case boughtNothing(f):
		return boughtNothingText(f)
	}
	if why := monthWithin(f, room); why != "" {
		return why
	}
	return drawWithin(f, rung, room)
}

// shownRefusal is why a press whose guard (RoomGuard) differs from the
// room r gives on f, worked out at the press, is refused, "" when they
// agree or r posts no guard, as the chat's and the rungs' answers do not.
// Its text names what fits, the press again.
func shownRefusal(f CaseFacts, r Resolution) string {
	room, ok := r.room(f)
	if !ok {
		return ""
	}
	shown := room.Shown()
	guarded, same := false, true
	for _, k := range RoomGuard {
		v, ok := r.Args[k]
		guarded = guarded || ok
		same = same && ok && guardHolds(k, fmt.Sprint(v), shown[k])
	}
	if !guarded || same {
		return ""
	}
	return room.movedText(f)
}

// guardDriftCents is how far a guard's money and spend limit may lie from
// the press's: a running card's money moves with each model call.
const guardDriftCents = 10

// guardHolds says posted, guard k as the readout showed it, agrees with
// now, as the press works it out: the room and the time exactly, the money
// and the spend limit within guardDriftCents.
func guardHolds(k, posted, now string) bool {
	if k != "shown_eur" && k != "shown_limit" {
		return posted == now
	}
	a, errA := strconv.ParseFloat(posted, 64)
	b, errB := strconv.ParseFloat(now, 64)
	return errA == nil && errB == nil && max(Cents(a)-Cents(b), Cents(b)-Cents(a)) <= guardDriftCents
}

// moneyRungs are the deciders who draw money for a card, limitRungs those
// who raise its spend limit.
var (
	moneyRungs = rungs(1, 2, 3)
	limitRungs = rungs(2, 3)
)

// worthWords says n sessions' worth: "1 session's worth", "2 sessions'
// worth".
func worthWords(n int) string {
	if n == 1 {
		return "1 session's worth"
	}
	return fmt.Sprintf("%d sessions' worth", n)
}

// moreWorthWords says n more sessions' worth: "1 more session's worth".
func moreWorthWords(n int) string { return strings.Replace(worthWords(n), " ", " more ", 1) }

// below are the rooms on f of the counts under room's, with its money,
// smallest first: the counts a refusal tries, from the one asked down to 1.
func below(f CaseFacts, room Room) []Room {
	var out []Room
	for n := 1; n < room.N; n++ {
		out = append(out, RoomFor(f, n, room.Money))
	}
	return out
}

// roomFit is the largest of rooms that fits, false for none.
func roomFit(rooms []Room, fits func(Room) bool) (Room, bool) {
	for _, o := range slices.Backward(rooms) {
		if fits(o) {
			return o, true
		}
	}
	return Room{}, false
}

// worthText is what r's count of sessions' worth is: "28 minutes and 2.40
// EUR", the money left out when r has none or a session's worth costs none.
func (r Room) worthText() string {
	words := minutesWord(time.Duration(r.N) * r.Est.Time)
	if eur := float64(r.N) * r.Est.EUR; r.Money && Cents(eur) > 0 {
		words += " and " + textutil.EUR(eur) + " EUR"
	}
	return words
}

// fitWords names the largest count below room's that fits, and what of:
// "1 session's worth fits under it", "no session's worth fits under it".
func fitWords(f CaseFacts, room Room, what string, fits func(Room) bool) string {
	if o, ok := roomFit(below(f, room), fits); ok {
		return worthWords(o.N) + " " + what
	}
	return "no session's worth " + what
}

// monthWithin is why room's money passes the project's money left this
// month, which every decider draws within, "" when it does not: the count
// that fits names what to give instead. An overspent month leaves nothing.
func monthWithin(f CaseFacts, room Room) string {
	switch {
	case room.EUR <= 0:
		return ""
	case slices.Contains(f.Partial, "month"):
		return "the project's money left this month could not be read, so a room's money cannot be weighed against it: try again in a minute, or give the room without its money"
	}
	left := max(0, f.MonthLeftEUR)
	if Cents(room.EUR) <= Cents(left) {
		return ""
	}
	fit, ok := roomFit(below(f, room), func(o Room) bool { return Cents(o.EUR) <= Cents(left) })
	if !ok {
		return fmt.Sprintf("the project has %s EUR left this month, which pays for no session's worth: raise the month cap by a commit (`budget.month_eur`)", textutil.EUR(left))
	}
	return fmt.Sprintf("the project has %s EUR left this month, which pays for %s: give that, or raise the month cap by a commit (`budget.month_eur`)", textutil.EUR(left), worthWords(fit.N))
}

// withDraw is f with the room r gives beside its action counted: the
// starts and agent time raise the caps, the money the room left to spend,
// so a decision that rides a room is weighed as one record.
func (f CaseFacts) withDraw(r Resolution) CaseFacts {
	room, ok := r.room(f)
	if !ok {
		return f
	}
	f.SessionCap += room.Starts
	f.AgentTimeCapS += room.AgentTime.Seconds()
	f.LeftEUR += room.EUR
	return f
}

// sessionRuns refuses an answer that needs the card's session ended.
const sessionRuns = "a session is running: wait for it to end, then answer again"

// sessionRoom is why f leaves no room to open a session, "" when it has
// it: none running, an attempt under the cap, the agent time a session
// needs (SessionLeastS), and the money an opening needs, each stop with
// the answers it holds (stopAnswers).
func sessionRoom(f CaseFacts) string {
	limit, left, need := f.agentTime()
	switch {
	case f.Running:
		return sessionRuns
	case !f.Planner && capWaitDecision(f.CapWait) != "":
		// the cap's own line names the cap, its turn and its lift
		return strings.ToLower(f.CapWait[:1]) + f.CapWait[1:]
	case f.Attempts >= f.SessionCap:
		return fmt.Sprintf("the session cap of %d is reached", f.SessionCap) + stopAnswers(CaseSessionCap, f)
	case slices.Contains(f.Partial, "config"):
		return "the agent time cap could not be read: try again in a minute, or close the card"
	case left <= 0:
		return fmt.Sprintf("the agent time cap of %s is reached", minutesWord(limit)) + stopAnswers(CaseAgentTimeCap, f)
	case left < need:
		return fmt.Sprintf("the agent time cap of %s leaves %s and a session needs %s", minutesWord(limit), minutesWord(left), minutesWord(need)) + stopAnswers(CaseAgentTimeCap, f)
	case slices.Contains(f.Partial, "floor"):
		return "what an opening needs could not be read: try again in a minute, or close the card"
	case f.unfunded():
		return fmt.Sprintf("%s EUR is left for a session and an opening needs %s EUR", textutil.EUR(f.SessionLeft()), textutil.EUR(f.FloorEUR)) + stopAnswers(CaseBudgetFloor, f)
	}
	return ""
}

// stopAnswers ends a refusal that names a stop of kind on f with the
// answers that stop's face holds, by key: no reader of the refusal is left
// without its ways out. A rung's statement and its answer's parse list the
// answers their rung may take (forRung). "" when the list holds none. It is
// set in init because the action table's rows call it and it reads the
// table.
var stopAnswers func(kind CaseKind, f CaseFacts) string

func init() { stopAnswers = faceAnswerKeys }

func faceAnswerKeys(kind CaseKind, f CaseFacts) string {
	rung := f.answersFor
	f.Kind, f.answersFor = kind, 0
	facts := Facts{CaseFacts: f}
	var names []string
	for _, key := range CaseFaces[kind].Answers(facts) {
		a, ok := AnswerFor(key, facts)
		if act, known := ActionByName(a.Action); !ok || !answerHolds(a, facts) || rung != 0 && known && !act.Rungs[rung] {
			continue
		}
		if !slices.Contains(names, "`"+a.Action+"`") {
			names = append(names, "`"+a.Action+"`")
		}
	}
	if len(names) == 0 {
		return ""
	}
	if rung != 0 {
		return " (the card's answers at this stop that " + who(rung) + " may take: " + textutil.AndList(names) + ")"
	}
	return " (the card's answers at this stop: " + textutil.AndList(names) + ")"
}

// forRung is f for a refusal rung alone reads: the stop's answers it lists
// are those rung may take.
func (f CaseFacts) forRung(rung int) CaseFacts {
	f.answersFor = rung
	return f
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
// unchanged since it, and a session ended since it (weighedDraw). A draw
// whose row recorded no findings, or
// one on facts whose timeline could not be read, is weighed on the head and
// the checks. At an agent time stop the draw weighed is the newest that
// raised agent time: one that raised nothing the stop names is no draw.
func boughtNothing(f CaseFacts) bool {
	d, ok := weighedDraw(f)
	return ok && !d.Pushed && !d.Revised && d.Head == f.Head && d.Checks == f.Checks &&
		(d.Findings == nil || slices.Contains(f.Partial, partialTimeline) || slices.Equal(d.Findings, f.Findings))
}

// weighedDraw is the draw the guard weighs on f: none until a session
// ended since it, as the card's attempts pass the sessions its row
// recorded, so a draw the running session still spends, or one before the
// first session, has bought nothing yet. A row that recorded no sessions
// is weighed once the card ran one.
func weighedDraw(f CaseFacts) (DrawFacts, bool) {
	var d DrawFacts
	switch {
	case f.Kind == CaseAgentTimeCap && f.TimeDraw != nil:
		d = *f.TimeDraw
	case f.Kind != CaseAgentTimeCap && f.LastDrawAt != nil:
		d = DrawFacts{At: *f.LastDrawAt, Head: f.DrawHead, Checks: f.DrawChecks, Findings: f.DrawFindings, FromSessions: f.DrawFromSessions,
			Pushed: f.PushedSinceDraw, Revised: f.RevisedSinceDraw}
	default:
		return DrawFacts{}, false
	}
	return d, f.Attempts > d.FromSessions
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
	return "the last " + what + " at " + (Moment{At: d.At, Layout: LayoutClock}).Words() + " bought nothing, with no " + since + " since and the head, " +
		"the checks and the findings unchanged: hand over, start a successor, replan or close"
}

// drawWithin is why room does not fit rung's bounds on f, "" when it fits:
// the card's draw bound for rungs 0 and 1, the pool for rungs 0 to 2, each
// refusal with the largest count that fits and what it is worth, as the
// store's own weighing refuses it.
func drawWithin(f CaseFacts, rung int, room Room) string {
	if e := newDrawBound(rung, room.draw(), below(f, room), f.CardDrawLeft, f.PoolLeft); e != nil {
		return e.Error()
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
	return fmt.Sprintf("%s, %s of agent time and %s EUR", textutil.Plural(d.Sessions, "session", "sessions"), minutesWord(d.AgentTime), textutil.EUR(d.EUR))
}

// who is rung as a refusal names it.
func who(rung int) string {
	return []string{"the factory", "the check-in", "the factory agent", "the human"}[min(max(rung, 0), 3)]
}

// rungsText names the deciders of: "the factory, the check-in and the
// factory agent", "the factory agent and the human".
func rungsText(of [4]bool) string {
	var items []string
	for n, ok := range of {
		if ok {
			items = append(items, who(n))
		}
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

// endsFreeze reports whether retry r on facts f ends the provider freeze
// that holds the card: no task_id and no failed dependency open to a
// retry, the freeze card set.
func endsFreeze(f CaseFacts, r Resolution) bool {
	id, _ := r.Args["task_id"].(string)
	return id == "" && f.FreezeCard != "" && !slices.ContainsFunc(f.Dependencies, func(d DepFact) bool { return retryRefusal(d) == "" })
}
