package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiteLLMBlockTaskKeys(t *testing.T) {
	var blocked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/key/list":
			io.WriteString(w, `{"keys":[
				{"key":"sk-t1-live","metadata":{"task_id":"t1"},"blocked":false},
				{"key":"sk-t1-dead","metadata":{"task_id":"t1"},"blocked":true},
				{"key":"sk-t2","metadata":{"task_id":"t2"},"blocked":false},
				{"key":"sk-notask","metadata":{"user":"x"},"blocked":false}]}`)
		case r.Method == "POST" && r.URL.Path == "/key/block":
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("block body: %v", err)
			}
			blocked = append(blocked, body["key"].(string))
			io.WriteString(w, `{"blocked":true}`)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	l := &LiteLLMAdmin{BaseURL: srv.URL, MasterKey: "mk"}
	n, err := l.BlockTaskKeys(context.Background(), "t1")
	if err != nil {
		t.Fatalf("BlockTaskKeys: %v", err)
	}
	if n != 1 || strings.Join(blocked, ",") != "sk-t1-live" {
		t.Fatalf("blocked = %d %v, want exactly the task's live key", n, blocked)
	}
}

func TestLiteLLMBlockTaskKeysPaginatesAndStopsOnRepeat(t *testing.T) {
	var listCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key/list" {
			listCalls++
			// The server ignores page params and always returns the same page:
			// the client must not loop forever.
			io.WriteString(w, `{"keys":[{"key":"sk-a","metadata":{"task_id":"t9"},"blocked":false}]}`)
			return
		}
		io.WriteString(w, `{"blocked":true}`)
	}))
	defer srv.Close()

	l := &LiteLLMAdmin{BaseURL: srv.URL, MasterKey: "mk", PageSize: 1}
	n, err := l.BlockTaskKeys(context.Background(), "t9")
	if err != nil {
		t.Fatalf("BlockTaskKeys: %v", err)
	}
	if n != 1 || listCalls != 2 {
		t.Fatalf("blocked = %d, list calls = %d (want 1 block, 2 pages: fetch then repeat-detect)", n, listCalls)
	}
}

func TestLiteLLMBlockSendsMasterKey(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"keys":[]}`)
	}))
	defer srv.Close()
	l := &LiteLLMAdmin{BaseURL: srv.URL, MasterKey: "mk"}
	if _, err := l.BlockTaskKeys(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer mk" {
		t.Fatalf("Authorization = %q", auth)
	}
}
