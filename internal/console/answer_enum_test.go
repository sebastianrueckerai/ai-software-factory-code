package console

import (
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The answer tool's action enum is every action rung 2 or the human may
// take, the board's rows among them; its refusals are Act's.
func TestAnswerToolActionEnumHoldsTheBoardActions(t *testing.T) {
	r := newRig(t)
	tool, _ := r.c.chatTool("answer")
	enum, _ := tool.params["action"].(map[string]any)["enum"].([]string)
	for _, name := range []string{factoryd.ActApproveMerge, factoryd.ActDenyMerge, factoryd.ActSetDependency, factoryd.ActRemoveDependency,
		factoryd.ActFileCard, factoryd.ActRestartPlan, factoryd.ActAnswerCriteria, factoryd.ActDeployNow} {
		if !slices.Contains(enum, name) {
			t.Errorf("the answer tool's action enum %v lacks %s", enum, name)
		}
	}
	for _, a := range factoryd.Actions {
		if (a.Rungs[2] || a.Rungs[3]) != slices.Contains(enum, a.Name) {
			t.Errorf("action %s: the enum holds it = %v, want rung 2 or 3 may take it = %v", a.Name, slices.Contains(enum, a.Name), a.Rungs[2] || a.Rungs[3])
		}
	}
}
