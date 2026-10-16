package factoryd

import (
	"fmt"
	"time"
)

// SessionFacts are the numbers a session starts with: factoryd states them
// in the brief, so no agent is asked for them (roadmap 199). A zero Attempt
// (a report or plan session) names no session.
type SessionFacts struct {
	BudgetEUR, SpentEUR float64
	TimeLeft            time.Duration
	Attempt, Sessions   int
}

func (f SessionFacts) Line() string {
	line := fmt.Sprintf("factoryd's facts for this session: budget %.2f of %.2f EUR left; %d minutes of agent time left",
		max(f.BudgetEUR-f.SpentEUR, 0), f.BudgetEUR, int(f.TimeLeft/time.Minute))
	if f.Attempt == 0 {
		return line + "."
	}
	line += fmt.Sprintf("; this is session %d of the %d the policy allows before a human is asked.", f.Attempt, f.Sessions)
	if f.Attempt >= f.Sessions {
		line += " It is the last one: push your work before it ends."
	}
	return line
}
