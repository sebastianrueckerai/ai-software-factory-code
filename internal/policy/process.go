package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Process is processes/<name>.yaml: the blocks a project's cards pass
// (ruling 206). A class is policy.yaml's; a process only names it.
type Process struct {
	// Lanes are where a card waits or starts: backlog and ready always,
	// plan for a process that builds from a spec. A card entering a
	// starting lane (plan, ready) starts a task of the lane's class.
	Lanes []Lane `yaml:"lanes" json:"lanes"`
	// Dependencies is how a plan's cards wait on each other in lane ready;
	// only in a process with a lane plan.
	Dependencies *Dependencies `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	// Stages are the stages a coding task's work passes, in this order
	// (ruling 177): checks, policy, approval and merge are required,
	// approval right before merge, merge last; checks, policy, approval
	// and merge always run; screening is first and for a human only.
	Stages []Stage `yaml:"stages" json:"stages"`
	// Merge is how the project's pull requests merge.
	Merge Merge `yaml:"merge" json:"merge"`
}

// Lane is one lane of a process.
type Lane struct {
	// Name is backlog, plan or ready, each at most once.
	Name string `yaml:"name" json:"name"`
	// Class is the class of the task a card entering the lane starts: on
	// ready a class with session code, on plan one with session plan; not
	// on backlog.
	Class string `yaml:"class,omitempty" json:"class,omitempty"`
	// MovedBy is who may put a card into a starting lane, at its creation
	// or later: writers of the card's repository, or the policy's
	// approvers. Not on backlog: no card moves into it.
	MovedBy string `yaml:"moved_by,omitempty" json:"moved_by,omitempty"`
	// WriteUp is, on backlog only, a class with session report that writes
	// a card up once when it enters (ruling 185).
	WriteUp string `yaml:"write_up,omitempty" json:"write_up,omitempty"`
	// Plan bounds the plan the planner answers; on lane plan only, and
	// required there.
	Plan *PlanShape `yaml:"plan,omitempty" json:"plan,omitempty"`
}

// PlanShape bounds a plan.
type PlanShape struct {
	// MaxCards is the most cards a plan may hold, 2 to 20.
	MaxCards int `yaml:"max_cards" json:"max_cards"`
	// Foundation: exactly one card of role foundation, which every other
	// card depends on.
	Foundation bool `yaml:"foundation" json:"foundation"`
	// Integration: exactly one card of role integration, which depends on
	// every other card.
	Integration bool `yaml:"integration" json:"integration"`
}

// Dependencies is how a card waits for the cards it depends on.
type Dependencies struct {
	// WaitUntil: done starts a card once every card it depends on is done.
	WaitUntil string `yaml:"wait_until" json:"wait_until"`
	// OnFailed: human opens the card when a card it depends on ends failed
	// or closed; an answer starts it anyway, a close ends it.
	OnFailed string `yaml:"on_failed" json:"on_failed"`
}

// Merge is how a project's pull requests merge.
type Merge struct {
	// Queue: none merges each approved pull request as it comes; project
	// merges one of the project's pull requests at a time, main merged in
	// and the checks run again when main moved.
	Queue string `yaml:"queue" json:"queue"`
}

// Stage is one named stage of a process and what its findings do.
type Stage struct {
	// Name is the stage's implementation. screening is first; migration
	// comes after policy, which finds the migrations; audit (ruling 188)
	// reads the reviewed head for a fixed security checklist, between
	// review and approval, and runs for the projects whose file sets
	// audit: true.
	Name string `yaml:"name" json:"name"`
	// OnFindings: agent gives the stage's findings to the next session
	// (the first return keeps the model, the next takes the stronger rung,
	// limits.sessions is the round cap), human opens the card with them,
	// fail fails the task. checks: agent or fail; policy: fail; screening,
	// audit, approval and merge: human.
	OnFindings string `yaml:"on_findings" json:"on_findings"`
	// Enabled false skips the stage (the timeline records it skipped).
	Enabled bool `yaml:"enabled" json:"enabled"`
	// When: outsider runs the stage only on issues by an outside account
	// (the screening reads an outsider's issue before the coder, ruling
	// 182). Absent: every task. The review takes no when (enabled: false
	// is its only off switch, backlog 68): a review that gives no verdict
	// is a finding on the card.
	When string `yaml:"when,omitempty" json:"when,omitempty"`
	// MaxReturns caps the stage's own returns to the agent before the card
	// (ruling 190); absent: the session cap decides. On a stage whose
	// on_findings is agent only, at least 1.
	MaxReturns *int `yaml:"max_returns,omitempty" json:"max_returns,omitempty"`
}

// Stage is the named stage and whether the process lists it.
func (p Process) Stage(name string) (Stage, bool) {
	i := slices.IndexFunc(p.Stages, func(s Stage) bool { return s.Name == name })
	if i < 0 {
		return Stage{}, false
	}
	return p.Stages[i], true
}

// Lane is the named lane and whether the process has it.
func (p Process) Lane(name string) (Lane, bool) {
	i := slices.IndexFunc(p.Lanes, func(l Lane) bool { return l.Name == name })
	if i < 0 {
		return Lane{}, false
	}
	return p.Lanes[i], true
}

// Lane names.
const (
	LaneBacklog = "backlog"
	LanePlan    = "plan"
	LaneReady   = "ready"
)

// LaneNames are the lanes, in the board's order.
var LaneNames = []string{LaneBacklog, LanePlan, LaneReady}

// Who may move a card into a starting lane.
const (
	MovedByWriters   = "writers"
	MovedByApprovers = "approvers"
)

// MovedByValues are the moved_by values.
var MovedByValues = []string{MovedByWriters, MovedByApprovers}

// Session modes of a class: code runs the coding lifecycle, plan the plan
// loop, report a write-up.
const (
	SessionCode   = "code"
	SessionPlan   = "plan"
	SessionReport = "report"
)

// Sessions are the session values.
var Sessions = []string{SessionCode, SessionPlan, SessionReport}

// The dependencies values.
var (
	WaitUntilValues = []string{"done"}
	OnFailedValues  = []string{"human"}
)

// Merge queues.
const (
	QueueNone    = "none"
	QueueProject = "project"
)

// Queues are the merge.queue values.
var Queues = []string{QueueNone, QueueProject}

// Stage names.
const (
	StageChecks       = "checks"
	StageScreening    = "screening"
	StageReproduction = "reproduction"
	StagePolicy       = "policy"
	StageMigration    = "migration"
	StageReview       = "review"
	StageAudit        = "audit"
	StageApproval     = "approval"
	StageMerge        = "merge"
)

// StageNames are the stage implementations.
var StageNames = []string{StageChecks, StageScreening, StageReproduction, StagePolicy, StageMigration, StageReview, StageAudit, StageApproval, StageMerge}

// What a stage's findings do.
const (
	FindingsAgent = "agent"
	FindingsHuman = "human"
	FindingsFail  = "fail"
)

// FindingsRules are the on_findings values.
var FindingsRules = []string{FindingsAgent, FindingsHuman, FindingsFail}

// WhenOutsider limits a stage to issues by an outside account.
const WhenOutsider = "outsider"

// StagePredicates are the when values.
var StagePredicates = []string{WhenOutsider}

// starting reports whether a card entering lane starts a task.
func starting(lane string) bool { return lane == LanePlan || lane == LaneReady }

// Validate checks a process on its own, each refusal one line
// "<block>: <reason>"; what it needs from policy.yaml and the projects is
// the tree's check.
func (p Process) Validate() error {
	var errs []error
	bad := func(block, format string, a ...any) {
		errs = append(errs, fmt.Errorf(block+": "+format, a...))
	}
	var seen []string
	for i, l := range p.Lanes {
		switch {
		case !slices.Contains(LaneNames, l.Name):
			bad("lanes", "lane %d: unknown name %q (known: %s)", i+1, l.Name, strings.Join(LaneNames, ", "))
			continue
		case slices.Contains(seen, l.Name):
			bad("lanes", "lane %s is listed twice", l.Name)
			continue
		}
		seen = append(seen, l.Name)
		block := "lanes." + l.Name
		if starting(l.Name) {
			if l.Class == "" {
				bad(block, "class is required on a starting lane")
			}
			switch {
			case l.MovedBy == "":
				bad(block, "moved_by is required on a starting lane, one of %v", MovedByValues)
			case !slices.Contains(MovedByValues, l.MovedBy):
				bad(block, "moved_by %q, want one of %v", l.MovedBy, MovedByValues)
			}
		} else if l.Class != "" || l.MovedBy != "" {
			bad(block, "class and moved_by are not allowed: backlog starts nothing and no card moves into it")
		}
		if l.WriteUp != "" && l.Name != LaneBacklog {
			bad(block, "write_up is allowed on lane backlog only")
		}
		switch {
		case l.Plan != nil && l.Name != LanePlan:
			bad(block, "plan is allowed on lane plan only")
		case l.Plan == nil && l.Name == LanePlan:
			bad(block, "plan is required: it bounds the plan the planner answers")
		case l.Plan != nil && (l.Plan.MaxCards < 2 || l.Plan.MaxCards > 20):
			bad(block, "plan.max_cards %d: want 2 to 20", l.Plan.MaxCards)
		}
	}
	if !slices.Contains(seen, LaneBacklog) {
		bad("lanes", "lane backlog is required: the cards that do not start wait there")
	}
	if !slices.Contains(seen, LaneReady) {
		bad("lanes", "lane ready is required: it starts the coding tasks")
	}
	_, plan := p.Lane(LanePlan)
	switch d := p.Dependencies; {
	case plan && d == nil:
		bad("lanes.plan", "needs the block dependencies before it: the plan's cards wait on each other in lane ready")
	case !plan && d != nil:
		bad("dependencies", "no lane plan makes dependencies: remove the block or add the lane")
	}
	if d := p.Dependencies; d != nil {
		if !slices.Contains(WaitUntilValues, d.WaitUntil) {
			bad("dependencies", "wait_until %q, want one of %v", d.WaitUntil, WaitUntilValues)
		}
		if !slices.Contains(OnFailedValues, d.OnFailed) {
			bad("dependencies", "on_failed %q, want one of %v", d.OnFailed, OnFailedValues)
		}
	}
	if !slices.Contains(Queues, p.Merge.Queue) {
		bad("merge", "queue %q, want one of %v", p.Merge.Queue, Queues)
	}
	return errors.Join(append(errs, validateStages(p.Stages))...)
}

// validateStages checks the stage list: known names, each once, known
// rules and predicates; the floor: checks, policy, approval and merge
// present and run for every task, checks' findings never to the card
// alone, policy failing the task, approval right before merge, merge last;
// review with no when (a review skipped for any reason but enabled false
// would be a pass without a verdict, backlog 68); approval, merge,
// screening and audit with findings for a human (ruling 182: a suspect
// issue never reaches the agent unread; ruling 188); screening first (it
// runs before the first session); migration after policy (policy finds
// the migrations); audit between review and approval (it reads the
// reviewed head); max_returns at least 1, on agent stages only.
func validateStages(stages []Stage) error {
	var errs []error
	bad := func(block, format string, a ...any) {
		errs = append(errs, fmt.Errorf(block+": "+format, a...))
	}
	var seen []string
	for i, s := range stages {
		switch {
		case !slices.Contains(StageNames, s.Name):
			bad("stages", "stage %d: unknown name %q (known: %v)", i+1, s.Name, StageNames)
		case slices.Contains(seen, s.Name):
			bad("stages", "stage %s is listed twice", s.Name)
		}
		seen = append(seen, s.Name)
		block := "stages." + s.Name
		if !slices.Contains(FindingsRules, s.OnFindings) {
			bad(block, "on_findings %q, want one of %v", s.OnFindings, FindingsRules)
		}
		if s.When != "" && !slices.Contains(StagePredicates, s.When) {
			bad(block, "when %q, want one of %v", s.When, StagePredicates)
		}
		if slices.Contains([]string{StageChecks, StagePolicy, StageApproval, StageMerge}, s.Name) && (!s.Enabled || s.When != "") {
			bad(block, "must run for every task (enabled, no when)")
		}
		if s.Name == StageReview && s.When != "" {
			bad(block, "when is not allowed: a review runs for every task, or enabled: false turns it off")
		}
		switch {
		case s.Name == StageChecks && s.OnFindings == FindingsHuman:
			bad(block, "on_findings must be agent or fail: no merge without green checks (floor)")
		case s.Name == StagePolicy && s.OnFindings != FindingsFail && slices.Contains(FindingsRules, s.OnFindings):
			bad(block, "on_findings must be fail: a forbidden path fails the task (floor)")
		case slices.Contains([]string{StageApproval, StageMerge, StageScreening, StageAudit}, s.Name) && s.OnFindings != FindingsHuman:
			bad(block, "on_findings must be human")
		}
		if s.MaxReturns != nil && *s.MaxReturns < 1 {
			bad(block, "max_returns must be at least 1, is %d", *s.MaxReturns)
		}
		if s.MaxReturns != nil && s.OnFindings != FindingsAgent {
			bad(block, "max_returns needs on_findings agent")
		}
	}
	at := func(name string) int { return slices.IndexFunc(stages, func(s Stage) bool { return s.Name == name }) }
	if !slices.Contains(seen, StageChecks) {
		bad("stages", "stage checks is required: no merge without green checks (floor)")
	}
	for _, name := range []string{StagePolicy, StageApproval, StageMerge} {
		if !slices.Contains(seen, name) {
			bad("stages", "stage %s is required", name)
		}
	}
	if i, m := at(StageApproval), at(StageMerge); i >= 0 && m >= 0 && i != m-1 {
		bad("stages", "stage approval must come right before merge")
	}
	if n := len(stages); n > 0 && stages[n-1].Name != StageMerge && slices.Contains(seen, StageMerge) {
		bad("stages", "stage merge must be last")
	}
	if i := at(StageScreening); i > 0 {
		bad("stages", "stage screening must be first: it runs before the first session")
	}
	if i, p := at(StageMigration), at(StagePolicy); i >= 0 && p >= 0 && i < p {
		bad("stages", "stage migration must come after policy, which finds the migrations")
	}
	if a, ap := at(StageAudit), at(StageApproval); a >= 0 && (a < at(StageReview) || at(StageReview) < 0 || ap >= 0 && a > ap) {
		bad("stages", "stage audit must come between review and approval: it reads the reviewed head")
	}
	return errors.Join(errs...)
}
