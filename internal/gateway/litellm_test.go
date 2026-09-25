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
			key, err := l.Mint(context.Background(), "42", 8)

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
			meta, _ := gotPayload["metadata"].(map[string]any)
			if meta["task_id"] != "42" {
				t.Fatalf("metadata.task_id = %v, want 42", meta["task_id"])
			}
		})
	}
}
