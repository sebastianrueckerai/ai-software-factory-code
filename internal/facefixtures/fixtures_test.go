package facefixtures_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// answer is a filled answer of action name: the note and the arguments a
// human's press sends with it.
func answer(name string) factoryd.Resolution {
	args := map[string]map[string]any{
		factoryd.ActHandover: {"model": "same"}, factoryd.ActExtend: {"sessions": 1}, factoryd.ActFund: {"eur": 1.0},
		factoryd.ActRaiseCeiling: {"eur": 1.0}, factoryd.ActSetDependency: {"depends_on": "t-2188"},
		factoryd.ActRemoveDependency: {"depends_on": "t-2188"}, factoryd.ActFileCard: {"title": "Board export: the snapshot writer"},
		factoryd.ActAnswerCriteria: {"criterion": "size_bound_escape", "verdict": "met"},
		factoryd.ActConfirmAudit:   {"findings": "secrets at acceptance/price_floor_acceptance_test.go:71"},
	}
	return factoryd.Resolution{Action: name, Args: args[name], Note: "the reason, one line"}
}

// Each fixture of a group names its kind once, and each fixture of a
// human action holds a case whose facts its predicate holds on, so the
// gallery shows a face where the action can happen.
func TestFixturesNameEachKindOnceAndActionsApply(t *testing.T) {
	seen := map[string]bool{}
	for _, fx := range facefixtures.All() {
		key := fx.Group + "/" + fx.Kind
		if seen[key] {
			t.Errorf("%s: two fixtures", key)
		}
		seen[key] = true
		if fx.Case != nil && factoryd.FactsOf(*fx.Case).Kind != fx.Case.Kind {
			t.Errorf("%s: the case's facts name kind %q, the case %q", key, factoryd.FactsOf(*fx.Case).Kind, fx.Case.Kind)
		}
		if fx.Group == facefixtures.GroupCase && (fx.Case == nil || string(fx.Case.Kind) != fx.Kind) {
			t.Errorf("%s: holds no case of its kind", key)
		}
		if fx.Group != facefixtures.GroupAction {
			continue
		}
		a, ok := factoryd.ActionByName(fx.Kind)
		switch {
		case !ok:
			t.Errorf("%s: no action of that name", key)
		case fx.Case == nil:
			t.Errorf("%s: no case to answer", key)
		default:
			if why := a.Applies(fx.Facts, answer(fx.Kind)); why != "" {
				t.Errorf("%s: the action does not apply to the case's facts: %s", key, why)
			}
		}
	}
}

// fixtureReads are the store's reads of a card's brief over the
// fixtures: their cards, the plan's revision and the plan's cards; no
// fixture holds a dependency or a plan question.
type fixtureReads []facefixtures.Fixture

func (r fixtureReads) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	for _, fx := range r {
		if fx.Task.ID == id {
			return fx.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (r fixtureReads) Dependencies(context.Context, string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (r fixtureReads) PlanRevision(_ context.Context, id string, _ int) (factoryd.PlanRevision, bool, error) {
	for _, fx := range r {
		if fx.Task.ID == id && fx.Plan != nil {
			raw, err := json.Marshal(fx.Plan)
			return factoryd.PlanRevision{TaskID: id, Revision: 1, Plan: raw}, true, err
		}
	}
	return factoryd.PlanRevision{}, false, nil
}

func (r fixtureReads) PlanCards(_ context.Context, plan string) ([]factoryd.Task, error) {
	var out []factoryd.Task
	for _, fx := range r {
		if t := fx.Task; t.Parent == plan && t.Source == factoryd.SourcePlan && t.PlanKey != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

func (r fixtureReads) PlanQuestions(context.Context, string, int) ([]factoryd.PlanQuestion, error) {
	return nil, nil
}

// Every fixture's card has a brief, the gallery's cards included: a card
// of plan #1947, the fixtures' plan, belongs to it at its place, with the
// plan's outcome.
func TestEveryFixtureHasACardBrief(t *testing.T) {
	all := facefixtures.All()
	plan := ""
	for _, fx := range all {
		if fx.Kind == "plan card" {
			plan = fx.Task.ID
		}
	}
	for _, fx := range all {
		if fx.Task.ID == "" {
			continue
		}
		b, err := factoryd.ReadCardBrief(context.Background(), fixtureReads(all), fx.Task.ID)
		if err != nil {
			t.Errorf("%s %s: %v", fx.Group, fx.Kind, err)
			continue
		}
		if b.Text.Title == "" {
			t.Errorf("%s %s: no title", fx.Group, fx.Kind)
		}
		if fx.Task.Parent != plan {
			continue
		}
		if p := b.Plan; p == nil || p.Issue != 1947 || p.Index == 0 || p.Of != 6 || p.Outcome == "" || len(p.Cards) != 6 {
			t.Errorf("%s %s: plan %+v, want card i of 6 of #1947 with its outcome", fx.Group, fx.Kind, p)
		}
	}
}
