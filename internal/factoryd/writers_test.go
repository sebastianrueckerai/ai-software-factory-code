package factoryd

import (
	"maps"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
)

// agentOnlyCalls are the model calls whose text only agents read: the
// function that makes the call, which call, and why.
var agentOnlyCalls = func() []struct{ Call, What, Why string } {
	const consoleTool = "a tool result for the project agent, which the console shows inside the tool block of the chat"
	const sessionTool = "a tool result for the session that called the tool, which a human sees only inside that call's tool block"
	const intoSession = "factoryd's own text into a running session, which writes on by the forms its start gave it"
	calls := []struct{ Call, What, Why string }{
		{"console.(*turn).research", "the research subagent's report", consoleTool},
		{"console.(*turn).lookImage", "the look at an image the human attached", consoleTool},
		{"console.(*turn).summarizeAttachment", "the digest of an attachment", consoleTool},
		{"console.(*turn).withDigest", "the digest of a read", consoleTool},
		{"factoryd.(*Runner).DigestHandler", "the digest tool's summary", sessionTool},
		{"factoryd.(*Runner).ImageLookHandler", "the analyze_image tool's look", sessionTool},
		{"gateway.(*Server).digest", "the digest tool's call", sessionTool},
		{"gateway.(*Server).analyzeImage", "the analyze_image tool's call", sessionTool},
		{"gateway.(*Server).replicate", "the replicate_predict tool's run of a hosted model", sessionTool},
		{"gateway.(*Server).fal", "the fal_run tool's run of a hosted model", sessionTool},
		{"factoryd.(*Runner).message", "the supervisor's guidance", intoSession},
		{"factoryd.(*Runner).nudge", "the nudge after identical calls", intoSession},
		{"factoryd.(*Runner).watchLimits", "the note on a limit", intoSession},
		{"factoryd.(*Runner).RunReport", "the prover's session", "it files tests, and factoryd writes its summary"},
		{"factoryd.(*Runner).followModelAnswers", "the follow judgment's liveness call", "its answer is only checked for being there, and no one reads it"},
	}
	for _, capture := range []string{"factoryd.agentDocAudit", "factoryd.agentDocBehaviours", "factoryd.agentDocDelta",
		"factoryd.agentDocGateSupervisor", "factoryd.agentDocJudgement", "factoryd.agentDocReview"} {
		calls = append(calls, struct{ Call, What, Why string }{capture, "the agent map's capture",
			"a capture of a writer against a fake gateway, whose answer no one reads"})
	}
	return calls
}()

// Every model call is a writer's, whose prompt carries the forms of its
// jobs, or one whose text only agents read; no list names a function the
// walk does not reach.
func TestWritersHoldsEveryModelCall(t *testing.T) {
	sites, err := agentdoc.ModelCalls("../..")
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, w := range Writers {
		for _, c := range w.Calls {
			held[c] = true
			if _, ok := sites[c]; !ok {
				t.Errorf("writer %s names %s, which makes no model call", w.Name, c)
			}
		}
	}
	for _, a := range agentOnlyCalls {
		held[a.Call] = true
		if _, ok := sites[a.Call]; !ok {
			t.Errorf("agentOnlyCalls holds %s for %s, which makes no model call (its reason: %s)", a.Call, a.What, a.Why)
		}
	}
	for _, c := range slices.Sorted(maps.Keys(sites)) {
		if !held[c] {
			t.Errorf("%s makes a model call at %s: add it to Writers with the forms of its job, to agentOnlyCalls with why only agents read its text, "+
				"or to agentdoc's carriers when it hands its caller's prompt on", c, sites[c].Pos)
		}
	}
}
