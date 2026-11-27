package factoryd

import (
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// Writer is a model call whose text a human reads. Its prompt carries the
// guide's forms of its jobs (writingguide.For). Calls are the functions
// that make the call, each named by its package, receiver and name.
type Writer struct {
	Name   string
	Prompt func() string
	Jobs   []writingguide.Job
	Calls  []string
}

// Writers are every model call whose text a human reads, each with a
// prompt built as its call builds it.
var Writers = []Writer{
	{Name: "planner and replanner", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobSummary, writingguide.JobBrief, writingguide.JobAsk},
		Calls: []string{"factoryd.(*Runner).RunPlan"},
		Prompt: func() string {
			return planGuide(policy.PlanShape{MinCards: 1, MaxCards: 20}, policy.Limits{}, nil, nil)
		}},
	{Name: "delta planner", Jobs: []writingguide.Job{writingguide.JobTitle, writingguide.JobWhy, writingguide.JobSummary, writingguide.JobBrief},
		Calls:  []string{"factoryd.(*Runner).draftDelta"},
		Prompt: func() string { return deltaPrompt(nil) }},
	{Name: "coding agent", Jobs: []writingguide.Job{writingguide.JobSummary, writingguide.JobAsk, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobBrief},
		Calls: []string{"factoryd.(*Runner).RunSession"},
		Prompt: func() string {
			return briefText(harness.TaskBrief{Objective: "Count the skipped rows.", Tools: []string{policy.ToolEscalate, policy.ToolRequestInfo, policy.ToolDelegate}}, "T")
		}},
	{Name: "coding agent on a plan's card", Jobs: []writingguide.Job{writingguide.JobSummary, writingguide.JobAsk, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobBrief},
		Calls: []string{"factoryd.(*Runner).RunSession"},
		Prompt: func() string {
			return briefText(harness.TaskBrief{Objective: "Count the skipped rows.", CardRole: RoleFeature, Tools: []string{policy.ToolEscalate, policy.ToolRequestInfo, policy.ToolDelegate}}, "T")
		}},
	{Name: "message responder", Jobs: []writingguide.Job{writingguide.JobChat},
		Calls:  []string{"factoryd.(*Runner).MessageSession"},
		Prompt: func() string { return sessionMessage("t-1", "sebastian", "Why is the check red?", true) }},
	{Name: "project agent", Jobs: []writingguide.Job{writingguide.JobChat, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobSummary, writingguide.JobBrief, writingguide.JobAsk},
		Calls:  []string{"console.(*turn).run"},
		Prompt: func() string { return ProjectAgentPrompt("app", "sebastian", policy.AutonomyManual) }},
	{Name: "check-in at the gate", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).runCheckin"},
		Prompt: func() string {
			return checkinStatement(&CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "no test covers the handler", Source: "review"}}},
				nil, 3, 1, 800)
		}},
	{Name: "check-in in a session", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).runCheckin"},
		Prompt: func() string {
			return checkinStatement(&CheckInAsk{Moment: CheckInSession, Trip: &TripReport{Detector: TripSize, Detail: "the card's remaining work"}},
				nil, 3, 1, 800)
		}},
	{Name: "check-in before a fresh session", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).runCheckin"},
		Prompt: func() string {
			return checkinStatement(&CheckInAsk{Moment: CheckInDead, Stop: "wall clock"}, nil, 3, 1, 800)
		}},
	{Name: "check-in on a case", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls:  []string{"factoryd.(*Runner).rung1Converse"},
		Prompt: func() string { return rungPrompt }},
	{Name: "case story writer", Jobs: []writingguide.Job{writingguide.JobAsk},
		Calls:  []string{"factoryd.(*BriefWriter).askCase"},
		Prompt: func() string { return caseBriefPrompt }},
	{Name: "approval brief writer", Jobs: []writingguide.Job{writingguide.JobAsk},
		Calls:  []string{"factoryd.(*BriefWriter).ask"},
		Prompt: func() string { return briefPrompt(ApprovalResource) }},
	{Name: "model review", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls:  []string{"factoryd.(*Runner).ModelReview"},
		Prompt: func() string { return reviewCallPrompt(GateResults{ChecksPassed: true}, false, nil, nil, false) }},
	{Name: "model review of a later part", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls:  []string{"factoryd.(*Runner).ModelReview"},
		Prompt: func() string { return reviewCallPrompt(GateResults{}, true, nil, nil, false) }},
	{Name: "the review's behaviours", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls:  []string{"factoryd.(*Runner).ModelReview"},
		Prompt: func() string { return reviewBehavioursPrompt }},
	{Name: "audit", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls:  []string{"factoryd.(*Runner).GateAudit"},
		Prompt: func() string { return auditPrompt }},
	{Name: "audit of a later part", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls:  []string{"factoryd.(*Runner).GateAudit"},
		Prompt: func() string { return auditPartPrompt }},
	{Name: "gate supervisor", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls:  []string{"factoryd.(*Runner).SuperviseGate"},
		Prompt: func() string { return gateSupervisorPrompt }},
	{Name: "hold-out proposer", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls:  []string{"factoryd.(*Runner).proposeHoldouts"},
		Prompt: func() string { return holdoutSystem }},
	{Name: "judgement", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).judgeArtifacts"},
		Prompt: func() string {
			return judgementCall(PlanCriterion{ID: "reads_calm", Prompt: "Does the page read calm?"}, GateArtifact{Name: "page.txt"}).Prompt
		}},
	{Name: "answer screener", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls:  []string{"factoryd.(*Runner).ScreenAnswer"},
		Prompt: func() string { return answerSystem("===ANSWER-T", "ANSWER-T===") }},
	{Name: "plan namer", Jobs: []writingguide.Job{writingguide.JobTitle},
		Calls:  []string{"factoryd.(*Namer).Name"},
		Prompt: func() string { return namerPrompt }},
	{Name: "chat thread title", Jobs: []writingguide.Job{writingguide.JobTitle},
		Calls:  []string{"console.(*Console).nameThread"},
		Prompt: func() string { return TitlePrompt }},
	{Name: "chat compaction", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls:  []string{"console.(*turn).compact"},
		Prompt: func() string { return CompactPrompt }},
	{Name: "screening", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls:  []string{"factoryd.(*Runner).RunReport"},
		Prompt: func() string { return (&Runner{}).reportGuide() }},
	{Name: "sizing", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls:  []string{"factoryd.(*Runner).RunReport"},
		Prompt: func() string { return sizingGuide(sizeLines(600)) }},
}
