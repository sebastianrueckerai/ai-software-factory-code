package factoryd

import (
	"fmt"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// RoomLines are the readout of room r on f, in the order a human reads it:
// one session's worth and where it comes from, the time, the money with
// the spend limit's raise, the starts when they rise, and the project's
// money left this month.
func RoomLines(f CaseFacts, r Room) []string {
	from := "the policy's estimate before its first session"
	if r.Est.FromSessions > 0 {
		from = "from its " + plural(r.Est.FromSessions, "session") + " so far"
	}
	lines := []string{fmt.Sprintf("About %s and %s EUR per session, %s", minText(r.Est.Time), textutil.EUR(r.Est.EUR), from)}
	timeLeft := leftText(minText(r.TimeLeft), r.TimeLeft.Round(time.Minute) > 0)
	if r.AgentTime > 0 {
		lines = append(lines, fmt.Sprintf("Adds %s: %s, then %s", minText(r.AgentTime), timeLeft, minText(r.TimeLeft+r.AgentTime)))
	} else {
		lines = append(lines, "Enough: "+timeLeft)
	}
	moneyLeft := leftText(textutil.EUR(r.MoneyLeft)+" EUR", Cents(r.MoneyLeft) > 0)
	switch {
	case !r.Money:
		lines = append(lines, "Adds no money: "+moneyLeft)
	case r.EUR > 0:
		lines = append(lines, fmt.Sprintf("Adds %s EUR: %s, then %s EUR", textutil.EUR(r.EUR), moneyLeft, textutil.EUR(r.MoneyLeft+r.EUR)))
	default:
		lines = append(lines, "Enough: "+moneyLeft)
	}
	if r.Limit > 0 {
		lines = append(lines, fmt.Sprintf("The spend limit rises from %s to %s EUR first.", textutil.EUR(f.CeilingEUR), textutil.EUR(f.CeilingEUR+r.Limit)))
	}
	if r.Starts > 0 {
		lines = append(lines, "It may also start "+textutil.Plural(r.Starts, "more session", "more sessions")+".")
	}
	return append(lines, "The project has "+textutil.EUR(max(0, f.MonthLeftEUR))+" EUR left this month.")
}

// leftText says figure left, "none left" when the figure shown is not above 0.
func leftText(figure string, above bool) string {
	if !above {
		return "none left"
	}
	return figure + " left"
}

// minText says d in whole minutes, as the readout counts them: "14 min".
func minText(d time.Duration) string {
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}
