package factoryd

import (
	"fmt"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	Carries             int64     // the pull request a carry card carries, 0 for none
}

// Line is the brief's line of the facts; a carry card's names the pull
// request whose head the clone holds, which the sandbox cannot fetch.
func (f SessionFacts) Line() string {
	line := fmt.Sprintf("factoryd's facts for this session: %s of the %s EUR budget left for its calls (the model review and the audit hold %s EUR of it); %d minutes of agent time left",
		textutil.EUR(max(policy.SessionHeadroom(f.BudgetEUR-f.SpentEUR, f.ReserveEUR), 0)), textutil.EUR(f.BudgetEUR), textutil.EUR(policy.GateHeld(f.BudgetEUR-f.SpentEUR, f.ReserveEUR)), int(f.TimeLeft/time.Minute))
	switch {
	case f.Attempt == 0:
		line += "."
	default:
		line += fmt.Sprintf("; this is session %d of the %d the policy allows before a human is asked.", f.Attempt, f.Sessions)
		if _, sized := cardLineBound(f.Size, f.LargeLines); sized {
			line += fmt.Sprintf(" This card is planned at most %d changed lines (size %s).", EffectiveLineBound(f.Size, f.LargeLines, f.Usage), f.Size)
		} else {
			line += fmt.Sprintf(" This card has no size, so it is planned at most %d changed lines, the policy's large_lines.", EffectiveLineBound(f.Size, f.LargeLines, f.Usage))
		}
		if f.Attempt >= f.Sessions {
			line += " It is the last one: push your work before it ends."
		}
	}
	if f.Carries != 0 {
		line += fmt.Sprintf(" This card carries pull request #%[1]d: its head is in your clone as branch pull/%[1]d. The sandbox holds no git credential and cannot fetch it, so work from that branch.", f.Carries)
	}
	return line
}
