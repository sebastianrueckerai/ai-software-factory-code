package console

import (
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The answer tool's action enum is every action rung 2 or the human may
// take but the room, which give_room gives, the board's rows among them;
// its refusals are Act's.
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
		if want := (a.Rungs[2] || a.Rungs[3]) && a.Name != factoryd.ActExtend; want != slices.Contains(enum, a.Name) {
			t.Errorf("action %s: the enum holds it = %v, want %v", a.Name, slices.Contains(enum, a.Name), want)
		}
	}
}
