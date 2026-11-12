package factoryd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/workflow"
)

// A close that is pending when the run's issue step refuses a card with no
// issue ends the card closed, as the close decides it. A run from before
// the change ends it failed with the step's reason.
func TestACloseOfACardWithNoIssueEndsItClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		before     bool
		final, why string
	}{
		"today":  {false, "closed", "issue closed without merge"},
		"before": {true, "failed", "ensure issue: task t1 has no linked issue"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.ensureIssue = errors.New("task t1 has no linked issue")
			if tc.before {
				h.env.OnGetVersion("ensure-issue-close", workflow.DefaultVersion, versionEnsureIssueClose).Return(workflow.DefaultVersion)
			}
			h.signalAfter(time.Millisecond, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			if got := h.run(t); got != tc.final || !strings.Contains(h.finals[0][1], tc.why) {
				t.Fatalf("final %q with %q, want %q with %q", got, h.finals[0][1], tc.final, tc.why)
			}
		})
	}
}

// A run recorded before the change, whose issue step refused a card with no
// issue while a close was pending and which ended failed, replays on today's
// code: the change's marker is read only after the step's failure, which the
// recording took the old way.
func TestWorkflowReplaysAHistoryRecordedBeforeTheEnsureIssueClose(t *testing.T) {
	replayHistory(t, "testdata/devserver-ensure-issue-refused.json")
}
