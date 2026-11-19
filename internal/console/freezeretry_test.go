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
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// freezeCard is a freeze card of the coder class on repository, at status,
// asking as the factory files it.
func freezeCard(id, repository string, issue int64, status string) factoryd.BoardTask {
	q := factoryd.FreezeQuestion(factoryd.FreezeEvent{Class: "coder", Reset: "2026-11-21T17:00:00Z", Words: "rate limit exceeded"}, 2,
		time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "human", Status: status, Source: factoryd.SourceFreezeCard, Repository: repository,
		IssueNumber: issue, Objective: factoryd.IssueObjective(q.Ask, q.Info)}, Escalation: q.Ask, Question: &q}
}

// An open freeze card asks on its opened card and offers Retry now first,
// with what it does; a done freeze card offers none, nor does an open one
// whose class resumed on its own and whose close is pending, which keeps
// its other answers; a writer who is no approver sees it disabled and reads
// who may choose it.
func TestFreezeCardShowsRetryNow(t *testing.T) {
	r := newRig(t)
	resumed := freezeCard("t-fz-resumed", "sf/code", 23, "waiting-human")
	resumed.Objective = factoryd.IssueObjective(resumed.Question.Ask, "Class: coder\n\nThe class resumed on its own at 2026-11-21T17:00:00Z: the freeze held 5h 00m. "+
		"The card closes itself at 2026-11-21T17:05:00Z.\n\nThe freeze started at 2026-11-21T12:00:00Z.\nThe reset the provider named was 2026-11-21T17:00:00Z.")
	r.tasks.tasks = []factoryd.BoardTask{freezeCard("t-fz", "sf/code", 21, "waiting-human"), freezeCard("t-fz-done", "sf/code", 22, "done"), resumed}
	ck, s := r.loggedIn(t)
	opened := func(ck *http.Cookie, id string) string {
		return r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
	}
	form := `<div class="o-answers"><form method="post" action="/tasks/t-fz/answer" hx-post="/tasks/t-fz/answer" class="answer" data-answer="choice:retry" data-to="off">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="case" value="1"><input type="hidden" name="value" value="retry">` + "\n" +
		`<button type="submit" class="pick">Retry now</button> <span class="effect">Ends the freeze now. The waiting cards call the provider again, and a new limit makes them wait until its reset.</span>`
	if ask := askBlockOf(t, opened(ck, "t-fz")); !strings.Contains(ask, form) {
		t.Errorf("the freeze card's ask does not offer Retry now first:\n%s", ask)
	}
	if done := opened(ck, "t-fz-done"); strings.Contains(done, "Retry now") || strings.Contains(done, `value="retry"`) {
		t.Errorf("the done freeze card offers Retry now:\n%s", done)
	}
	if ask := askBlockOf(t, opened(ck, "t-fz-resumed")); strings.Contains(ask, "Retry now") || strings.Contains(ask, `value="retry"`) ||
		!strings.Contains(ask, `<button type="submit" class="pick">Noted</button>`) {
		t.Errorf("the resumed class's freeze card offers Retry now, or lost its other answers:\n%s", ask)
	}
	ak, _ := r.as(t, "alice")
	hers := opened(ak, "t-fz")
	if !strings.Contains(hers, `<p class="o-who">Only the project&#39;s approvers may choose Retry now.</p>`) ||
		!strings.Contains(hers, `<button type="button" class="pick" disabled>Retry now</button>`) || strings.Contains(hers, `value="retry"`) {
		t.Errorf("alice, a writer and no approver, may press Retry now, or is not told who may:\n%s", hers)
	}
}

// Retry now, by an approver, is the freeze card's answer through Act, which
// presses the retry switch: nothing is commented and no issue is patched,
// as the switch closes the card itself. A human who is no approver gets 403
// and nothing is pressed; Act's refusal answers 409 with its sentence, a
// failure past the clear 500 saying the hold cleared and what failed, any
// other failure 500 with the error and what to do.
func TestRetryNowPressesTheRetrySwitch(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{freezeCard("t-fz", "sf/code", 21, "waiting-human")}
	ck, s := r.loggedIn(t)
	ak, as := r.as(t, "alice")
	press := func(ck *http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := post("/tasks/t-fz/answer", url.Values{"csrf": {csrf}, "case": {"1"}, "value": {factoryd.ActRetry}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	if rec := press(ak, as.csrf); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project ai-software-factory-code may end a provider freeze." ||
		len(r.acted) != 0 {
		t.Fatalf("alice presses: %d %q, Act took %+v", rec.Code, rec.Body, r.acted)
	}
	if rec := press(ck, s.csrf); !pressRefreshes(rec) {
		t.Fatalf("the press: %d %.400s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the press", got, factoryd.ActInput{TaskID: "t-fz", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActRetry, Note: "Retry now"}})
	if bodies := r.fg.commentBodies("/api/v1/repos/sf/code/issues/21/comments"); len(bodies) != 0 || len(r.fg.patched) != 0 {
		t.Fatalf("comments %q, patched %v; want none: the press is Act's", bodies, r.fg.patched)
	}
	const ended = "the freeze of #21 ended, the card is done: nothing waits for a retry, and a new limit on the class opens a freeze card again"
	r.actErr = factoryd.ErrActRefused(ended)
	if rec := press(ck, s.csrf); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != ended {
		t.Errorf("a refused press: %d %q", rec.Code, rec.Body)
	}
	r.actErr = &factoryd.PartialPressError{Err: errors.New("wake t-z: temporal unavailable")}
	if rec := press(ck, s.csrf); rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) !=
		"The hold cleared, but part of the press failed: wake t-z: temporal unavailable. A frozen card it did not wake starts at its reset, and a freeze card it did not close closes five minutes after its class resumed." {
		t.Errorf("a press that failed past the clear: %d %q", rec.Code, rec.Body)
	}
	r.actErr = errors.New("end the coder class's freeze: status 502: bad gateway")
	if rec := press(ck, s.csrf); rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) !=
		"The freeze could not be ended: end the coder class's freeze: status 502: bad gateway. Press Retry now again, or wait for the reset on the card." {
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

// The chat's retry on a freeze card answers the card's ask through Act,
// where the retry of a freeze card runs the retry switch: the reply is
// Act's, and the human card's close takes no part, no issue patched and
// nothing posted. A writer who is no approver presses nothing through the
// chat, on either rung, as Retry refuses them, and a press that failed past
// the clear answers as the button does.
func TestTheChatsRetryOnAFreezeCardGoesToAct(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{freezeCard("t-fz", "sf/code", 31, "waiting-human")}
	said, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-fz", Action: factoryd.ActRetry})
	if err != nil || said != "Case 1 took retry." {
		t.Fatalf("the retry = %q, %v; want Act's reply", said, err)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the retry", got, factoryd.ActInput{TaskID: "t-fz", CaseID: 1, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActRetry, Note: "Retry now"}})
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
			err.Error() != "Only the approvers of project ai-software-factory-code may end a provider freeze." {
			t.Fatalf("alice's retry as %s = %v, want the approvers' refusal", decider, err)
		}
	}
	if len(r.acted) != 2 {
		t.Fatalf("Act ran %d times, want only sebastian's two retries", len(r.acted))
	}
}
