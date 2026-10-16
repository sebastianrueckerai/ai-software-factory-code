package factoryd

import (
	"fmt"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The kinds of warning a session gets, once each.
const (
	warnBudget = "budget"
	warnTime   = "time"
)

const (
	codingWrapUp = "Finish what runs, run bash check if it is quick, and end the session with your last message, so the factory pushes your work."
	planWrapUp   = "File the plan now with the file_plan tool, with what you have read: a plan filed late is better than none."
)

type warning struct{ kind, text string }

// limitWatch decides which wrap-up warning a running session is due: its
// card's budget spent, or its agent time used, past the policy's share.
// A warning stays due until sent says it reached the agent.
type limitWatch struct {
	share   float64
	wrapUp  string
	started time.Time
	span    time.Duration
	done    map[string]bool
}

func newLimitWatch(l policy.Limits, planner bool, started time.Time, span time.Duration) *limitWatch {
	w := &limitWatch{share: l.WarnShare(), wrapUp: codingWrapUp, started: started, span: span, done: map[string]bool{}}
	if planner {
		w.share, w.wrapUp = l.PlanWarnShare(), planWrapUp
	}
	return w
}

func (w *limitWatch) due(spent, budget float64, now time.Time) []warning {
	var out []warning
	if !w.done[warnBudget] && budget > 0 && spent >= w.share*budget {
		out = append(out, warning{warnBudget, fmt.Sprintf("factoryd: %.0f percent of this card's budget is spent, %.2f of %.2f EUR are left. %s",
			100*spent/budget, max(budget-spent, 0), budget, w.wrapUp)})
	}
	if elapsed := now.Sub(w.started); !w.done[warnTime] && w.span > 0 && float64(elapsed) >= w.share*float64(w.span) {
		out = append(out, warning{warnTime, fmt.Sprintf("factoryd: %d minutes of agent time are left. %s", int((w.span-elapsed)/time.Minute), w.wrapUp)})
	}
	return out
}

func (w *limitWatch) sent(kind string) { w.done[kind] = true }
