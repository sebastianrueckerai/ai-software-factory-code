package factoryd

import (
	"testing"
	"time"
)

func TestSessionFactsLine(t *testing.T) {
	// 5.00 budget, 1.80 spent: 3.20 left; 71m30s is 71 whole minutes
	f := SessionFacts{BudgetEUR: 5, SpentEUR: 1.8, TimeLeft: 71*time.Minute + 30*time.Second, Attempt: 2, Sessions: 3}
	want := "factoryd's facts for this session: budget 3.20 of 5.00 EUR left; 71 minutes of agent time left; this is session 2 of the 3 the policy allows before a human is asked."
	if got := f.Line(); got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
	f.Attempt = 3
	last := "factoryd's facts for this session: budget 3.20 of 5.00 EUR left; 71 minutes of agent time left; this is session 3 of the 3 the policy allows before a human is asked. It is the last one: push your work before it ends."
	if got := f.Line(); got != last {
		t.Fatalf("last session: %q, want %q", got, last)
	}
}

func TestSessionFactsWithoutAnAttemptNamesNoSession(t *testing.T) {
	f := SessionFacts{BudgetEUR: 2, SpentEUR: 2.5, TimeLeft: 90 * time.Second}
	want := "factoryd's facts for this session: budget 0.00 of 2.00 EUR left; 1 minutes of agent time left."
	if got := f.Line(); got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
}
