package factoryd

// The agent context map's capture drivers for factoryd's direct model
// clients. Each agent runs the real client against the fake gateway with
// fixture inputs built by the real unexported builders, and the spans it
// returns name what each builder contributed to the request that crossed
// the wire. Fixtures are data standing in for live cards, never live data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
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
		"judgement", "review-behaviours", "delta-planner", "holdout-proposer",
		"answer-screener", "brief-writer", "case-brief-writer", "title"}
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
	case "judgement":
		return agentDocJudgement(ctx, gwURL, v, hc)
	case "review-behaviours":
		return agentDocBehaviours(ctx, gwURL, v, hc)
	case "delta-planner":
		return agentDocDelta(ctx, gwURL, v, hc)
	case "holdout-proposer":
		return agentDocHoldout(ctx, gwURL, v, hc)
	case "answer-screener":
		return agentDocAnswer(ctx, gwURL, v, hc)
	case "brief-writer":
		return agentDocBrief(ctx, gwURL, v, hc)
	case "case-brief-writer":
		return agentDocCaseBrief(ctx, gwURL, v, hc)
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

// agentDocInputLead is the heading every JSON-mode call's user message
// opens with.
var agentDocInputLead = agentdoc.Span{Name: "the input heading", Symbol: "factoryd.chatInputLead", Kind: agentdoc.KindBuilder,
	Place: agentdoc.PlaceUser, Text: chatInputLead}

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
	timeCap := callsLeft * policy.CheckinCallTime
	spans := []agentdoc.Span{
		{Name: "session persona", Symbol: "factoryd.checkinSystemFor", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem,
			Text: checkinSystemFor(ask.Moment, ask.Replan)},
		{Name: "the statement", Symbol: "factoryd.checkinStatement", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: checkinStatement(ask, parent, callsLeft, eurLeft, handoverCap)},
		{Name: "the answer deadline", Symbol: "factoryd.checkinDueLine", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: checkinDueLine(checkinAnswerDue(timeCap, 0))},
		{Name: "the writing guide", Symbol: "writingguide.Guide", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: writingguide.Guide},
		{Name: "the parent session's tail", Symbol: "factoryd.agentDocCheckin", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser,
			Text: string(parent)},
	}
	for _, tool := range checkinTools {
		raw, err := json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		spans = append(spans, agentdoc.Span{Name: "tool " + tool.Name, Symbol: "factoryd.checkinTools", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceTools, Text: string(raw)})
	}
	answerTool, err := json.Marshal(momentAnswerTool(ask))
	if err != nil {
		return nil, err
	}
	spans = append(spans, agentdoc.Span{Name: "tool " + checkinAnswerTool, Symbol: "factoryd.momentAnswerTool", Kind: agentdoc.KindBuilder,
		Place: agentdoc.PlaceTools, Text: string(answerTool)})
	(&Runner{}).runCheckin(ctx, agentDocTask, alias, ask, parent,
		&checkinGateway{URL: gwURL, Token: "agentdoc", HTTP: hc}, agentDocPodTools{}, callsLeft, eurLeft, 1, handoverCap, timeCap)
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
		agentDocInputLead,
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
		agentDocInputLead,
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
		agentDocInputLead,
		{Name: "the gate supervisor prompt", Symbol: "factoryd.gateSupervisorPrompt", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceSystem, Text: gateSupervisorPrompt},
		{Name: "the diagnosis bundle", Symbol: "factoryd.scrubbed", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(user)},
	}, nil
}

// agentDocPlan is the fixture plan the behaviours, delta and hold-out
// captures share.
func agentDocPlan() Plan {
	return Plan{
		Spec:    &Spec{Outcome: "The counter counts every row the query returns, skipped ones included."},
		Summary: "One card fixes the counter; one criterion proves it.",
		Criteria: []PlanCriterion{{
			ID: "c1", Kind: "test", Claim: "the counter counts a row the query skips",
			Inputs: "the fixture table with one skipped row", Expected: "a count of 1",
		}},
		Cards: []PlanCard{{
			Key: "f1", Role: RoleFeature, Title: "Fix the counter", Brief: "Count skipped rows.",
			Directories: []string{"internal/foo"}, Size: "small",
		}},
	}
}

// agentDocJudgement drives the judgement call (judgementCall) on a
// judgement criterion and the artifact its proof wrote, through callAll
// as the gate makes it.
func agentDocJudgement(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	c := PlanCriterion{ID: "reads_calm", Kind: CriterionJudgement, Claim: "The sizes page reads calm", Inputs: "the sizes page with three sizes",
		Prompt: "Does the page read calm? Answer met or unmet."}
	a := GateArtifact{Criterion: c.ID, Name: "reads_calm.html", Head: "fixture-head-sha", Content: "<main><h1>Sizes</h1><p>S, M and L</p></main>"}
	call := judgementCall(c, a)
	results, err := (&Runner{supervisor: m}).callAll(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout, time.Time{}, []reviewCall{call})
	if err != nil {
		return nil, err
	}
	if err := results[0].Err; err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		{Name: "the judgement prompt", Symbol: "factoryd.judgementCall", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: call.Prompt},
		{Name: "the criterion's prompt", Symbol: "factoryd.PlanCriterion.Prompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: c.Prompt},
		{Name: "the answer's form", Symbol: "factoryd.judgementAnswer", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: judgementAnswer},
		{Name: "the artifact", Symbol: "factoryd.GateArtifact", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(call.Input())},
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
		agentDocInputLead,
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
		agentDocInputLead,
		{Name: "the delta prompt", Symbol: "factoryd.deltaPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system},
		{Name: "the delta call's facts", Symbol: "factoryd.deltaInput", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(in)},
		{Name: "the spec", Symbol: "factoryd.Spec", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(p.Spec)},
	}, nil
}

// agentDocHoldout drives the hold-out round's proposer.
func agentDocHoldout(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	p := agentDocPlan()
	input := struct {
		Spec         *Spec           `json:"spec"`
		Criteria     []PlanCriterion `json:"criteria"`
		PastFailures []string        `json:"past_failures"`
		Max          int             `json:"max"`
	}{Spec: p.Spec, Criteria: p.Criteria, PastFailures: []string{"a past fixture failure"}, Max: maxHoldouts}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, policy.PurposeReview, v.Document.Limits.SupervisorTimeout, holdoutSystem, input); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the proposer prompt", Symbol: "factoryd.holdoutSystem", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: holdoutSystem},
		{Name: "the hold-out call's facts", Symbol: "factoryd.(*Runner).proposeHoldouts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(input)},
		agentDocInputLead,
		{Name: "the spec", Symbol: "factoryd.Spec", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(p.Spec)},
	}, nil
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
		agentDocInputLead,
		{Name: "the screening prompt", Symbol: "factoryd.answerPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system},
		{Name: "the screening call's facts", Symbol: "factoryd.answerInput", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: marshalled(in)},
		{Name: "the answer between markers", Symbol: "sanitize.Inbound", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: agentDocJSON(in.Answer)},
	}, nil
}

// agentDocBrief drives the approval brief writer on a resource request, the
// kind no case carries.
func agentDocBrief(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, alias, err := agentDocModel(gwURL, v, policy.PurposeBrief, hc)
	if err != nil {
		return nil, err
	}
	spec, err := json.Marshal(cloudres.ResourceSpec{ServerType: "cx33", Location: "fsn1", Purpose: "load test the counter",
		LeaseMinutes: 300, HourlyEUR: 0.0144, ReservationEUR: 0.072, BeyondEnvelope: []string{"a lease of 300 minutes exceeds the envelope's 4 h"}})
	if err != nil {
		return nil, err
	}
	a := ApprovalRequest{
		Kind: ApprovalResource, Project: "owner/name", Spec: spec,
		Reason: "load test (beyond the envelope)", CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	t := agentDocCard()
	life := agentDocLife().Render()
	facts, acts, err := briefFacts(a, t, nil, life)
	if err != nil {
		return nil, err
	}
	system := briefPrompt(a.Kind, acts)
	if _, _, err := m.Chat(ctx, t.ID, alias, policy.PurposeBrief, v.Document.Limits.SupervisorTimeout, system, facts); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		{Name: "the brief prompt", Symbol: "factoryd.briefPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system},
		{Name: "the request's facts", Symbol: "factoryd.briefFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(facts)},
		{Name: "the card's life", Symbol: "factoryd.CardDigest.Render", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocJSON(life)},
	}, nil
}

func agentDocCard() Task {
	return Task{ID: agentDocTask, Objective: "Fix the counter so it counts the skipped rows too.",
		Repository: "owner/name", IssueNumber: 42, Author: "fixture-author", RequiredCapabilities: agentDocClasses}
}

func agentDocLife() CardDigest {
	at := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	return CardDigest{Head: DigestHead{Issue: 42, BudgetEUR: 3, SpentEUR: 0.41, Status: "in-review", Gate: GateApproval},
		Steps: []DigestStep{
			{N: 1, At: at, Kind: "filed", Actor: "fixture-author", Outcome: "direct card", Count: 1},
			{N: 2, At: at.Add(5 * time.Minute), Offset: 5 * time.Minute, Kind: "session", Actor: "1 small", Outcome: "pushed a1b2c3d", Count: 1},
			{N: 3, At: at.Add(30 * time.Minute), Offset: 30 * time.Minute, Kind: "review", Outcome: "changes_requested", Count: 1},
		}}
}

// agentDocCaseStore serves the case writer's reads from one fixture case.
type agentDocCaseStore struct {
	BriefStore
	steps []CaseStep
	req   ApprovalRequest
}

func (s agentDocCaseStore) CaseSteps(context.Context, int64) ([]CaseStep, error) { return s.steps, nil }
func (s agentDocCaseStore) LastAnswerAt(context.Context, string) (*time.Time, error) {
	return nil, nil
}
func (s agentDocCaseStore) ApprovalRequests(context.Context, string) ([]ApprovalRequest, error) {
	return []ApprovalRequest{s.req}, nil
}

// agentDocCaseBrief drives the case brief writer on a merge case that a
// rung handed up.
func agentDocCaseBrief(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, _, err := agentDocModel(gwURL, v, policy.PurposeBrief, hc)
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
	req := ApprovalRequest{ID: 9, Kind: ApprovalMerge, TaskID: agentDocTask, Project: "owner/name", PRNumber: 4711, Spec: spec,
		Reason: "the merge rule asks a human", CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	c := Case{ID: 3, TaskID: agentDocTask, Repository: "owner/name", Kind: CaseMergeApproval, Status: CaseAtHuman,
		Problem: "Pull request #4711 waits for a merge approval", Facts: map[string]any{"approval": float64(req.ID)}}
	store := agentDocCaseStore{req: req, steps: []CaseStep{
		{Rung: 2, Actor: "factory agent", Found: "the review asked for one change and the gates are green", Decision: ActApproveMerge,
			Grounds: "the change is small and the gates are green", Outcome: StepHandedUp}}}
	w := &BriefWriter{Store: store, Model: m, Current: func() policy.Version { return v },
		CardDigest: func(context.Context, string) (CardDigest, error) { return agentDocLife(), nil }}
	t := agentDocCard()
	facts, err := w.caseFacts(ctx, c, t)
	if err != nil {
		return nil, err
	}
	// the capture's answer is no brief; the call is what the map records
	if _, _, err := w.askCase(ctx, c, t, Task{ID: t.ID}); err != nil {
		var noBrief *noBriefError
		if !errors.As(err, &noBrief) {
			return nil, err
		}
	}
	life := agentDocLife().Render()
	return []agentdoc.Span{
		agentDocInputLead,
		{Name: "the case brief prompt", Symbol: "factoryd.caseBriefPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: caseBriefPrompt},
		{Name: "the case's facts", Symbol: "factoryd.(*BriefWriter).caseFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: string(facts)},
		{Name: "the card's life", Symbol: "factoryd.CardDigest.Render", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocJSON(life)},
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
