// Package pg holds the Postgres-backed tests split out of factoryd's own
// suite. The alias shim below keeps the moved files' bare factoryd names
// compiling against the package's export surface.
package pg

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"

// Unexported factoryd names the moved files reach through the surface.
var (
	holdsSlot            = factoryd.PgHoldsSlot
	toInt                = factoryd.PgToInt
	rungsOff             = factoryd.PgRungsOff
	monthCapCardQuestion = factoryd.PgMonthCapCardQuestion
)

type tripAttempt = factoryd.PgTripAttempt

const (
	capReached = factoryd.PgCapReached
	leftQueue  = factoryd.PgLeftQueue
)

// Exported factoryd names the moved files use bare.
var (
	ActAnswerCriteria      = factoryd.ActAnswerCriteria
	ActApproveMerge        = factoryd.ActApproveMerge
	ActClose               = factoryd.ActClose
	ActConfirmCriteria     = factoryd.ActConfirmCriteria
	ActContinue            = factoryd.ActContinue
	ActDenyCriteria        = factoryd.ActDenyCriteria
	ActDenyMerge           = factoryd.ActDenyMerge
	ActExtend              = factoryd.ActExtend
	ActGrantCriteria       = factoryd.ActGrantCriteria
	ActHandover            = factoryd.ActHandover
	ActNote                = factoryd.ActNote
	ActRemoveDependency    = factoryd.ActRemoveDependency
	ActRestartPlan         = factoryd.ActRestartPlan
	ActResume              = factoryd.ActResume
	ActRetry               = factoryd.ActRetry
	AgentAsks              = factoryd.AgentAsks
	AnswerClose            = factoryd.AnswerClose
	AnswersByOffers        = factoryd.AnswersByOffers
	AnswerFund             = factoryd.AnswerFund
	AnswerPark             = factoryd.AnswerPark
	AnswersFrom            = factoryd.AnswersFrom
	ApprovalCriteria       = factoryd.ApprovalCriteria
	ApprovalMerge          = factoryd.ApprovalMerge
	AskOffers              = factoryd.AskOffers
	BriefTerminal          = factoryd.BriefTerminal
	BudgetQuestion         = factoryd.BudgetQuestion
	CapReached             = factoryd.CapReached
	CaseMonthCap           = factoryd.CaseMonthCap
	CasePlanCriteria       = factoryd.CasePlanCriteria
	CatchDelta             = factoryd.CatchDelta
	CatchDeltaFiled        = factoryd.CatchDeltaFiled
	CaseAtHuman            = factoryd.CaseAtHuman
	CaseCheckinHuman       = factoryd.CaseCheckinHuman
	CaseCheckinShare       = factoryd.CaseCheckinShare
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
	ChatUser               = factoryd.ChatUser
	CheckTimeCap           = factoryd.CheckTimeCap
	ChoiceCase             = factoryd.ChoiceCase
	ConsoleApproved        = factoryd.ConsoleApproved
	CriterionInvariant     = factoryd.CriterionInvariant
	CriterionTest          = factoryd.CriterionTest
	ErrAnswered            = factoryd.ErrAnswered
	ErrNotDonePlan         = factoryd.ErrNotDonePlan
	ErrNotFound            = factoryd.ErrNotFound
	ErrPlanClosed          = factoryd.ErrPlanClosed
	EscalationQuestion     = factoryd.EscalationQuestion
	EventApprovalRequested = factoryd.EventApprovalRequested
	EventCardCreated       = factoryd.EventCardCreated
	EventCardExtended      = factoryd.EventCardExtended
	EventCardFunded        = factoryd.EventCardFunded
	EventGateHead          = factoryd.EventGateHead
	EventModelReview       = factoryd.EventModelReview
	EventPlanClosed        = factoryd.EventPlanClosed
	EventQuestionAnswered  = factoryd.EventQuestionAnswered
	EventQuestionAsked     = factoryd.EventQuestionAsked
	EventSessionPushed     = factoryd.EventSessionPushed
	FactoryDecider         = factoryd.FactoryDecider
	FactsOf                = factoryd.FactsOf
	FindingID              = factoryd.FindingID
	FinderReview           = factoryd.FinderReview
	GateMerge              = factoryd.GateMerge
	HealPass               = factoryd.HealPass
	HealedReason           = factoryd.HealedReason
	LeaveCaseToAct         = factoryd.LeaveCaseToAct
	OffersFor              = factoryd.OffersFor
	OpenStore              = factoryd.OpenStore
	PgRigRunner            = factoryd.PgRigRunner
	PgRunner               = factoryd.PgRunner
	PlanDelivered          = factoryd.PlanDelivered
	QuestionConfirmation   = factoryd.QuestionConfirmation
	RungFrontier           = factoryd.RungFrontier
	SourceMonthCap         = factoryd.SourceMonthCap
	SourcePlan             = factoryd.SourcePlan
	SourceRoutine          = factoryd.SourceRoutine
	SourceSuccessor        = factoryd.SourceSuccessor
	StepHandedUp           = factoryd.StepHandedUp
	StepInProgress         = factoryd.StepInProgress
	StepResolved           = factoryd.StepResolved
	StepWithdrawn          = factoryd.StepWithdrawn
	SuccessorID            = factoryd.SuccessorID
	ViaButton              = factoryd.ViaButton
	ViaChat                = factoryd.ViaChat
	ViaConsole             = factoryd.ViaConsole
	ViaFactoryd            = factoryd.ViaFactoryd
)

type (
	ActInput         = factoryd.ActInput
	ActResult        = factoryd.ActResult
	ApprovalRequest  = factoryd.ApprovalRequest
	AskOffer         = factoryd.AskOffer
	CardCapError     = factoryd.CardCapError
	Case             = factoryd.Case
	CaseAction       = factoryd.CaseAction
	CaseDraw         = factoryd.CaseDraw
	CaseHealReport   = factoryd.CaseHealReport
	CaseKind         = factoryd.CaseKind
	CaseMoney        = factoryd.CaseMoney
	CaseRead         = factoryd.CaseRead
	CaseResolution   = factoryd.CaseResolution
	CaseStep         = factoryd.CaseStep
	CheckinRequest   = factoryd.CheckinRequest
	CheckinResponse  = factoryd.CheckinResponse
	CriteriaSpec     = factoryd.CriteriaSpec
	CriterionVerdict = factoryd.CriterionVerdict
	Dependency       = factoryd.Dependency
	DigestFlag       = factoryd.DigestFlag
	ErrCaseEnded     = factoryd.ErrCaseEnded
	ErrActRefused    = factoryd.ErrActRefused
	Event            = factoryd.Event
	Forgejo          = factoryd.Forgejo
	GateWatch        = factoryd.GateWatch
	HumanAsk         = factoryd.HumanAsk
	HumanQuestion    = factoryd.HumanQuestion
	Intake           = factoryd.Intake
	IntakeEvent      = factoryd.IntakeEvent
	MergeSpec        = factoryd.MergeSpec
	ModelLadder      = factoryd.ModelLadder
	PGStore          = factoryd.PGStore
	Plan             = factoryd.Plan
	PlanAnswer       = factoryd.PlanAnswer
	PlanClose        = factoryd.PlanClose
	QuestionChoice   = factoryd.QuestionChoice
	Resolution       = factoryd.Resolution
	RoutineRunInput  = factoryd.RoutineRunInput
	Runner           = factoryd.Runner
	RunnerConfig     = factoryd.RunnerConfig
	RunningRun       = factoryd.RunningRun
	Session          = factoryd.Session
	SessionKill      = factoryd.SessionKill
	Store            = factoryd.Store
	SupervisorConfig = factoryd.SupervisorConfig
	Task             = factoryd.Task
	TaskSignal       = factoryd.TaskSignal
	TaskUsage        = factoryd.TaskUsage
	Timeline         = factoryd.Timeline
	TripInput        = factoryd.TripInput
	TripOutcome      = factoryd.TripOutcome
	Workflows        = factoryd.Workflows
)
