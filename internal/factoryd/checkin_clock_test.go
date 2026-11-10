package factoryd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/workflow"
)

func checkinClockRun(t *testing.T, pinned bool) *workflowHarness {
	t.Helper()
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	if pinned {
		h.env.OnGetVersion("checkin-clock", workflow.DefaultVersion, versionCheckinClock).Return(workflow.DefaultVersion)
	}
	deadSessions(h,
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped again",
			CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: "go on",
				Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1", Duration: 40 * time.Second}}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	return h
}

// Every session here takes one minute of the workflow's clock. The second
// one's check-in took 40 seconds of it, which is no agent time: the card
// has used 1m after s1, 1m + (1m - 40s) = 1m20s after s2 and 2m20s after s3.
// A run from before the change counts the whole minute: 1m, 2m, 3m.
func TestWorkflowCheckinTimeIsNoAgentTime(t *testing.T) {
	for name, tc := range map[string]struct {
		pinned bool
		spent  []time.Duration
	}{
		"gated":  {false, []time.Duration{time.Minute, 80 * time.Second, 140 * time.Second}},
		"pinned": {true, []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			h := checkinClockRun(t, tc.pinned)
			var spent []time.Duration
			for _, u := range h.usage {
				spent = append(spent, u.AgentTime)
			}
			if len(spent) < 3 || spent[0] != tc.spent[0] || spent[1] != tc.spent[1] || spent[2] != tc.spent[2] {
				t.Fatalf("agent time recorded = %v, want %v first", spent, tc.spent)
			}
		})
	}
}

// The session that opens with a check-in gets the longest check-in a
// policy allows on top of its time, so the check-in never eats the
// session's own time; one with no check-in gets nothing extra.
func TestWorkflowSessionWithACheckinGetsTheCheckinsTimeOnTop(t *testing.T) {
	h := checkinClockRun(t, false)
	if got := []time.Duration{h.sessions[0].CheckinExtra, h.sessions[1].CheckinExtra, h.sessions[2].CheckinExtra}; got[0] != 0 || got[1] != 30*time.Minute || got[2] != 0 {
		t.Fatalf("extra time of the three sessions = %v, want 0, 30m (policy.MaxCheckinTime), 0", got)
	}
	if got := h.sessions[1].Remaining; got != 19*time.Minute {
		t.Fatalf("remaining of the check-in's session = %v, want 19m: the 20m cap less s1's 1m", got)
	}
	h = checkinClockRun(t, true)
	if h.sessions[1].CheckinExtra != 0 {
		t.Fatalf("a run from before the change gave extra time %v", h.sessions[1].CheckinExtra)
	}
}

// A history recorded before the change ids carries no checkin-clock and no
// session-kill marker: it replays on the old branch, with the session's
// activity scheduled for its own time alone.
func TestWorkflowReplaysAHistoryFromBeforeCheckinClock(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "checkin-clock") || strings.Contains(string(data), "session-kill") || !strings.Contains(string(data), `"RunSession"`) {
		t.Fatal("the recording must run a session and hold no checkin-clock or session-kill marker")
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"burst-accept"` {
			id[0].Data = []byte(`"blind-proofs"`)
		}
	}
	replaySplice(t, base.Events)
}

// The deadline moves out by what a start check-in took, never by more than
// the extra the workflow gave: a 10 minute check-in against 30 minutes of
// extra moves it 10 minutes, a 45 minute one (cut by its own cap, but
// recorded long) 30, and a session with no extra or no check-in none.
func TestCheckinTookIsCappedByTheExtra(t *testing.T) {
	rec := func(d time.Duration) *CheckInOutcome { return &CheckInOutcome{Record: &CheckInRecord{Duration: d}} }
	for name, tc := range map[string]struct {
		ck    *CheckInOutcome
		extra time.Duration
		want  time.Duration
	}{
		"within the extra": {rec(10 * time.Minute), 30 * time.Minute, 10 * time.Minute},
		"past the extra":   {rec(45 * time.Minute), 30 * time.Minute, 30 * time.Minute},
		"no extra":         {rec(10 * time.Minute), 0, 0},
		"no check-in":      {nil, 30 * time.Minute, 0},
		"no record":        {&CheckInOutcome{}, 30 * time.Minute, 0},
	} {
		if got := checkinTook(tc.ck, tc.extra); got != tc.want {
			t.Errorf("%s: checkinTook = %v, want %v", name, got, tc.want)
		}
	}
}

// A deadline moves only as far as the end of the activity's window: a
// window that ended sooner than the extra the workflow asked for holds it.
func TestPastCheckinHoldsTheWindowsEnd(t *testing.T) {
	now := time.Now()
	s := sessionSpec{ceiling: now.Add(40 * time.Minute)}
	if got, want := s.pastCheckin(now.Add(30*time.Minute)), now.Add(30*time.Minute); !got.Equal(want) {
		t.Errorf("inside the window: %v, want %v", got, want)
	}
	if got, want := s.pastCheckin(now.Add(50*time.Minute)), now.Add(40*time.Minute); !got.Equal(want) {
		t.Errorf("past the window: %v, want %v", got, want)
	}
	if got, want := (sessionSpec{}).pastCheckin(now.Add(50*time.Minute)), now.Add(50*time.Minute); !got.Equal(want) {
		t.Errorf("no window: %v, want %v", got, want)
	}
}
