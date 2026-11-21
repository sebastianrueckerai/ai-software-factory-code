//go:build unix

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// parseHash decodes MESSAGE_TOKEN_SHA256: the worker holds only the hash of
// the session's message secret, so nothing in the pod can present it.
func parseHash(hexed string) ([32]byte, error) {
	var h [32]byte
	b, err := hex.DecodeString(hexed)
	if err != nil || len(b) != len(h) {
		return h, errors.New("MESSAGE_TOKEN_SHA256 must be 64 hex digits")
	}
	copy(h[:], b)
	return h, nil
}

// bearer reports whether r carries the bearer whose hash is tokenSHA256,
// and answers 401 when it does not.
func bearer(tokenSHA256 [32]byte, w http.ResponseWriter, r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(token))
	if !ok || subtle.ConstantTimeCompare(sum[:], tokenSHA256[:]) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// messageHandler serves factoryd's requests into the pod, each with the
// session's bearer: POST /message {text}, handed to send, the harness's
// Message, and POST /push {token, name}, which gate takes.
// messageHandler serves factoryd's messages: text for the agent's next
// turn, with model when the session continues on another model from then
// on (roadmap 222, item 5).
func messageHandler(tokenSHA256 [32]byte, gate *pushGate, send func(ctx context.Context, text, model string) error, ck *checkinServe, wip *wipServe) http.Handler {
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool { return bearer(tokenSHA256, w, r) }
	mux.HandleFunc("POST /message", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in struct {
			Text  string `json:"text"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
			http.Error(w, "want {\"text\": \"...\"}", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), harness.MessageTimeout)
		defer cancel()
		switch err := send(ctx, in.Text, in.Model); {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, harness.ErrNotSupported):
			http.Error(w, err.Error(), http.StatusNotImplemented)
		case errors.Is(err, harness.ErrNotRunning):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
	})
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in struct {
			Token string `json:"token"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.Token == "" {
			http.Error(w, "want {\"token\": \"...\"}", http.StatusBadRequest)
			return
		}
		if !gate.take(pushCred{token: in.Token, name: in.Name}) {
			http.Error(w, "no push waits for a token", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ck.serve(mux, authorized)
	if wip != nil {
		wip.serve(mux, authorized)
	}
	return mux
}

// pushCred is one push credential factoryd answered with: the token
// secret, and the name it was minted under, which a refused push names in
// its event.
type pushCred struct {
	token, name string
}

// pushGate hands factoryd's push credentials to the session's push: it
// takes a credential only while ask waits — the push asks once every agent
// process is dead, and re-asks, each ask its own ordinal on the wire, until
// answered — and one credential per ask.
type pushGate struct {
	mu     sync.Mutex
	asking bool
	token  chan pushCred
	taken  pushCred // the credential handed over, for the session's scrub
	asks   int      // the asks factoryd answered; the next ask's ordinal
}

func newPushGate() *pushGate { return &pushGate{token: make(chan pushCred, 1)} }

// pushRetryEvery paces the push request's re-asks inside its one window:
// factoryd's tap answers every ask, so one transient failure on its side
// costs an interval, not the push.
var pushRetryEvery = 5 * time.Second

// take hands cred to the waiting ask; false when none waits.
func (g *pushGate) take(cred pushCred) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.asking {
		return false
	}
	g.asking = false
	g.asks++
	g.taken = cred
	g.token <- cred
	return true
}

// sent is the push token factoryd sent, "" before it arrived.
func (g *pushGate) sent() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.taken.token
}

// ask writes the push request for commit on w, the stdout factoryd reads,
// its ordinal one past every ask factoryd answered, re-writing it every
// pushRetryEvery until factoryd's POST /push arrives or wait runs out.
func (g *pushGate) ask(commit string, wait time.Duration, w io.Writer) (pushCred, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	g.mu.Lock()
	line, _ := json.Marshal(harness.WorkerEnvelope{Type: "push", Push: &harness.PushRequest{Commit: commit, Own: g.asks + 1}})
	g.mu.Unlock()
	for {
		g.mu.Lock()
		g.asking = true
		g.mu.Unlock()
		fmt.Fprintln(w, string(line))
		retry := time.NewTimer(pushRetryEvery)
		select {
		case cred := <-g.token:
			retry.Stop()
			return cred, nil
		case <-retry.C:
		case <-deadline.C:
			retry.Stop()
			g.mu.Lock()
			g.asking = false
			g.mu.Unlock()
			select {
			case cred := <-g.token:
				return cred, nil
			default:
				return pushCred{}, fmt.Errorf("no push token from factoryd within %s", wait)
			}
		}
	}
}

// serveMessages serves the session's messageHandler until the returned stop.
func serveMessages(tokenSHA256 [32]byte, gate *pushGate, send func(ctx context.Context, text, model string) error, ck *checkinServe, wip *wipServe, log func(string, ...any)) (stop func(), err error) {
	return servePort(messageHandler(tokenSHA256, gate, send, ck, wip), log)
}

// listenPort opens the listener servePort serves on: harness.MessagePort
// on every interface. A var, so a test can hand it a free loopback port.
var listenPort = func() (net.Listener, error) {
	return net.Listen("tcp", ":"+strconv.Itoa(harness.MessagePort)) // #nosec G102 -- the harness reaches this endpoint across the pod network; its handler rejects every request without the session token
}

// servePort serves h on listenPort's listener until the returned stop,
// which lets the requests in flight finish.
func servePort(h http.Handler, log func(string, ...any)) (stop func(), err error) {
	ln, err := listenPort()
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log("message endpoint: %v", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) // #nosec G104 -- the port is done with; a hung request is cut
	}, nil
}
