package facefixtures_test

import (
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
			if why := a.Applies(factoryd.FactsOf(*fx.Case), answer(fx.Kind)); why != "" {
				t.Errorf("%s: the action does not apply to the case's facts: %s", key, why)
			}
		}
	}
}
