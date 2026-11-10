package factoryd

import (
	"net/http"
	"strings"
	"testing"
)

// A failed removal refuses the factory close before a record, a comment or
// the close, with Forgejo's reason in the error.
func TestCloseIssueAsFactoryRefusedWhenARemovalFails(t *testing.T) {
	fj := &planForgejo{issues: map[int64]*planIssue{5: {state: "open", deps: []int64{6}}, 6: {state: "open"}}, failCall: "DELETE issues/5/dependencies", failStatus: http.StatusInternalServerError}
	f := &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	store := newFakeStore()
	err := closeIssueAsFactory(t.Context(), store, f, Task{ID: "t-a", Repository: "linkshort/linkshort", IssueNumber: 5}, "x")
	if err == nil || fj.issues[5].state != "open" || len(fj.comments) != 0 || len(store.intakeEvents) != 0 {
		t.Fatalf("err = %v, #5 %s, comments %d, rows %d; want the refusal and nothing written", err, fj.issues[5].state, len(fj.comments), len(store.intakeEvents))
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want Forgejo's status in it", err)
	}
}
