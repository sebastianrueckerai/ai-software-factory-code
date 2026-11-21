package factoryd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Readout is room r on f as a human reads it under its count: a row each
// for the agent time and the money, the spend limit's raise and the
// starts, each "" unless it rises, and the project's money left this
// month.
type Readout struct {
	Time, Money   RoomRow
	Limit, Starts string
	Month         string
}

// RoomRow is a row of a readout: what is left, what the press adds and
// what there is then. Adds is "" where the press adds nothing, and Not
// then says why: "enough", or "left out" for money the press leaves out.
type RoomRow struct{ Left, Adds, Not, Then string }

// EachLine says one session's worth on f and where it comes from, as it
// stands beside the count.
func EachLine(f CaseFacts) string {
	est := SessionEstimate(f)
	from := "the policy's estimate before its first session"
	if est.FromSessions > 0 {
		from = "from its " + plural(est.FromSessions, "session") + " so far"
	}
	return fmt.Sprintf("Each is about %s and %s EUR, %s.", minText(est.Time), textutil.EUR(est.EUR), from)
}

// ReadoutOf is the readout of room r on f.
func ReadoutOf(f CaseFacts, r Room) Readout {
	out := Readout{Time: RoomRow{Left: minShown(r.TimeLeft)}, Limit: r.limitLine(f), Starts: r.startsLine(), Month: MonthLine(f)}
	if r.AgentTime > 0 {
		out.Time.Adds, out.Time.Then = minText(r.AgentTime), minShown(r.TimeLeft+r.AgentTime)
	} else {
		out.Time.Not, out.Time.Then = "enough", out.Time.Left
	}
	out.Money = MoneyRow(r.MoneyLeft, r.EUR)
	if !r.Money {
		out.Money = RoomRow{Left: out.Money.Left, Not: "left out", Then: out.Money.Left}
	}
	return out
}

// MoneyRow is the row of a raise of eur on left EUR left; with nothing
// to add the money is enough.
func MoneyRow(left, eur float64) RoomRow {
	row := RoomRow{Left: eurShown(left)}
	if Cents(eur) <= 0 {
		row.Not, row.Then = "enough", row.Left
		return row
	}
	row.Adds, row.Then = textutil.EUR(eur)+" EUR", eurShown(left+eur)
	return row
}

// MonthLine says the project's money left this month on f.
func MonthLine(f CaseFacts) string {
	if slices.Contains(f.Partial, "month") {
		return "The project's money left this month could not be read."
	}
	return "The project has " + eurShown(f.MonthLeftEUR) + " left this month."
}

// limitLine says the spend limit's raise of room r on f, "" for none.
func (r Room) limitLine(f CaseFacts) string {
	if r.Limit <= 0 {
		return ""
	}
	return fmt.Sprintf("The spend limit rises from %s to %s EUR first.", textutil.EUR(f.CeilingEUR), textutil.EUR(f.CeilingEUR+r.Limit))
}

// startsLine says the starts room r adds, "" for none.
func (r Room) startsLine() string {
	if r.Starts <= 0 {
		return ""
	}
	return "It may also start " + textutil.Plural(r.Starts, "more session", "more sessions") + "."
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
	return strings.Join(slices.DeleteFunc([]string{"The room changed since this opened: " + worthWords(r.N) + " now add " + textutil.AndList(adds) + ".",
		r.limitLine(f), r.startsLine(), "Press again to give them."}, func(s string) bool { return s == "" }), " ")
}

// minShown says d in whole minutes, "none" when that is not above 0.
func minShown(d time.Duration) string {
	if d.Round(time.Minute) <= 0 {
		return "none"
	}
	return minText(d)
}

// eurShown says eur in EUR, "none" when its cents are not above 0.
func eurShown(eur float64) string {
	if Cents(eur) <= 0 {
		return "none"
	}
	return textutil.EUR(eur) + " EUR"
}

// minText says d in whole minutes, as the readout counts them: "14 min".
func minText(d time.Duration) string {
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}
