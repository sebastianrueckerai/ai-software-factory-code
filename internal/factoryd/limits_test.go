package factoryd

import (
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestLimitWatchWarnsOnBudgetOnce(t *testing.T) {
	w := newLimitWatch(policy.Limits{}, false, time.Unix(0, 0), 2*time.Hour)
	if got := w.due(3.9, 5, time.Unix(0, 0)); len(got) != 0 {
		t.Fatalf("78 percent warned: %v", got)
	}
	// 4.5 of 5 is 90 percent, 0.50 left
	got := w.due(4.5, 5, time.Unix(0, 0))
	want := "factoryd: 90 percent of this card's budget is spent, 0.50 of 5.00 EUR are left. " + codingWrapUp
	if len(got) != 1 || got[0].kind != warnBudget || got[0].text != want {
		t.Fatalf("due = %+v, want one budget warning %q", got, want)
	}
	w.sent(warnBudget)
	if got := w.due(4.9, 5, time.Unix(0, 0)); len(got) != 0 {
		t.Fatalf("warned again after sent: %v", got)
	}
}

// A warning that did not reach the agent comes again.
func TestLimitWatchKeepsAWarningUntilItIsSent(t *testing.T) {
	w := newLimitWatch(policy.Limits{}, false, time.Unix(0, 0), 2*time.Hour)
	if len(w.due(4.5, 5, time.Unix(0, 0))) != 1 || len(w.due(4.5, 5, time.Unix(1, 0))) != 1 {
		t.Fatal("an unsent warning must stay due")
	}
}

func TestLimitWatchWarnsOnTimeAtTheCodingShare(t *testing.T) {
	// 2h span, 80 percent: 96 minutes in; 24 minutes left
	w := newLimitWatch(policy.Limits{}, false, time.Unix(0, 0), 2*time.Hour)
	if got := w.due(0, 5, time.Unix(0, 0).Add(95*time.Minute)); len(got) != 0 {
		t.Fatalf("95 of 120 minutes warned: %v", got)
	}
	got := w.due(0, 5, time.Unix(0, 0).Add(97*time.Minute))
	if len(got) != 1 || got[0].kind != warnTime || got[0].text != "factoryd: 23 minutes of agent time are left. "+codingWrapUp {
		t.Fatalf("due = %+v", got)
	}
}

// The planner is warned at two thirds of its time and told to file.
func TestLimitWatchWarnsAPlannerAtTwoThirds(t *testing.T) {
	// 30m span, 67 percent: 20.1 minutes in
	w := newLimitWatch(policy.Limits{}, true, time.Unix(0, 0), 30*time.Minute)
	if got := w.due(0, 5, time.Unix(0, 0).Add(20*time.Minute)); len(got) != 0 {
		t.Fatalf("20 of 30 minutes warned: %v", got)
	}
	got := w.due(0, 5, time.Unix(0, 0).Add(21*time.Minute))
	if len(got) != 1 || got[0].text != "factoryd: 9 minutes of agent time are left. "+planWrapUp {
		t.Fatalf("due = %+v", got)
	}
}

func TestLimitWatchSkipsWhatHasNoBudgetOrSpan(t *testing.T) {
	w := newLimitWatch(policy.Limits{}, false, time.Unix(0, 0), 0)
	if got := w.due(1, 0, time.Unix(0, 0).Add(time.Hour)); !slices.Equal(got, nil) {
		t.Fatalf("a zero budget and a zero span warned: %v", got)
	}
}
