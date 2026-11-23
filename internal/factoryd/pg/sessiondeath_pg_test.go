package pg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The death of the session before a stop reaches the stop's case as typed
// facts, with the last command the session ran, so the rung that reads the
// case can restart with the right command named.
func TestOpenTripCarriesTheDeath(t *testing.T) {
	g := newTripRig(t, "success")
	death := &DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, Reason: "OOMKilled", ExitCode: 137, Container: "worker",
		Limit: "4Gi", LastCommand: "go test -race ./...", Lines: []string{"the line"}}
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: tripCap, Death: death}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := caseByID(t, g.s, out.CaseID)
	d, _ := c.Facts["death"].(map[string]any)
	if d["class"] != "own_limit" || d["rule"] != "memory_limit" || d["reason"] != "OOMKilled" || d["exit_code"] != float64(137) ||
		d["container"] != "worker" || d["limit"] != "4Gi" || d["last_command"] != "go test -race ./..." {
		t.Fatalf("case facts death = %v, want the memory limit on go test -race ./...", d)
	}
	if _, ok := c.Facts["killed"]; ok {
		t.Fatal("the case carries a killed fact beside the death")
	}
	if lines, _ := d["lines"].([]any); len(lines) != 1 || lines[0] != "the line" {
		t.Fatalf("case facts death lines = %v, want the line", d["lines"])
	}
	if _, ok := d["events"]; ok {
		t.Fatalf("case facts death = %v, want no events", d)
	}
	g2 := newTripRig(t, "success")
	plain := g2.trip(t, tripCap)
	if _, ok := caseByID(t, g2.s, plain.CaseID).Facts["death"]; ok {
		t.Fatal("a stop with no death carries a death fact")
	}
}

// A case stored with the memory kill before the death record reads back
// with it as a death: an own memory limit with the kill's exit code,
// reason, container and command. A case without one is left as it was.
func TestCaseDeathMigration(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "pg_case_death")
	migration, err := os.ReadFile(filepath.Join("..", "..", "schema", "migrations", "factoryd", "20261129100100_case_death.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO cases (id, task_id, kind, state_key, status, problem, facts) VALUES
			(801, '` + caseTask + `', 'session_cap', 'k1', 'resolved', 'p', '{"head": "h1", "killed": {"reason": "OOMKilled", "exit_code": 137, "container": "worker", "command": "go test -race ./..."}}'),
			(802, '` + caseTask + `', 'session_cap', 'k2', 'resolved', 'p', '{"head": "h2", "killed": {"reason": "OOMKilled", "exit_code": 137, "container": "agent"}}'),
			(803, '` + caseTask + `', 'session_cap', 'k3', 'resolved', 'p', '{"head": "h3"}')`,
		string(migration),
	} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
	for id, want := range map[int64]map[string]any{
		801: {"head": "h1", "death": map[string]any{"class": "own_limit", "rule": "memory_limit", "reason": "OOMKilled", "exit_code": float64(137),
			"container": "worker", "last_command": "go test -race ./..."}},
		802: {"head": "h2", "death": map[string]any{"class": "own_limit", "rule": "memory_limit", "reason": "OOMKilled", "exit_code": float64(137),
			"container": "agent"}},
		803: {"head": "h3"},
	} {
		if got := caseByID(t, s, id).Facts; !reflect.DeepEqual(got, want) {
			t.Errorf("case %d facts = %v, want %v", id, got, want)
		}
	}
	c, err := s.CaseByID(ctx, 801)
	if err != nil {
		t.Fatal(err)
	}
	var f CaseFacts
	raw, _ := json.Marshal(c.Facts)
	if err := json.Unmarshal(raw, &f); err != nil || f.Death == nil || f.Death.Rule != RuleMemoryLimit || f.Death.LastCommand != "go test -race ./..." {
		t.Fatalf("facts read as %+v, %v; want the memory limit", f.Death, err)
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
