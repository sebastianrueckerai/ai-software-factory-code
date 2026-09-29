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
// predicates, policy, approval and merge present, merge last.
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
