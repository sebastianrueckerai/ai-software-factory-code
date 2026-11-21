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
	// 4.5 of 5 is 90 percent, 0.50 left, 0.10 of it past the 0.40 reserve
	got := w.due(4.5, 5, time.Unix(0, 0))
	want := "factoryd: 90 percent of this card's budget is spent, 0.10 of 5.00 EUR are left for its calls (the model review and the audit hold 0.40 EUR of it). " + codingWrapUp
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

// A raise during the session grows the watch's span and makes a sent
// warning due again when the new figures put the session under its share.
// 97 minutes into a 2h span the time warning went; 30 more minutes make the
// span 150, whose 80 percent is 120: due again, and due at 120. The budget
// warning went at 4.50 of 5.00; a budget of 6.00 puts 4.50 under its 4.80,
// one of 5.50 leaves it over its 4.40.
func TestLimitWatchLengthenMakesAWarningDueAgain(t *testing.T) {
	start := time.Unix(0, 0)
	w := newLimitWatch(policy.Limits{}, false, start, 2*time.Hour)
	w.sent(warnTime)
	w.sent(warnBudget)
	w.Lengthen(30*time.Minute, 30*time.Minute, 0, 4.5, 5.5, start.Add(97*time.Minute))
	if w.span != 150*time.Minute || w.done[warnTime] || !w.done[warnBudget] {
		t.Fatalf("span %v, done %v; want 150m, the time warning due again and the budget one sent", w.span, w.done)
	}
	if got := w.due(4.5, 5.5, start.Add(119*time.Minute)); len(got) != 1 || got[0].kind != warnRoom {
		t.Fatalf("due at 119m = %+v, want the room line alone", got)
	}
	w.sent(warnRoom)
	if got := w.due(4.5, 5.5, start.Add(120*time.Minute)); len(got) != 1 || got[0].text != "factoryd: 30 minutes of agent time are left. "+codingWrapUp {
		t.Fatalf("due at 120m = %+v, want the time warning with 30 minutes left", got)
	}
	w.Lengthen(0, 0, 50, 4.5, 6, start.Add(120*time.Minute))
	if w.done[warnBudget] {
		t.Fatal("a budget of 6.00 leaves the budget warning sent at 4.50")
	}
}

// The room line names only what grew, and stays due until it reaches the
// agent; two raises before it does are one line. Where the window cut the
// card's raise, it names the session's share beside it.
func TestLimitWatchOwesTheRoomLineUntilItIsSent(t *testing.T) {
	start := time.Unix(0, 0)
	line := func(w *limitWatch) string {
		for _, wn := range w.due(0, 10, start) {
			if wn.kind == warnRoom {
				return wn.text
			}
		}
		return ""
	}
	w := newLimitWatch(policy.Limits{}, false, start, time.Hour)
	w.Lengthen(time.Minute, time.Minute, 0, 0, 10, start)
	if got := line(w); got != "factoryd: the card got 1 more minute of agent time. Go on with the work in this session." {
		t.Fatalf("time alone: %q", got)
	}
	w.Lengthen(0, 0, 150, 0, 10, start)
	if got := line(w); got != "factoryd: the card got 1 more minute of agent time and 1.50 EUR. Go on with the work in this session." {
		t.Fatalf("time and money: %q", got)
	}
	w.sent(warnRoom)
	if got := line(w); got != "" {
		t.Fatalf("a sent line is due again: %q", got)
	}
	w.Lengthen(30*time.Second, 30*time.Second, 120, 0, 10, start)
	if got := line(w); got != "factoryd: the card got 1.20 EUR. Go on with the work in this session." {
		t.Fatalf("money and half a minute: %q", got)
	}
	w.sent(warnRoom)
	w.Lengthen(30*time.Second, 30*time.Second, 0, 0, 10, start)
	if got := line(w); got != "" {
		t.Fatalf("half a minute alone is owed, not told: %q", got)
	}
	w.sent(warnRoom)
	w.Lengthen(200*time.Minute, 70*time.Minute, 150, 0, 10, start)
	if got := line(w); got != "factoryd: the card got 200 more minutes of agent time and 1.50 EUR; this session may use 70 of the minutes. Go on with the work in this session." {
		t.Fatalf("a raise the window cut: %q", got)
	}
	w.sent(warnRoom)
	w.Lengthen(10*time.Minute, 0, 0, 0, 10, start)
	if got := line(w); got != "" {
		t.Fatalf("a raise the session gets none of is the next session's, not told: %q", got)
	}
}
