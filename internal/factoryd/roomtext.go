package factoryd

import (
	"fmt"
	"slices"
	"strings"
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
	lines = append(lines, r.alsoLines(f)...)
	if slices.Contains(f.Partial, "month") {
		return append(lines, "The project's money left this month could not be read.")
	}
	return append(lines, "The project has "+leftText(textutil.EUR(f.MonthLeftEUR)+" EUR", Cents(f.MonthLeftEUR) > 0)+" this month.")
}

// alsoLines are the readout's lines of what room r raises beside the time
// and the money: the spend limit first, then the starts.
func (r Room) alsoLines(f CaseFacts) []string {
	var lines []string
	if r.Limit > 0 {
		lines = append(lines, fmt.Sprintf("The spend limit rises from %s to %s EUR first.", textutil.EUR(f.CeilingEUR), textutil.EUR(f.CeilingEUR+r.Limit)))
	}
	if r.Starts > 0 {
		lines = append(lines, "It may also start "+textutil.Plural(r.Starts, "more session", "more sessions")+".")
	}
	return lines
}

// movedText is the refusal of a press whose readout showed a room other
// than r on f: the card ran on, or someone gave room, between the two, so
// it names r's raises as they are now.
func (r Room) movedText(f CaseFacts) string {
	var adds []string
	if r.AgentTime > 0 {
		adds = append(adds, minText(r.AgentTime))
	}
	if r.EUR > 0 {
		adds = append(adds, textutil.EUR(r.EUR)+" EUR")
	}
	if len(adds) == 0 {
		adds = []string{"no time and no money"}
	}
	return strings.Join(append([]string{"The room changed since this opened: " + worthWords(r.N) + " now add " + textutil.AndList(adds) + "."},
		append(r.alsoLines(f), "Press again to give them.")...), " ")
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
