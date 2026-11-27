package console

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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

// The session steps keep the tool sets their map pages show: every
// built-in tool, and the gateway's tools their classes grant.
func TestSessionStepsKeepTheirToolSets(t *testing.T) {
	analyzer := []string{"analyze_image", "deployment_health", "digest", "escalate", "manual", "request_info", "web_scrape", "web_search"}
	coder := []string{"analyze_image", "cloud_prices", "commission_server", "delegate", "deployment_health", "digest", "escalate", "fal_run", "list_resources", "manual",
		"release_resource", "replicate_predict", "request_cut", "request_info", "request_tool", "resource_request_status", "ssh_certificate", "web_scrape", "web_search"}
	want := map[string][]string{
		"sizing":      analyzer,
		"screening":   analyzer,
		"planner":     {"analyze_image", "digest", "escalate", "file_plan", "manual", "web_scrape", "web_search"},
		"prover":      {"file_proofs", "manual"},
		"coding":      coder,
		"coding-card": coder,
	}
	profiles := map[string]prompts.Profile{}
	for _, p := range prompts.Profiles() {
		profiles[p.Step] = p
	}
	for step, gateway := range want {
		p, ok := profiles[step]
		switch {
		case !ok:
			t.Errorf("step %s has no profile", step)
		case !slices.Equal(p.Tools.Gateway, gateway):
			t.Errorf("step %s: tools.gateway = %q, want %q", step, p.Tools.Gateway, gateway)
		case !slices.Equal(p.Tools.Builtin, prompts.BuiltinTools):
			t.Errorf("step %s: tools.builtin = %q, want every built-in tool", step, p.Tools.Builtin)
		}
	}
}

// noPromptCalls are the model calls that send no text of ours: the
// function that makes the call, which call, and why.
var noPromptCalls = func() []struct{ Call, What, Why string } {
	const consoleTool = "a tool result for the project agent, which the console shows inside the tool block of the chat"
	const sessionTool = "a tool result for the session that called the tool, which a human sees only inside that call's tool block"
	calls := []struct{ Call, What, Why string }{
		{"console.(*turn).lookImage", "the look at an image the human attached", consoleTool},
		{"console.(*turn).summarizeAttachment", "the digest of an attachment", consoleTool},
		{"console.(*turn).withDigest", "the digest of a read", consoleTool},
		{"factoryd.(*Runner).DigestHandler", "the digest tool's summary", sessionTool},
		{"factoryd.(*Runner).ImageLookHandler", "the analyze_image tool's look", sessionTool},
		{"gateway.(*Server).digest", "the digest tool's call", sessionTool},
		{"gateway.(*Server).analyzeImage", "the analyze_image tool's call", sessionTool},
		{"gateway.(*Server).replicate", "the replicate_predict tool's run of a hosted model", sessionTool},
		{"gateway.(*Server).fal", "the fal_run tool's run of a hosted model", sessionTool},
	}
	for _, capture := range []string{"factoryd.agentDocAudit", "factoryd.agentDocBehaviours", "factoryd.agentDocDelta",
		"factoryd.agentDocGateSupervisor", "factoryd.agentDocJudgement", "factoryd.agentDocReview"} {
		calls = append(calls, struct{ Call, What, Why string }{capture, "the agent map's capture",
			"a capture of a step against a fake gateway, whose answer no one reads"})
	}
	return calls
}()

// Every model call is a step's, whose prompt its profile builds, or one
// that sends no text of ours; no list names a function the walk does not
// reach.
func TestStepsHoldEveryModelCall(t *testing.T) {
	sites, err := agentdoc.ModelCalls("../..")
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, r := range prompts.Registered() {
		for _, c := range r.Calls {
			held[c] = true
			if _, ok := sites[c]; !ok {
				t.Errorf("step %s names %s, which makes no model call", r.Step, c)
			}
		}
	}
	for _, a := range noPromptCalls {
		held[a.Call] = true
		if _, ok := sites[a.Call]; !ok {
			t.Errorf("noPromptCalls holds %s for %s, which makes no model call (its reason: %s)", a.Call, a.What, a.Why)
		}
	}
	for _, c := range slices.Sorted(maps.Keys(sites)) {
		if !held[c] {
			t.Errorf("%s makes a model call at %s: register its step with prompts.RegisterStep and write its profile, add it to noPromptCalls with why it sends no text of ours, "+
				"or to agentdoc's carriers when it hands its caller's prompt on", c, sites[c].Pos)
		}
	}
}

// A step whose text a human reads carries the guide or its forms, each
// one its profile lists; a step another model or code reads carries no
// form.
func TestEveryStepCarriesItsForms(t *testing.T) {
	fixtures := map[string]any{}
	for _, r := range prompts.Registered() {
		fixtures[r.Step] = r.Fixture
	}
	for _, p := range prompts.Profiles() {
		built, err := prompts.Build(p.Step, fixtures[p.Step])
		if err != nil {
			t.Errorf("step %s: %v", p.Step, err)
			continue
		}
		text := built.Text()
		if p.Guide && !strings.Contains(text, strings.TrimSpace(writingguide.Guide)) {
			t.Errorf("step %s lacks the guide", p.Step)
		}
		for _, name := range writingguide.FormNames {
			form, _ := writingguide.Form(name)
			if want := p.Reader == prompts.ReaderHuman && slices.Contains(p.Forms, name); strings.Contains(text, form) != want {
				t.Errorf("step %s: carries the %s form %v, want %v", p.Step, name, !want, want)
			}
		}
	}
}

// Every profile is a registered step and every registered step has a
// profile.
func TestEveryProfileHasAStep(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range prompts.Registered() {
		registered[r.Step] = true
	}
	profiles := map[string]bool{}
	for _, p := range prompts.Profiles() {
		profiles[p.Step] = true
		if !registered[p.Step] {
			t.Errorf("profile %s has no registered step: call prompts.RegisterStep beside its call", p.Step)
		}
	}
	for step := range registered {
		if !profiles[step] {
			t.Errorf("step %s has no profile: write internal/prompts/profiles/%s.yaml", step, step)
		}
	}
}
