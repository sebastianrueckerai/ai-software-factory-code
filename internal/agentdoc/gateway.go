package agentdoc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Recorded is one chat call as it crossed the wire: the literal POST body
// plus the task and purpose headers the gateway bills by.
type Recorded struct {
	Raw     []byte
	Task    string
	Purpose string
}

// Gateway is an in-process stand-in for the real model gateway. It records
// every /v1/chat/completions POST verbatim and answers with the scripted
// content — as a completion for unstreamed requests, as an SSE stream ending
// on [DONE] for streamed ones — so the real client completes its call.
type Gateway struct {
	URL     string
	content string
	srv     *httptest.Server
	mu      sync.Mutex
	recs    []Recorded
}

func NewGateway(content string) *Gateway {
	g := &Gateway{content: content}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	g.URL = g.srv.URL
	return g
}

func (g *Gateway) Close() { g.srv.Close() }

// Bodies returns the recorded calls in the order they crossed the wire.
func (g *Gateway) Bodies() []Recorded {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Recorded, len(g.recs))
	copy(out, g.recs)
	return out
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	// the window ask is no chat call: it is not recorded, and the served
	// window answers it
	if strings.HasPrefix(r.URL.Path, "/v1/window/") {
		fmt.Fprint(w, `{"window":128000}`)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.recs = append(g.recs, Recorded{Raw: raw, Task: r.Header.Get("X-Factory-Task"), Purpose: r.Header.Get("X-Factory-Purpose")})
	g.mu.Unlock()
	var req struct {
		Stream bool `json:"stream"`
	}
	stream := json.Unmarshal(raw, &req) == nil && req.Stream
	w.Header().Set("Content-Type", "application/json")
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(http.StatusOK)
	if stream {
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": g.content}}}})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	answer, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": g.content}}}})
	w.Write(answer) // #nosec G104 -- a handler whose write fails has no reader to tell
}
