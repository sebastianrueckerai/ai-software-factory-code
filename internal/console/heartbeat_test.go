package console

import (
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// PulseState answers the heartbeat's two questions from the same
// snapshot: work sits open, and a card waits on a human.
func TestPulseStateReadsTheBoard(t *testing.T) {
	r := newRig(t)
	r.approvals.rows = nil
	for status, want := range map[string][2]bool{
		"running": {true, false}, "queued": {false, false}, "waiting-human": {false, true},
	} {
		r.tasks.set("", factoryd.Task{ID: "t-hb", Kind: "agent", Status: status, Author: "alice", Repository: "sf/code", IssueNumber: 3, Objective: "Beat."})
		open, waiting, err := r.c.PulseState(t.Context(), projectRows()[0])
		if err != nil || open != want[0] || waiting != want[1] {
			t.Errorf("%s: PulseState = %v, %v, %v; want %v, %v", status, open, waiting, err, want[0], want[1])
		}
	}
}
