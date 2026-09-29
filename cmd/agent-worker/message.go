package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
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

// messageHandler is the one endpoint factoryd reaches in the pod: POST
// /message {text} with the session's bearer, handed to send, the
// harness's Message.
func messageHandler(tokenSHA256 [32]byte, send func(context.Context, string) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /message", func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(sum[:], tokenSHA256[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
			http.Error(w, "want {\"text\": \"...\"}", http.StatusBadRequest)
			return
		}
		switch err := send(r.Context(), in.Text); {
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
	return mux
}

// serveMessages listens on harness.MessagePort until the returned stop.
func serveMessages(tokenSHA256 [32]byte, send func(context.Context, string) error, log func(string, ...any)) (stop func(), err error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(harness.MessagePort))
	if err != nil {
		return nil, fmt.Errorf("message endpoint: %w", err)
	}
	srv := &http.Server{Handler: messageHandler(tokenSHA256, send), ReadHeaderTimeout: 10 * time.Second}
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
