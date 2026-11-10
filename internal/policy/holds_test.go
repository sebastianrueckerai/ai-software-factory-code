package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The gate reserve holds a card's budget for its gates (issue #1636): a
// session's call fits budget - spent - reserve, and its refused_budget
// refusal at the session ceiling — the money stop — names what the card
// spent and what the model review and the audit keep. A gate call keeps
// the full budget - spent: the review and the audit of a card whose
// sessions spent to the ceiling run without a fund, and a grant raises
// the ceiling by its grant alone, the reserve whole.
func TestGateReserveCeiling(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('card', 5, 4.75, 'running')`); err != nil {
		t.Fatal(err)
	}
	session := HoldRequest{TaskID: "card", Kind: KindModel, Ref: "small", Session: "s1", ReserveEUR: DefaultGateReserveEUR,
		Bytes: 1000, Price: testPrice, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf}

	// the session ceiling is the 0.25 EUR headroom less the reserve: the
	// call is refused with the money stop's figures
	var r *Refusal
	if _, err := TakeHold(ctx, db, session); !errors.As(err, &r) || r.Decision != RefusedBudget {
		t.Fatalf("session call at the ceiling = %v, want refused_budget", err)
	}
	for _, want := range []string{"task 0.000000", "the card spent 4.75 EUR, and 0.25 EUR is held for the model review and the audit"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("session refusal = %q, want it to name %q", r.Reason, want)
		}
	}

	// the review, without the reserve, fits the same headroom; released
	// at its real cost it leaves the audit room too: both gates run
	// without a fund
	review := session
	review.Ref, review.Session, review.ReserveEUR = "review/small", "", 0
	h, err := TakeHold(ctx, db, review)
	if err != nil {
		t.Fatalf("review at the ceiling: %v", err)
	}
	cost := 0.10
	if _, err := ReleaseHold(ctx, db, h, &cost, Usage{}); err != nil {
		t.Fatal(err)
	}
	audit := review
	audit.Ref = "audit/small"
	h, err = TakeHold(ctx, db, audit)
	if err != nil {
		t.Fatalf("audit at the ceiling: %v", err)
	}
	cost = 0.05
	if _, err := ReleaseHold(ctx, db, h, &cost, Usage{}); err != nil {
		t.Fatal(err)
	}

	// the card spent 4.90 EUR of its 5: the sessions' ceiling is still
	// below the floor of a call, and the stop names the new spent figure
	if _, err := TakeHold(ctx, db, session); !errors.As(err, &r) || r.Decision != RefusedBudget {
		t.Fatalf("session call after the gates = %v, want refused_budget", err)
	}
	if !strings.Contains(r.Reason, "the card spent 4.90 EUR, and 0.25 EUR is held for the model review and the audit") {
		t.Errorf("session refusal after the gates = %q, want it to name the card's spent 4.90 EUR", r.Reason)
	}

	// a grant of 1.00 EUR raises the budget, and so the session ceiling,
	// by its grant alone: the session fits, the reserve never stored nor
	// spent
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET budget_eur = 6 WHERE id = 'card'`); err != nil {
		t.Fatal(err)
	}
	h, err = TakeHold(ctx, db, session)
	if err != nil {
		t.Fatalf("session call after the grant: %v", err)
	}
	if !near(h.EUR, 0.85) { // 6 - 4.90 - 0.25, the input beside the output
		t.Errorf("session hold after the grant = %.6f EUR, want 0.85: the grant alone raised the ceiling", h.EUR)
	}
}

// A factoryd call's refusal keeps its reason: no reserve held out, no
// money stop appended — the stop is a session call's alone.
func TestGateReserveStopIsTheSessions(t *testing.T) {
	f := Free{Task: 0, Day: 1, Month: 1, Project: inf, Reserve: DefaultGateReserveEUR, Spent: 4.75}
	if got := f.stop(RefusedBudget, "headroom"); !strings.Contains(got, "the card spent 4.75 EUR, and 0.25 EUR is held for the model review and the audit") {
		t.Errorf("session stop = %q, want it to name the card's figures", got)
	}
	f.Reserve = 0
	if got := f.stop(RefusedBudget, "headroom"); got != "headroom" {
		t.Errorf("factoryd refusal = %q, want the reason unchanged", got)
	}
	f.Reserve = DefaultGateReserveEUR
	if got := f.stop(RefusedDayCap, "headroom"); got != "headroom" {
		t.Errorf("day-cap refusal = %q, want the reason unchanged: the stop belongs to a budget refusal alone", got)
	}
}
