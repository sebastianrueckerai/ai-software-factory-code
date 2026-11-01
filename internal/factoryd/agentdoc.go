package factoryd

// The agent context map's capture drivers for factoryd's direct model
// clients. Each agent runs the real client against the fake gateway with
// fixture inputs built by the real unexported builders, and the spans it
// returns name what each builder contributed to the request that crossed
// the wire. Fixtures are data standing in for live cards, never live data.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// agentDocTask is the task id every capture bills under; the fake gateway
// records it with the purpose header.
const agentDocTask = "agentdoc-fixture"

// agentDocClasses are the capability classes captures resolve aliases for:
// the coder class is the one that names the factoryd purposes' aliases in
// the policy.
var agentDocClasses = []string{"coder"}

// AgentDocDirectIDs lists the direct factoryd agents the map captures.
func AgentDocDirectIDs() []string {
	return []string{"checkin", "model-review", "security-audit", "gate-supervisor",
		"review-criteria", "review-behaviours", "delta-planner", "holdout-proposer",
		"holdout-redrafter", "answer-screener", "brief-writer", "title"}
}

// AgentDocDirect drives agent id's first model call through the real
// client against the gateway at gwURL and returns the spans the map
// locates in it. hc nil means http.DefaultClient.
func AgentDocDirect(ctx context.Context, id, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	switch id {
	case "checkin":
		return agentDocCheckin(ctx, gwURL, v, hc)
	case "model-review":
		return agentDocReview(ctx, gwURL, v, hc)
	case "security-audit":
		return agentDocAudit(ctx, gwURL, v, hc)
	case "gate-supervisor":
		return agentDocGateSupervisor(ctx, gwURL, v, hc)
	case "review-criteria":
		return agentDocCriterion(ctx, gwURL, v, hc)
	case "review-behaviours":
		return agentDocBehaviours(ctx, gwURL, v, hc)
	case "delta-planner":
		return agentDocDelta(ctx, gwURL, v, hc)
	case "holdout-proposer":
		return agentDocHoldout(ctx, gwURL, v, hc, false)
	case "holdout-redrafter":
		return agentDocHoldout(ctx, gwURL, v, hc, true)
	case "answer-screener":
		return agentDocAnswer(ctx, gwURL, v, hc)
	case "brief-writer":
		return agentDocBrief(ctx, gwURL, v, hc)
	case "title":
		return agentDocTitle(ctx, gwURL, hc)
	}
	return nil, fmt.Errorf("agentdoc: unknown factoryd agent %q", id)
}

// agentDocModel is the supervisor client as production wires it
// (cmd/factoryd supervisorModel): the same budget, so max_tokens on the
// page is the real one.
func agentDocModel(gwURL string, v policy.Version, purpose string, hc *http.Client) (*SupervisorModel, string, error) {
	alias := v.Document.FactoryModel(agentDocClasses, purpose)
	if alias == "" {
		return nil, "", fmt.Errorf("agentdoc: the policy names no %s model for class %v", purpose, agentDocClasses)
	}
	return &SupervisorModel{GatewayURL: gwURL, Token: "agentdoc",
		Budget: TokenBudget{AnswerTokens: 2048, HeadroomTokens: 16384}, HTTP: hc}, alias, nil
}

// agentDocJSON is a string as it sits inside a marshalled JSON input:
// string spans locate in the decoded user content, where every embedded
// string appears in this escaped form.
func agentDocJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return string(b)
}

// agentDocFiles is the fixture diff one file wide, the review and audit
// captures both read.
func agentDocFiles() []fileDiff {
	return []fileDiff{{
		Path: "internal/foo/bar.go",
		Text: "diff --git a/internal/foo/bar.go b/internal/foo/bar.go\n" +
			"--- a/internal/foo/bar.go\n+++ b/internal/foo/bar.go\n" +
			"@@ -1,3 +1,4 @@\n package foo\n \n-func Old() int { return 0 }\n+func New() int { return 1 }\n",
		Added: 1, Removed: 1,
	}}
}

// agentDocObjective is the fixture task's objective as the brief builder
// writes it.
func agentDocObjective() harness.TaskBrief {
	return InboundBrief(harness.TaskBrief{
		Objective:      briefObjective("Fix the counter so it counts the skipped rows too.", false),
		Repository:     "owner/name",
		SHA:            "fixture-sha",
		Author:         "fixture-author",
		AuthorStanding: harness.StandingOutside,
	})
}

// agentDocCheckin drives the real runCheckin loop at the session moment:
// the first call's statement, persona and tools are the loop's own
// assembly, and the fixture answer closes it after one call.
func agentDocCheckin(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	alias := v.Document.FactoryModel(agentDocClasses, policy.PurposeCheckin)
	if alias == "" {
		return nil, fmt.Errorf("agentdoc: the policy names no %s model for class %v", policy.PurposeCheckin, agentDocClasses)
	}
	ask := &CheckInAsk{
		Moment:        CheckInSession,
		ParentSession: "s-fixture-01",
		Trip:          &TripReport{Detector: TripSize, Detail: "the card's remaining work against its bounds"},
		Messages:      []string{"Wrap the change up; the session's budget is thin."},
	}
	parent := []byte("2026-10-31T12:00:00Z agent message: the fix is half done\n")
	const callsLeft, eurLeft, handoverCap = 3, 1.0, 800
	spans := []agentdoc.Span{
		{Name: "session persona", Symbol: "factoryd.checkinSystemFor", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem,
			Text: checkinSystemFor(ask.Moment, ask.Replan)},
		{Name: "the statement", Symbol: "factoryd.checkinStatement", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: checkinStatement(ask, parent, callsLeft, eurLeft, handoverCap)},
		{Name: "the writing guide", Symbol: "writingguide.Guide", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: writingguide.Guide},
		{Name: "the parent session's tail", Symbol: "factoryd.agentDocCheckin", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser,
			Text: string(parent)},
	}
	for _, tool := range checkinToolsFor(ask.Moment) {
		raw, err := json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		spans = append(spans, agentdoc.Span{Name: "tool " + tool.Name, Symbol: "factoryd.checkinToolsFor", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceTools, Text: string(raw)})
	}
	(&Runner{}).runCheckin(ctx, agentDocTask, alias, ask, parent,
		&checkinGateway{URL: gwURL, Token: "agentdoc", HTTP: hc}, agentDocPodTools{}, callsLeft, eurLeft, 1, handoverCap)
	return spans, nil
}

// agentDocPodTools answers nothing: the capture's answer parses, so the
// loop never runs a tool. A call here means the fixture answer rotted.
type agentDocPodTools struct{}

func (agentDocPodTools) Tool(context.Context, string, map[string]any) (string, bool, error) {
	return "the capture runs no tools", false, nil
}

// agentDocReview drives the model review's call on a one-part diff.
func agentDocReview(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	gates := GateResults{
		Policy:       PolicyGate{Class: "small", Outcome: "auto", Paths: []string{"internal/foo/bar.go"}, Changed: 1, Lines: 2, MergeRule: "auto"},
		Reproduction: ReproResult{Ran: true, Command: "go test ./internal/foo/ -run TestCounter", Exit: 0, BaseExit: 1, Output: "ok"},
		ChecksPassed: true,
	}
	files := agentDocFiles()
	diff := joinFiles(diffParts(files, reviewDiffMax)[0])
	in := reviewInput{
		Brief: agentDocObjective(),
		PR:    4711, Commit: "fixture-head-sha",
		Diff:        diff,
		Concurrency: concurrencyBlock(concurrencySites(files)),
		Gates:       reviewGates{Policy: reviewPolicyOf(gates.Policy), Reproduction: gates.Reproduction},
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout,
		reviewPromptFor(gates), in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the review prompt", Symbol: "factoryd.reviewPromptFor", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem,
			Text: reviewPromptFor(gates)},
		{Name: "the review's facts", Symbol: "factoryd.reviewInput", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(in)},
		{Name: "the task brief", Symbol: "factoryd.InboundBrief", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(in.Brief)},
		{Name: "the diff", Symbol: "factoryd.joinFiles", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocJSON(diff)},
		{Name: "the concurrency summary", Symbol: "factoryd.concurrencyBlock", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: agentDocJSON(concurrencyBlock(concurrencySites(files)))},
		{Name: "the gates' results", Symbol: "factoryd.reviewGates", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(in.Gates)},
	}, nil
}

// agentDocAudit drives the security audit's call on the same one-part
// diff.
func agentDocAudit(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeAudit, hc)
	if err != nil {
		return nil, err
	}
	files := agentDocFiles()
	diff := joinFiles(diffParts(files, reviewDiffMax)[0])
	in := auditInput{
		Repository: "owner/name", PR: 4711, Commit: "fixture-head-sha", Diff: diff,
		Concurrency: concurrencyBlock(concurrencySites(files)),
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeAudit, v.Document.Limits.SupervisorTimeout, auditPrompt, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the audit prompt", Symbol: "factoryd.auditPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: auditPrompt},
		{Name: "the audit's facts", Symbol: "factoryd.auditInput", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(in)},
		{Name: "the diff", Symbol: "factoryd.joinFiles", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocJSON(diff)},
		{Name: "the concurrency summary", Symbol: "factoryd.concurrencyBlock", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: agentDocJSON(concurrencyBlock(concurrencySites(files)))},
	}, nil
}

// agentDocGateSupervisor drives the gate supervisor's call on a scrubbed
// fixture bundle.
func agentDocGateSupervisor(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeGateSupervisor, hc)
	if err != nil {
		return nil, err
	}
	bundle := gateBundle{
		Activity: "check", Attempt: 1, Bound: "10m", Elapsed: "12m",
		Objective: "Fix the counter so it counts the skipped rows too.",
		Input:     `{"checks": ["build", "vet", "race"]}`,
		Timeline:  json.RawMessage(`[{"step": "session", "summary": "the fix landed"}, {"step": "check", "summary": "waiting on CI"}]`),
		LastError: "fixture last error",
		State:     map[string]any{"check runs": []string{"build: running"}},
		Allowed:   []SupervisorAction{ActionWait, ActionCancel, ActionEscalate},
	}
	user := scrubbed(bundle, redact.New())
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeGateSupervisor, v.Document.Limits.SupervisorTimeout,
		gateSupervisorPrompt, user); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the gate supervisor prompt", Symbol: "factoryd.gateSupervisorPrompt", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceSystem, Text: gateSupervisorPrompt},
		{Name: "the diagnosis bundle", Symbol: "factoryd.scrubbed", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(user)},
	}, nil
}

// agentDocPlan is the fixture plan the criteria, behaviours, delta and
// hold-out captures share.
func agentDocPlan() Plan {
	return Plan{
		Spec:    &Spec{Outcome: "The counter counts every row the query returns, skipped ones included."},
		Summary: "One card fixes the counter; one criterion proves it.",
		Criteria: []PlanCriterion{{
			ID: "c1", Kind: "test", Test: "go test ./internal/foo/ -run TestCounter",
			Inputs: "the fixture table with one skipped row", Expected: "the count includes the skipped row",
		}},
		Cards: []PlanCard{{
			Key: "f1", Role: RoleFeature, Title: "Fix the counter", Brief: "Count skipped rows.",
			Directories: []string{"internal/foo"}, Size: "small",
		}},
	}
}

// agentDocCriterion drives the review's criterion call on the fixture
// plan's one criterion.
func agentDocCriterion(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	p := agentDocPlan()
	files := agentDocFiles()
	diff := joinFiles(files)
	in := criterionInput{Objective: agentDocObjective().Objective, Criterion: p.Criteria[0], Cards: []string{"f1"}, Diff: diff}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout,
		reviewCriterionPrompt, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the criterion prompt", Symbol: "factoryd.reviewCriterionPrompt", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceSystem, Text: reviewCriterionPrompt},
		{Name: "the criterion call's facts", Symbol: "factoryd.criterionInput", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: marshalled(in)},
		{Name: "the objective", Symbol: "factoryd.InboundBrief", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: agentDocJSON(in.Objective)},
		{Name: "the criterion", Symbol: "factoryd.PlanCriterion", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(p.Criteria[0])},
		{Name: "the diff", Symbol: "factoryd.joinFiles", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocJSON(diff)},
	}, nil
}

// agentDocBehaviours drives the review's behaviours call.
func agentDocBehaviours(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	p := agentDocPlan()
	groups := [][]fileDiff{agentDocFiles()}
	in := behavioursInput{
		Objective: agentDocObjective().Objective, Criteria: p.Criteria,
		Cards: []behaviourCard{{Key: "f1", Title: "Fix the counter", Brief: "Count skipped rows."}},
		Files: fileList(groups, "card"), Names: newNameIndex(groups).All(),
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout,
		reviewBehavioursPrompt, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the behaviours prompt", Symbol: "factoryd.reviewBehavioursPrompt", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceSystem, Text: reviewBehavioursPrompt},
		{Name: "the behaviours call's facts", Symbol: "factoryd.behavioursInput", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: marshalled(in)},
		{Name: "the objective", Symbol: "factoryd.InboundBrief", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: agentDocJSON(in.Objective)},
		{Name: "the criteria", Symbol: "factoryd.PlanCriterion", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(p.Criteria)},
		{Name: "the files list", Symbol: "factoryd.fileList", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: agentDocJSON(in.Files)},
	}, nil
}

// agentDocDelta drives the delta round's planning call.
func agentDocDelta(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	p := agentDocPlan()
	in := deltaInput{
		Spec: p.Spec, Summary: p.Summary,
		Criteria: []deltaCriterion{{PlanCriterion: p.Criteria[0], Evidence: "the diff shows no test of the skipped row"}},
		Cards:    p.Cards,
	}
	system := deltaPrompt(projectPins(v, "owner/name"))
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout, system, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the delta prompt", Symbol: "factoryd.deltaPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system},
		{Name: "the delta call's facts", Symbol: "factoryd.deltaInput", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(in)},
		{Name: "the spec", Symbol: "factoryd.Spec", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(p.Spec)},
	}, nil
}

// agentDocHoldout drives the hold-out round's proposer or redrafter.
func agentDocHoldout(ctx context.Context, gwURL string, v policy.Version, hc *http.Client, fix bool) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	p := agentDocPlan()
	var system string
	var input any
	var spans []agentdoc.Span
	if fix {
		system = holdoutFixSystem
		type failed struct {
			ID       string `json:"id"`
			TestName string `json:"test_name"`
			TestCode string `json:"test_code"`
			Output   string `json:"output"`
		}
		input = struct {
			Spec   *Spec    `json:"spec"`
			Failed []failed `json:"failed"`
		}{Spec: p.Spec, Failed: []failed{{
			ID: "h1", TestName: "TestCounterSkips", TestCode: "func TestCounterSkips(t *testing.T) {}",
			Output: "fixture dry-run output: the test did not compile",
		}}}
		spans = append(spans,
			agentdoc.Span{Name: "the redrafter prompt", Symbol: "factoryd.holdoutFixSystem", Kind: agentdoc.KindBuilder,
				Place: agentdoc.PlaceSystem, Text: holdoutFixSystem})
	} else {
		input = struct {
			Spec         *Spec           `json:"spec"`
			Criteria     []PlanCriterion `json:"criteria"`
			PastFailures []string        `json:"past_failures"`
			Max          int             `json:"max"`
		}{Spec: p.Spec, Criteria: p.Criteria, PastFailures: []string{"a past fixture failure"}, Max: maxHoldouts}
		system = holdoutSystem
		spans = append(spans,
			agentdoc.Span{Name: "the proposer prompt", Symbol: "factoryd.holdoutSystem", Kind: agentdoc.KindBuilder,
				Place: agentdoc.PlaceSystem, Text: holdoutSystem})
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout, system, input); err != nil {
		return nil, err
	}
	cover := agentdoc.Span{Name: "the hold-out call's facts", Symbol: "factoryd.(*Runner).redraftHoldouts",
		Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(input)}
	if !fix {
		cover.Name, cover.Symbol = "the hold-out call's facts", "factoryd.(*Runner).proposeHoldouts"
	}
	return append(spans, cover, agentdoc.Span{Name: "the spec", Symbol: "factoryd.Spec", Kind: agentdoc.KindBuilder,
		Place: agentdoc.PlaceUser, Text: marshalled(p.Spec)}), nil
}

// agentDocAnswerTag is the marker tag this capture scripts, so the page
// regenerates byte for byte.
const agentDocAnswerTag = "answerdoc1"

// agentDocAnswer drives the answer screening's call, with the markers the
// capture scripts.
func agentDocAnswer(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	text := sanitize.Inbound("The counter skips rows when the batch is empty; the log shows count=0 for batch 7.")
	open, closing := "===ANSWER-"+agentDocAnswerTag, "ANSWER-"+agentDocAnswerTag+"==="
	system := fmt.Sprintf(answerPrompt, open, closing)
	in := answerInput{Repository: "owner/name", Answer: open + "\n" + text + "\n" + closing}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout, system, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the screening prompt", Symbol: "factoryd.answerPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system},
		{Name: "the screening call's facts", Symbol: "factoryd.answerInput", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: marshalled(in)},
		{Name: "the answer between markers", Symbol: "sanitize.Inbound", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: agentDocJSON(in.Answer)},
	}, nil
}

// agentDocBrief drives the needs-you card's brief writer on a merge
// approval.
func agentDocBrief(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeBrief, hc)
	if err != nil {
		return nil, err
	}
	spec, err := json.Marshal(MergeSpec{
		RiskClass: "small", Review: Review{Verdict: "changes_requested", Summary: "fixture review summary"},
		VerdictSummary: "changes requested once", WhyHuman: []string{"the merge rule asks a human below auto"},
		Recommendation: "approve: the gates are green", HeadSHA: "fixture-head-sha", PRURL: "https://forgejo.example/owner/name/pull/4711",
	})
	if err != nil {
		return nil, err
	}
	a := ApprovalRequest{
		Kind: ApprovalMerge, Project: "owner/name", PRNumber: 4711, Spec: spec,
		Reason: "the merge rule asks a human", CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	t := Task{ID: agentDocTask, Objective: "Fix the counter so it counts the skipped rows too.",
		Repository: "owner/name", IssueNumber: 42, Author: "fixture-author"}
	facts, acts, err := briefFacts(a, t, nil)
	if err != nil {
		return nil, err
	}
	system := briefPrompt(a.Kind, acts)
	if _, _, err := m.Chat(ctx, t.ID, alias, policy.PurposeBrief, v.Document.Limits.SupervisorTimeout, system, facts); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the brief prompt", Symbol: "factoryd.briefPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system},
		{Name: "the card's facts", Symbol: "factoryd.briefFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(facts)},
	}, nil
}

// agentDocTitle drives the naming call (roadmap 304): the cheap alias
// reads a chat thread's first message and answers the title alone. The
// console fires it for threads the human just opened.
func agentDocTitle(ctx context.Context, gwURL string, hc *http.Client) ([]agentdoc.Span, error) {
	m := &OperatorModel{GatewayURL: gwURL, Token: "agentdoc", HTTP: hc}
	thread := "owner/name:fixture-user:1"
	first := "Set the staging budget to 40 euros."
	if _, err := m.Title(ctx, agentDocTask, thread, first); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the naming rules", Symbol: "factoryd.titlePrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: titlePrompt},
		{Name: "the thread's first message", Symbol: "(*OperatorModel).Title", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser, Text: first},
	}, nil
}

// marshalled is a value as it sits inside a marshalled JSON input: struct
// spans locate in the decoded user content byte for byte.
func marshalled(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
