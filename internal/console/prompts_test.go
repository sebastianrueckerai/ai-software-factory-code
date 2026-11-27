package console

import (
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// directSteps are the steps that call a model with a system text.
var directSteps = []string{
	"checkin-gate", "checkin-session", "checkin-dead", "case-checkin", "case-brief-writer", "brief-writer",
	"model-review", "model-review-part", "review-behaviours", "security-audit", "security-audit-part",
	"gate-supervisor", "delta-planner", "holdout-proposer", "judgement", "answer-screener", "plan-namer",
	"follow-liveness", "project-agent", "title", "compact", "research",
}

// Every direct step has a profile, a renderer and a fixture it builds on.
func TestProfileForEveryDirectStep(t *testing.T) {
	profiles := map[string]bool{}
	for _, p := range prompts.Profiles() {
		profiles[p.Step] = true
	}
	fixtures := map[string]any{}
	for _, r := range prompts.Registered() {
		fixtures[r.Step] = r.Fixture
	}
	for _, step := range directSteps {
		fixture, ok := fixtures[step]
		switch {
		case !profiles[step]:
			t.Errorf("step %s has no profile", step)
		case !ok || fixture == nil:
			t.Errorf("step %s has no fixture: register it with prompts.RegisterStep", step)
		default:
			if _, err := prompts.Build(step, fixture); err != nil {
				t.Errorf("step %s: %v", step, err)
			}
		}
	}
}

// Every step's fixture prompt stays within its profile's budget.
func TestBudgets(t *testing.T) {
	for step, words := range fixtureWords(t) {
		if budget := budgetOf(t, step); words > budget {
			t.Errorf("step %s: its fixture prompt holds %d words, over its budget of %d", step, words, budget)
		}
	}
}

// A budget holds no room: the fixture prompt fills at least 90 percent of it.
func TestBudgetsAreTight(t *testing.T) {
	for step, words := range fixtureWords(t) {
		if budget := budgetOf(t, step); words*10 < budget*9 {
			t.Errorf("step %s: its fixture prompt holds %d words, under 90 percent of its budget of %d: lower budget_words", step, words, budget)
		}
	}
}

// fixtureWords are the words of each registered step's fixture prompt.
func fixtureWords(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range prompts.Registered() {
		p, err := prompts.Build(r.Step, r.Fixture)
		if err != nil {
			t.Fatalf("step %s: %v", r.Step, err)
		}
		out[r.Step] = p.Parts().Total()
	}
	return out
}

func budgetOf(t *testing.T, step string) int {
	t.Helper()
	for _, p := range prompts.Profiles() {
		if p.Step == step {
			return p.BudgetWords
		}
	}
	t.Fatalf("step %s has no profile", step)
	return 0
}
