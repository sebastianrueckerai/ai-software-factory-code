package factoryd

import (
	"math"
	"testing"
	"time"
)

func roomFacts(sessionCap int, capMin float64, leftEUR float64) CaseFacts {
	return CaseFacts{
		Attempts: 3, AgentTimeS: 42 * 60, SpentEUR: 3.60, FloorEUR: 0.50,
		BudgetEUR: 6.00, CeilingEUR: 8.00, ReserveEUR: 0.30,
		SessionCap: sessionCap, AgentTimeCapS: capMin * 60, LeftEUR: leftEUR,
	}
}

func TestRoomForTheSpecRows(t *testing.T) {
	m := time.Minute
	for _, c := range []struct {
		name      string
		f         CaseFacts
		n         int
		now, strt int
		agent     time.Duration
		eur, lim  float64
	}{
		{"ahead of time", roomFacts(4, 60, 2.40), 2, 1, 2, 24 * m, 1.50, 0},
		{"session-cap stop", roomFacts(3, 60, 2.40), 1, 0, 1, 0, 0, 0},
		{"agent-time stop", roomFacts(4, 44, 2.40), 1, 0, 0, 12 * m, 0, 0},
		{"money stop", roomFacts(5, 72, 0.70), 1, 0, 0, 0, 0.80, 0},
	} {
		r := RoomFor(c.f, c.n, true)
		if r.Now != c.now || r.Starts != c.strt || r.AgentTime != c.agent || math.Abs(r.EUR-c.eur) > 1e-9 || math.Abs(r.Limit-c.lim) > 1e-9 {
			t.Errorf("%s: got now %d starts %d time %v eur %v limit %v", c.name, r.Now, r.Starts, r.AgentTime, r.EUR, r.Limit)
		}
	}
}

func TestRoomForRaisesTheLimitFirst(t *testing.T) {
	f := roomFacts(4, 60, 2.40)
	f.CeilingEUR = 7.00
	r := RoomFor(f, 2, true)
	if math.Abs(r.EUR-1.50) > 1e-9 || math.Abs(r.Limit-0.50) > 1e-9 {
		t.Errorf("got eur %v limit %v", r.EUR, r.Limit)
	}
}

func TestRoomForMoneyLeftOut(t *testing.T) {
	r := RoomFor(roomFacts(5, 72, 0.70), 1, false)
	if r.Now != 2 || r.Starts != 1 || r.AgentTime != 12*time.Minute || math.Abs(r.EUR) > 1e-9 || r.Limit != 0 || math.Abs(r.MoneyLeft-0.40) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestSessionEstimateBeforeTheFirstSession(t *testing.T) {
	got := SessionEstimate(CaseFacts{AgentTimeCapS: 60 * 60, FloorEUR: 0.80})
	if want := (Estimate{15 * time.Minute, 0.80, 0}); got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestSessionEstimateFloors(t *testing.T) {
	got := SessionEstimate(CaseFacts{Attempts: 3, AgentTimeS: 6 * 60, SpentEUR: 0.30, FloorEUR: 0.50})
	if want := (Estimate{5 * time.Minute, 0.50, 3}); got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestRoomForAPriceOfZero(t *testing.T) {
	r := RoomFor(CaseFacts{SessionCap: 1}, 1, true)
	if r.Est != (Estimate{5 * time.Minute, 0, 0}) || r.Now != 0 || r.Starts != 0 || r.AgentTime != 5*time.Minute || math.Abs(r.EUR) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestRoomForMoneySpentIntoTheReserve(t *testing.T) {
	r := RoomFor(roomFacts(5, 72, 0.10), 1, true)
	if math.Abs(r.MoneyLeft+0.20) > 1e-9 || r.Now != 0 || math.Abs(r.EUR-1.40) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestSessionRoomWithoutMoney(t *testing.T) {
	now, starts, agent := SessionRoom(0, 18*time.Minute, 14*time.Minute, math.MaxInt, 1)
	if now != 0 || starts != 1 || agent != 0 {
		t.Errorf("got %d %d %v", now, starts, agent)
	}
}

func TestRoomForAPriceOfZeroInTheReserve(t *testing.T) {
	r := RoomFor(CaseFacts{SessionCap: 1, ReserveEUR: 0.30}, 1, true)
	if math.Abs(r.EUR) > 1e-9 {
		t.Errorf("got eur %v", r.EUR)
	}
}

func TestSessionRoomRoundsMinutesUp(t *testing.T) {
	now, starts, agent := SessionRoom(1, 10*time.Minute, 14*time.Minute+30*time.Second, math.MaxInt, 1)
	if now != 0 || starts != 0 || agent != 5*time.Minute {
		t.Errorf("got %d %d %v", now, starts, agent)
	}
	r := RoomFor(CaseFacts{Attempts: 2, AgentTimeS: 29 * 60, SpentEUR: 2, FloorEUR: 0.5, SessionCap: 3, AgentTimeCapS: 39 * 60, LeftEUR: 5, ReserveEUR: 0}, 1, false)
	if r.Est.Time != 14*time.Minute+30*time.Second || r.Now != 0 || r.AgentTime != 5*time.Minute {
		t.Errorf("got %+v", r)
	}
}
