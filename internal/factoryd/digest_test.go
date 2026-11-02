package factoryd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type fakeDigester struct {
	mu     sync.Mutex
	calls  []string
	answer string
	err    error
}

func (f *fakeDigester) Digest(_ context.Context, taskID, thread, ask, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"|"+thread+"|"+ask+"|"+text)
	return f.answer, f.err
}

func (e *lookEnv) digest(t *testing.T, d Digester, body string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	e.runner.DigestHandler(d)(rr, httptest.NewRequest(http.MethodPost, "/internal/digest", strings.NewReader(body)))
	return rr.Code, strings.TrimSpace(rr.Body.String())
}

// A digest reads the workspace file from the running session's pod with
// the session's message bearer and hands the ask and the text to one
// Small call on the card's own task, no chat thread; the answer comes
// back labelled with the path. No ask asks for a summary.
func TestDigestOfAWorkspaceFile(t *testing.T) {
	e := newLookEnv(t)
	d := &fakeDigester{answer: "The store card, then the page card."}
	code, body := e.digest(t, d, `{"task_id":"t-1","path":"docs/plan.md","ask":"which cards, in which order?"}`)
	if code != http.StatusOK || body != `{"text":"docs/plan.md: The store card, then the page card."}` {
		t.Fatalf("digest = %d %s", code, body)
	}
	if len(e.workerIn) != 1 || e.workerIn[0] != `/file Bearer msg-tok {"path":"docs/plan.md"}` {
		t.Fatalf("worker = %v", e.workerIn)
	}
	if code, _ := e.digest(t, d, `{"task_id":"t-1","path":"docs/plan.md"}`); code != http.StatusOK {
		t.Fatalf("no ask = %d", code)
	}
	want := []string{"t-1||which cards, in which order?|" + planText, "t-1||Summarise this file.|" + planText}
	if strings.Join(d.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("digest calls = %q, want %q", d.calls, want)
	}
}

// Every refusal is one sentence naming the state and what to do: the
// worker's own passed on with its status, a worker that sends more than
// the cap refused as the worker would, a refused or failed or empty call
// named, and nothing asked of the model without the file's text.
func TestDigestRefusals(t *testing.T) {
	e := newLookEnv(t)
	d := &fakeDigester{answer: "an answer"}
	for _, tc := range []struct {
		in     string
		status int
		want   string
	}{
		{`{"task_id":"t-1"}`, http.StatusBadRequest, "task_id and path are required"},
		{`{"task_id":"t-9","path":"docs/plan.md"}`, http.StatusNotFound, "no task t-9"},
		{`{"task_id":"t-2","path":"docs/plan.md"}`, http.StatusConflict, "task t-2 runs no session now: a workspace file is read from the running session's sandbox"},
		{`{"task_id":"t-1","path":"docs/none.md"}`, http.StatusNotFound, "no file docs/none.md in the workspace"},
		{`{"task_id":"t-1","path":"/etc/hostname"}`, http.StatusForbidden, "/etc/hostname is outside the workspace"},
		{`{"task_id":"t-1","path":"old.png"}`, http.StatusConflict, "task t-1's session runs a worker without the workspace read: its next session has it"},
		{`{"task_id":"t-1","path":"docs/huge.md"}`, http.StatusRequestEntityTooLarge,
			"docs/huge.md is larger than 128 KiB, more than one digest reads: read the part you need with the read tool's offset and limit"},
		{`{"task_id":"t-1","path":"docs/big.md"}`, http.StatusRequestEntityTooLarge,
			"docs/big.md is larger than 128 KiB, more than one digest reads: read the part you need with the read tool's offset and limit"},
	} {
		if code, body := e.digest(t, d, tc.in); code != tc.status || body != tc.want {
			t.Errorf("%s = %d %q, want %d %q", tc.in, code, body, tc.status, tc.want)
		}
	}
	if len(d.calls) != 0 {
		t.Fatalf("digest calls = %q, want none", d.calls)
	}

	ok := `{"task_id":"t-1","path":"docs/plan.md","ask":"the cards"}`
	d.err = &policy.Refusal{Decision: "refused_budget", Reason: "task t-1 has 0.000100 EUR left"}
	if code, body := e.digest(t, d, ok); code != http.StatusConflict || body != "the digest of docs/plan.md was refused (refused_budget): task t-1 has 0.000100 EUR left" {
		t.Fatalf("refused = %d %q", code, body)
	}
	d.err = errors.New("stream ended before [DONE]")
	if code, body := e.digest(t, d, ok); code != http.StatusBadGateway || body != "the small model did not answer on docs/plan.md: ask again shortly" {
		t.Fatalf("failed = %d %q", code, body)
	}
	d.err, d.answer = nil, " \n"
	if code, body := e.digest(t, d, ok); code != http.StatusBadGateway || body != "the small model answered nothing on docs/plan.md: ask again, or read the part you need" {
		t.Fatalf("empty = %d %q", code, body)
	}
}
