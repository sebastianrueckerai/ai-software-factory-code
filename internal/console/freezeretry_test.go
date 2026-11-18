package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// freezeCard is a freeze card of the coder class on repository, at status.
func freezeCard(id, repository string, issue int64, status string) factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "human", Status: status, Source: factoryd.SourceFreezeCard, Repository: repository,
		IssueNumber: issue, Objective: "The provider limit freezes the coder class\n\nClass: coder\n\nThe class resets at 2026-11-21T17:00:00Z."}}
}

// An open freeze card, which asks nothing and so holds no case, offers
// Retry now to an approver, on its face and in its drawer; a writer who is
// no approver sees none, and a done freeze card offers none, nor an open
// one whose class resumed on its own and whose close is pending.
func TestFreezeCardShowsRetryNow(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.openCases = map[string][]factoryd.Case{}
	resumed := freezeCard("t-fz-resumed", "linkshort/linkshort", 23, "waiting-human")
	resumed.Objective = "The provider limit freezes the coder class\n\nClass: coder\n\nThe class resumed on its own at 2026-11-21T17:00:00Z: the freeze held 5h 00m. The card closes itself at 2026-11-21T17:05:00Z."
	r.tasks.tasks = append(planTasks(), freezeCard("t-fz", "linkshort/linkshort", 21, "waiting-human"), freezeCard("t-fz-done", "linkshort/linkshort", 22, "done"), resumed)
	ck, s := r.loggedIn(t)
	form := `<form method="post" action="/tasks/t-fz/retry-freeze" hx-post="/tasks/t-fz/retry-freeze" class="inline">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort">` +
		`<button type="submit" aria-label="Retry the provider now, ending the freeze of #21 before its reset">Retry now</button></form>`
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if !strings.Contains(body, form) {
		t.Errorf("the freeze card's face lacks Retry now:\n%s", body)
	}
	if strings.Contains(body, "/tasks/t-fz-done/retry-freeze") {
		t.Error("the done freeze card offers Retry now")
	}
	if !strings.Contains(body, "#23") || strings.Contains(body, "/tasks/t-fz-resumed/retry-freeze") {
		t.Error("the resumed class's freeze card offers Retry now, or is not on the board")
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-fz/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(drawer, `action="/tasks/t-fz/retry-freeze"`) || !strings.Contains(drawer, "Retry now ends the freeze at once") {
		t.Errorf("the freeze card's drawer lacks Retry now:\n%s", drawer)
	}
	ak, _ := r.as(t, "alice")
	if strings.Contains(r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ak).Body.String(), "/retry-freeze") {
		t.Error("alice, no approver, sees Retry now")
	}
}

// Retry now, by an approver, presses the retry switch on the freeze card
// (Config.RetryFreeze). A writer who is no approver gets 403 and nothing is
// pressed; a refusal answers 409 with its sentence, a failure past the
// clear 500 saying the hold cleared and what failed, any other failure 500
// with the error and what to do.
func TestRetryNowPressesTheRetrySwitch(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), freezeCard("t-fz", "linkshort/linkshort", 21, "waiting-human"))
	ck, s := r.loggedIn(t)
	ak, as := r.as(t, "alice")
	press := func(ck *http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := post("/tasks/t-fz/retry-freeze", url.Values{"csrf": {csrf}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	if rec := press(ak, as.csrf); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project linkshort may end a provider freeze." ||
		len(r.unfrozen) != 0 {
		t.Fatalf("alice presses: %d %q, pressed %v", rec.Code, rec.Body, r.unfrozen)
	}
	if rec := press(ck, s.csrf); !pressRefreshes(rec) {
		t.Fatalf("the press: %d %.400s", rec.Code, rec.Body)
	}
	if !slices.Equal(r.unfrozen, []string{"t-fz by sebastian via button"}) {
		t.Fatalf("RetryFreeze calls %v, want the one press by sebastian", r.unfrozen)
	}
	const ended = "the freeze of #21 ended, the card is done: no hold waits on a retry, and a new limit on the class opens a freeze card again"
	r.unfreezeErr = factoryd.ErrNoFreeze(ended)
	if rec := press(ck, s.csrf); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != ended {
		t.Errorf("a refused press: %d %q", rec.Code, rec.Body)
	}
	r.unfreezeErr = &factoryd.PartialPressError{Err: errors.New("wake t-z: temporal unavailable")}
	if rec := press(ck, s.csrf); rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) !=
		"The hold cleared, but part of the press failed: wake t-z: temporal unavailable. A frozen card it did not wake starts at its reset, and a freeze card it did not close closes five minutes after its class resumed." {
		t.Errorf("a press that failed past the clear: %d %q", rec.Code, rec.Body)
	}
	r.unfreezeErr = errors.New("clear the coder class's hold: status 502: bad gateway")
	if rec := press(ck, s.csrf); rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) !=
		"The freeze could not be ended: clear the coder class's hold: status 502: bad gateway. Press Retry now again, or wait for the reset on the card." {
		t.Errorf("a failed press: %d %q", rec.Code, rec.Body)
	}
}

// runToolAs runs chat tool name for user, a writer on sf/code, as runTool
// runs it for sebastian.
func runToolAs(t *testing.T, r *rig, user, name, decider string, in toolInput) (string, error) {
	t.Helper()
	_, s := r.as(t, user)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, ok := r.c.chatTool(name)
	if !ok {
		t.Fatalf("there is no %s tool", name)
	}
	note, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: decider},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, in)
	return note, err
}

// The chat's retry on a freeze card, which holds no case, goes to Act on no
// case, where the retry of a frozen case runs the retry switch: the reply
// is Act's, and the human card's close takes no part, no issue patched and
// nothing posted. A writer who is no approver presses nothing through the
// chat, on either rung, as Retry refuses them, and a press that failed past
// the clear answers as the button does.
func TestTheChatsRetryOnAFreezeCardGoesToAct(t *testing.T) {
	r := newRig(t)
	r.openCases = map[string][]factoryd.Case{}
	r.tasks.tasks = []factoryd.BoardTask{freezeCard("t-fz", "sf/code", 31, "waiting-human")}
	said, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-fz", Action: factoryd.ActRetry})
	if err != nil || said != "Case 0 took retry." {
		t.Fatalf("the retry = %q, %v; want Act's reply", said, err)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the retry", got, factoryd.ActInput{TaskID: "t-fz", By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActRetry}})
	if bodies := r.fg.commentBodies("/api/v1/repos/sf/code/issues/31/comments"); len(bodies) != 0 || len(r.fg.patched) != 0 ||
		slices.ContainsFunc(r.fg.calls, func(c string) bool { return strings.HasPrefix(c, "PATCH ") }) {
		t.Fatalf("comments %q, patched %v; want none: the press is Act's", bodies, r.fg.patched)
	}
	// a press that cleared the hold and failed part after it says so, as
	// the button does
	r.actErr = &factoryd.PartialPressError{Err: errors.New("wake t-z: temporal unavailable")}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-fz", Action: factoryd.ActRetry}); err == nil || err.Error() !=
		"The hold cleared, but part of the press failed: wake t-z: temporal unavailable. A frozen card it did not wake starts at its reset, and a freeze card it did not close closes five minutes after its class resumed." {
		t.Fatalf("the chat's retry past the clear = %v, want the hold-cleared sentence", err)
	}
	r.actErr = nil
	for _, decider := range []string{"member", "agent"} {
		if _, err := runToolAs(t, r, "alice", "answer", decider, toolInput{TaskID: "t-fz", Action: factoryd.ActRetry}); err == nil ||
			err.Error() != "Only the approvers of project ai-software-factory-code may retry a card." {
			t.Fatalf("alice's retry as %s = %v, want the approvers' refusal", decider, err)
		}
	}
	if len(r.acted) != 2 {
		t.Fatalf("Act ran %d times, want only sebastian's two retries", len(r.acted))
	}
}
