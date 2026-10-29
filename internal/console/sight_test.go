package console

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyRepo answers the read of the code project's repository, a reader's
// sight of its board: 502 while fail holds, else 200 where shows and 404
// where not; asks counts the reads. Its fields are atomics, so a test
// flips them while a stream's handler reads them.
type flakyRepo struct {
	base  http.RoundTripper
	shows bool
	fail  atomic.Bool
	asks  atomic.Int64
}

func (f *flakyRepo) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != "/api/v1/repos/sf/code" {
		return f.base.RoundTrip(r)
	}
	f.asks.Add(1)
	status := http.StatusNotFound
	switch {
	case f.fail.Load():
		status = http.StatusBadGateway
	case f.shows:
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

// readerRig is the members' rig with bob logged in, who writes demo and
// sees the code project only where Forgejo shows him its repository.
func readerRig(t *testing.T, shows bool) (*rig, *flakyRepo, *http.Cookie) {
	r := newRig(t)
	members(r)
	f := &flakyRepo{base: http.DefaultTransport, shows: shows}
	r.c.cfg.HTTP = &http.Client{Transport: f}
	ck, _ := memberLogin(t, r, "bob")
	return r, f, ck
}

// A stream connect whose sight check errors still follows the board
// without a page reload: a read that fails during the connect is asked
// again within a bound instead of answered 503, which closes the
// browser's EventSource for good.
func TestAStreamConnectSurvivesAnErroredSightCheck(t *testing.T) {
	r, f, ck := readerRig(t, true)
	f.fail.Store(true)
	srv := httptest.NewServer(r.c)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); srv.Close() }()
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
	f.fail.Store(false) // Forgejo answers again while the connect waits
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
}

// A connect whose sight check keeps failing is re-asked within the bound
// and then answers 503: an outage cannot hold connects open forever.
func TestAStreamConnectEndsWhenTheSightCheckKeepsFailing(t *testing.T) {
	r, f, ck := readerRig(t, true)
	f.fail.Store(true)
	start := time.Now()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events/stream", nil), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a connect whose sight check keeps failing = %d, want 503", rec.Code)
	}
	// the bound the fix states: sightWait with a breath of slack
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("the connect held on %s, past the stated bound", took)
	}
	if n := f.asks.Load(); n < 2 {
		t.Fatalf("the connect asked %d times, want it re-asked within the bound", n)
	}
}

// A refusal is not a failure: the connect answers it at once, with one
// ask and no retries, and the stream never opens.
func TestARefusedSightStillEndsTheStreamAtOnce(t *testing.T) {
	r, f, ck := readerRig(t, false) // bob sees no code project
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events/stream", nil), ck); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "you read no project") {
		t.Fatalf("a refused sight = %d %.200q, want 403 at once", rec.Code, rec.Body.String())
	}
	if n := f.asks.Load(); n != 1 {
		t.Fatalf("the refused sight asked %d times, want once and no retries", n)
	}
}
