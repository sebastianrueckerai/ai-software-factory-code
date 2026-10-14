package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// drainServer serves h on a local port with the calls context drainCalls
// cancels, and answers the server, its cancel and its URL.
func drainServer(t *testing.T, h http.HandlerFunc) (*http.Server, context.CancelFunc, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	calls, endCalls := context.WithCancel(context.Background())
	server := &http.Server{Handler: h, BaseContext: func(net.Listener) context.Context { return calls }}
	go server.Serve(l) // #nosec G104 -- ends with ErrServerClosed at the test's shutdown
	return server, endCalls, "http://" + l.Addr().String()
}

// A roll lets a call in flight finish within the drain: its caller gets
// the whole answer.
func TestDrainLetsACallInFlightFinish(t *testing.T) {
	started := make(chan struct{})
	server, endCalls, url := drainServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, "answer") // #nosec G104 -- the test reads it back
	})
	got := make(chan string, 1)
	go func() {
		res, err := http.Get(url)
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		got <- string(b)
	}()
	<-started
	drainCalls(server, endCalls, 5*time.Second, time.Second)
	if s := <-got; s != "answer" {
		t.Fatalf("the caller got %q, want the whole answer", s)
	}
}

// A call still running when the drain ends is cancelled, and its handler
// finishes its bookkeeping (the gateway books the hold and audits the
// call) before the shutdown returns, not after the process is gone.
func TestDrainCancelsACallPastItAndLetsItSettle(t *testing.T) {
	started := make(chan struct{})
	var settled atomic.Bool
	server, endCalls, url := drainServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		time.Sleep(50 * time.Millisecond) // the hold's release and the audit row
		settled.Store(true)
	})
	go http.Get(url) // #nosec G104 G107 -- the call is cut by design
	<-started
	begin := time.Now()
	drainCalls(server, endCalls, 100*time.Millisecond, 2*time.Second)
	if !settled.Load() {
		t.Fatal("the shutdown returned before the cut call settled")
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("the shutdown took %s, want the drain plus the settling only", d)
	}
}
