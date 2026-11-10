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
// existed: one model review call and one audit call at their configured
// models, priced as the gateway prices a call — a call costs about 0.10
// EUR at the small alias — so 0.25 EUR fits the two with margin.
const DefaultGateReserveEUR = 0.25

// GateReserve is GateReserveEUR, or DefaultGateReserveEUR when the
// policy sets none (0), as a version pinned before the key reads it.
func (l Limits) GateReserve() float64 {
	if l.GateReserveEUR == 0 {
		return DefaultGateReserveEUR
	}
	return l.GateReserveEUR
}
