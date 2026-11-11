package factoryd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

const (
	judgmentHandover = `{"findings":[{"text":"TasksByPR is undefined","verdict":"confirmed"}],"decision":"handover","handover":"fix the caller first"}`
	secretError      = "dial https://user:pass@host failed: Authorization: Bearer abc123"
)

// judgmentRig is a card whose check-in share (0.03 of its 0.3 EUR) is spent
// to the cent, its check-in calls recorded with the task they book on.
func judgmentRig(t *testing.T, answers ...CheckinResponse) (*Runner, *idChat, *fakeStore) {
	t.Helper()
	runner, _, _, _, _, store, _ := liveRig(t)
	chat := &idChat{scriptedChat: scriptedChat{answers: answers}}
	runner.checkin = chat
	store.checkinSpent = map[string]float64{"t2": 0.03}
	return runner, chat, store
}

func chatTaskOf(r *Runner, repository string) string {
	name, _, _ := r.cfg.Policy().ProjectOf(repository)
	return ChatTaskID(name, r.clock())
}

// Under checkin-judgment a gate moment on a card whose share is spent runs
// the conversation, booked on the project's chat task.
func TestRunSessionCheckinJudgmentGateMomentRunsOnTheChatTask(t *testing.T) {
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", CheckinJudgment: true,
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.CheckIn == nil || res.CheckIn.Decision != CheckHandover || res.CheckIn.Share != 0 {
		t.Fatalf("result = %+v, want the pushed session on the handover, with no card share", res)
	}
	if want := chatTaskOf(runner, "sf/repo"); chat.calls != 1 || !slices.Equal(chat.ids, []string{want}) || !strings.HasPrefix(want, "t-chat-") {
		t.Fatalf("calls %d on %q, want one on %q", chat.calls, chat.ids, want)
	}
	if got := chat.gots[0].Input; !strings.Contains(got, "calls left 20,") {
		t.Fatalf("statement = %q, want the policy's calls", got)
	}
}

// The same card without the flag stops on its spent share before any call.
func TestRunSessionCheckinWithoutJudgmentStillStopsOnTheShare(t *testing.T) {
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil)
	if err != nil || res.Outcome != OutcomeCheckinHuman || res.CheckIn.Reason != CheckinShareSpent || chat.calls != 0 {
		t.Fatalf("result = (%+v, %v) after %d calls, want the share stop with no call", res, err, chat.calls)
	}
}

// The session moment under the flag judges the trip on the chat task.
func TestRunSessionCheckinJudgmentSessionMomentRunsOnTheChatTask(t *testing.T) {
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the reads differ each time"}`})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", CheckinJudgment: true}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.CheckIn != nil {
		t.Fatalf("result = (%+v, %v), want the session run on after the continue", res, err)
	}
	if want := chatTaskOf(runner, "sf/repo"); !slices.Equal(chat.ids, []string{want}) {
		t.Fatalf("calls booked on %q, want %q", chat.ids, want)
	}
}

// Without the flag the session moment stops on the spent share.
func TestRunSessionTripWithoutJudgmentStillStopsOnTheShare(t *testing.T) {
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, commands(0, 24))
	if err != nil || res.Outcome != OutcomeCheckinHuman || res.CheckIn == nil || res.CheckIn.Reason != CheckinShareSpent || chat.calls != 0 {
		t.Fatalf("result = (%+v, %v) after %d calls, want the share stop with no call", res, err, chat.calls)
	}
}

// A surveying session, reads and commands with no file change, ends at its
// first trip when the check-in hands over: it surveys no further, and the
// handover is the one conversation that ran.
func TestRunSessionSurveyingSessionEndsAtItsFirstTrip(t *testing.T) {
	note := "stop reading; fix the caller"
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"` + note + `"}`})
	logs := commands(0, 24) +
		envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" + pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", CheckinJudgment: true}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeNoPush || res.Trip != "repetition" || res.CheckIn == nil || res.CheckIn.Handover != note || chat.calls != 1 {
		t.Fatalf("result = %+v after %d calls, want the session ended at its first trip on the handover", res, chat.calls)
	}
}

// A moment whose chat task cannot be filed ends as refused calls, masked.
func TestRunSessionCheckinJudgmentChatTaskErrorIsMasked(t *testing.T) {
	runner, chat, store := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
	store.chatErr = errors.New(secretError)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", CheckinJudgment: true,
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.CheckIn == nil || chat.calls != 0 {
		t.Fatalf("result = (%+v, %v) after %d calls, want a refused moment and no call", res, err, chat.calls)
	}
	reason := res.CheckIn.Record.Reason
	if !strings.HasPrefix(reason, checkinRefusedReason) || strings.Contains(reason, "pass") || strings.Contains(reason, "abc123") {
		t.Fatalf("reason = %q, want the refusal with both secrets masked", reason)
	}
}

// A gateway refusal in the conversation reaches the record masked.
func TestRunSessionCheckinJudgmentGatewayRefusalIsMasked(t *testing.T) {
	runner, _, _ := judgmentRig(t)
	runner.checkin = failingChat{errors.New(secretError)}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", CheckinJudgment: true,
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.CheckIn == nil {
		t.Fatalf("result = (%+v, %v)", res, err)
	}
	reason := res.CheckIn.Record.Reason
	if !strings.HasPrefix(reason, checkinRefusedReason) || strings.Contains(reason, "pass") || strings.Contains(reason, "abc123") {
		t.Fatalf("reason = %q, want the refusal with both secrets masked", reason)
	}
}

type failingChat struct{ err error }

func (c failingChat) Call(context.Context, string, string, CheckinRequest) (CheckinResponse, error) {
	return CheckinResponse{}, c.err
}
