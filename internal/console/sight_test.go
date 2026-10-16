package console

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// flakyMay is Config.MayMove for the sight tests: it answers a canceled
// ask while fail holds, then as the rig's fake does. Its fields are
// atomics, so a test flips them while a stream's handler reads them.
type flakyMay struct {
	r    *rig
	fail atomic.Bool
	asks atomic.Int64
}

func (f *flakyMay) may(_ context.Context, repository, user, movedBy string) (bool, error) {
	f.asks.Add(1)
	if f.fail.Load() {
		return false, context.Canceled
	}
	if movedBy == policy.MovedByApprovers {
		return user == "sebastian", nil
	}
	return slices.Contains(f.r.repoWriters[repository], user), nil
}

// A canceled MayMove ask is remembered for a breath, not for writersTTL:
// the render that hit it hides its buttons once, and the next render
// asks again and shows them, instead of a minute of hidden buttons.
func TestACanceledMoveAskDoesNotPoisonTheMinute(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-idea", Kind: "agent",
		Status: "backlog", Lane: "backlog", Process: "spec", Objective: "an idea",
		Repository: "linkshort/linkshort", IssueNumber: 8}})
	ck, _ := r.loggedIn(t)
	r.mayErr = context.Canceled // the browser aborted the render: its asks died with it
	if body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(); moveForms(body, "t-idea") != nil ||
		!strings.Contains(body, "who may move a card could not be checked: the move buttons are hidden") {
		t.Fatalf("a failed check shows buttons or no problem line:\n%s", body)
	}
	r.mayErr = nil // Forgejo is healthy again, well inside writersTTL of the failed ask
	time.Sleep(2200 * time.Millisecond)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if moveForms(body, "t-idea") == nil || strings.Contains(body, "who may move a card could not be checked") {
		t.Fatalf("a render inside writersTTL served the failed ask as its answer; the buttons stay hidden:\n%s", body)
	}
}

// connect opens the code project's stream on a real server as ck and
// answers its response, its body reader and the function that ends it.
func connect(t *testing.T, r *rig, ck *http.Cookie) (*http.Response, *bufio.Reader, func()) {
	t.Helper()
	srv := httptest.NewServer(r.c)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/projects/ai-software-factory-code/events/stream", nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatal(err)
	}
	req.AddCookie(ck)
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatal(err)
	}
	var once sync.Once
	end := func() { once.Do(func() { cancel(); resp.Body.Close(); srv.Close() }) }
	t.Cleanup(end)
	return resp, bufio.NewReader(resp.Body), end
}

// A stream connect whose sight check errors still follows the board
// without a page reload: a failed ask remembered from an earlier canceled
// render (a), and one that dies during the connect itself (b), are
// re-asked within a bound instead of answered 503, which closes the
// browser's EventSource for good.
func TestAStreamConnectSurvivesAnErroredSightCheck(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	ctx := context.Background()

	// a: the failed ask of an earlier connect is remembered; Forgejo
	// answers again by the time the stream connects
	r.mayErr = context.Canceled
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events/stream", nil), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a connect whose sight check fails = %d, want 503", rec.Code)
	}
	r.mayErr = nil
	resp, br, end := connect(t, r, ck)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the next connect served the remembered failure: %d, want 200", resp.StatusCode)
	}
	r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-x", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	if got := nextMessage(t, br); len(got) < 2 || got[1] != "event: event" || !strings.Contains(strings.Join(got, "\n"), "card_moved") {
		t.Fatalf("the stream that recovered sent %q, want the recorded event", got)
	}
	end()

	// b: the ask dies during the connect, as a browser abort left the
	// earlier render's; the connect re-asks instead of ending the follow
	flaky := &flakyMay{r: r}
	r.c.cfg.MayMove = flaky.may
	r.c.mu.Lock()
	r.c.moves = nil // the green answer of a's connect is not this one's
	r.c.mu.Unlock()
	flaky.fail.Store(true)
	srv := httptest.NewServer(r.c)
	reqCtx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		resp *http.Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/projects/ai-software-factory-code/events/stream", nil)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		req.AddCookie(ck)
		resp, err := srv.Client().Do(req)
		done <- outcome{resp: resp, err: err}
	}()
	time.Sleep(time.Second)
	flaky.fail.Store(false) // Forgejo answers again while the connect waits
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		defer o.resp.Body.Close()
		if o.resp.StatusCode != http.StatusOK {
			t.Fatalf("a connect whose sight check errored once = %d, want 200", o.resp.StatusCode)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the connect did not recover within the bound")
	}
	defer func() { cancel(); srv.Close() }()
}

// A connect whose sight check keeps failing is re-asked within the bound
// and then answers 503: an outage cannot hold connects open forever.
func TestAStreamConnectEndsWhenTheSightCheckKeepsFailing(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.mayErr = context.Canceled
	start := time.Now()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events/stream", nil), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a connect whose sight check keeps failing = %d, want 503", rec.Code)
	}
	// the bound the fix states: sightWait with a breath of slack
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("the connect held on %s, past the stated bound", took)
	}
	if len(r.mayAsked) < 2 {
		t.Fatalf("the connect asked %v once, want it re-asked within the bound", r.mayAsked)
	}
}

// A refusal is not a failure: the connect answers it at once, with one
// ask and no retries, and the stream never opens.
func TestARefusedSightStillEndsTheStreamAtOnce(t *testing.T) {
	r := newRig(t)
	members(r)
	flaky := &flakyMay{r: r}
	r.c.cfg.MayMove = flaky.may
	ck, _ := memberLogin(t, r, "bob") // bob sees no code project
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events/stream", nil), ck); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "you read no project") {
		t.Fatalf("a refused sight = %d %.200q, want 403 at once", rec.Code, rec.Body.String())
	}
	if flaky.asks.Load() != 1 {
		t.Fatalf("the refused sight asked %d times, want once and no retries", flaky.asks.Load())
	}
}
