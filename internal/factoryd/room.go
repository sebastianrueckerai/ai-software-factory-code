package factoryd

import (
	"math"
	"time"
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
// time per session, else a slice of the agent-time cap, never under minSession.
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
		Time:         EstimateTime(seconds(f.AgentTimeS), f.Attempts, seconds(f.AgentTimeCapS)),
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
	need := time.Duration(target)*per - timeLeft
	agentTime = max(0, (need+time.Minute-1)/time.Minute*time.Minute)
	return now, starts, agentTime
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
	moneyRoom := math.MaxInt
	if est.EUR > 0 {
		moneyRoom = int(math.Floor(r.MoneyLeft / est.EUR))
	}
	r.Now, r.Starts, r.AgentTime = SessionRoom(r.StartsLeft, r.TimeLeft, est.Time, moneyRoom, n)
	if money {
		r.EUR = fundFor(float64(r.Now+n)*est.EUR, r.MoneyLeft)
	}
	r.Limit = float64(max(0, Cents(f.BudgetEUR+r.EUR-f.CeilingEUR))) / 100
	return r
}
