package factoryd

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"

// Writer is a model call whose text a human reads, with the guide's forms
// of its jobs. Calls are the functions that make the call, each named by
// its package, receiver and name.
type Writer struct {
	Name  string
	Jobs  []writingguide.Job
	Calls []string
}

// Writers are every model call whose text a human reads.
var Writers = []Writer{
	{Name: "planner and replanner", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobSummary, writingguide.JobBrief, writingguide.JobAsk},
		Calls: []string{"factoryd.(*Runner).RunPlan"}},
	{Name: "delta planner", Jobs: []writingguide.Job{writingguide.JobTitle, writingguide.JobWhy, writingguide.JobSummary, writingguide.JobBrief},
		Calls: []string{"factoryd.(*Runner).draftDelta"}},
	{Name: "coding agent", Jobs: []writingguide.Job{writingguide.JobSummary, writingguide.JobAsk, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobBrief},
		Calls: []string{"factoryd.(*Runner).RunSession"}},
	{Name: "coding agent on a plan's card", Jobs: []writingguide.Job{writingguide.JobSummary, writingguide.JobAsk, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobBrief},
		Calls: []string{"factoryd.(*Runner).RunSession"}},
	{Name: "message responder", Jobs: []writingguide.Job{writingguide.JobChat},
		Calls: []string{"factoryd.(*Runner).MessageSession"}},
	{Name: "project agent", Jobs: []writingguide.Job{writingguide.JobChat, writingguide.JobTitle, writingguide.JobWhy, writingguide.JobSummary, writingguide.JobBrief, writingguide.JobAsk},
		Calls: []string{"console.(*turn).run"}},
	{Name: "check-in at the gate", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).runCheckin"}},
	{Name: "check-in in a session", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).runCheckin"}},
	{Name: "check-in before a fresh session", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).runCheckin"}},
	{Name: "check-in on a case", Jobs: []writingguide.Job{writingguide.JobLine, writingguide.JobWhy},
		Calls: []string{"factoryd.(*Runner).rung1Converse"}},
	{Name: "case story writer", Jobs: []writingguide.Job{writingguide.JobAsk},
		Calls: []string{"factoryd.(*BriefWriter).askCase"}},
	{Name: "approval brief writer", Jobs: []writingguide.Job{writingguide.JobAsk},
		Calls: []string{"factoryd.(*BriefWriter).ask"}},
	{Name: "model review", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls: []string{"factoryd.(*Runner).ModelReview"}},
	{Name: "model review of a later part", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls: []string{"factoryd.(*Runner).ModelReview"}},
	{Name: "the review's behaviours", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).ModelReview"}},
	{Name: "audit", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls: []string{"factoryd.(*Runner).GateAudit"}},
	{Name: "audit of a later part", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls: []string{"factoryd.(*Runner).GateAudit"}},
	{Name: "gate supervisor", Jobs: []writingguide.Job{writingguide.JobFinding},
		Calls: []string{"factoryd.(*Runner).SuperviseGate"}},
	{Name: "hold-out proposer", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).proposeHoldouts"}},
	{Name: "judgement", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).judgeArtifacts"}},
	{Name: "answer screener", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).ScreenAnswer"}},
	{Name: "plan namer", Jobs: []writingguide.Job{writingguide.JobTitle},
		Calls: []string{"factoryd.(*Namer).Name"}},
	{Name: "chat thread title", Jobs: []writingguide.Job{writingguide.JobTitle},
		Calls: []string{"console.(*Console).nameThread"}},
	{Name: "chat compaction", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"console.(*turn).compact"}},
	{Name: "screening", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).RunReport"}},
	{Name: "sizing", Jobs: []writingguide.Job{writingguide.JobLine},
		Calls: []string{"factoryd.(*Runner).RunReport"}},
}
