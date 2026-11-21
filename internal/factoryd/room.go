package factoryd

import (
	"math"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// MaxRoomSessions is the most sessions' worth one press of "Give more room" gives.
const MaxRoomSessions = 5

// Estimate is one session's worth on a card. FromSessions is the count of
// sessions it is read from; 0 means the policy's estimate.
type Estimate struct {
	Time         time.Duration
	EUR          float64
	FromSessions int
}

// EstimateTime is the agent time of one session's worth: the card's agent
// time per session, else a slice of its sized agent-time cap, capTime,
// which no time given since moves; never under minSession.
func EstimateTime(agentTime time.Duration, sessions int, capTime time.Duration) time.Duration {
	per := AnswerSlice(capTime)
	if sessions > 0 {
		per = agentTime / time.Duration(sessions)
	}
	return max(per, minSession)
}

// SessionEstimate is f's session's worth, with the money never under the
// opening floor of the next session's model.
func SessionEstimate(f CaseFacts) Estimate {
	eur := f.FloorEUR
	if f.Attempts > 0 {
		eur = max(f.SpentEUR/float64(f.Attempts), f.FloorEUR)
	}
	return Estimate{
		Time:         EstimateTime(seconds(f.AgentTimeS), f.Attempts, seconds(f.SizedTimeS)),
		EUR:          eur,
		FromSessions: f.Attempts,
	}
}

// SessionRoom is the room now, in sessions' worth of per, and the starts and
// agent time, rounded up to whole minutes, that make it now+n. moneyRoom is
// math.MaxInt when money does not limit.
func SessionRoom(startsLeft int, timeLeft, per time.Duration, moneyRoom, n int) (now, starts int, agentTime time.Duration) {
	now = max(0, min(startsLeft, int(timeLeft/per), moneyRoom))
	target := now + n
	starts = max(0, target-startsLeft)
	agentTime = max(0, wholeMinutes(time.Duration(target)*per-timeLeft))
	return now, starts, agentTime
}

// wholeMinutes is d rounded up to a whole minute, the grain of every agent
// time a room or a grant adds.
func wholeMinutes(d time.Duration) time.Duration {
	return (d + time.Minute - 1) / time.Minute * time.Minute
}

// Room is the card's room now and the raises that make it n more sessions' worth.
type Room struct {
	N          int
	Money      bool
	Est        Estimate
	Now        int
	StartsLeft int
	TimeLeft   time.Duration
	MoneyLeft  float64
	Starts     int
	AgentTime  time.Duration
	EUR, Limit float64
}

// RoomFor is the raises that give f room for n more sessions' worth; the
// money is left out when money is false.
func RoomFor(f CaseFacts, n int, money bool) Room {
	est := SessionEstimate(f)
	r := Room{
		N: n, Money: money, Est: est,
		StartsLeft: f.SessionCap - f.Attempts,
		TimeLeft:   seconds(f.AgentTimeCapS - f.AgentTimeS),
		MoneyLeft:  f.SessionLeft(),
	}
	priced := money && est.EUR > 0
	moneyRoom := math.MaxInt
	if priced {
		moneyRoom = int(math.Floor(r.MoneyLeft / est.EUR))
	}
	r.Now, r.Starts, r.AgentTime = SessionRoom(r.StartsLeft, r.TimeLeft, est.Time, moneyRoom, n)
	if priced {
		r.EUR = fundFor(float64(r.Now+n)*est.EUR, r.MoneyLeft)
	}
	if r.EUR > 0 {
		r.Limit = float64(max(0, Cents(f.BudgetEUR+r.EUR-f.CeilingEUR))) / 100
	}
	return r
}

// RoomGuard are the args a human's press of a room posts beside its count
// and its tick: the room the card had and the raises its readout showed
// (Room.Shown), which Act works out again at the press. Any give changes
// one of them, so a second press on one readout meets another room.
var RoomGuard = []string{"shown_now", "shown_starts", "shown_agent_time_s", "shown_eur", "shown_limit"}

// Shown are r's room now and its raises by RoomGuard's names, as its
// readout's press posts them.
func (r Room) Shown() map[string]string {
	return map[string]string{"shown_now": strconv.Itoa(r.Now), "shown_starts": strconv.Itoa(r.Starts),
		"shown_agent_time_s": strconv.FormatInt(int64(r.AgentTime/time.Second), 10), "shown_eur": textutil.EUR(r.EUR), "shown_limit": textutil.EUR(r.Limit)}
}
