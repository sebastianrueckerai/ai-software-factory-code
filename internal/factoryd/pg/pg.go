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

	// Const aliases the moved files reach in their own const expressions:
	// a var alias is no constant, so these carry the const-ness over.
	CheckPolicyClass = factoryd.CheckPolicyClass
	RungHandUp       = factoryd.RungHandUp
)

// Exported factoryd names the moved files use bare.
var (
	ActAnswerCriteria      = factoryd.ActAnswerCriteria
	ActApproveMerge        = factoryd.ActApproveMerge
	ActClose               = factoryd.ActClose
	ActConfirmCriteria     = factoryd.ActConfirmCriteria
	ActContinue            = factoryd.ActContinue
	ActDeployNow           = factoryd.ActDeployNow
	ActDenyCriteria        = factoryd.ActDenyCriteria
	ActDenyMerge           = factoryd.ActDenyMerge
	ActExtend              = factoryd.ActExtend
	ActFund                = factoryd.ActFund
	ActGrantCriteria       = factoryd.ActGrantCriteria
	ActHandover            = factoryd.ActHandover
	ActNote                = factoryd.ActNote
	ActRaiseCeiling        = factoryd.ActRaiseCeiling
	ActReplan              = factoryd.ActReplan
	ActRemoveDependency    = factoryd.ActRemoveDependency
	ActRestartPlan         = factoryd.ActRestartPlan
	ActRouteApproval       = factoryd.ActRouteApproval
	ActResume              = factoryd.ActResume
	ActRetry               = factoryd.ActRetry
	ActWithdraw            = factoryd.ActWithdraw
	AgentAsks              = factoryd.AgentAsks
	Actions                = factoryd.Actions
	AnswerClose            = factoryd.AnswerClose
	AnswerDismiss          = factoryd.AnswerDismiss
	AnswersByOffers        = factoryd.AnswersByOffers
	AnswerCheckAgain       = factoryd.AnswerCheckAgain
	AnswerFund             = factoryd.AnswerFund
	AnswerGo               = factoryd.AnswerGo
	AnswerHoldWait         = factoryd.AnswerHoldWait
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
	CaseAgentEscalate      = factoryd.CaseAgentEscalate
	CaseAnswer             = factoryd.CaseAnswer
	CaseAtHuman            = factoryd.CaseAtHuman
	CaseCheckinHuman       = factoryd.CaseCheckinHuman
	CaseCheckinShare       = factoryd.CaseCheckinShare
	CaseDayCap             = factoryd.CaseDayCap
	CaseDependencyFailed   = factoryd.CaseDependencyFailed
	CaseDependencyClosed   = factoryd.CaseDependencyClosed
	CaseGateStop           = factoryd.CaseGateStop
	CaseHumanCard          = factoryd.CaseHumanCard
	CaseMergeApproval      = factoryd.CaseMergeApproval
	CaseMonitorFlag        = factoryd.CaseMonitorFlag
	CaseOpen               = factoryd.CaseOpen
	CaseOther              = factoryd.CaseOther
	CasePolicyClass        = factoryd.CasePolicyClass
	CasePlanQuestion       = factoryd.CasePlanQuestion
	CaseRedMain            = factoryd.CaseRedMain
	CaseResolved           = factoryd.CaseResolved
	CaseRollHeld           = factoryd.CaseRollHeld
	CaseSessionCap         = factoryd.CaseSessionCap
	CaseWithdrawn          = factoryd.CaseWithdrawn
	ChatUser               = factoryd.ChatUser
	DepEndedReason         = factoryd.DepEndedReason
	CheckTimeCap           = factoryd.CheckTimeCap
	CheckinAnswerTool      = factoryd.CheckinAnswerTool
	CheckinSpendAsk        = factoryd.CheckinSpendAsk
	CheckinStopCallCap     = factoryd.CheckinStopCallCap
	ChoiceCase             = factoryd.ChoiceCase
	ConsoleApproved        = factoryd.ConsoleApproved
	CriterionInvariant     = factoryd.CriterionInvariant
	CriterionTest          = factoryd.CriterionTest
	ErrAnswered            = factoryd.ErrAnswered
	ErrDrawRefused         = factoryd.ErrDrawRefused
	ErrNotDonePlan         = factoryd.ErrNotDonePlan
	ErrNotHeld             = factoryd.ErrNotHeld
	ErrNotFound            = factoryd.ErrNotFound
	ErrPlanClosed          = factoryd.ErrPlanClosed
	ErrWakeVenueGone       = factoryd.ErrWakeVenueGone
	EscalationQuestion     = factoryd.EscalationQuestion
	EventApprovalRequested = factoryd.EventApprovalRequested
	EventCardCreated       = factoryd.EventCardCreated
	EventCardExtended      = factoryd.EventCardExtended
	EventCardFunded        = factoryd.EventCardFunded
	EventCaseClimbed       = factoryd.EventCaseClimbed
	EventGateHead          = factoryd.EventGateHead
	EventModelReview       = factoryd.EventModelReview
	EventPlanClosed        = factoryd.EventPlanClosed
	EventQuestionAnswered  = factoryd.EventQuestionAnswered
	EventQuestionAsked     = factoryd.EventQuestionAsked
	EventSessionGranted    = factoryd.EventSessionGranted
	EventSessionPushed     = factoryd.EventSessionPushed
	EventTaskEnded         = factoryd.EventTaskEnded
	FactoryDecider         = factoryd.FactoryDecider
	FactsOf                = factoryd.FactsOf
	ChatBudgetQuestion     = factoryd.ChatBudgetQuestion
	CaseJudgmentBudget     = factoryd.CaseJudgmentBudget
	GrantKey               = factoryd.GrantKey
	SplitRepo              = factoryd.SplitRepo
	FindingID              = factoryd.FindingID
	FinderReview           = factoryd.FinderReview
	GateMerge              = factoryd.GateMerge
	HealPass               = factoryd.HealPass
	HealedReason           = factoryd.HealedReason
	IntakeFactoryClosed    = factoryd.IntakeFactoryClosed
	IdentityKey            = factoryd.IdentityKey
	LeaveCaseToAct         = factoryd.LeaveCaseToAct
	NewEventBus            = factoryd.NewEventBus
	OffersFor              = factoryd.OffersFor
	OpenStore              = factoryd.OpenStore
	PgRigRunner            = factoryd.PgRigRunner
	PgRunner               = factoryd.PgRunner
	PlanDelivered          = factoryd.PlanDelivered
	Proposes               = factoryd.Proposes
	QuestionConfirmation   = factoryd.QuestionConfirmation
	RungFrontier           = factoryd.RungFrontier
	SourceMonthCap         = factoryd.SourceMonthCap
	SourcePlan             = factoryd.SourcePlan
	SourceProductionHeld   = factoryd.SourceProductionHeld
	SourceRoutine          = factoryd.SourceRoutine
	SourceStillRed         = factoryd.SourceStillRed
	SourceSuccessor        = factoryd.SourceSuccessor
	StepHandedUp           = factoryd.StepHandedUp
	StepInProgress         = factoryd.StepInProgress
	StepResolved           = factoryd.StepResolved
	StepWithdrawn          = factoryd.StepWithdrawn
	SuccessorID            = factoryd.SuccessorID
	SignalAnswerWhole      = factoryd.SignalAnswerWhole
	SignalCaseResolved     = factoryd.SignalCaseResolved
	ViaButton              = factoryd.ViaButton
	ViaChat                = factoryd.ViaChat
	ViaConsole             = factoryd.ViaConsole
	ViaFactoryd            = factoryd.ViaFactoryd
	ViaWebhook             = factoryd.ViaWebhook
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
	CaseFacts        = factoryd.CaseFacts
	CaseHealReport   = factoryd.CaseHealReport
	CaseKind         = factoryd.CaseKind
	CaseMoney        = factoryd.CaseMoney
	CaseRead         = factoryd.CaseRead
	CaseResolution   = factoryd.CaseResolution
	CaseSignal       = factoryd.CaseSignal
	CaseStep         = factoryd.CaseStep
	CheckinRequest   = factoryd.CheckinRequest
	CheckinResponse  = factoryd.CheckinResponse
	ChatBudgetCards  = factoryd.ChatBudgetCards
	Completion       = factoryd.Completion
	CriteriaSpec     = factoryd.CriteriaSpec
	CriterionVerdict = factoryd.CriterionVerdict
	Dependency       = factoryd.Dependency
	DigestFlag       = factoryd.DigestFlag

	DrawBoundError        = factoryd.DrawBoundError
	ErrCaseEnded          = factoryd.ErrCaseEnded
	ErrActRefused         = factoryd.ErrActRefused
	Event                 = factoryd.Event
	ErrMainRed            = factoryd.ErrMainRed
	Forgejo               = factoryd.Forgejo
	GateWatch             = factoryd.GateWatch
	HumanAsk              = factoryd.HumanAsk
	HumanQuestion         = factoryd.HumanQuestion
	Intake                = factoryd.Intake
	IntakeEvent           = factoryd.IntakeEvent
	LiteLLMAdmin          = factoryd.LiteLLMAdmin
	MergeSpec             = factoryd.MergeSpec
	ModelLadder           = factoryd.ModelLadder
	PGStore               = factoryd.PGStore
	Plan                  = factoryd.Plan
	PlanAnswer            = factoryd.PlanAnswer
	PlanClose             = factoryd.PlanClose
	Project               = factoryd.Project
	QuestionChoice        = factoryd.QuestionChoice
	ReleaseKeeper         = factoryd.ReleaseKeeper
	Resolution            = factoryd.Resolution
	RoutineRunInput       = factoryd.RoutineRunInput
	RungTwoRecommendation = factoryd.RungTwoRecommendation
	Runner                = factoryd.Runner
	RunnerConfig          = factoryd.RunnerConfig
	RunningRun            = factoryd.RunningRun
	Session               = factoryd.Session
	SessionKill           = factoryd.SessionKill
	Store                 = factoryd.Store
	SupervisorConfig      = factoryd.SupervisorConfig
	Task                  = factoryd.Task
	TaskSignal            = factoryd.TaskSignal
	TaskUsage             = factoryd.TaskUsage
	Timeline              = factoryd.Timeline
	TripInput             = factoryd.TripInput
	TripOutcome           = factoryd.TripOutcome
	Waker                 = factoryd.Waker
	Workflows             = factoryd.Workflows
)
