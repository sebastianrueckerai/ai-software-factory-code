package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLiteLLMMint(t *testing.T) {
	tests := []struct {
		name       string
		respStatus int
		respBody   string
		wantKey    string
		wantErr    string
	}{
		{
			name:       "key field",
			respStatus: http.StatusOK,
			respBody:   `{"key":"sk-new-virtual-key","expires":"none"}`,
			wantKey:    "sk-new-virtual-key",
		},
		{
			name:       "token field fallback",
			respStatus: http.StatusOK,
			respBody:   `{"token":"sk-token-shaped"}`,
			wantKey:    "sk-token-shaped",
		},
		{
			name:       "non-200 refuses",
			respStatus: http.StatusUnauthorized,
			respBody:   `{"error":"bad master key"}`,
			wantErr:    "401",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth, gotPath string
			var gotPayload map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				json.Unmarshal(body, &gotPayload)
				gotAuth = r.Header.Get("Authorization")
				gotPath = r.URL.Path
				w.WriteHeader(tt.respStatus)
				w.Write([]byte(tt.respBody))
			}))
			defer srv.Close()

			l := &LiteLLM{BaseURL: srv.URL, MasterKey: "sk-master", HTTP: srv.Client()}
			key, err := l.Mint(context.Background(), "42", "42", []string{"small", "big"})

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Mint succeeded with %q, want error", key)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Mint error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			if key != tt.wantKey {
				t.Fatalf("key = %q, want %q", key, tt.wantKey)
			}
			if gotAuth != "Bearer sk-master" {
				t.Fatalf("auth = %q, want master key bearer", gotAuth)
			}
			if gotPath != "/key/generate" {
				t.Fatalf("path = %q, want /key/generate", gotPath)
			}
			if _, ok := gotPayload["max_budget"]; ok {
				t.Fatalf("max_budget = %v, want none: the gateway's holds are the budget", gotPayload["max_budget"])
			}
			if models, _ := json.Marshal(gotPayload["models"]); string(models) != `["small","big"]` {
				t.Fatalf("models = %s, want the task's aliases", models)
			}
			meta, _ := gotPayload["metadata"].(map[string]any)
			if meta["task_id"] != "42" {
				t.Fatalf("metadata.task_id = %v, want 42", meta["task_id"])
			}
			if meta["slot"] != "agent" {
				t.Fatalf("metadata.slot = %v, want agent for the task's own slot", meta["slot"])
			}
		})
	}
}

// The factoryd slot is named in the key's metadata, so factoryd's reconcile
// can tell its own keys from the agent's and leave them live.
func TestLiteLLMMintNamesTheFactorydSlot(t *testing.T) {
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotPayload)
		io.WriteString(w, `{"key":"sk-factoryd-key"}`)
	}))
	defer srv.Close()
	l := &LiteLLM{BaseURL: srv.URL, MasterKey: "sk-master", HTTP: srv.Client()}
	if _, err := l.Mint(context.Background(), "42/factoryd", "42", []string{"big"}); err != nil {
		t.Fatal(err)
	}
	meta, _ := gotPayload["metadata"].(map[string]any)
	if meta["slot"] != "factoryd" || meta["task_id"] != "42" {
		t.Fatalf("metadata = %v, want slot factoryd on task 42", meta)
	}
}

func TestLiteLLMMintRefusesNoModels(t *testing.T) {
	l := &LiteLLM{BaseURL: "http://127.0.0.1:1", MasterKey: "sk-master"}
	if _, err := l.Mint(context.Background(), "42", "42", nil); err == nil || !strings.Contains(err.Error(), "no models") {
		t.Fatalf("Mint without models = %v, want a refusal", err)
	}
}

func TestLiteLLMBlock(t *testing.T) {
	var got map[string]string
	var path, auth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	l := &LiteLLM{BaseURL: srv.URL, MasterKey: "sk-master"}
	if err := l.Block(context.Background(), "sk-old"); err != nil || path != "/key/block" || auth != "Bearer sk-master" || got["key"] != "sk-old" {
		t.Fatalf("Block = %v: %s %s %v", err, path, auth, got)
	}
	status = http.StatusNotFound
	if err := l.Block(context.Background(), "sk-old"); err == nil {
		t.Fatal("Block with a 404 succeeded")
	}
}

// CallSpend sums the spend rows LiteLLM wrote for one hold's calls, found
// by the hold id the gateway tags upstream calls with. The deployed
// LiteLLM (controller probe, 2026-10-16) requires start_date and end_date
// both, with the date's space as %20, and keys rows by the token's SHA-256.
func TestLiteLLMCallSpendSumsTheRowsOfAHold(t *testing.T) {
	var gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		io.WriteString(w, `{"data":[
			{"spend":0.01,"metadata":{"spend_logs_metadata":{"hold_id":7}}},
			{"spend":0.02,"metadata":{"spend_logs_metadata":{"hold_id":7}}},
			{"spend":0.5,"metadata":{"spend_logs_metadata":{"hold_id":8}}},
			{"spend":0.7,"metadata":{}}]}`)
	}))
	defer srv.Close()
	l := &LiteLLM{BaseURL: srv.URL, MasterKey: "master"}
	since := time.Date(2026, 10, 15, 10, 0, 30, 0, time.UTC)
	cost, found, err := l.CallSpend(context.Background(), "sk-task", 7, since)
	if err != nil || !found || cost != 0.03 {
		t.Fatalf("CallSpend = %v, %v, %v; want 0.03, true", cost, found, err)
	}
	sum := sha256.Sum256([]byte("sk-task"))
	head, tail := "api_key="+hex.EncodeToString(sum[:])+"&end_date=", "&page_size=100&start_date=2026-10-15%2009:59:30"
	if !strings.HasPrefix(gotQuery, head) || !strings.HasSuffix(gotQuery, tail) {
		t.Fatalf("query = %q; want %s<end_date>%s", gotQuery, head, tail)
	}
	// end_date bounds the read at about now: a cancelled call's row lands
	// after its start, and LiteLLM's clock may sit a little off the gateway's
	end, err := time.Parse("2006-01-02 15:04:05", strings.Replace(strings.TrimSuffix(strings.TrimPrefix(gotQuery, head), tail), "%20", " ", 1))
	if err != nil || time.Until(end) < -time.Minute || time.Until(end) > 5*time.Minute {
		t.Fatalf("end_date = %q (%v); want now, within a few minutes", end, err)
	}
	if gotAuth != "Bearer master" {
		t.Fatalf("auth = %q, want Bearer master", gotAuth)
	}
	if _, found, err := l.CallSpend(context.Background(), "sk-task", 99, since); err != nil || found {
		t.Fatalf("an unknown hold = %v, %v; want not found", found, err)
	}
}

func TestLiteLLMCallSpendErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	if _, _, err := (&LiteLLM{BaseURL: srv.URL}).CallSpend(context.Background(), "k", 1, time.Now()); err == nil {
		t.Fatal("a 502 must be an error")
	}
}
