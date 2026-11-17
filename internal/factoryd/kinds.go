package factoryd

import (
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// CaseKinds are every kind a case is opened as, in the order cases.go
// declares them.
func CaseKinds() []CaseKind {
	return []CaseKind{
		CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor, CaseDayCap, CaseMonthCap, CaseProjectCap,
		CaseCheckinHuman, CaseCheckinShare, CaseCheckinFailed, CaseGateStop, CaseFindingsRepeated,
		CaseDependencyClosed, CaseDependencyFailed, CaseWaitExpired, CasePodNeverStarted, CaseProviderRefused,
		CaseNoAnswer, CaseContentFilter, CaseImageWait, CaseScreeningSuspect, CaseAgentEscalate,
		CaseAgentRequestInfo, CasePlanQuestion, CasePlanConfirmation, CaseMergeApproval, CaseHumanCard,
		CaseOther, CaseMonitorFlag, CaseJudgmentBudget, CasePolicyClass, CaseRedMain, CaseRollHeld,
		CaseConfigRefused, CasePlanCriteria,
	}
}

// CardSources are every source a card can have: how a human, a model or
// factoryd filed it.
func CardSources() []string {
	return []string{
		SourceWebhook, SourceAPI, SourcePlan, SourceSuccessor, SourceDelegate, SourceRoutine,
		SourceScreening, SourceSizing, SourceProver, policy.SourceToolRequest,
		SourceRoll, SourceRollRefused, SourceRollFailed, SourceMainFix, SourceRerun, SourceRevert, SourceStillRed,
		SourceGroupRevert, SourceChainRepair, SourceChainStall, SourceAlert,
		SourceGitHubSync, SourceSchemaCopy, SourceTemplateUpdate, SourceImageRemoval,
		SourceChat, SourceChatBudget, SourceMonthCap, SourceConfig, SourceProductionHeld,
	}
}

// HumanActions are the actions of Actions a human may pick, in the
// table's order.
func HumanActions() []string {
	var out []string
	for _, a := range Actions {
		if a.Rungs[3] {
			out = append(out, a.Name)
		}
	}
	return out
}

// ApprovalKinds are every kind of approval request.
func ApprovalKinds() []string {
	return []string{ApprovalMerge, ApprovalResource, ApprovalPlan, ApprovalCriteria, ApprovalCriteriaRerun}
}

// MonitorFlags are every flag the card digest raises, each a sub-kind of
// the monitor flag case.
func MonitorFlags() []string {
	return []string{
		"prover_refused_thrice", "grant_bought_nothing", "session_no_push", "checkin_cut",
		"finding_confirmed_twice", "checkin_refused", "opened_proof_red_twice", "opened_proof_changed",
		"criterion_unproven", "answer_changed_nothing", "issue_open_after_end",
		"dependency_failed", "proofs_missing", "budget_moved_no_event", "wait_over_bound",
		"queued_over_bound", "review_refused_not_run",
	}
}

// BirthKinds are every birth kind of a chat row the factory writes: the
// events a poke answers and the wakes'. A row no event wrote has none.
func BirthKinds() []string {
	return slices.Concat(pokeKinds, []string{WakeAsk, EventCaseClimbed, WakeCard, WakeTimer, WakeRefill})
}
