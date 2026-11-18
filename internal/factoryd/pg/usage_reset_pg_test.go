package pg

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A reset starts a card's counters over and keeps its line raise, which the
// card that lands its work inherits; the raised sessions and agent time go.
func TestResetTaskUsageKeepsExtraLines(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "usage_reset")
	if err := s.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Status: "running", Objective: "x", Repository: "sf/r", Track: policy.TrackDirect,
		Usage: TaskUsage{ExtraLines: 600, RaisedSessions: 1, RaisedTime: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskUsage(ctx, "t1", TaskUsage{AgentTime: time.Minute, Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetTaskUsage(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Task(ctx, "t1")
	if err != nil || got.Usage != (TaskUsage{ExtraLines: 600}) {
		t.Fatalf("usage after the reset = %+v, %v; want 600 extra lines, 0 raised sessions, 0 raised time and the counters at zero", got.Usage, err)
	}
}
