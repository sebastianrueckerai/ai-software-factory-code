package factoryd

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	task := store.tasks["t2"]
	task.Repository = judgmentRepository
	store.tasks["t2"] = task
	return runner, chat, store
}

// judgmentRepository is the test policy's one project, ai-software-factory-infra.
const judgmentRepository = "sf/ai-software-factory-infra"

// chatTaskOf is the project's chat task of this month: the project's name
// and the month of the runner's clock, written out.
func chatTaskOf(r *Runner) string {
	return "t-chat-ai-software-factory-infra-" + r.clock().UTC().Format("2006-01")
}

// Under checkin-judgment a gate moment on a card whose share is spent runs
// the conversation, booked on the project's chat task.
func TestRunSessionCheckinJudgmentGateMomentRunsOnTheChatTask(t *testing.T) {
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.CheckIn == nil || res.CheckIn.Decision != CheckHandover {
		t.Fatalf("result = %+v, want the pushed session on the handover", res)
	}
	if want := chatTaskOf(runner); chat.calls != 1 || !slices.Equal(chat.ids, []string{want}) || !strings.HasPrefix(want, "t-chat-") {
		t.Fatalf("calls %d on %q, want one on %q", chat.calls, chat.ids, want)
	}
	// the default chat budget of 10 EUR, nothing spent
	if got := chat.gots[0].Input; !strings.Contains(got, "calls left 20, 10.00 EUR left in the project's judgment budget this month") {
		t.Fatalf("statement = %q, want the policy's calls and the project's judgment money", got)
	}
}

// The session moment under the flag judges the trip on the chat task.
func TestRunSessionCheckinJudgmentSessionMomentRunsOnTheChatTask(t *testing.T) {
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the reads differ each time"}`})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.CheckIn != nil {
		t.Fatalf("result = (%+v, %v), want the session run on after the continue", res, err)
	}
	if want := chatTaskOf(runner); !slices.Equal(chat.ids, []string{want}) {
		t.Fatalf("calls booked on %q, want %q", chat.ids, want)
	}
}

// A surveying session, reads and commands with no file change, ends at its
// first trip when the check-in hands over: it surveys no further, and the
// handover is the one conversation that ran.
func TestRunSessionSurveyingSessionEndsAtItsFirstTrip(t *testing.T) {
	note := "stop reading; fix the caller"
	runner, chat, _ := judgmentRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"` + note + `"}`})
	kube := runner.kube.(*fakeKube)
	logs := commands(0, 24) +
		envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" + pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeNoPush || res.Trip != "repetition" || res.CheckIn == nil || res.CheckIn.Handover != note || chat.calls != 1 {
		t.Fatalf("result = %+v after %d calls, want the session ended at its first trip on the handover", res, chat.calls)
	}
	if len(kube.deletedPods) != 1 {
		t.Fatalf("deleted pods = %q, want the pod stopped at the trip", kube.deletedPods)
	}
}

// A moment whose chat task cannot be filed ends as refused calls, masked.
func TestRunSessionCheckinJudgmentChatTaskErrorIsMasked(t *testing.T) {
	runner, chat, store := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
	store.chatErr = errors.New(secretError)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
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
	runner.checkin = &failingChat{err: errors.New(secretError)}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
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

type failingChat struct {
	err   error
	calls int
}

func (c *failingChat) Call(context.Context, string, string, CheckinRequest) (CheckinResponse, error) {
	c.calls++
	return CheckinResponse{}, c.err
}

// A spent judgment budget stops no card for its share: the gate moment's
// calls are made and the gateway refuses them, which ends the moment as
// refused calls.
func TestRunSessionCheckinJudgmentSpentChatTaskEndsTheGateMomentRefused(t *testing.T) {
	runner, _, store := judgmentRig(t)
	store.chatSpent = 5
	chat := &failingChat{err: errors.New("gateway budget: the task's budget is spent")}
	runner.checkin = chat
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.CheckIn == nil || chat.calls == 0 {
		t.Fatalf("result = (%+v, %v) after %d calls, want the moment run and refused, no share stop", res, err, chat.calls)
	}
	if rec := res.CheckIn.Record; res.CheckIn.Reason == spentStop || !strings.HasPrefix(rec.Reason, checkinRefusedReason) {
		t.Fatalf("check-in = %+v, want refused calls", res.CheckIn)
	}
}

// The session moment under a spent judgment budget calls the model too and
// runs on as unjudged when the gateway refuses.
func TestRunSessionCheckinJudgmentSpentChatTaskEndsTheSessionMomentRefused(t *testing.T) {
	runner, _, store := judgmentRig(t)
	store.chatSpent = 5
	chat := &failingChat{err: errors.New("gateway budget: the task's budget is spent")}
	runner.checkin = chat
	objects := runner.objects.(*fakeObjectStore)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls == 0 {
		t.Fatalf("result = (%+v, %v) after %d calls, want the session run on after refused calls", res, err, chat.calls)
	}
	record := string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))
	if !strings.Contains(record, checkinRefusedReason) || strings.Contains(record, spentStop) {
		t.Fatalf("record = %s, want the refusal and no share stop", record)
	}
}

// The session moment's chat-task error reaches the record and the issue
// masked.
func TestRunSessionCheckinJudgmentSessionMomentChatTaskErrorIsMasked(t *testing.T) {
	runner, _, store := judgmentRig(t)
	store.chatErr = errors.New(secretError)
	objects := runner.objects.(*fakeObjectStore)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want the session run on", res, err)
	}
	record := string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl")) + string(objGet(objects, "sessions/t2/timeline.jsonl"))
	if !strings.Contains(record, checkinRefusedReason) || strings.Contains(record, "user:pass") || strings.Contains(record, "abc123") {
		t.Fatalf("record = %s, want the refusal with both secrets masked", record)
	}
}

// runSpentReport runs a screening report session whose card share is spent
// to the activity with args after the task and the model, and returns the
// chat calls and the session records.
func runSpentReport(t *testing.T, args ...any) (*idChat, *Runner, string) {
	t.Helper()
	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: judgmentRepository, SHA: "abc123", Process: "maintenance",
		Parent: "t-parent", Source: SourceScreening, Track: policy.TrackDirect, Author: "visitor", RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, store := newTestRunner(t, report, kube, objects)
	if err := store.CreateTask(context.Background(), parentTask()); err != nil {
		t.Fatal(err)
	}
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	chat := &idChat{scriptedChat: scriptedChat{answers: []CheckinResponse{{Text: `{"decision":"continue","reason":"the reads differ"}`}}}}
	runner.checkin = chat
	verdict := `{"verdict": "clear", "reasons": []}`
	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) +
		envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": true, "summary": verdict}}) + "\n" + resultLine(true, verdict)
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunReport, append([]any{"t1", "factory/small"}, args...)...)
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, logs)
	<-done
	objects.mu.Lock()
	defer objects.mu.Unlock()
	var records strings.Builder
	for _, p := range objects.puts {
		records.Write(p.body)
	}
	return chat, runner, records.String()
}

// A screening report session at a spent card share runs its check-in on
// the chat task, as a work session does.
func TestRunReportCheckinJudgmentRunsTheCheckinAtASpentShare(t *testing.T) {
	chat, runner, records := runSpentReport(t, int64(0))
	if want := chatTaskOf(runner); chat.calls != 1 || !slices.Equal(chat.ids, []string{want}) {
		t.Fatalf("calls %d on %q, want one on %q", chat.calls, chat.ids, want)
	}
	if strings.Contains(records, spentStop) {
		t.Fatalf("records = %s, want no share stop", records)
	}
}

// A repository the policy's projects do not name cannot book on a chat
// task: the moment ends as refused calls, and the refusal names the fix.
func TestRunSessionCheckinJudgmentNoProjectRefusalNamesTheFix(t *testing.T) {
	runner, chat, store := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
	task := store.tasks["t2"]
	task.Repository = "sf/no-project"
	store.tasks["t2"] = task
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.CheckIn == nil || chat.calls != 0 {
		t.Fatalf("result = (%+v, %v) after %d calls, want a refused moment and no call", res, err, chat.calls)
	}
	if want := checkinRefusedReason + "repository sf/no-project names no project: add the repository to a project in the policy"; res.CheckIn.Record.Reason != want {
		t.Fatalf("reason = %q, want %q", res.CheckIn.Record.Reason, want)
	}
}

// A class with no check-in model files no chat task: with the store failing,
// the gate moment ends with no check-in and the session moment records no
// refusal.
func TestRunSessionCheckinJudgmentWithoutAModelFilesNoChatTask(t *testing.T) {
	setup := func(t *testing.T) (*Runner, *fakeStore) {
		runner, _, store := judgmentRig(t, CheckinResponse{Text: judgmentHandover})
		noAlias := testPolicyVersion()
		cl := noAlias.Document.Classes["coder"]
		cl.ReviewModel = ""
		noAlias.Document.Classes["coder"] = cl
		runner.cfg.Policy = func() policy.Version { return noAlias }
		store.chatErr = errors.New(secretError)
		return runner, store
	}
	t.Run("gate moment", func(t *testing.T) {
		runner, _ := setup(t)
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
			CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed || res.CheckIn != nil {
			t.Fatalf("result = (%+v, %v), want the session run with no check-in", res, err)
		}
	})
	t.Run("session moment", func(t *testing.T) {
		runner, _ := setup(t)
		objects := runner.objects.(*fakeObjectStore)
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
			commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed {
			t.Fatalf("result = (%+v, %v)", res, err)
		}
		if record := string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl")); strings.Contains(record, checkinRefusedReason) {
			t.Fatalf("record = %s, want no refusal", record)
		}
	})
}
