package factoryd

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// [continue_carries_todays_message] The old decision message folds into
// continue: a session moment answered continue with the guidance today's
// message carried makes the runner deliver exactly one message whose text
// is that guidance, and message appears among none of the moments' answer
// enums.
func TestProofContinueCarriesTodaysMessage(t *testing.T) {
	requireStoredDecisionsOnTheRungsNames(t)
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	runner.cfg.Supervisor.RepetitionEvents = 2
	guidance := "call the store's New, not the old constructor"
	chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"decision":"continue","guidance":"` + guidance + `"}`}}}
	runner.Checkin = chat
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	logs := reads(0, 2) + reads(2, 1) +
		envLine(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "a.go"}}) + "\n" +
		pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want the session running on to push c1", res, err)
	}
	worker.mu.Lock()
	texts := withoutWipAsks(worker.texts)
	worker.mu.Unlock()
	var messages []string
	for _, s := range texts {
		if after, ok := strings.CutPrefix(s, "POST /message "); ok {
			messages = append(messages, after)
		}
	}
	if len(messages) != 1 || messages[0] != guidance {
		t.Fatalf("the runner delivered %q, want the one message %q", messages, guidance)
	}
	if chat.calls != 1 {
		t.Fatalf("check-in conversations = %d, want 1", chat.calls)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], guidance) {
		t.Fatalf("comments = %q", hub.comments)
	}
	for _, moment := range []string{CheckInSession, CheckInGate, CheckInDead} {
		for _, replan := range []bool{true, false} {
			if slices.Contains(momentDecisions(moment, replan), "message") {
				t.Errorf("the %s moment still offers the old decision message", moment)
			}
		}
	}
}
