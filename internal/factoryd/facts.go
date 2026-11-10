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
	ReserveEUR          float64 // the gate reserve a session's calls leave to the gates
	TimeLeft            time.Duration
	Attempt, Sessions   int
	Size                string    // the card's size; "" for an unsized card
	LargeLines          int       // the policy's risk.large_lines
	Usage               TaskUsage // the card's usage, whose added lines raise the bound
}

func (f SessionFacts) Line() string {
	line := fmt.Sprintf("factoryd's facts for this session: %.2f of the %.2f EUR budget left for its calls (the model review and the audit hold %.2f EUR of it); %d minutes of agent time left",
		max(f.BudgetEUR-f.SpentEUR-f.ReserveEUR, 0), f.BudgetEUR, f.ReserveEUR, int(f.TimeLeft/time.Minute))
	if f.Attempt == 0 {
		return line + "."
	}
	line += fmt.Sprintf("; this is session %d of the %d the policy allows before a human is asked.", f.Attempt, f.Sessions)
	if _, sized := cardLineBound(f.Size, f.LargeLines); sized {
		line += fmt.Sprintf(" This card is planned at most %d changed lines (size %s).", EffectiveLineBound(f.Size, f.LargeLines, f.Usage), f.Size)
	} else {
		line += fmt.Sprintf(" This card has no size, so it is planned at most %d changed lines, the policy's large_lines.", EffectiveLineBound(f.Size, f.LargeLines, f.Usage))
	}
	if f.Attempt >= f.Sessions {
		line += " It is the last one: push your work before it ends."
	}
	return line
}
