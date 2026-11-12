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

// A card whose session cap the project agent's change request answered,
// recorded on a dev server before the agent-draw change: factory-chat's
// REQUEST_CHANGES at the cap bought the fourth session as a rung 3 grant
// (RecordGrant). It carries no agent-draw marker, so GetVersion answers
// DefaultVersion there and the replay schedules the same activities, the
// rung 3 grant and no AgentDraw.
func TestWorkflowReplaysAnAgentChangeRequestRecordedBeforeAgentDraw(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-agent-change-request-at-cap.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	grantBy := ""
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"agent-draw"` {
			t.Fatal("the recording holds the agent-draw marker: it is no run from before")
		}
		a := e.GetActivityTaskScheduledEventAttributes()
		if name := a.GetActivityType().GetName(); name != "" {
			recorded = append(recorded, name)
			if name == ActivityRecordGrant && len(a.GetInput().GetPayloads()) > 1 {
				grantBy = string(a.GetInput().GetPayloads()[1].GetData())
			}
		}
	}
	count := func(name string) int {
		n := 0
		for _, r := range recorded {
			if r == name {
				n++
			}
		}
		return n
	}
	if count(ActivityRunSession) != 4 || count(ActivityRequestApproval) != 2 || count(ActivityRecordGrant) != 1 || grantBy != `"`+ChatUser+`"` ||
		slices.Contains(recorded, ActivityAgentDraw) {
		t.Fatalf("the recording schedules %q, grant by %s; want four sessions, two approvals and factory-chat's one rung 3 grant", recorded, grantBy)
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	if !slices.Equal(got, recorded) {
		t.Fatalf("the replay scheduled %q, the recording %q", strings.Join(got, " "), strings.Join(recorded, " "))
	}
}
