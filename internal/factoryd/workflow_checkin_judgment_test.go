package factoryd

import (
	"path/filepath"
	"testing"
)

// A run recorded on a dev server under the change carries its marker and
// replays on today's code, whose minimum for the change is its version.
func TestWorkflowReplaysAHistoryRecordedUnderCheckinJudgment(t *testing.T) {
	f := filepath.Join("testdata", "devserver-checkin-judgment-session.json")
	hist := loadHistory(t, f)
	marked := false
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-judgment"` {
			marked = true
		}
	}
	if !marked {
		t.Fatal("the recording holds no checkin-judgment marker: it is no run under the change")
	}
	replayHistory(t, f)
}
