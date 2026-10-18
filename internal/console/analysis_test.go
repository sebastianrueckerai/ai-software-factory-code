package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// attach_analysis records what the wake turn found inside the card's open
// ask: the tool resolves the card, hands its found and its recommendation
// to the factory's AttachAnalysis and names the card. An ask already
// settled refuses with its word and its actor; a card with no ask row
// refuses with the way to file one.
func TestAttachAnalysisTool(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	tool, ok := r.c.chatTool("attach_analysis")
	if !ok {
		t.Fatal("no attach_analysis tool among the chat's tools")
	}
	_, _, err := tool.run(trn, toolInput{TaskID: "13", Found: "The replica adds 12 ms of lag."})
	equal(t, "without the factory's seam", err.Error(), "this factory records no analysis in its asks")
	type filed struct{ repository, task, found, recommendation, by string }
	var calls []filed
	r.c.cfg.AttachAnalysis = func(_ context.Context, repository, taskID, found, recommendation, by string) error {
		calls = append(calls, filed{repository, taskID, found, recommendation, by})
		return nil
	}
	for _, in := range []toolInput{
		{TaskID: "13", Recommendation: "Keep one replica"},
		{TaskID: "13", Found: strings.Repeat("x", 2001)},
		{TaskID: "13", Found: "found", Recommendation: strings.Repeat("y", 2001)},
	} {
		if _, _, err := tool.run(trn, in); err == nil {
			t.Fatalf("the malformed call %+v ran", in)
		}
	}
	note, _, err := func() (string, *inverse, error) {
		in := toolInput{TaskID: "13", Found: "The replica adds 12 ms of lag.", Recommendation: "Keep one replica"}
		if err := trn.byTaskID(&in); err != nil {
			t.Fatal(err)
		}
		return tool.run(trn, in)
	}()
	equal(t, "the tool's answer", []any{note, err},
		[]any{"Recorded the analysis inside the ask of #13; the members read it with the question.", error(nil)})
	equal(t, "what it filed", calls, []filed{{"sf/code", "t-e", "The replica adds 12 ms of lag.", "Keep one replica", "sebastian"}})
	for _, tc := range []struct {
		err    error
		status int
		want   string
	}{
		{&factoryd.AskSettledError{Word: "answered", By: "sebastian"}, http.StatusConflict,
			"The ask of #13 is answered by sebastian. An ask already answered is done."},
		{factoryd.ErrNoAsk, http.StatusNotFound,
			"Card #13 asks nothing in the chat; escalate files an ask when you need a person."},
		{errors.New("the store is down"), http.StatusBadGateway,
			"The analysis could not be recorded; see the factoryd log."},
	} {
		err := tc.err
		r.c.cfg.AttachAnalysis = func(context.Context, string, string, string, string, string) error { return err }
		in := toolInput{TaskID: "t-e", Found: "found"}
		_, _, got := tool.run(trn, in)
		var no *refusal
		if !errors.As(got, &no) || no.status != tc.status || no.msg != tc.want {
			t.Fatalf("the refusal = %v, want %d %q", got, tc.status, tc.want)
		}
	}
}
