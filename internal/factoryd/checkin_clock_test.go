package factoryd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
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
	results := []SessionResult{
		{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped"},
		{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped again",
			CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: "go on",
				Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1", Duration: 40 * time.Second}}},
		pushed("s3", "c3")}
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			h.windows = append(h.windows, activity.GetInfo(ctx).StartToCloseTimeout)
			return results[min(len(h.sessions), len(results))-1], nil
		})
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
// session-kill marker: it replays on the old branch and the new version
// calls add no command. No recorded history holds a session with a start
// check-in, so the old window of such a session is asserted by
// TestWorkflowSessionWindowCarriesTheCheckinExtra's pinned run.
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

type liveSpy struct {
	Store
	mu   sync.Mutex
	puts []LiveSession
}

func (s *liveSpy) PutLiveSession(ctx context.Context, l LiveSession) error {
	s.mu.Lock()
	s.puts = append(s.puts, l)
	s.mu.Unlock()
	return s.Store.PutLiveSession(ctx, l)
}

// A session whose card has one second of agent time left opens with a check-in
// that takes 1.5s. With the extra time the workflow gives such a session
// the check-in is cut at its policy cap, not at the card's second: it answers,
// and the live row's deadline moves out by the check-in's time. A session
// with no extra (a run from before the change) cuts the check-in at the
// second its deadline leaves, and it stops at its time cap.
func TestRunSessionCheckinTakesNoTimeOfTheSession(t *testing.T) {
	for name, tc := range map[string]struct {
		extra     time.Duration
		answered  bool
		moved     bool
		wantKind  string
		remaining time.Duration
	}{
		"with the extra":    {extra: 30 * time.Minute, answered: true, moved: true, remaining: time.Second},
		"without the extra": {extra: 0, answered: false, wantKind: CheckTimeCap},
	} {
		t.Run(name, func(t *testing.T) {
			runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"go on"}`})
			chat.delay = 1500 * time.Millisecond
			runner.stopWait, runner.endpointWait = 100*time.Millisecond, 100*time.Millisecond
			spy := &liveSpy{Store: store}
			runner.store = spy
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", AgentTime: time.Second,
				CheckInAsk: &CheckInAsk{Moment: CheckInDead}, CheckinExtra: tc.extra, Remaining: tc.remaining}, nil, resultLine(true, "done"))
			if err != nil {
				t.Fatal(err)
			}
			if res.CheckIn == nil {
				t.Fatalf("result %+v holds no check-in", res)
			}
			if got := res.CheckIn.Decision == CheckHandover; got != tc.answered || res.CheckIn.ReasonKind != tc.wantKind {
				t.Fatalf("check-in = %q kind %q (%q), want answered %v kind %q", res.CheckIn.Decision, res.CheckIn.ReasonKind, res.CheckIn.Reason, tc.answered, tc.wantKind)
			}
			spy.mu.Lock()
			defer spy.mu.Unlock()
			if len(spy.puts) == 0 {
				t.Fatal("no live row was written")
			}
			if moved := len(spy.puts) > 1 && spy.puts[len(spy.puts)-1].Deadline.Sub(spy.puts[0].Deadline) >= time.Second; moved != tc.moved {
				t.Fatalf("the live row's deadline moved = %v over %d writes, want %v", moved, len(spy.puts), tc.moved)
			}
		})
	}
}

// The window of a session's activity is the card's remaining time and the
// grace, and for the session that opens with a check-in the 30m extra on top
// of them. With the 20m cap, s1 gets 20m + 2m = 22m. After s1's minute the
// card has 19m left: s2 gets 19m + 30m + 2m = 51m, and a run from before
// the change 19m + 2m = 21m. s2 used 1m less its check-in's 40s, so the card
// has used 1m20s and s3 gets 18m40s + 2m = 20m40s; before the change it has
// used 2m and s3 gets 18m + 2m = 20m.
func TestWorkflowSessionWindowCarriesTheCheckinExtra(t *testing.T) {
	for name, tc := range map[string]struct {
		pinned bool
		want   []time.Duration
	}{
		"gated":  {false, []time.Duration{22 * time.Minute, 51 * time.Minute, 20*time.Minute + 40*time.Second}},
		"pinned": {true, []time.Duration{22 * time.Minute, 21 * time.Minute, 20 * time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			h := checkinClockRun(t, tc.pinned)
			if len(h.windows) != 3 || h.windows[0] != tc.want[0] || h.windows[1] != tc.want[1] || h.windows[2] != tc.want[2] {
				t.Fatalf("session windows = %v, want %v", h.windows, tc.want)
			}
		})
	}
}
