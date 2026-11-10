package policy

// The gate reserve's default and its Limits accessor (issue #1636): the
// EUR a card's budget holds for its gates, the price of one model
// review call and one audit call at their configured models, so a card
// whose sessions spend to the session ceiling still runs both without a
// fund. The reserve is computed from the policy in force wherever a
// session's ceiling is read; it is never stored and never spent, so a
// grant that raises the budget leaves it whole.

// DefaultGateReserveEUR is the gate reserve of a policy without
// limits.gate_reserve_eur, and of a version pinned before the key
// existed: the whole of a model review and the whole of an audit at the
// models the gates use (review_model and audit_model, big in the factory),
// as factoryd prices them before the first call (Runner.unfunded). A live
// review costs about 0.19 EUR and an audit about 0.10, so 0.40 EUR fits
// both with margin.
const DefaultGateReserveEUR = 0.40

// GateReserve is GateReserveEUR, or DefaultGateReserveEUR when the
// policy sets none (0), as a version pinned before the key reads it.
func (l Limits) GateReserve() float64 {
	if l.GateReserveEUR == 0 {
		return DefaultGateReserveEUR
	}
	return l.GateReserveEUR
}
