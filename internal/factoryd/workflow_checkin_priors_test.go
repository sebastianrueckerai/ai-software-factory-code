package factoryd

import (
	"path/filepath"
	"strings"
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

// A run recorded before the checkin-mark change stops at a check-in and
// replays on today's code with no nondeterminism: the changed path runs at
// that stop. The replay compares commands, not the trip's input, so it does
// not pin the version gate; the "before the change" case of
// TestARestartedRunFormsTheDeadlockAskAtTheSecondEqualStop does.
func TestWorkflowReplaysACheckinStopRecordedBeforeTheMarkWasKept(t *testing.T) {
	f := filepath.Join("testdata", "devserver-checkin-priors-repeats.json")
	hist := loadHistory(t, f)
	stopped := false
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-mark"` {
			t.Fatal("the recording holds a checkin-mark marker: it is no run from before the change")
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil && a.GetActivityType().GetName() == ActivityOpenTrip &&
			strings.Contains(string(a.GetInput().GetPayloads()[0].GetData()), `"Reason":"check-in: `) {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("the recording opens no check-in stop's trip: the change's code never runs in its replay")
	}
	replayHistory(t, f)
}
