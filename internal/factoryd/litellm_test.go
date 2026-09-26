package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// keyHub serves the wire shapes recorded from the live proxy: /key/list
// returns hashes with total_pages, /key/info carries metadata and blocked.
type keyHub struct {
	pages   map[int]string // page number → list body
	infos   map[string]string
	blocked []string
	lists   []string
}

func (h *keyHub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/key/list":
			page := r.URL.Query().Get("page")
			h.lists = append(h.lists, page)
			var pn int
			fmt.Sscanf(page, "%d", &pn)
			io.WriteString(w, h.pages[pn])
		case r.Method == "GET" && r.URL.Path == "/key/info":
			io.WriteString(w, h.infos[r.URL.Query().Get("key")])
		case r.Method == "POST" && r.URL.Path == "/key/block":
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("block body: %v", err)
			}
			h.blocked = append(h.blocked, body["key"].(string))
			io.WriteString(w, `{"blocked":true}`)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLiteLLMBlockTaskKeys(t *testing.T) {
	hub := &keyHub{
		pages: map[int]string{1: `{"keys":["h-t1-live","h-t1-dead","h-t2","h-notask"],"total_count":4,"current_page":1,"total_pages":1}`},
		infos: map[string]string{
			"h-t1-live": `{"key":"h-t1-live","info":{"key_name":"sk-...ive","blocked":null,"metadata":{"task_id":"t1"}}}`,
			"h-t1-dead": `{"key":"h-t1-dead","info":{"key_name":"sk-...ead","blocked":true,"metadata":{"task_id":"t1"}}}`,
			"h-t2":      `{"key":"h-t2","info":{"key_name":"sk-...t2","blocked":null,"metadata":{"task_id":"t2"}}}`,
			"h-notask":  `{"key":"h-notask","info":{"key_name":"sk-...nsk","blocked":null,"metadata":{"user":"x"}}}`,
		},
	}
	l := &LiteLLMAdmin{BaseURL: hub.srv(t).URL, MasterKey: "mk"}
	n, err := l.BlockTaskKeys(context.Background(), "t1")
	if err != nil {
		t.Fatalf("BlockTaskKeys: %v", err)
	}
	if n != 1 || strings.Join(hub.blocked, ",") != "h-t1-live" {
		t.Fatalf("blocked = %d %v, want exactly the task's live key by hash", n, hub.blocked)
	}
	// Pages start at 1: page=0 is a live-proxy 422, never a valid page.
	if len(hub.lists) == 0 || hub.lists[0] != "1" {
		t.Fatalf("list pages requested = %v, want first request at page 1", hub.lists)
	}
}

func TestLiteLLMBlockTaskKeysWalksEveryPage(t *testing.T) {
	hub := &keyHub{
		pages: map[int]string{
			1: `{"keys":["h-a","h-b"],"total_count":3,"current_page":1,"total_pages":2}`,
			2: `{"keys":["h-c"],"total_count":3,"current_page":2,"total_pages":2}`,
		},
		infos: map[string]string{
			"h-a": `{"key":"h-a","info":{"blocked":null,"metadata":{"task_id":"t9"}}}`,
			"h-b": `{"key":"h-b","info":{"blocked":null,"metadata":{"task_id":"other"}}}`,
			"h-c": `{"key":"h-c","info":{"blocked":null,"metadata":{"task_id":"t9"}}}`,
		},
	}
	l := &LiteLLMAdmin{BaseURL: hub.srv(t).URL, MasterKey: "mk", PageSize: 2}
	n, err := l.BlockTaskKeys(context.Background(), "t9")
	if err != nil {
		t.Fatalf("BlockTaskKeys: %v", err)
	}
	// h-c lives on page 2: a client that stops at the first page misses it.
	if n != 2 || strings.Join(hub.blocked, ",") != "h-a,h-c" {
		t.Fatalf("blocked = %d %v, want keys from both pages", n, hub.blocked)
	}
}

func TestLiteLLMBlockTaskKeysNoKeys(t *testing.T) {
	hub := &keyHub{pages: map[int]string{1: `{"keys":[],"total_count":0,"current_page":1,"total_pages":0}`}}
	l := &LiteLLMAdmin{BaseURL: hub.srv(t).URL, MasterKey: "mk"}
	n, err := l.BlockTaskKeys(context.Background(), "t1")
	if err != nil || n != 0 {
		t.Fatalf("BlockTaskKeys = %d, %v; want 0, nil on an empty proxy", n, err)
	}
}

func TestLiteLLMBlockSendsMasterKey(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"keys":[],"total_pages":0}`)
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
