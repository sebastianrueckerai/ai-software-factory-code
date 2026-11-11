package factoryd

import (
	"context"
	"testing"
)

// The kill of the session before a stop reaches the stop's case as a typed
// fact, with the last command the session ran, so the rung that reads the
// case can restart with the right command named.
func TestOpenTripCarriesTheSessionKill(t *testing.T) {
	g := newTripRig(t, "success")
	kill := &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "worker", Command: "go test -race ./..."}
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: tripCap, Killed: kill}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := caseByID(t, g.s, out.CaseID).Facts["killed"].(map[string]any)
	if k["reason"] != "OOMKilled" || k["exit_code"] != float64(137) || k["container"] != "worker" || k["command"] != "go test -race ./..." {
		t.Fatalf("case facts killed = %v, want the OOM kill on go test -race ./...", k)
	}
	g2 := newTripRig(t, "success")
	plain := g2.trip(t, tripCap)
	if _, ok := caseByID(t, g2.s, plain.CaseID).Facts["killed"]; ok {
		t.Fatal("a stop with no kill carries a killed fact")
	}
}

// A retried trip finds the case an attempt before it opened for the same
// trip even when the retry reads the stop as another kind, as a retry on a
// newer binary across a roll does: one case per stop.
func TestOpenTripRetryAcrossAKindChangeKeepsOneCase(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	reason := "check-in: " + CheckTimeCap + ": check-in time cap reached after 10m0s"
	first, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: reason, Kind: CaseCheckinShare, StateKey: "old"}, tripAttempt{ID: "run/a", N: 1})
	if err != nil {
		t.Fatal(err)
	}
	again, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: reason}, tripAttempt{ID: "run/a", N: 2})
	if err != nil || again != first {
		t.Fatalf("retry = %+v, %v; want the first attempt's %+v", again, err, first)
	}
	cases, err := g.s.CasesOf(ctx, "t1")
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases = %d, %v; want one", len(cases), err)
	}
}
