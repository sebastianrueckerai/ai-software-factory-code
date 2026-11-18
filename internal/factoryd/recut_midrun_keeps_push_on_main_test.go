package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// liveTokenNames are the token names Forgejo still holds: minted and not
// yet revoked.
func liveTokenNames(h *forgejoHub) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.tokens))
	for _, t := range h.tokens {
		out = append(out, t.Name)
	}
	return out
}

func hubCalls(h *forgejoHub) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// A session whose card is re-cut mid-run keeps a working push. The re-cut's
// cleanup, Completion.reconcile, still revokes the live session's push
// token — deliberately, for a fresh name per ask keeps the retry alive
// (completion.go names issue 1836) — so the session's push on its first
// ask's token fails naming the refused credential, its bounded re-ask is
// answered with exactly one fresh write-scoped mint under the next own
// ordinal, the push lands on it, and the session's end revokes every
// minted name.
func TestCardRecutMidRunKeepsAWorkingPush(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, task, kube, objects)

	// the re-cut's cleanup runs once, while the session pushes on its
	// first ask's token: the push is mid-flight at the POST that carries
	// it, so the hook errors unless that token was live and asserts the
	// re-cut revoked it
	var hookErr, recutErr error
	var before, after []string
	var recut atomic.Bool
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent, onPush: func() {
		if !recut.CompareAndSwap(false, true) {
			return // the fresh token's post needs no re-cut
		}
		before = liveTokenNames(hub)
		if !slices.Contains(before, PushTokenName("t1", "own-1")) {
			hookErr = fmt.Errorf("live tokens at the re-cut = %v, want the session's push token task-t1-push-own-1 among them", before)
			return
		}
		if recutErr = runner.completion.reconcile(context.Background(), "t1"); recutErr != nil {
			return
		}
		if after = liveTokenNames(hub); len(after) != 0 {
			hookErr = fmt.Errorf("live tokens after the re-cut = %v, want every card-named token revoked, the session's push token with them", after)
		}
	}}
	worker.start(t, runner)

	// the refused push's failure, named for the credential git rendered as
	// "Repository not found" — the shape cmd/agent-worker reports it in
	refused := envLine(harness.Event{Kind: harness.EventPushFailed, Time: time.Now(), Detail: map[string]any{
		"branch": "factory/t1",
		"error": "push credential task-t1-push-own-1 refused by http://forgejo-http.forgejo.svc.cluster.local:3000/factory-agent/x.git: " +
			"git push: exit status 128: remote: Repository not found; fatal: repository 'http://forgejo-http.forgejo.svc.cluster.local:3000/factory-agent/x.git/' not found",
	}})
	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		pushAskLine("c1", 1) + "\n" + tokenLogs + "\n" +
		refused + "\n" +
		pushAskLine("c1", 2) + "\n" + tokenLogs + pushedLine("c1") + "\n" +
		resultLine(true, "pushed branch factory/t1 at c1")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if recutErr != nil {
		t.Fatalf("the re-cut's cleanup: %v", recutErr)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = %+v, want pushed c1 on the fresh token", res)
	}
	if !recut.Load() {
		t.Fatal("the re-cut never ran mid-push")
	}

	// the re-cut fired while the session pushed, and took the live token
	if !slices.Contains(before, PushTokenName("t1", "own-1")) {
		t.Fatalf("live tokens at the re-cut = %v, want the session's push token among them", before)
	}
	if len(after) != 0 {
		t.Fatalf("live tokens after the re-cut = %v, want every card-named token revoked", after)
	}

	// exactly one fresh write-scoped mint follows the revocation, and the
	// push lands on it
	wantMints := []string{
		"mint task-t1-push-own-1 write:repository factory-agent/x",
		"mint task-t1-push-own-2 write:repository factory-agent/x",
	}
	if mints := writeMints(hub); !slices.Equal(mints, wantMints) {
		t.Fatalf("write mints = %v, want the first ask's token then exactly one fresh mint after the revocation %v", mints, wantMints)
	}
	calls := hubCalls(hub)
	first, second := slices.Index(calls, wantMints[0]), slices.Index(calls, wantMints[1])
	if first < 0 || second < 0 {
		t.Fatalf("hub calls = %v, want both mints", calls)
	}
	if revokedBetween := slices.ContainsFunc(calls[first+1:second], func(call string) bool {
		return strings.HasPrefix(call, "revoke ")
	}); !revokedBetween {
		t.Fatalf("hub calls = %v, want the re-cut's revocation between the two mints", calls)
	}

	// the push landed on the fresh token, the refused one named before it
	worker.mu.Lock()
	texts := withoutWipAsks(worker.texts)
	worker.mu.Unlock()
	wantTexts := []string{
		"POST /brief ",
		"POST /push tok-task-t1-push-own-1task-t1-push-own-1",
		"POST /push tok-task-t1-push-own-2task-t1-push-own-2",
	}
	if !slices.Equal(texts, wantTexts) {
		t.Fatalf("worker got %q, want each ask's own token, named", texts)
	}

	// the failed push names the refused credential in the session's record
	if tl := string(objGet(objects, "sessions/t1/"+res.SessionID+".jsonl")); !strings.Contains(tl, "push_failed") || !strings.Contains(tl, "task-t1-push-own-1") {
		t.Fatalf("the session's record does not name the refused credential: %s", tl)
	}

	// the session's end leaves zero tokens live: the base read token and
	// the revoked push token fell at the re-cut (101, 102), the fresh mint
	// at the end (103) — the revoked-ID shape of the fresh-name test
	if live := liveTokenNames(hub); len(live) != 0 {
		t.Fatalf("live tokens after the session = %v, want none", live)
	}
	hub.mu.Lock()
	revoked := slices.Clone(hub.revoked)
	hub.mu.Unlock()
	if want := []int64{101, 102, 103}; !slices.Equal(revoked, want) {
		t.Fatalf("revoked = %v, want the read token and the session's token at the re-cut, the fresh mint at the session's end %v", revoked, want)
	}
}
