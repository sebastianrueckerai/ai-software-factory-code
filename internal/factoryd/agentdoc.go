package factoryd

// The agent context map's capture drivers for factoryd's direct model
// clients. Each agent runs the real client against the fake gateway with
// fixture inputs built by the real unexported builders, and the spans it
// returns name what each builder contributed to the request that crossed
// the wire. Fixtures are data standing in for live cards, never live data.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
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
		"judgement", "delta-planner", "holdout-proposer",
		"answer-screener", "brief-writer", "case-brief-writer",
		"case-checkin", "plan-namer", "digest", "image-look", "follow-liveness"}
}

// AgentDocDirectAnswer is the answer the fake gateway scripts for direct
// agent id: the one answer that ends its capture on its first call.
func AgentDocDirectAnswer(id string) string {
	switch id {
	case "case-checkin":
		return agentDocRungAnswer
	case "plan-namer":
		return `{"name":"Count skipped rows"}`
	case "holdout-proposer":
		return `{"holdouts":[]}`
	}
	return `{"decision":"continue"}`
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
	case "case-checkin":
		return agentDocCaseCheckin(ctx, gwURL, v, hc)
	case "plan-namer":
		return agentDocNamer(ctx, gwURL, v, hc)
	case "digest":
		return agentDocDigest(ctx, gwURL, v, hc)
	case "image-look":
		return agentDocImageLook(ctx, gwURL, v, hc)
	case "follow-liveness":
		return agentDocFollow(ctx, gwURL, v, hc)
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
var agentDocInputLead = agentdoc.Span{Name: "the input heading", Symbol: "factoryd.ChatInputLead", Kind: agentdoc.KindBuilder,
	Place: agentdoc.PlaceUser, Text: ChatInputLead}

// agentDocStanding is the span of what p's profile joins before its
// facts: the guide, the forms, the chunks and the paragraph, and the blank
// line Build puts before the facts.
func agentDocStanding(p prompts.Prompt) agentdoc.Span {
	text := p.Standing()
	if text != "" && p.Facts() != "" {
		text += "\n\n"
	}
	return agentdoc.Span{Name: "the profile's texts", Symbol: "prompts.Build", Kind: agentdoc.KindBuilder,
		Place: agentdoc.PlaceSystem, Text: text}
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

// agentDocCardBrief is the fixture task's card brief: an outside account's
// issue, its title and its why line.
var agentDocCardBrief = CardBrief{Text: BriefText{Title: "Fix the counter so it counts the skipped rows too.",
	Why: "The nightly report undercounts the rows it skipped."}}

// agentDocObjective is the fixture task's brief as the brief builder
// writes it.
func agentDocObjective() harness.TaskBrief {
	return InboundBrief(harness.TaskBrief{
		Objective:      agentDocCardBrief.AgentText(),
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
	system, err := prompts.Build(checkinPromptStep(ask.Moment), nil)
	if err != nil {
		return nil, err
	}
	spans := []agentdoc.Span{
		agentDocStanding(system),
		{Name: "the statement", Symbol: "factoryd.checkinStatement", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: checkinStatement(ask, parent, callsLeft, eurLeft, handoverCap)},
		{Name: "the answer deadline", Symbol: "factoryd.checkinDueLine", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: checkinDueLine(checkinAnswerDue(timeCap, 0))},
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
	spans = append(spans, agentdoc.Span{Name: "tool " + CheckinAnswerTool, Symbol: "factoryd.momentAnswerTool", Kind: agentdoc.KindBuilder,
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
	system, err := prompts.Build("model-review", reviewFacts{Gates: gates})
	if err != nil {
		return nil, err
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, v.Document.Limits.SupervisorTimeout, system, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the review prompt", Symbol: "factoryd.reviewCallPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem,
			Text: system.Facts()},
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
	system, err := prompts.Build("security-audit", auditFacts{})
	if err != nil {
		return nil, err
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, v.Document.Limits.SupervisorTimeout, system, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
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
	system, err := prompts.Build("gate-supervisor", gateSupervisorPrompt)
	if err != nil {
		return nil, err
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, v.Document.Limits.SupervisorTimeout, system, user); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the gate supervisor prompt", Symbol: "factoryd.gateSupervisorPrompt", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceSystem, Text: gateSupervisorPrompt},
		{Name: "the diagnosis bundle", Symbol: "factoryd.scrubbed", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: marshalled(user)},
	}, nil
}

// agentDocPlan is the fixture plan the delta, hold-out and prover
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
	call, err := judgementCall(c, a)
	if err != nil {
		return nil, err
	}
	results, err := (&Runner{supervisor: m}).callAll(ctx, agentDocTask, alias, v.Document.Limits.SupervisorTimeout, time.Time{}, []reviewCall{call})
	if err != nil {
		return nil, err
	}
	if err := results[0].Err; err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(call.Prompt),
		{Name: "the judgement prompt", Symbol: "factoryd.judgementText", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: call.Prompt.Facts()},
		{Name: "the criterion's prompt", Symbol: "factoryd.PlanCriterion.Prompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: c.Prompt},
		{Name: "the answer's form", Symbol: "factoryd.judgementAnswer", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: judgementAnswer},
		{Name: "the artifact", Symbol: "factoryd.GateArtifact", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(call.Input())},
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
	system, err := prompts.Build("delta-planner", deltaFacts{projectPins(v, "owner/name")})
	if err != nil {
		return nil, err
	}
	if _, _, err := m.Chat(ctx, agentDocTask, alias, v.Document.Limits.SupervisorTimeout, system, in); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the delta prompt", Symbol: "factoryd.deltaPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system.Facts()},
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
	p, reverts := agentDocPlan(), []string{"a past fixture failure"}
	r := &Runner{store: agentDocCardStore{}, supervisor: m, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	if _, err := r.proposeHoldouts(ctx, agentDocCard(), 1, p, alias, reverts); err != nil {
		return nil, err
	}
	input := struct {
		Spec         *Spec           `json:"spec"`
		Criteria     []PlanCriterion `json:"criteria"`
		PastFailures []string        `json:"past_failures"`
		Max          int             `json:"max"`
	}{Spec: p.Spec, Criteria: p.Criteria, PastFailures: reverts, Max: maxHoldouts}
	system, err := prompts.Build("holdout-proposer", holdoutSystem)
	if err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocStanding(system),
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

// agentDocAnswer drives the answer screening on a late answer of the
// issue's author, with the markers the capture scripts.
func agentDocAnswer(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, _, err := agentDocModel(gwURL, v, policy.PurposeReview, hc)
	if err != nil {
		return nil, err
	}
	const answer = "The counter skips rows when the batch is empty, and the log shows count=0 for batch 7."
	r := &Runner{store: agentDocCardStore{}, supervisor: m, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	restoreTag := randomTag
	randomTag = func() string { return agentDocAnswerTag }
	_, err = r.ScreenAnswer(ctx, agentDocTask, answer, 0)
	randomTag = restoreTag
	if err != nil {
		return nil, err
	}
	text := sanitize.Inbound(answer)
	open, closing := "===ANSWER-"+agentDocAnswerTag, "ANSWER-"+agentDocAnswerTag+"==="
	system, err := prompts.Build("answer-screener", screenMarkers{open, closing})
	if err != nil {
		return nil, err
	}
	in := answerInput{Repository: agentDocCard().Repository, Answer: open + "\n" + text + "\n" + closing}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the screening prompt", Symbol: "factoryd.answerSystem", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system.Facts()},
		{Name: "the screening call's facts", Symbol: "factoryd.answerInput", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: marshalled(in)},
		{Name: "the answer between markers", Symbol: "sanitize.Inbound", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceUser, Text: agentDocJSON(in.Answer)},
	}, nil
}

// agentDocBrief drives the approval brief writer on a resource request, the
// kind no case carries.
func agentDocBrief(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, _, err := agentDocModel(gwURL, v, policy.PurposeBrief, hc)
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
	w := &BriefWriter{Model: m, Current: func() policy.Version { return v },
		CardDigest: func(context.Context, string) (CardDigest, error) { return agentDocLife(), nil }}
	// the capture's answer is no brief, and the call is what the map records
	if _, _, err := w.ask(ctx, a, t, t); err != nil {
		var noBrief *noBriefError
		if !errors.As(err, &noBrief) {
			return nil, err
		}
	}
	life := agentDocLife().Render()
	facts, err := briefFacts(a, t, life)
	if err != nil {
		return nil, err
	}
	raw, err := w.scrubbedJSON(facts)
	if err != nil {
		return nil, err
	}
	system, err := prompts.Build("brief-writer", briefKind{a.Kind})
	if err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the brief prompt", Symbol: "factoryd.briefPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: system.Facts()},
		{Name: "the request's facts", Symbol: "factoryd.briefFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: string(raw)},
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
}

func (s agentDocCaseStore) CaseSteps(context.Context, int64) ([]CaseStep, error) { return s.steps, nil }

// agentDocCaseBrief drives the case story writer on a check-in's stop that
// a rung handed up.
func agentDocCaseBrief(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, _, err := agentDocModel(gwURL, v, policy.PurposeBrief, hc)
	if err != nil {
		return nil, err
	}
	c := Case{ID: 3, TaskID: agentDocTask, Repository: "owner/name", Kind: CaseCheckinHuman, Status: CaseAtHuman,
		Problem: CaseFaces[CaseCheckinHuman].Ask(Facts{}),
		Facts:   map[string]any{"reason": "check-in: goal conflict: the change edits the importer, not the counter the card names"}}
	store := agentDocCaseStore{steps: []CaseStep{
		{Rung: 2, Actor: "factory agent", Found: "the session edited the importer, and the counter still skips rows", Decision: ActContinue,
			Grounds: "the counter is the card's work", Outcome: StepHandedUp}}}
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
	system, err := prompts.Build("case-brief-writer", caseBriefPrompt)
	if err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the case brief prompt", Symbol: "factoryd.caseBriefPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: caseBriefPrompt},
		{Name: "the case's facts", Symbol: "factoryd.(*BriefWriter).caseFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: string(facts)},
		{Name: "the card's life", Symbol: "factoryd.CardDigest.Render", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocJSON(life)},
	}, nil
}

// agentDocFinding is the fixture case's one finding on the head.
const agentDocFinding = "3f2a1c9b0e44"

// agentDocRungAnswer is the case check-in capture's scripted answer: rung
// 1 hands the fixture case up with its recommendation, an answer the
// case's facts parse, so the conversation ends on its first call.
const agentDocRungAnswer = `{"decision":"hand_up","reason_kind":"gate_stop","verdicts":{"` + agentDocFinding +
	`":{"verdict":"confirmed","evidence":"internal/foo/bar.go:4 at the head: New still returns 1"}},"note":"",` +
	`"recommendation":{"action":"successor","grounds":"the counter still skips the row the finding names"}}`

// agentDocCaseCheckin drives rung 1's conversation on a fixture case, a
// review that stopped the card on one finding, through rung1Converse, the
// assembly rungConversation runs once it has read the card.
func agentDocCaseCheckin(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	alias := v.Document.FactoryModel(agentDocClasses, policy.PurposeCheckin)
	if alias == "" {
		return nil, fmt.Errorf("agentdoc: the policy names no %s model for class %v", policy.PurposeCheckin, agentDocClasses)
	}
	c := Case{ID: 4, TaskID: agentDocTask, Repository: "owner/name", Kind: CaseGateStop, Status: CaseOpen,
		Facts: map[string]any{"gate": "review", "reason": reviewConcernsMark}}
	c.Problem = CaseFaces[CaseGateStop].Ask(Facts{Case: c})
	f := CaseFacts{Kind: CaseGateStop, Gate: "review", Attempts: 1, SessionCap: 3, AgentTimeCapS: 3600,
		BudgetEUR: 5, SpentEUR: 1, LeftEUR: 4, FloorEUR: 0.3, CeilingEUR: 7, MonthLeftEUR: 40, PROpen: true, Head: "c1", Checks: "green",
		Findings:     []string{agentDocFinding},
		FindingTexts: map[string]string{agentDocFinding: "New returns 1 where the counter needs the skipped rows counted"},
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		PoolLeft:     policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}
	r := &Runner{Scrub: redact.New(), cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	digest := DigestText(r.Scrub, agentDocLife(), nil)
	out := r.rung1Converse(ctx, &checkinGateway{URL: gwURL, Token: "agentdoc", HTTP: hc}, agentDocTask, agentDocCard(), c, f, digest, nil, agentDocPodTools{})
	if out.Answer == nil {
		return nil, fmt.Errorf("agentdoc: the fixture answer did not end rung 1's conversation: %s", out.Found)
	}
	system, err := prompts.Build("case-checkin", nil)
	if err != nil {
		return nil, err
	}
	spans := []agentdoc.Span{
		agentDocStanding(system),
		{Name: "the statement", Symbol: "factoryd.(*Runner).rungStatement", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: r.rungStatement(c, f, digest, nil, 1)},
		{Name: "the card's life", Symbol: "factoryd.CardDigest.Render", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: agentDocLife().Render()},
		{Name: "the calls left and the answer's due time", Symbol: "factoryd.dueOwnLine", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: dueOwnLine(v.Document.Limits.CheckinCallCap(), checkinAnswerDue(v.Document.Limits.CheckinTimeCap(), 0))},
	}
	for _, tool := range CaseEvidence() {
		raw, err := json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		spans = append(spans, agentdoc.Span{Name: "tool " + tool.Name, Symbol: "factoryd.CaseEvidence", Kind: agentdoc.KindBuilder,
			Place: agentdoc.PlaceTools, Text: string(raw)})
	}
	answerTool, err := json.Marshal(rungAnswerTool(f, 1))
	if err != nil {
		return nil, err
	}
	return append(spans, agentdoc.Span{Name: "tool " + CheckinAnswerTool, Symbol: "factoryd.rungAnswerTool", Kind: agentdoc.KindBuilder,
		Place: agentdoc.PlaceTools, Text: string(answerTool)}), nil
}

// agentDocNameStore serves the namer the names of the project's other open
// plans and takes the name it records.
type agentDocNameStore struct {
	NameStore
	taken []string
}

func (s agentDocNameStore) OpenPlanNames(context.Context, string, string) ([]string, error) {
	return s.taken, nil
}

func (agentDocNameStore) SetPlanName(context.Context, string, string) error { return nil }

// agentDocNamer drives the plan namer on the fixture plan as its first
// revision files, beside one other open plan of the project.
func agentDocNamer(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, _, err := agentDocModel(gwURL, v, policy.PurposeTitle, hc)
	if err != nil {
		return nil, err
	}
	p, title, taken := agentDocPlan(), "Count the skipped rows", []string{"Nightly report"}
	n := &Namer{Store: agentDocNameStore{taken: taken}, Model: m, Current: func() policy.Version { return v }}
	if n.Name(ctx, agentDocCard(), title, p.Spec.Outcome, p.Summary) == "" {
		return nil, errors.New("agentdoc: the fixture answer named no plan")
	}
	system, err := prompts.Build("plan-namer", namerPrompt)
	if err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		agentDocStanding(system),
		{Name: "the naming rules", Symbol: "factoryd.namerPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: namerPrompt},
		{Name: "the plan's facts", Symbol: "factoryd.nameFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: agentDocJSON(nameFacts(title, p.Spec.Outcome, p.Summary, taken, 1))},
	}, nil
}

// agentDocCardStore serves the drives the fixture card, its running
// session, its project's chat task, and no earlier proofs.
type agentDocCardStore struct{ Store }

func (agentDocCardStore) Task(_ context.Context, id string) (Task, bool, error) {
	return agentDocCard(), id == agentDocTask, nil
}

func (agentDocCardStore) InsertAlert(context.Context, Alert) (int64, error) { return 0, nil }

func (agentDocCardStore) PlanProofs(context.Context, string) ([]PlanProof, error) { return nil, nil }

func (agentDocCardStore) EnsureChatTask(_ context.Context, _, repository string, _ float64, _ time.Time) (Task, error) {
	return Task{ID: agentDocTask, Repository: repository, RequiredCapabilities: []string{policy.ClassChat}}, nil
}

func (agentDocCardStore) LiveSession(context.Context, string) (LiveSession, bool, error) {
	return LiveSession{TaskID: agentDocTask, SessionID: "s-fixture-01", Namespace: "factory-agents-fixture", Pod: "agent-fixture",
		MessageToken: "agentdoc"}, true, nil
}

// agentDocPod answers the session pod's address: the capture's loopback
// worker.
type agentDocPod struct{ Kube }

func (agentDocPod) PodIP(context.Context, string, string) (string, error) { return "127.0.0.1", nil }

// agentDocSessionTool drives door, a session tool's handler at factoryd,
// on the call body the gateway hands it, with the session's worker
// serving file out of its workspace.
func agentDocSessionTool(ctx context.Context, v policy.Version, door func(*Runner) http.HandlerFunc, file []byte, body string) error {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(file) // #nosec G104 -- the capture's own reader, and a short write fails the call it serves
	}))
	defer worker.Close()
	r := &Runner{store: agentDocCardStore{}, kube: agentDocPod{}, workerPort: worker.Listener.Addr().(*net.TCPAddr).Port,
		cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	rec := httptest.NewRecorder()
	door(r)(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		return fmt.Errorf("agentdoc: the session tool answered %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return nil
}

// agentDocDigestFile is the workspace file the fixture session digests.
const agentDocDigestFile = "# The nightly job\n\nThe job counts the day's rows and writes the report at 02:00 UTC.\n" +
	"A batch that fails is requeued once, and a second failure pages the human on call.\n"

// agentDocDigest drives the session's digest tool at factoryd's door on a
// workspace file, the ask left to the door's default.
func agentDocDigest(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	op := &OperatorModel{GatewayURL: gwURL, Token: "agentdoc", HTTP: hc}
	if err := agentDocSessionTool(ctx, v, func(r *Runner) http.HandlerFunc { return r.DigestHandler(op) }, []byte(agentDocDigestFile),
		`{"task_id":"`+agentDocTask+`","path":"docs/runbook.md"}`); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the digest's message", Symbol: "factoryd.DigestMessage", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: DigestMessage(fileDigestAsk, agentDocDigestFile)},
		{Name: "the default ask", Symbol: "factoryd.fileDigestAsk", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: fileDigestAsk},
		{Name: "the file's text", Symbol: "factoryd.agentDocDigest", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser, Text: agentDocDigestFile},
	}, nil
}

// agentDocImageLook drives the session's analyze_image tool at factoryd's
// door on a workspace image, the ask left to the look's default.
func agentDocImageLook(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	img, err := agentdoc.PNG()
	if err != nil {
		return nil, err
	}
	op := &OperatorModel{GatewayURL: gwURL, Token: "agentdoc", HTTP: hc}
	if err := agentDocSessionTool(ctx, v, func(r *Runner) http.HandlerFunc { return r.ImageLookHandler(op) }, img,
		`{"task_id":"`+agentDocTask+`","path":"screens/sizes.png"}`); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the default ask", Symbol: "factoryd.VisionAsk", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: VisionAsk},
		{Name: "the image", Symbol: "factoryd.agentDocImageLook", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser,
			Text: "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)},
	}, nil
}

// agentDocRoutines serves the follow the fixture project.
type agentDocRoutines struct{ RoutineRunStore }

func (agentDocRoutines) Project(_ context.Context, name string) (Project, bool, error) {
	return Project{Name: name, Repository: "owner/name"}, true, nil
}

// agentDocFollow drives the follow judgment's liveness call after a move
// of the fixture project.
func agentDocFollow(ctx context.Context, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	m, _, err := agentDocModel(gwURL, v, policy.PurposeDigest, hc)
	if err != nil {
		return nil, err
	}
	r := &Runner{store: agentDocCardStore{}, Routines: agentDocRoutines{}, supervisor: m, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	if err := r.followModelAnswers(ctx, FollowMoveInput{Project: "fixture"}); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		agentDocInputLead,
		{Name: "the liveness ask", Symbol: "factoryd.followModelSystem", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: followModelSystem},
		{Name: "the liveness input", Symbol: "factoryd.followModelInput", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: marshalled(followModelInput)},
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
