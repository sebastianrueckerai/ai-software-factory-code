package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			key, err := l.Mint(context.Background(), "42", 8, []string{"coding-cheap", "coding-frontier"})

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
			if gotPayload["max_budget"] != 8.0 {
				t.Fatalf("max_budget = %v, want 8", gotPayload["max_budget"])
			}
			if models, _ := json.Marshal(gotPayload["models"]); string(models) != `["coding-cheap","coding-frontier"]` {
				t.Fatalf("models = %s, want the task's aliases", models)
			}
			meta, _ := gotPayload["metadata"].(map[string]any)
			if meta["task_id"] != "42" {
				t.Fatalf("metadata.task_id = %v, want 42", meta["task_id"])
			}
		})
	}
}

func TestLiteLLMMintRefusesNoModels(t *testing.T) {
	l := &LiteLLM{BaseURL: "http://127.0.0.1:1", MasterKey: "sk-master"}
	if _, err := l.Mint(context.Background(), "42", 8, nil); err == nil || !strings.Contains(err.Error(), "no models") {
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
