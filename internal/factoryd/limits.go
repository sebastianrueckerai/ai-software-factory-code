package factoryd

import (
	"fmt"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The kinds of warning a session gets, once each, and the line that tells
// it of a raise.
const (
	warnBudget = "budget"
	warnTime   = "time"
	warnRoom   = "room"
)

const (
	codingWrapUp = "Finish what runs, run bash check changed if it is quick, and end the session with your last message, so the factory pushes your work."
	planWrapUp   = "File the plan now with the file_plan tool, with what you have read: a plan filed late is better than none."
)

type warning struct{ kind, text string }

// limitWatch decides which wrap-up warning a running session is due: its
// card's budget spent, or its agent time used, past the policy's share.
// A warning stays due until sent says it reached the agent, and so does
// the line on a raise the session took.
type limitWatch struct {
	share   float64
	reserve float64
	wrapUp  string
	started time.Time
	span    time.Duration
	done    map[string]bool
	// failed holds the kinds whose send failed at least once
	failed map[string]bool
	// added and budget are the agent time added to the card's cap and its
	// budget as the session last took them
	added  time.Duration
	budget float64
	// owedTime and owedCents are the raise the agent was not told of yet
	owedTime  time.Duration
	owedCents int64
}

func newLimitWatch(l policy.Limits, planner bool, started time.Time, span time.Duration) *limitWatch {
	w := &limitWatch{share: l.WarnShare(), reserve: l.GateReserve(), wrapUp: codingWrapUp, started: started, span: span, done: map[string]bool{}, failed: map[string]bool{}}
	if planner {
		w.share, w.wrapUp = l.PlanWarnShare(), planWrapUp
	}
	return w
}

// Lengthen takes a raise into the session: d more agent time into its span
// and cents more money, both owed to the agent in one line. A warning sent
// is due again when the span or the budget puts the session under its
// share, spent being the card's spend.
func (w *limitWatch) Lengthen(d time.Duration, cents int64, spent, budget float64, now time.Time) {
	w.span += d
	w.owedTime += d
	w.owedCents += cents
	if float64(now.Sub(w.started)) < w.share*float64(w.span) {
		delete(w.done, warnTime)
	}
	if spent < w.share*budget {
		delete(w.done, warnBudget)
	}
}

func (w *limitWatch) due(spent, budget float64, now time.Time) []warning {
	var out []warning
	if line := w.roomLine(); line != "" {
		out = append(out, warning{warnRoom, line})
	}
	if !w.done[warnBudget] && budget > 0 && spent >= w.share*budget {
		out = append(out, warning{warnBudget, fmt.Sprintf("factoryd: %.0f percent of this card's budget is spent, %s of %s EUR are left for its calls (the model review and the audit hold %s EUR of it). %s",
			100*spent/budget, textutil.EUR(max(policy.SessionHeadroom(budget-spent, w.reserve), 0)), textutil.EUR(budget), textutil.EUR(policy.GateHeld(budget-spent, w.reserve)), w.wrapUp)})
	}
	// the share applies to the span left when the pod is ready, not to the card's whole cap
	if elapsed := now.Sub(w.started); !w.done[warnTime] && w.span > 0 && float64(elapsed) >= w.share*float64(w.span) {
		out = append(out, warning{warnTime, fmt.Sprintf("factoryd: %d minutes of agent time are left. %s", int((w.span-elapsed)/time.Minute), w.wrapUp)})
	}
	return out
}

// roomLine tells the agent the raise it is owed, naming only the parts that
// grew; "" for none.
func (w *limitWatch) roomLine() string {
	var got []string
	if m := int(w.owedTime / time.Minute); m > 0 {
		got = append(got, plural(m, "more minute")+" of agent time")
	}
	if w.owedCents > 0 {
		got = append(got, textutil.EUR(float64(w.owedCents)/100)+" EUR")
	}
	if len(got) == 0 {
		return ""
	}
	return "factoryd: the card got " + strings.Join(got, " and ") + ". Go on with the work in this session."
}

func (w *limitWatch) sent(kind string) {
	if kind == warnRoom {
		w.owedTime, w.owedCents = 0, 0
		return
	}
	w.done[kind] = true
}
