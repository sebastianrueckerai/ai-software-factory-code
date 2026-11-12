package factoryd

import (
	"path/filepath"
	"testing"
)

// A run recorded on a dev server under checkin-priors carries its marker and
// replays on today's code: the session's ask lists the earlier findings, and
// the finding the review worded in other case counts under the earlier one.
func TestWorkflowReplaysAHistoryRecordedUnderCheckinPriors(t *testing.T) {
	f := filepath.Join("testdata", "devserver-checkin-priors-repeats.json")
	hist := loadHistory(t, f)
	marked := false
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-priors"` {
			marked = true
		}
	}
	if !marked {
		t.Fatal("the recording holds no checkin-priors marker: it is no run under the change")
	}
	replayHistory(t, f)
}
