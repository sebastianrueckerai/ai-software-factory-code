package policy

import "math"

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

// RoundMicro is x rounded to the micro-euro: the one grain money is
// compared at, finer than the cent a budget is kept in and coarser than
// any float noise a subtraction leaves (the 0.40 reserve has no exact
// binary form, so 1.01 - 0.31 - 0.40 is 0.29999999999999993, not 0.30).
func RoundMicro(x float64) float64 { return math.Round(x*1e6) / 1e6 }

// CeilMicro is x rounded up to the micro-euro, a figure the grain reads
// as itself: a price of 0.30 stays 0.30 whatever noise its product left.
func CeilMicro(x float64) float64 { return math.Ceil(x*1e6-1e-6) / 1e6 }

// SessionHeadroom is what a session may spend of left, the EUR a card has
// free: left less the gate reserve, rounded to the micro-euro. It is
// negative once the gates spent into the reserve. Every admission reads
// it here: the gateway's hold, the fund's floor, the rungs' facts, the
// check-in's cost block and the raise a fund offers.
func SessionHeadroom(left, reserve float64) float64 { return RoundMicro(left - reserve) }

// GateHeld is what the gates still have of reserve out of left: the whole
// reserve, or what the card has left when that is less.
func GateHeld(left, reserve float64) float64 { return RoundMicro(min(reserve, max(left, 0))) }

// GateReserve is GateReserveEUR, or DefaultGateReserveEUR when the
// policy sets none (0), as a version pinned before the key reads it.
func (l Limits) GateReserve() float64 {
	if l.GateReserveEUR == 0 {
		return DefaultGateReserveEUR
	}
	return l.GateReserveEUR
}
