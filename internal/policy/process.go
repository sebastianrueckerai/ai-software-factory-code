package policy

import (
	"errors"
	"fmt"
	"slices"
)

// Process is processes/<name>.yaml: the blocks a project's cards pass
// (ruling 206). A class is policy.yaml's; a process only names it.
type Process struct {
	// Tracks are how a card's work runs, chosen by whoever files it
	// (roadmap 225): direct always, plan for a process that builds from a
	// spec. A card starts a task of its track's class.
	Tracks Tracks `yaml:"tracks" json:"tracks"`
	// Dependencies is how a plan's cards wait on each other; only in a
	// process with a plan track.
	Dependencies *Dependencies `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	// Stages are the stages a coding task's work passes, in this order
	// (ruling 177): checks, policy, approval and merge are required,
	// approval right before merge, merge last; checks, policy, approval
	// and merge always run; screening is first and for a human only. The
	// local gates (policy, migration, reproduction) come before the
	// checks, which wait on CI: their reads and commands answer before a
	// minute of it is spent, and the model review runs beside its wait
	// (roadmap 222, item b).
	Stages []Stage `yaml:"stages" json:"stages"`
	// Merge is how the project's pull requests merge.
	Merge Merge `yaml:"merge" json:"merge"`
	// Time overrides limits.agent_time and limits.check_in (and the
	// class's, if set) for tasks of this process; a project's Time wins
	// over it.
	Time *Time `yaml:"time,omitempty" json:"time,omitempty"`
}

// Tracks are a process's tracks.
type Tracks struct {
	// Direct builds a card at once: a class with session code.
	Direct Track `yaml:"direct" json:"direct"`
	// Plan plans a card first, a class with session plan, and files the
	// plan's cards on the direct track.
	Plan *Track `yaml:"plan,omitempty" json:"plan,omitempty"`
}

// Track is one track of a process.
type Track struct {
	// Class is the class of the task a card on the track starts.
	Class string `yaml:"class" json:"class"`
	// Plan bounds the plan the planner answers; on the plan track only,
	// and required there.
	Plan *PlanShape `yaml:"plan,omitempty" json:"plan,omitempty"`
}

// PlanShape bounds a plan.
type PlanShape struct {
	// MinCards is the least cards a plan may hold, 1 to 20 and at most
	// MaxCards.
	MinCards int `yaml:"min_cards" json:"min_cards"`
	// MaxCards is the most cards a plan may hold, 2 to 20.
	MaxCards int `yaml:"max_cards" json:"max_cards"`
	// Foundation: exactly one card of role foundation, which every other
	// card depends on.
	Foundation bool `yaml:"foundation" json:"foundation"`
	// Integration: exactly one card of role integration, which depends on
	// every other card.
	Integration bool `yaml:"integration" json:"integration"`
}

// DefaultMinCards is the plan minimum of a version stored before
// min_cards existed.
const DefaultMinCards = 2

// Minimum is the least cards a plan of this shape may hold: a MinCards of
// 0, from such a stored version, keeps the two-card minimum.
func (s PlanShape) Minimum() int {
	if s.MinCards == 0 {
		return DefaultMinCards
	}
	return s.MinCards
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

// Track is the named track and whether the process has it.
func (t Tracks) Track(name string) (Track, bool) {
	switch {
	case name == TrackDirect:
		return t.Direct, true
	case name == TrackPlan && t.Plan != nil:
		return *t.Plan, true
	}
	return Track{}, false
}

// Track names.
const (
	TrackDirect = "direct"
	TrackPlan   = "plan"
)

// TrackNames are the tracks.
var TrackNames = []string{TrackDirect, TrackPlan}

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

// removedLanes refuses a process file that still carries the lanes block,
// by name: a removed schema, not an ignored one (roadmap 225).
func removedLanes(b []byte) error {
	var doc map[string]any
	if decodeOne(b, &doc, false) != nil {
		return nil
	}
	if _, ok := doc["lanes"]; ok {
		return errors.New("lanes: removed: whoever files a card sets its track, plan or direct, and the classes and the plan bounds sit under tracks")
	}
	return nil
}

// Validate checks a process on its own, each refusal one line
// "<block>: <reason>"; what it needs from policy.yaml and the projects is
// the tree's check.
func (p Process) Validate() error {
	var errs []error
	bad := func(block, format string, a ...any) {
		errs = append(errs, fmt.Errorf(block+": "+format, a...))
	}
	if p.Tracks.Direct.Class == "" {
		bad("tracks.direct", "class is required: it builds every card")
	}
	if p.Tracks.Direct.Plan != nil {
		bad("tracks.direct", "plan is allowed on the plan track only")
	}
	if t := p.Tracks.Plan; t != nil {
		switch {
		case t.Class == "":
			bad("tracks.plan", "class is required: it plans the card")
		case t.Plan == nil:
			bad("tracks.plan", "plan is required: it bounds the plan the planner answers")
		case t.Plan.MaxCards < 2 || t.Plan.MaxCards > 20:
			bad("tracks.plan", "plan.max_cards %d: want 2 to 20", t.Plan.MaxCards)
		case t.Plan.MinCards < 1 || t.Plan.MinCards > 20:
			bad("tracks.plan", "plan.min_cards %d: want 1 to 20", t.Plan.MinCards)
		case t.Plan.MinCards > t.Plan.MaxCards:
			bad("tracks.plan", "plan.min_cards %d: above plan.max_cards %d", t.Plan.MinCards, t.Plan.MaxCards)
		}
	}
	switch plan, d := p.Tracks.Plan != nil, p.Dependencies; {
	case plan && d == nil:
		bad("tracks.plan", "needs the block dependencies: the plan's cards wait on each other")
	case !plan && d != nil:
		bad("dependencies", "no plan track makes dependencies: remove the block or add the track")
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
	if p.Time != nil {
		if err := p.Time.validate(); err != nil {
			bad("time", "%v", err)
		}
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
