// Package pg holds the Postgres-backed tests split out of factoryd's own
// suite. The alias shim below keeps the moved files' bare factoryd names
// compiling against the package's export surface.
package pg

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"

// Unexported factoryd names the moved files reach through the surface.
var (
	holdsSlot = factoryd.PgHoldsSlot
	toInt     = factoryd.PgToInt
)

const (
	capReached = factoryd.PgCapReached
	leftQueue  = factoryd.PgLeftQueue
)

// Exported factoryd names the moved files use bare.
var (
	ActApproveMerge        = factoryd.ActApproveMerge
	ActClose               = factoryd.ActClose
	ActContinue            = factoryd.ActContinue
	ActRetry               = factoryd.ActRetry
	AnswerFund             = factoryd.AnswerFund
	ApprovalMerge          = factoryd.ApprovalMerge
	BriefTerminal          = factoryd.BriefTerminal
	BudgetQuestion         = factoryd.BudgetQuestion
	CapReached             = factoryd.CapReached
	CatchDelta             = factoryd.CatchDelta
	CatchDeltaFiled        = factoryd.CatchDeltaFiled
	CaseAtHuman            = factoryd.CaseAtHuman
	CaseCheckinHuman       = factoryd.CaseCheckinHuman
	CaseDayCap             = factoryd.CaseDayCap
	CaseDependencyFailed   = factoryd.CaseDependencyFailed
	CaseGateStop           = factoryd.CaseGateStop
	CaseMergeApproval      = factoryd.CaseMergeApproval
	CaseMonitorFlag        = factoryd.CaseMonitorFlag
	CaseOpen               = factoryd.CaseOpen
	CaseOther              = factoryd.CaseOther
	CasePlanQuestion       = factoryd.CasePlanQuestion
	CaseResolved           = factoryd.CaseResolved
	CaseSessionCap         = factoryd.CaseSessionCap
	CaseWithdrawn          = factoryd.CaseWithdrawn
	ConsoleApproved        = factoryd.ConsoleApproved
	CriterionInvariant     = factoryd.CriterionInvariant
	CriterionTest          = factoryd.CriterionTest
	ErrAnswered            = factoryd.ErrAnswered
	ErrNotDonePlan         = factoryd.ErrNotDonePlan
	ErrNotFound            = factoryd.ErrNotFound
	ErrPlanClosed          = factoryd.ErrPlanClosed
	EventApprovalRequested = factoryd.EventApprovalRequested
	EventCardExtended      = factoryd.EventCardExtended
	EventPlanClosed        = factoryd.EventPlanClosed
	EventQuestionAsked     = factoryd.EventQuestionAsked
	FactoryDecider         = factoryd.FactoryDecider
	FinderReview           = factoryd.FinderReview
	OpenStore              = factoryd.OpenStore
	PgRunner               = factoryd.PgRunner
	PlanDelivered          = factoryd.PlanDelivered
	QuestionConfirmation   = factoryd.QuestionConfirmation
	RungFrontier           = factoryd.RungFrontier
	SourcePlan             = factoryd.SourcePlan
	SourceSuccessor        = factoryd.SourceSuccessor
	StepHandedUp           = factoryd.StepHandedUp
	StepInProgress         = factoryd.StepInProgress
	StepResolved           = factoryd.StepResolved
	StepWithdrawn          = factoryd.StepWithdrawn
	SuccessorID            = factoryd.SuccessorID
	ViaConsole             = factoryd.ViaConsole
	ViaFactoryd            = factoryd.ViaFactoryd
)

type (
	ApprovalRequest  = factoryd.ApprovalRequest
	Case             = factoryd.Case
	CaseAction       = factoryd.CaseAction
	CaseDraw         = factoryd.CaseDraw
	CaseHealReport   = factoryd.CaseHealReport
	CaseKind         = factoryd.CaseKind
	CaseMoney        = factoryd.CaseMoney
	CaseRead         = factoryd.CaseRead
	CaseResolution   = factoryd.CaseResolution
	CaseStep         = factoryd.CaseStep
	CriterionVerdict = factoryd.CriterionVerdict
	Dependency       = factoryd.Dependency
	ErrCaseEnded     = factoryd.ErrCaseEnded
	Event            = factoryd.Event
	MergeSpec        = factoryd.MergeSpec
	ModelLadder      = factoryd.ModelLadder
	PGStore          = factoryd.PGStore
	Plan             = factoryd.Plan
	PlanAnswer       = factoryd.PlanAnswer
	PlanClose        = factoryd.PlanClose
	Runner           = factoryd.Runner
	RunnerConfig     = factoryd.RunnerConfig
	Session          = factoryd.Session
	Task             = factoryd.Task
	TaskUsage        = factoryd.TaskUsage
)
