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

// keyHub serves the wire shapes recorded from the live proxy (1.102.1):
// /key/list?return_full_object=true returns key objects whose token is the
// hash, with metadata, spend and blocked, and total_pages.
type keyHub struct {
	pages   map[int]string // page number → list body
	blocked []string
	lists   []string
}

func (h *keyHub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/key/list":
			page := r.URL.Query().Get("page")
			if r.URL.Query().Get("return_full_object") != "true" {
				t.Errorf("key/list without return_full_object: %s", r.URL.RawQuery)
			}
			h.lists = append(h.lists, page)
			var pn int
			fmt.Sscanf(page, "%d", &pn)
			io.WriteString(w, h.pages[pn])
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
		pages: map[int]string{1: `{"keys":[
			{"token":"h-t1-live","key_name":"sk-...ive","blocked":null,"spend":0.1,"metadata":{"task_id":"t1"}},
			{"token":"h-t1-dead","key_name":"sk-...ead","blocked":true,"spend":0.2,"metadata":{"task_id":"t1"}},
			{"token":"h-t2","key_name":"sk-...t2","blocked":null,"spend":0,"metadata":{"task_id":"t2"}},
			{"token":"h-notask","key_name":"sk-...nsk","blocked":null,"spend":null,"metadata":{"user":"x"}},
			{"token":"h-nometa","key_name":"sk-...nmt","blocked":null,"spend":1,"metadata":null}
		],"total_count":5,"current_page":1,"total_pages":1}`},
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

// A reconcile runs at the start and the end of every session attempt, while
// the gateway still holds its factoryd-slot key cached: blocking that key
// would kill factoryd's own next call, so only the agent's keys go.
func TestLiteLLMBlockTaskKeysLeavesTheFactorydSlotLive(t *testing.T) {
	hub := &keyHub{
		pages: map[int]string{1: `{"keys":[
			{"token":"h-agent","blocked":null,"spend":0.1,"metadata":{"task_id":"t1","slot":"agent"}},
			{"token":"h-factoryd","blocked":null,"spend":0.2,"metadata":{"task_id":"t1","slot":"factoryd"}}
		],"total_count":2,"current_page":1,"total_pages":1}`},
	}
	l := &LiteLLMAdmin{BaseURL: hub.srv(t).URL, MasterKey: "mk"}
	n, err := l.BlockTaskKeys(context.Background(), "t1")
	if err != nil {
		t.Fatalf("BlockTaskKeys: %v", err)
	}
	if n != 1 || strings.Join(hub.blocked, ",") != "h-agent" {
		t.Fatalf("blocked = %d %v, want only the agent key, not the factoryd slot", n, hub.blocked)
	}
}

func TestLiteLLMBlockTaskKeysWalksEveryPage(t *testing.T) {
	hub := &keyHub{
		pages: map[int]string{
			1: `{"keys":[{"token":"h-a","blocked":null,"metadata":{"task_id":"t9"}},{"token":"h-b","blocked":null,"metadata":{"task_id":"other"}}],"total_count":3,"current_page":1,"total_pages":2}`,
			2: `{"keys":[{"token":"h-c","blocked":null,"metadata":{"task_id":"t9"}}],"total_count":3,"current_page":2,"total_pages":2}`,
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

// One pass lists every key once, page by page, and sums spend per task.
func TestLiteLLMSpendByTaskListsEveryKeyOnce(t *testing.T) {
	hub := &keyHub{
		pages: map[int]string{
			1: `{"keys":[{"token":"h-a","spend":0.25,"metadata":{"task_id":"t1"}},{"token":"h-b","spend":0.5,"metadata":{"task_id":"t2"}}],"total_pages":2}`,
			2: `{"keys":[{"token":"h-c","spend":0.125,"metadata":{"task_id":"t1"}},{"token":"h-d","spend":4,"metadata":{"user":"x"}}],"total_pages":2}`,
		},
	}
	l := &LiteLLMAdmin{BaseURL: hub.srv(t).URL, MasterKey: "mk", PageSize: 2}
	got, err := l.SpendByTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// t1: 0.25 + 0.125 across both pages; t2: 0.5; the task-less key is not a task
	if len(got) != 2 || got["t1"] != 0.375 || got["t2"] != 0.5 {
		t.Fatalf("spend = %v, want map[t1:0.375 t2:0.5]", got)
	}
	if strings.Join(hub.lists, ",") != "1,2" {
		t.Fatalf("list pages = %v, want 1,2 once each", hub.lists)
	}
}
