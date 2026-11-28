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
	"model-review", "model-review-part", "security-audit", "security-audit-part",
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

// sessionProfiles are the profiles of the seven session steps, by step.
func sessionProfiles(t *testing.T) map[string]prompts.Profile {
	t.Helper()
	out := map[string]prompts.Profile{}
	for _, p := range prompts.Profiles() {
		if len(p.Tools.Builtin) > 0 {
			out[p.Step] = p
		}
	}
	if want := []string{"coding", "coding-card", "planner-cards", "planner-criteria", "planner-replan", "planner-revert", "planner-spec", "prover", "repo-map", "screening", "sizing"}; !slices.Equal(slices.Sorted(maps.Keys(out)), want) {
		t.Fatalf("the session steps are %q, want %q", slices.Sorted(maps.Keys(out)), want)
	}
	return out
}

// Each session step keeps the built-in tools its job uses: the coder
// all six, the prover no todo list, the planner no edit, and screening,
// sizing and the map step only the three that read.
func TestSessionToolSets(t *testing.T) {
	want := map[string][]string{
		"coding":       {"bash", "edit", "glob", "grep", "read", "todowrite"},
		"coding-card":  {"bash", "edit", "glob", "grep", "read", "todowrite"},
		"prover":       {"bash", "edit", "glob", "grep", "read"},
		"planner-spec": {"bash", "glob", "grep", "read"}, "planner-criteria": {"bash", "glob", "grep", "read"}, "planner-cards": {"bash", "glob", "grep", "read"},
		"planner-replan": {"bash", "glob", "grep", "read"}, "planner-revert": {"bash", "glob", "grep", "read"},
		"repo-map":  {"glob", "grep", "read"},
		"sizing":    {"glob", "grep", "read"},
		"screening": {"glob", "grep", "read"},
	}
	for step, p := range sessionProfiles(t) {
		if got := slices.Sorted(slices.Values(p.Tools.Builtin)); !slices.Equal(got, want[step]) {
			t.Errorf("step %s: tools.builtin = %q, want %q", step, got, want[step])
		}
	}
}

// The steps that only read cannot write: screening, sizing, the planner and
// the map step have no edit, and screening, sizing and the map step, which
// read text an outsider wrote, no shell.
func TestReadOnlyStepsCannotWrite(t *testing.T) {
	profiles := sessionProfiles(t)
	for _, step := range []string{"screening", "sizing", "planner-spec", "planner-criteria", "planner-cards", "planner-replan", "planner-revert", "repo-map"} {
		if slices.Contains(profiles[step].Tools.Builtin, "edit") {
			t.Errorf("step %s keeps edit", step)
		}
	}
	for _, step := range []string{"screening", "sizing", "repo-map"} {
		if slices.Contains(profiles[step].Tools.Builtin, "bash") {
			t.Errorf("step %s keeps bash", step)
		}
	}
}

// stepClass is the policy class each session step runs as.
var stepClass = map[string]string{
	"coding": "coder", "coding-card": "coder", "prover": policy.ClassProver,
	"planner-spec": "planner", "planner-criteria": "planner", "planner-cards": "planner", "planner-replan": "planner", "planner-revert": "planner",
	"repo-map": "planner", "screening": "analyzer", "sizing": "analyzer",
}

// shippedClassTools are the gateway tools of the session classes in the
// infra repository's factory/policy.yaml.
var shippedClassTools = map[string][]string{
	"coder": {"analyze_image", "cloud_prices", "commission_server", "delegate", "deployment_health", "digest", "escalate", "fal_run", "list_resources",
		"manual", "release_resource", "replicate_predict", "request_cut", "request_info", "request_tool", "resource_request_status", "ssh_certificate",
		"web_scrape", "web_search"},
	"analyzer":         {"analyze_image", "deployment_health", "digest", "escalate", "file_screening", "file_size", "manual", "request_info", "web_scrape", "web_search"},
	"planner":          {"analyze_image", "digest", "escalate", "file_cards", "file_criteria", "file_map", "file_replan", "file_revert", "file_spec", "manual", "web_scrape", "web_search"},
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
// at an image, their filing and the manual.
func TestSessionGatewaySets(t *testing.T) {
	coder := []string{"analyze_image", "delegate", "deployment_health", "digest", "escalate", "manual", "request_cut", "request_info", "request_tool", "web_scrape", "web_search"}
	want := map[string][]string{
		"coding": coder, "coding-card": coder,
		"planner-spec":     {"analyze_image", "digest", "file_spec", "manual", "web_scrape", "web_search"},
		"planner-criteria": {"analyze_image", "digest", "file_criteria", "manual", "web_scrape", "web_search"},
		"planner-cards":    {"analyze_image", "digest", "file_cards", "manual", "web_scrape", "web_search"},
		"planner-replan":   {"analyze_image", "digest", "file_replan", "manual", "web_scrape", "web_search"},
		"planner-revert":   {"analyze_image", "digest", "file_revert", "manual", "web_scrape", "web_search"},
		"prover":           {"file_proofs", "manual"},
		"repo-map":         {"file_map", "manual"},
		"sizing":           {"analyze_image", "file_size", "manual"},
		"screening":        {"analyze_image", "file_screening", "manual"},
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
	for _, capture := range []string{"factoryd.agentDocAudit", "factoryd.agentDocDelta",
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

// built is step's prompt on its registered fixture.
func built(t *testing.T, step string) prompts.Prompt {
	t.Helper()
	for _, r := range prompts.Registered() {
		if r.Step == step {
			p, err := prompts.Build(step, r.Fixture)
			if err != nil {
				t.Fatalf("step %s: %v", step, err)
			}
			return p
		}
	}
	t.Fatalf("step %s is not registered", step)
	var none prompts.Prompt
	return none
}

// lacks fails for each of banned that step's prompt holds.
func lacks(t *testing.T, step string, banned ...string) {
	t.Helper()
	text := built(t, step).Text()
	for _, b := range banned {
		if strings.Contains(text, b) {
			t.Errorf("step %s holds %q:\n%s", step, b, text)
		}
	}
}

// The case brief writer writes two lines a human reads on the card: the
// line form, not the ask form.
func TestCaseBriefWriterCarriesTheLineForm(t *testing.T) {
	if text := built(t, "case-brief-writer").Text(); !strings.Contains(text, "This text is one short line or one short paragraph that a human reads on a card.") {
		t.Errorf("the case brief writer lacks the line form:\n%s", text)
	}
	lacks(t, "case-brief-writer", "The story has three short lines:")
}

// The delta planner's prompt spells no bound of the brief and no rule on
// the keys: AdmitBrief and readPlan refuse them.
func TestDeltaPromptHasNoBriefLines(t *testing.T) {
	lacks(t, "delta-planner", "under 70 characters", "names only keys the plan holds")
}

// The thread title's bounds are cleanTitle's: its length and its quotes.
func TestTitleCarriesNoBounds(t *testing.T) {
	lacks(t, "title", "60 characters", "surrounding quotes")
}

// The plan name's bound is normalizePlanName's.
func TestPlanNamerCarriesNoBounds(t *testing.T) {
	lacks(t, "plan-namer", "24 characters")
}

const (
	lineFormSentence = "This text is one short line or one short paragraph that a human reads on a card."
	findingForm      = "A finding names one fault"
)

// The audit's findings are lines a human reads before the merge: the line
// form, not the finding form.
func TestAuditCarriesTheLineForm(t *testing.T) {
	for _, step := range []string{"security-audit", "security-audit-part"} {
		if text := built(t, step).Text(); !strings.Contains(text, lineFormSentence) {
			t.Errorf("step %s lacks the line form:\n%s", step, text)
		}
		lacks(t, step, findingForm)
	}
}

func TestGateSupervisorCarriesTheLineForm(t *testing.T) {
	if text := built(t, "gate-supervisor").Text(); !strings.Contains(text, lineFormSentence) {
		t.Errorf("the gate supervisor lacks the line form:\n%s", text)
	}
	lacks(t, "gate-supervisor", findingForm)
}

// Only the part call's own text speaks of a part of the diff.
func TestAuditPromptHasNoPartParagraph(t *testing.T) {
	lacks(t, "security-audit", "When part is present", "each later part")
}

// The researcher's tools ride in the call's tool list, not in its text.
func TestResearchNamesNoTools(t *testing.T) {
	lacks(t, "research", "read_doc", "web_search", "web_scrape", "grep")
}

// The parser cuts a list past its maximum and refuses a claim that holds a
// question: the prompt states neither.
func TestHoldoutCarriesNoBoundLines(t *testing.T) {
	lacks(t, "holdout-proposer", "Propose at most", "no question")
}

func TestModelReviewKeepsTheFindingForm(t *testing.T) {
	for _, step := range []string{"model-review", "model-review-part"} {
		if text := built(t, step).Text(); !strings.Contains(text, findingForm) {
			t.Errorf("step %s lacks the finding form:\n%s", step, text)
		}
		lacks(t, step, lineFormSentence)
	}
}

// The standing text holds the first sentence of each of the project
// agent's six forms.
func TestProjectAgentKeepsItsForms(t *testing.T) {
	standing := built(t, "project-agent").Standing()
	for _, name := range []string{"chat", "title", "why", "summary", "brief", "ask"} {
		form, ok := writingguide.Form(name)
		if !ok {
			t.Fatalf("form %s is missing", name)
		}
		first, _, _ := strings.Cut(strings.TrimSpace(form), ". ")
		if !strings.Contains(standing, first) {
			t.Errorf("the standing text lacks the first sentence of the %s form: %q", name, first)
		}
	}
}

func TestProjectAgentHasNoOwnSentences(t *testing.T) {
	lacks(t, "project-agent", "You run no code and change no file", "Answer briefly.", "fix that and file again")
}

// Standing words, counted from the files: guide 948, the six forms
// 228 chat + 71 title + 157 why + 162 summary + 358 brief + 434 ask = 1,410,
// factory 55, paragraph 102: 2,515.
func TestProjectAgentStandingWords(t *testing.T) {
	parts := built(t, "project-agent").Parts()
	got := [4]int{parts.Guide, parts.Forms, parts.Chunks, parts.Paragraph}
	if want := [4]int{948, 1410, 55, 102}; got != want {
		t.Errorf("guide, forms, chunks, paragraph = %v, want %v", got, want)
	}
	if total := got[0] + got[1] + got[2] + got[3]; total != 2515 {
		t.Errorf("standing words = %d, want 2515", total)
	}
}

// The line on starting and filing work stands only above manual.
func TestProjectAgentAutonomyLine(t *testing.T) {
	const line = "At assisted and above you start and file ready work yourself."
	for level, want := range map[string]bool{policy.AutonomyManual: false, policy.AutonomyAssisted: true, policy.AutonomyAutonomous: true} {
		p, err := prompts.Build("project-agent", agentFacts{Project: "app", Human: "sebastian", Level: level, Today: "2026-11-27", Now: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(p.Facts(), line); got != want {
			t.Errorf("level %s: line present = %v, want %v", level, got, want)
		}
	}
}

// A screening reads an issue and edits nothing: opencode loads no project
// config, so no AGENTS.md.
func TestScreeningLoadsNoProjectConfig(t *testing.T) {
	env, err := prompts.SessionEnv("screening")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Errorf("the screening's environment is %q, want OPENCODE_DISABLE_PROJECT_CONFIG=1", env)
	}
}

// The verdict's shape is file_screening's schema, not the prompt's.
func TestScreeningPromptHasNoShape(t *testing.T) {
	lacks(t, "screening", "clear|suspect", "Answer with one JSON object", "You are an analyzer")
}

// Standing words, counted from the files: the line form 71, factory 55 and
// narration 59 (114 chunks), paragraph 105: 290.
func TestScreeningStandingWords(t *testing.T) {
	parts := built(t, "screening").Parts()
	got := [4]int{parts.Guide, parts.Forms, parts.Chunks, parts.Paragraph}
	if want := [4]int{0, 71, 114, 105}; got != want {
		t.Errorf("guide, forms, chunks, paragraph = %v, want %v", got, want)
	}
	if total := got[0] + got[1] + got[2] + got[3]; total != 290 {
		t.Errorf("standing words = %d, want 290", total)
	}
}

// A sizing reads an issue and edits nothing: no project config, so no
// AGENTS.md, and no prompt text of the fixture's own.
func TestSizingLoadsNoProjectConfig(t *testing.T) {
	env, err := prompts.SessionEnv("sizing")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Errorf("the sizing's environment is %q, want OPENCODE_DISABLE_PROJECT_CONFIG=1", env)
	}
	lacks(t, "sizing", "Rules of this repository", "You read: do not change code", "Answer with one JSON object")
}

// The line bounds are the sizing's facts, for a risk bound of 600.
func TestSizingBoundsRenderAsFacts(t *testing.T) {
	const want = "small is at most 180, medium at most 360, large anything more."
	if p := built(t, "sizing"); !strings.Contains(p.Facts(), want) || strings.Contains(p.Standing(), "at most") {
		t.Errorf("the sizing's facts lack %q or its standing names a bound:\n%s", want, p.Text())
	}
}

// Standing words, counted from the files: the line form 71, factory 55 and
// narration 59 (114 chunks), paragraph 102: 287.
func TestSizingStandingWords(t *testing.T) {
	parts := built(t, "sizing").Parts()
	got := [4]int{parts.Guide, parts.Forms, parts.Chunks, parts.Paragraph}
	if want := [4]int{0, 71, 114, 102}; got != want {
		t.Errorf("guide, forms, chunks, paragraph = %v, want %v", got, want)
	}
	if total := got[0] + got[1] + got[2] + got[3]; total != 287 {
		t.Errorf("standing words = %d, want 287", total)
	}
}

// The refusals, the filing cap, report mode and the tool's schema carry the
// filing rules: the prover's text names none.
func TestProverCarriesNoFilingRules(t *testing.T) {
	lacks(t, "prover", "three filings at most", "must fail on main", "You push nothing", "per criterion its id, path, suite")
}

// The command the gate runs is the paragraph's, whole.
func TestProverKeepsTheRunCommand(t *testing.T) {
	const run = "mkdir -p /tmp/art && CHECK_REPORT=/tmp/<id>.xml CHECK_ARTIFACTS=/tmp/art bash check test <suite> <name>"
	if p := built(t, "prover"); !strings.Contains(p.Standing(), run) {
		t.Errorf("the prover's standing text lacks %q:\n%s", run, p.Standing())
	}
}

// Standing words, counted from the files: no form, factory 55 and
// narration 59 (114 chunks), paragraph 148: 262.
func TestProverStandingWords(t *testing.T) {
	parts := built(t, "prover").Parts()
	got := [4]int{parts.Guide, parts.Forms, parts.Chunks, parts.Paragraph}
	if want := [4]int{0, 0, 114, 148}; got != want {
		t.Errorf("guide, forms, chunks, paragraph = %v, want %v", got, want)
	}
	if total := got[0] + got[1] + got[2] + got[3]; total != 262 {
		t.Errorf("standing words = %d, want 262", total)
	}
	env, err := prompts.SessionEnv("prover")
	if err != nil || !slices.Contains(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Errorf("the prover's environment is %q (%v), want OPENCODE_DISABLE_PROJECT_CONFIG=1", env, err)
	}
}

// The map step reads the repository's AGENTS.md once, so its session loads
// the project config: its environment sets nothing that disables it.
func TestRepoMapLoadsProjectConfig(t *testing.T) {
	env, err := prompts.SessionEnv("repo-map")
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 0 {
		t.Errorf("the map step's environment is %q, want none", env)
	}
	p := sessionProfiles(t)["repo-map"]
	if got := slices.Sorted(slices.Values(p.Tools.Builtin)); !slices.Equal(got, []string{"glob", "grep", "read"}) {
		t.Errorf("the map step's built-in tools = %q, want glob, grep and read", got)
	}
}

// Standing words, counted from the files: factory 55 and narration 59 (114
// chunks), paragraph 63: 177.
func TestRepoMapStandingWords(t *testing.T) {
	parts := built(t, "repo-map").Parts()
	got := [4]int{parts.Guide, parts.Forms, parts.Chunks, parts.Paragraph}
	if want := [4]int{0, 0, 114, 63}; got != want {
		t.Errorf("guide, forms, chunks, paragraph = %v, want %v", got, want)
	}
}

// Each planner step keeps the forms its reader needs: the spec the line,
// summary and ask forms and no brief, the criteria the line form alone, the
// cards the title, why, summary, brief and ask forms, the replan all six and
// the revert the line form alone.
func TestEachPlannerStepKeepsItsForms(t *testing.T) {
	want := map[string][]string{
		"planner-spec":     {"line", "summary", "ask"},
		"planner-criteria": {"line"},
		"planner-cards":    {"title", "why", "summary", "brief", "ask"},
		"planner-replan":   {"line", "title", "why", "summary", "brief", "ask"},
		"planner-revert":   {"line"},
	}
	for step, forms := range want {
		p := sessionProfiles(t)[step]
		if !slices.Equal(p.Forms, forms) {
			t.Errorf("step %s: forms %q, want %q", step, p.Forms, forms)
		}
		if p.Guide || p.AgentsMD || p.Reader != prompts.ReaderHuman {
			t.Errorf("step %s: guide %v, agents_md %v, reader %s, want a human reader, no guide and no AGENTS.md", step, p.Guide, p.AgentsMD, p.Reader)
		}
		if env, err := prompts.SessionEnv(step); err != nil || !slices.Contains(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
			t.Errorf("step %s: environment %q (%v), want the project config off", step, env, err)
		}
	}
}

// Standing words, counted from the files by hand: the form words are line
// 71, summary 162, title 71, why 157, brief 358 and ask 434; the chunks are
// 114 (factory 55, narration 59). Spec 667 + 114 + 104 = 885, criteria
// 71 + 114 + 114 = 299, cards 1,182 + 114 + 111 = 1,407, replan
// 1,253 + 114 + 106 = 1,473 and revert 71 + 114 + 62 = 247.
func TestPlannerStepsStandingWords(t *testing.T) {
	for step, want := range map[string][5]int{
		"planner-spec":     {0, 667, 114, 104, 885},
		"planner-criteria": {0, 71, 114, 114, 299},
		"planner-cards":    {0, 1182, 114, 111, 1407},
		"planner-replan":   {0, 1253, 114, 106, 1473},
		"planner-revert":   {0, 71, 114, 62, 247},
	} {
		parts := built(t, step).Parts()
		got := [5]int{parts.Guide, parts.Forms, parts.Chunks, parts.Paragraph, parts.Guide + parts.Forms + parts.Chunks + parts.Paragraph}
		if got != want {
			t.Errorf("step %s: guide, forms, chunks, paragraph, standing = %v, want %v", step, got, want)
		}
	}
}

// file_plan and the planner profile are gone: the five planner steps and
// their tools replace them.
func TestFilePlanIsGone(t *testing.T) {
	if _, ok := prompts.ProfileOf("planner"); ok {
		t.Error("the planner profile is still there")
	}
	for _, tool := range slices.Concat(policy.FreeTools, policy.PaidTools) {
		if tool == "file_plan" {
			t.Error("file_plan is still a tool")
		}
	}
	for _, p := range prompts.Profiles() {
		if slices.Contains(p.Tools.Gateway, "file_plan") {
			t.Errorf("step %s still lists file_plan", p.Step)
		}
	}
	for _, tool := range []string{"file_spec", "file_criteria", "file_cards", "file_replan", "file_revert"} {
		if !slices.Contains(policy.FreeTools, tool) {
			t.Errorf("%s is no tool", tool)
		}
	}
}
