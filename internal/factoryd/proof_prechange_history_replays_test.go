package factoryd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	client "go.temporal.io/sdk/client"
)

// [prechange_history_replays] A dev-server history recorded before the
// change, carrying a check-in decision under the moments' own name, holds
// no marker of the new change id and replays unchanged against the
// version-gated workflow: the replay schedules the same activities the
// recording holds, in the same order.
func TestProofPrechangeHistoryReplays(t *testing.T) {
	requireStoredDecisionsOnTheRungsNames(t)
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-checkin-priors-repeats.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	carries := false
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-decision"` {
			t.Fatal("the recording holds the checkin-decision marker: it is no run from before the change")
		}
		a := e.GetActivityTaskScheduledEventAttributes()
		if a == nil {
			continue
		}
		recorded = append(recorded, a.GetActivityType().GetName())
		if a.GetActivityType().GetName() == ActivityRecordStep {
			payloads := a.GetInput().GetPayloads()
			if len(payloads) == 3 && string(payloads[1].GetData()) == `"checkin"` &&
				strings.Contains(string(payloads[2].GetData()), `"decision":"human"`) {
				carries = true
			}
		}
	}
	if !carries {
		t.Fatal("the recording holds no check-in step under the moments' own decision name")
	}
	src, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), `workflow.GetVersion(ctx, "checkin-decision"`); n != 1 {
		t.Fatalf("workflow.go reads the change id checkin-decision at %d places, want the one", n)
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	if !slices.Equal(got, recorded) {
		t.Fatalf("the replay scheduled %q, the recording %q", got, recorded)
	}
}
