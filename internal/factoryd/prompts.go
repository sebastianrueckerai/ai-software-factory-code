package factoryd

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"

// checkinPersona is the check-in's ask, whose moment and decisions its
// persona names.
type checkinPersona struct{ Ask *CheckInAsk }

// checkinPromptStep is the check-in's step at moment: checkin-gate,
// checkin-session or checkin-dead.
func checkinPromptStep(moment string) string { return "checkin-" + moment }

// auditFacts are the audit's call: on a later part of a diff in parts, or
// on the first.
type auditFacts struct{ Part bool }

func auditText(f auditFacts) (string, error) {
	if f.Part {
		return auditPartPrompt, nil
	}
	return auditPrompt, nil
}

func init() {
	prompts.RegisterFacts("checkin", func(f checkinPersona) (string, error) { return checkinSystemFor(f.Ask), nil })
	prompts.RegisterFacts("audit", auditText)
	prompts.RegisterFacts("review", reviewCallPrompt)
	prompts.RegisterFacts("delta", deltaPrompt)
	prompts.RegisterFacts("judgement", judgementText)
	prompts.RegisterFacts("answer", answerSystem)
	prompts.RegisterFacts("brief", briefPrompt)

	checkin := []string{"factoryd.(*Runner).runCheckin"}
	prompts.RegisterStep("checkin-gate", checkin, checkinPersona{&CheckInAsk{Moment: CheckInGate,
		Findings: []CheckFinding{{Text: "no test covers the handler", Source: "review"}}}})
	prompts.RegisterStep("checkin-session", checkin, checkinPersona{&CheckInAsk{Moment: CheckInSession,
		Trip: &TripReport{Detector: TripSize, Detail: "the card's remaining work"}}})
	prompts.RegisterStep("checkin-dead", checkin, checkinPersona{&CheckInAsk{Moment: CheckInDead, Stop: "wall clock"}})
	prompts.RegisterStep("case-checkin", []string{"factoryd.(*Runner).rung1Converse"}, rungPrompt)
	prompts.RegisterStep("case-brief-writer", []string{"factoryd.(*BriefWriter).askCase"}, caseBriefPrompt)
	prompts.RegisterStep("brief-writer", []string{"factoryd.(*BriefWriter).ask"}, briefKind{ApprovalResource})
	review := []string{"factoryd.(*Runner).ModelReview"}
	prompts.RegisterStep("model-review", review, reviewFacts{Gates: GateResults{ChecksPassed: true}})
	prompts.RegisterStep("model-review-part", review, reviewFacts{Part: true})
	prompts.RegisterStep("review-behaviours", review, reviewBehavioursPrompt)
	audit := []string{"factoryd.(*Runner).GateAudit"}
	prompts.RegisterStep("security-audit", audit, auditFacts{})
	prompts.RegisterStep("security-audit-part", audit, auditFacts{Part: true})
	prompts.RegisterStep("gate-supervisor", []string{"factoryd.(*Runner).SuperviseGate"}, gateSupervisorPrompt)
	prompts.RegisterStep("delta-planner", []string{"factoryd.(*Runner).draftDelta"}, deltaFacts{})
	prompts.RegisterStep("holdout-proposer", []string{"factoryd.(*Runner).proposeHoldouts"}, holdoutSystem)
	prompts.RegisterStep("judgement", []string{"factoryd.(*Runner).judgeArtifacts"}, judgementFacts{"Does the page read calm?"})
	prompts.RegisterStep("answer-screener", []string{"factoryd.(*Runner).ScreenAnswer"}, screenMarkers{"===ANSWER-T", "ANSWER-T==="})
	prompts.RegisterStep("plan-namer", []string{"factoryd.(*Namer).Name"}, namerPrompt)
	prompts.RegisterStep("follow-liveness", []string{"factoryd.(*Runner).followModelAnswers"}, followModelSystem)
}
