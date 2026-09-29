package policy

import (
	"errors"
	"fmt"
	"slices"
)

// Workflow is workflow.yaml: the stages a task's work passes, in order
// (ruling 177).
type Workflow struct {
	// Stages run in this order; policy, approval and merge are required,
	// merge last.
	Stages []Stage `yaml:"stages" json:"stages"`
}

// Stage is one named stage of the workflow and what its findings do.
type Stage struct {
	// Name is the stage's implementation.
	Name string `yaml:"name" json:"name"`
	// OnFindings: agent gives the findings to the next session, human opens
	// the card with them, fail fails the task.
	OnFindings string `yaml:"on_findings" json:"on_findings"`
	// Enabled false skips the stage; the timeline records it skipped.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// When limits the stage to some tasks: outsider runs it only on issues
	// whose author is an outside account. Absent: every task.
	When string `yaml:"when,omitempty" json:"when,omitempty"`
	// MaxReturns caps how often the stage sends its findings back to the
	// agent; its next findings go to the human card. Absent: no cap of its
	// own, the session cap decides (ruling 190). On a stage whose
	// on_findings is agent only, at least 1.
	MaxReturns *int `yaml:"max_returns,omitempty" json:"max_returns,omitempty"`
}

// Stage is the named stage and whether the workflow lists it.
func (w Workflow) Stage(name string) (Stage, bool) {
	i := slices.IndexFunc(w.Stages, func(s Stage) bool { return s.Name == name })
	if i < 0 {
		return Stage{}, false
	}
	return w.Stages[i], true
}

// Stage names.
const (
	StageChecks       = "checks"
	StageScreening    = "screening"
	StageReproduction = "reproduction"
	StagePolicy       = "policy"
	StageMigration    = "migration"
	StageReview       = "review"
	StageApproval     = "approval"
	StageMerge        = "merge"
)

// StageNames are the stage implementations.
var StageNames = []string{StageChecks, StageScreening, StageReproduction, StagePolicy, StageMigration, StageReview, StageApproval, StageMerge}

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

// Validate checks the stage list: known names, each once, known rules and
// predicates; policy, approval and merge present, approval and merge the
// last two; checks, policy, approval and merge enabled for every task (no
// when); approval, merge and screening with findings for a human (ruling
// 182: a suspect issue never reaches the agent unread); screening first
// (it runs before the first session); migration after policy (policy
// finds the migrations); max_returns at least 1, on agent stages only.
func (w Workflow) Validate() error {
	var errs []error
	var seen []string
	for i, s := range w.Stages {
		switch {
		case !slices.Contains(StageNames, s.Name):
			errs = append(errs, fmt.Errorf("workflow: stage %d: unknown name %q (known: %v)", i+1, s.Name, StageNames))
		case slices.Contains(seen, s.Name):
			errs = append(errs, fmt.Errorf("workflow: stage %s is listed twice", s.Name))
		}
		seen = append(seen, s.Name)
		if !slices.Contains(FindingsRules, s.OnFindings) {
			errs = append(errs, fmt.Errorf("workflow: stage %s: on_findings %q, want one of %v", s.Name, s.OnFindings, FindingsRules))
		}
		if s.When != "" && !slices.Contains(StagePredicates, s.When) {
			errs = append(errs, fmt.Errorf("workflow: stage %s: when %q, want one of %v", s.Name, s.When, StagePredicates))
		}
		if slices.Contains([]string{StageChecks, StagePolicy, StageApproval, StageMerge}, s.Name) && (!s.Enabled || s.When != "") {
			errs = append(errs, fmt.Errorf("workflow: stage %s must run for every task (enabled, no when)", s.Name))
		}
		if slices.Contains([]string{StageApproval, StageMerge, StageScreening}, s.Name) && s.OnFindings != FindingsHuman {
			errs = append(errs, fmt.Errorf("workflow: stage %s: on_findings must be human", s.Name))
		}
		if s.MaxReturns != nil && *s.MaxReturns < 1 {
			errs = append(errs, fmt.Errorf("workflow: stage %s: max_returns must be at least 1, is %d", s.Name, *s.MaxReturns))
		}
		if s.MaxReturns != nil && s.OnFindings != FindingsAgent {
			errs = append(errs, fmt.Errorf("workflow: stage %s: max_returns needs on_findings agent", s.Name))
		}
	}
	at := func(name string) int { return slices.IndexFunc(w.Stages, func(s Stage) bool { return s.Name == name }) }
	if i, m := at(StageApproval), at(StageMerge); i >= 0 && m >= 0 && i != m-1 {
		errs = append(errs, errors.New("workflow: stage approval must come right before merge"))
	}
	if i := at(StageScreening); i > 0 {
		errs = append(errs, errors.New("workflow: stage screening must be first: it runs before the first session"))
	}
	if i, p := at(StageMigration), at(StagePolicy); i >= 0 && p >= 0 && i < p {
		errs = append(errs, errors.New("workflow: stage migration must come after policy, which finds the migrations"))
	}
	for _, name := range []string{StagePolicy, StageApproval, StageMerge} {
		if !slices.Contains(seen, name) {
			errs = append(errs, fmt.Errorf("workflow: stage %s is required", name))
		}
	}
	if n := len(w.Stages); n > 0 && w.Stages[n-1].Name != StageMerge && slices.Contains(seen, StageMerge) {
		errs = append(errs, errors.New("workflow: stage merge must be last"))
	}
	return errors.Join(errs...)
}
