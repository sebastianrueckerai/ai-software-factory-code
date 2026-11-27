package factoryd

import (
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// checkinPromptStep is the check-in's step at moment: checkin-gate,
// checkin-session or checkin-dead.
func checkinPromptStep(moment string) string { return "checkin-" + moment }

// auditFacts are the audit's call: on a later part of a diff in parts, or
// on the first.
type auditFacts struct{ Part bool }

func auditText(f auditFacts) (string, error) {
	if f.Part {
		return auditPartFacts, nil
	}
	return "", nil
}

// sessionBrief is a session's brief and the tag of its markers.
type sessionBrief struct {
	Brief harness.TaskBrief
	Tag   string
}

// sessionFixture is a session step's example brief: the card's brief, and
// the guide of a report session.
func sessionFixture(step string, b harness.TaskBrief) sessionBrief {
	b.Step = step
	if b.Objective == "" {
		b.Objective = "Count the skipped rows."
	}
	return sessionBrief{b, "T"}
}

func init() {
	prompts.RegisterFacts("session", func(f sessionBrief) (string, error) { return briefText(f.Brief, f.Tag), nil })
	prompts.RegisterFacts("audit", auditText)
	prompts.RegisterFacts("review", reviewCallPrompt)
	prompts.RegisterFacts("delta", deltaPrompt)
	prompts.RegisterFacts("judgement", judgementText)
	prompts.RegisterFacts("answer", answerSystem)
	prompts.RegisterFacts("brief", briefPrompt)
	prompts.RegisterFacts("case-brief", caseBriefPrompt)

	checkin := []string{"factoryd.(*Runner).runCheckin"}
	prompts.RegisterStep("checkin-gate", checkin, "")
	prompts.RegisterStep("checkin-session", checkin, "")
	prompts.RegisterStep("checkin-dead", checkin, "")
	prompts.RegisterStep("case-checkin", []string{"factoryd.(*Runner).rung1Converse"}, "")
	prompts.RegisterStep("case-brief-writer", []string{"factoryd.(*BriefWriter).askCase"}, caseBriefFacts{})
	prompts.RegisterStep("brief-writer", []string{"factoryd.(*BriefWriter).ask"}, briefKind{ApprovalResource})
	review := []string{"factoryd.(*Runner).ModelReview"}
	prompts.RegisterStep("model-review", review, reviewFacts{Gates: GateResults{ChecksPassed: true}})
	prompts.RegisterStep("model-review-part", review, reviewFacts{Part: true})
	audit := []string{"factoryd.(*Runner).GateAudit"}
	prompts.RegisterStep("security-audit", audit, auditFacts{})
	prompts.RegisterStep("security-audit-part", audit, auditFacts{Part: true})
	prompts.RegisterStep("gate-supervisor", []string{"factoryd.(*Runner).SuperviseGate"}, gateSupervisorPrompt)
	prompts.RegisterStep("delta-planner", []string{"factoryd.(*Runner).draftDelta"}, deltaFacts{})
	prompts.RegisterStep("holdout-proposer", []string{"factoryd.(*Runner).proposeHoldouts"}, holdoutSystem)
	prompts.RegisterStep("judgement", []string{"factoryd.(*Runner).judgeArtifacts"}, judgementFacts{"Does the page read calm?"})
	prompts.RegisterStep("answer-screener", []string{"factoryd.(*Runner).ScreenAnswer"}, screenMarkers{"===ANSWER-T", "ANSWER-T==="})
	prompts.RegisterStep("plan-namer", []string{"factoryd.(*Namer).Name"}, "")
	prompts.RegisterStep("follow-liveness", []string{"factoryd.(*Runner).followModelAnswers"}, followModelSystem)

	coder := []string{policy.ToolEscalate, policy.ToolRequestInfo, policy.ToolDelegate}
	prompts.RegisterStep("coding", []string{"factoryd.(*Runner).RunSession"}, sessionFixture("coding", harness.TaskBrief{Tools: coder}))
	prompts.RegisterStep("coding-card", []string{"factoryd.(*Runner).RunSession"},
		sessionFixture("coding-card", harness.TaskBrief{Tools: coder, CardRole: RoleFeature}))
	prompts.RegisterStep("planner", []string{"factoryd.(*Runner).RunPlan"},
		sessionFixture("planner", harness.TaskBrief{Report: planGuide(policy.PlanShape{MinCards: 1, MaxCards: 20}, policy.Limits{}, nil, nil)}))
	prompts.RegisterStep("prover", []string{"factoryd.(*Runner).RunReport"},
		sessionFixture("prover", harness.TaskBrief{Report: proverGuide(agentDocPlan(), nil, nil)}))
	prompts.RegisterStep("screening", []string{"factoryd.(*Runner).RunReport"},
		sessionFixture("screening", harness.TaskBrief{Report: (&Runner{}).reportGuide()}))
	prompts.RegisterStep("sizing", []string{"factoryd.(*Runner).RunReport"},
		sessionFixture("sizing", harness.TaskBrief{Report: sizingGuide(sizeLines(600))}))
	prompts.RegisterStep("message-responder", []string{"factoryd.(*Runner).MessageSession"}, sessionMessage("sebastian", "Why is the check red?"))
	prompts.RegisterStep("supervisor-guidance", []string{"factoryd.(*Runner).message"}, "Run bash check changed before you end the session.")
	prompts.RegisterStep("nudge", []string{"factoryd.(*Runner).nudge"}, Nudge{Tool: "bash", Call: "go test ./...", Count: 3}.Text())
	prompts.RegisterStep("limit-note", []string{"factoryd.(*Runner).watchLimits"},
		"factoryd: 12 minutes of agent time are left. Wrap up: commit what works and end with your last message.")
	prompts.RegisterStep("workflow-warning", nil, WorkflowDirMessage(".forgejo", ".forgejo/workflows/ci.yml"))
}
