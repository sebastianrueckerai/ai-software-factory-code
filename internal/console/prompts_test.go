package console

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// sessionProfiles are the profiles of the six session steps, by step.
func sessionProfiles(t *testing.T) map[string]prompts.Profile {
	t.Helper()
	out := map[string]prompts.Profile{}
	for _, p := range prompts.Profiles() {
		if len(p.Tools.Builtin) > 0 {
			out[p.Step] = p
		}
	}
	if want := []string{"coding", "coding-card", "planner", "prover", "screening", "sizing"}; !slices.Equal(slices.Sorted(maps.Keys(out)), want) {
		t.Fatalf("the session steps are %q, want %q", slices.Sorted(maps.Keys(out)), want)
	}
	return out
}

// Each session step keeps the built-in tools its job uses: the coder
// all six, the prover no todo list, the planner no edit, and screening
// and sizing only the three that read.
func TestSessionToolSets(t *testing.T) {
	want := map[string][]string{
		"coding":      {"bash", "edit", "glob", "grep", "read", "todowrite"},
		"coding-card": {"bash", "edit", "glob", "grep", "read", "todowrite"},
		"prover":      {"bash", "edit", "glob", "grep", "read"},
		"planner":     {"bash", "glob", "grep", "read"},
		"sizing":      {"glob", "grep", "read"},
		"screening":   {"glob", "grep", "read"},
	}
	for step, p := range sessionProfiles(t) {
		if got := slices.Sorted(slices.Values(p.Tools.Builtin)); !slices.Equal(got, want[step]) {
			t.Errorf("step %s: tools.builtin = %q, want %q", step, got, want[step])
		}
	}
}

// The steps that only read cannot write: screening, sizing and the
// planner have no edit, and screening and sizing, which read text an
// outsider wrote, no shell.
func TestReadOnlyStepsCannotWrite(t *testing.T) {
	profiles := sessionProfiles(t)
	for _, step := range []string{"screening", "sizing", "planner"} {
		if slices.Contains(profiles[step].Tools.Builtin, "edit") {
			t.Errorf("step %s keeps edit", step)
		}
	}
	for _, step := range []string{"screening", "sizing"} {
		if slices.Contains(profiles[step].Tools.Builtin, "bash") {
			t.Errorf("step %s keeps bash", step)
		}
	}
}

// stepClass is the policy class each session step runs as.
var stepClass = map[string]string{
	"coding": "coder", "coding-card": "coder", "planner": "planner", "prover": policy.ClassProver,
	"screening": "analyzer", "sizing": "analyzer",
}

// shippedClassTools are the gateway tools of the session classes in the
// infra repository's factory/policy.yaml.
var shippedClassTools = map[string][]string{
	"coder": {"analyze_image", "cloud_prices", "commission_server", "delegate", "deployment_health", "digest", "escalate", "fal_run", "list_resources",
		"manual", "release_resource", "replicate_predict", "request_cut", "request_info", "request_tool", "resource_request_status", "ssh_certificate",
		"web_scrape", "web_search"},
	"analyzer":         {"analyze_image", "deployment_health", "digest", "escalate", "manual", "request_info", "web_scrape", "web_search"},
	"planner":          {"analyze_image", "digest", "escalate", "file_plan", "manual", "web_scrape", "web_search"},
	policy.ClassProver: {"file_proofs", "manual"},
}

// shippedDocument is a policy whose session classes list the shipped tools.
func shippedDocument() policy.Document {
	d := policy.Document{Classes: map[string]policy.Class{}}
	for class, tools := range shippedClassTools {
		caps := map[string]policy.ToolCap{}
		for _, tool := range tools {
			caps[tool] = policy.ToolCap{}
		}
		d.Classes[class] = policy.Class{Tools: caps}
	}
	return d
}

// The copy above is the infra repository's: with INFRA_FACTORY_DIR set to
// its factory/ directory, each session class lists the same tools.
func TestShippedClassToolsAreTheCopy(t *testing.T) {
	dir := os.Getenv("INFRA_FACTORY_DIR")
	if dir == "" {
		t.Skip("INFRA_FACTORY_DIR unset")
	}
	files, err := policy.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range shippedClassTools {
		if got := slices.Sorted(maps.Keys(tree.Document.Classes[class].Tools)); !slices.Equal(got, want) {
			t.Errorf("class %s: the infra repository lists %q, the copy %q", class, got, want)
		}
	}
}

// Each session step is offered the gateway tools its job uses: the coder
// eleven, the planner six, the prover two, and screening and sizing a look
// at an image and the manual.
func TestSessionGatewaySets(t *testing.T) {
	coder := []string{"analyze_image", "delegate", "deployment_health", "digest", "escalate", "manual", "request_cut", "request_info", "request_tool", "web_scrape", "web_search"}
	want := map[string][]string{
		"coding": coder, "coding-card": coder,
		"planner":   {"analyze_image", "digest", "file_plan", "manual", "web_scrape", "web_search"},
		"prover":    {"file_proofs", "manual"},
		"sizing":    {"analyze_image", "manual"},
		"screening": {"analyze_image", "manual"},
	}
	for step, p := range sessionProfiles(t) {
		if got := slices.Sorted(slices.Values(p.Tools.Gateway)); !slices.Equal(got, want[step]) {
			t.Errorf("step %s: tools.gateway = %q, want %q", step, got, want[step])
		}
	}
}

// A bug-fix card rents no server and runs no hosted model: the coder's
// profiles name none of the resource tools, the hosted-model runners or
// task_costs.
func TestCodingDropsTheEightTools(t *testing.T) {
	profiles := sessionProfiles(t)
	for _, step := range []string{"coding", "coding-card"} {
		for _, tool := range []string{"cloud_prices", "commission_server", "list_resources", "release_resource", "resource_request_status",
			"ssh_certificate", "replicate_predict", "fal_run", "task_costs"} {
			if slices.Contains(profiles[step].Tools.Gateway, tool) {
				t.Errorf("step %s lists %s", step, tool)
			}
		}
	}
}

// Every gateway tool a session step names is one its shipped class lists:
// the class is the ceiling, the profile the offer.
func TestProfileToolsAreListedByAClass(t *testing.T) {
	for step, p := range sessionProfiles(t) {
		for _, tool := range p.Tools.Gateway {
			if !slices.Contains(shippedClassTools[stepClass[step]], tool) {
				t.Errorf("step %s names %s, which its class %s does not list", step, tool, stepClass[step])
			}
		}
	}
}

// The harness offers what the brief names: the gateway tools a session's
// config leaves undenied, within its shipped class, are Offered's list.
func TestOfferedMatchesTheHarness(t *testing.T) {
	d := shippedDocument()
	for step := range sessionProfiles(t) {
		allowed := d.Tools([]string{stepClass[step]}, false)
		cfg, err := prompts.SessionConfig(step)
		if err != nil {
			t.Fatalf("step %s: %v", step, err)
		}
		permission := cfg["permission"].(map[string]any)
		var undenied []string
		for _, tool := range allowed {
			if permission["factory-tools_"+tool] != "deny" {
				undenied = append(undenied, tool)
			}
		}
		if got := prompts.Offered(step, allowed); !slices.Equal(got, undenied) || len(got) == 0 {
			t.Errorf("step %s: Offered = %q, the harness leaves %q", step, got, undenied)
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

// The factory chunk goes to the steps that act across the factory's flow,
// the narration chunk to the sessions that write most.
func TestChunkCarriers(t *testing.T) {
	t.Skip("carriers land task by task")
	producers := []string{"coding", "coding-card", "prover", "repo-map", "screening", "sizing"}
	planner := []string{"planner-cards", "planner-criteria", "planner-replan", "planner-revert", "planner-spec"}
	want := map[string][]string{
		"factory": slices.Sorted(slices.Values(slices.Concat([]string{"case-checkin", "checkin-dead", "checkin-gate", "checkin-session",
			"delta-planner", "project-agent"}, producers, planner))),
		"narration": slices.Sorted(slices.Values(slices.Concat(producers, planner))),
	}
	if len(want["factory"]) != 17 || len(want["narration"]) != 11 {
		t.Fatalf("the lists hold %d and %d steps, want 17 and 11", len(want["factory"]), len(want["narration"]))
	}
	for chunk, steps := range want {
		var got []string
		for _, p := range prompts.Profiles() {
			if slices.Contains(p.Chunks, chunk) {
				got = append(got, p.Step)
			}
		}
		if !slices.Equal(got, steps) {
			t.Errorf("chunk %s is carried by %q, want %q", chunk, got, steps)
		}
	}
}
