package factoryd

import (
	"testing"
	"time"
)

func TestSessionFactsLine(t *testing.T) {
	// 5.00 budget, 1.80 spent: 3.20 left; 71m30s is 71 whole minutes
	f := SessionFacts{BudgetEUR: 5, SpentEUR: 1.8, TimeLeft: 71*time.Minute + 30*time.Second, Attempt: 2, Sessions: 3, Size: "medium", LargeLines: 1000}
	want := "factoryd's facts for this session: budget 3.20 of 5.00 EUR left; 71 minutes of agent time left; this is session 2 of the 3 the policy allows before a human is asked. This card is planned at most 600 changed lines (size medium)."
	if got := f.Line(); got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
	f.Attempt = 3
	last := "factoryd's facts for this session: budget 3.20 of 5.00 EUR left; 71 minutes of agent time left; this is session 3 of the 3 the policy allows before a human is asked. This card is planned at most 600 changed lines (size medium). It is the last one: push your work before it ends."
	if got := f.Line(); got != last {
		t.Fatalf("last session: %q, want %q", got, last)
	}
}

func TestSessionFactsUnsizedCardSaysSo(t *testing.T) {
	f := SessionFacts{BudgetEUR: 5, SpentEUR: 1.8, TimeLeft: 71 * time.Minute, Attempt: 1, Sessions: 3, LargeLines: 1000}
	want := "factoryd's facts for this session: budget 3.20 of 5.00 EUR left; 71 minutes of agent time left; this is session 1 of the 3 the policy allows before a human is asked. This card has no size, so it is planned at most 1000 changed lines, the policy's large_lines."
	if got := f.Line(); got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
}

func TestSessionFactsWithoutAnAttemptNamesNoSession(t *testing.T) {
	f := SessionFacts{BudgetEUR: 2, SpentEUR: 2.5, TimeLeft: 90 * time.Second}
	want := "factoryd's facts for this session: budget 0.00 of 2.00 EUR left; 1 minutes of agent time left."
	if got := f.Line(); got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
}

func TestSessionFactsWithoutABudgetShowsZero(t *testing.T) {
	// no budget, nothing spent: 0.00 of 0.00 left; 5m is 5 minutes
	f := SessionFacts{TimeLeft: 5 * time.Minute}
	want := "factoryd's facts for this session: budget 0.00 of 0.00 EUR left; 5 minutes of agent time left."
	if got := f.Line(); got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
}
