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

// messageHandler serves factoryd's two requests into the pod, each with
// the session's bearer: POST /message {text}, handed to send, the
// harness's Message, and POST /push {token}, which gate takes.
// messageHandler serves factoryd's messages: text for the agent's next
// turn, with model when the session continues on another model from then
// on (roadmap 222, item 5).
func messageHandler(tokenSHA256 [32]byte, gate *pushGate, send func(ctx context.Context, text, model string) error, ck *checkinServe) http.Handler {
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(sum[:], tokenSHA256[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
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
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.Token == "" {
			http.Error(w, "want {\"token\": \"...\"}", http.StatusBadRequest)
			return
		}
		if !gate.take(in.Token) {
			http.Error(w, "no push waits for a token", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ck.serve(mux, authorized)
	return mux
}

// pushGate hands factoryd's push token to the session's push: it takes a
// token only while ask waits — the push asks once every agent process is
// dead, and re-asks until answered — and only one token is ever taken.
type pushGate struct {
	mu     sync.Mutex
	asking bool
	token  chan string
}

func newPushGate() *pushGate { return &pushGate{token: make(chan string, 1)} }

// pushRetryEvery paces the push request's re-asks inside its one window:
// factoryd's tap answers every ask, so one transient failure on its side
// costs an interval, not the push.
var pushRetryEvery = 5 * time.Second

// take hands token to the waiting ask; false when none waits.
func (g *pushGate) take(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.asking {
		return false
	}
	g.asking = false
	g.token <- token
	return true
}

// ask writes the push request for commit on w, the stdout factoryd reads,
// re-writing it every pushRetryEvery until factoryd's POST /push arrives
// or wait runs out.
func (g *pushGate) ask(commit string, wait time.Duration, w io.Writer) (string, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	line, _ := json.Marshal(harness.WorkerEnvelope{Type: "push", Push: &harness.PushRequest{Commit: commit}})
	for {
		g.mu.Lock()
		g.asking = true
		g.mu.Unlock()
		fmt.Fprintln(w, string(line))
		retry := time.NewTimer(pushRetryEvery)
		select {
		case token := <-g.token:
			retry.Stop()
			return token, nil
		case <-retry.C:
		case <-deadline.C:
			retry.Stop()
			g.mu.Lock()
			g.asking = false
			g.mu.Unlock()
			select {
			case token := <-g.token:
				return token, nil
			default:
				return "", fmt.Errorf("no push token from factoryd within %s", wait)
			}
		}
	}
}

// serveMessages listens on harness.MessagePort until the returned stop.
func serveMessages(tokenSHA256 [32]byte, gate *pushGate, send func(ctx context.Context, text, model string) error, ck *checkinServe, log func(string, ...any)) (stop func(), err error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(harness.MessagePort))
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: messageHandler(tokenSHA256, gate, send, ck), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log("message endpoint: %v", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) // #nosec G104 -- the session is over; a hung request is cut
	}, nil
}
