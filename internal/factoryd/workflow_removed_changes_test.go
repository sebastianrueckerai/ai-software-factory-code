package factoryd

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

// The dev-server recordings made while the workflow still read the change
// ids older than cases hold the marker of each of them read at a run's
// start, and each replays on today's code, which reads none of them: the
// replay skips the marker of a call that is gone.
func TestWorkflowReplaysHistoriesHoldingTheMarkersOfRemovedChanges(t *testing.T) {
	lazy := []string{"checkin-answer", "conflict-fact", "stopped-parent", "verified-head"}
	var atStart []string
	for _, id := range removedChangeIDs {
		if !slices.Contains(lazy, id) && id != "split" && id != "review-holds-merge" {
			atStart = append(atStart, id)
		}
	}
	if len(atStart) != 81 {
		t.Fatalf("%d removed ids read at the start, want the 81 older than cases", len(atStart))
	}
	for _, f := range []string{"devserver-checkin-judgment-session.json", "devserver-last-round-findings-approval.json",
		"devserver-closing-restart.json", "devserver-report-closing-restart.json", "devserver-report-gate-escalate.json"} {
		t.Run(f, func(t *testing.T) {
			path := filepath.Join("testdata", f)
			var held []string
			for _, e := range loadHistory(t, path).Events {
				id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads()
				var name string
				if len(id) == 1 && json.Unmarshal(id[0].GetData(), &name) == nil && slices.Contains(removedChangeIDs, name) {
					held = append(held, name)
				}
			}
			slices.Sort(held)
			want := slices.Sorted(slices.Values(atStart))
			if !slices.Equal(held, want) {
				t.Fatalf("the recording holds the removed markers %q, want %q", held, want)
			}
			replayHistory(t, path)
		})
	}
}
