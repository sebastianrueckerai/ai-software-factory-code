package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// the freeze store serves the retry switch's reads too.
var _ RetrySwitchStore = (*freezeStore)(nil)

// ListFrozenCards lists the fake's frozen cards of the class, in id order,
// as the store's read does.
func (s *freezeStore) ListFrozenCards(ctx context.Context, repository, class string) ([]Task, error) {
	var out []Task
	for _, id := range slices.Sorted(maps.Keys(s.tasks)) {
		t := s.tasks[id]
		if t.Repository == repository && t.ModelAlias == class && slices.Contains([]string{"running", "waiting", "queued"}, t.Status) {
			out = append(out, t)
		}
	}
	return out, nil
}

// retryGateway is the gateway's hold as a test server: the retry switch's
// POST /v1/hold/clear beside the classes' armed holds, and the
// class_frozen and class_resumed rows a real gateway writes to the events
// table the freeze watcher reads.
type retryGateway struct {
	*httptest.Server
	mu      sync.Mutex
	armed   map[string]string // class -> the reset it holds
	words   string
	repo    string
	store   *freezeStore
	nextID  int64
	cleared []string // the cleared classes, in press order
}

func newRetryGateway(t *testing.T, repo string, store *freezeStore) *retryGateway {
	g := &retryGateway{armed: map[string]string{}, repo: repo, store: store}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hold/clear" || r.Method != http.MethodPost {
			http.Error(w, "not the hold-clear door", http.StatusNotFound)
			return
		}
		var req struct {
			Class string `json:"class"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil || req.Class == "" {
			http.Error(w, `the body names the class to clear as {"class": ...}`, http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		if reset, ok := g.armed[req.Class]; ok {
			delete(g.armed, req.Class)
			g.row("class_resumed", req.Class, reset)
		}
		g.cleared = append(g.cleared, req.Class)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"class":%q}`, req.Class)
	}))
	t.Cleanup(g.Close)
	return g
}

// arm arms class's hold at reset and writes the class_frozen row, as the
// gateway's first dated refusal does.
func (g *retryGateway) arm(class, reset, words string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed[class], g.words = reset, words
	g.row("class_frozen", class, reset)
}

func (g *retryGateway) row(kind, class, reset string) {
	g.nextID++
	g.store.rows = append(g.store.rows, FreezeEvent{ID: g.nextID, Repository: g.repo, At: time.Now(), Kind: kind,
		Class: class, Reset: reset, Words: g.words})
}

// TestRetrySwitchEndsFreeze is the criterion retry_switch_ends_freeze at
// the runner's entry point: RetryFreeze clears the class's armed hold
// through POST /v1/hold/clear (one class_resumed row), wakes every frozen
// card's workflow with the provider_retry signal — the woken cards' next
// sessions are the probes — and closes the freeze card at once through
// FreezeCard.CloseOnRetry. A probe that succeeds ends the freeze for all;
// a probe that meets a dated refusal naming a new reset re-arms the hold
// (a fresh class_frozen row) and the cards re-freeze at the new reset
// with no human question. Two frozen cards stand: t1 runs in the workflow
// rig, t2 beside it.
func TestRetrySwitchEndsFreeze(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	g := newRetryGateway(t, repo.String(), store)
	now := oct(14, 9, 0)
	w := freezeWatcher(store, forgejo, &now)
	const words = "rate limit exceeded until 2026-10-14 18:04:50 (Asia/Singapore)"
	// the two frozen cards of the big class
	for _, id := range []string{"t1", "t2"} {
		if err := store.CreateTask(ctx, Task{ID: id, Kind: "agent", Track: policy.TrackDirect, Status: "running",
			Repository: repo.String(), ModelAlias: "big"}); err != nil {
			t.Fatal(err)
		}
	}
	// the armed hold and its freeze card
	g.arm("big", "2026-10-14T09:30:00Z", words)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	var card Task
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard {
			card = c
		}
	}
	if card.ID == "" || card.Status != "waiting-human" {
		t.Fatalf("the freeze card = %+v, want one waiting on the armed hold", card)
	}

	// the press wires the switch: the hold clears at the gateway's door,
	// every frozen card's workflow takes the signal, the freeze card
	// closes at once
	press := func(h *workflowHarness, signalled *[]string) *RetrySwitch {
		return &RetrySwitch{Store: store, Freeze: w, GatewayURL: g.URL, Token: "gateway-token",
			Signal: func(_ context.Context, id string) error {
				*signalled = append(*signalled, id)
				if id == "t1" {
					h.env.SignalWorkflow(SignalProviderRetry, TaskSignal{Author: "sebastian"})
				}
				return nil
			}}
	}

	// a retry whose next upstream call succeeds: the freeze ends for all
	h, began, waits := providerLimitHarness(t, limitRefused(oct(14, 9, 30), words), pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	var signalled []string
	p := press(h, &signalled)
	h.env.RegisterDelayedCallback(func() {
		if err := p.RetryFreeze(ctx, "t1", "big"); err != nil {
			t.Errorf("RetryFreeze: %v", err)
		}
	}, 20*time.Minute)
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(*began, []time.Time{oct(14, 9, 0), oct(14, 9, 20)}) || len(h.sessions) != 2 {
		t.Fatalf("sessions began at %v, want the probe at the press 09:20", *began)
	}
	if !slices.Equal(signalled, []string{"t1", "t2"}) {
		t.Fatalf("the press signalled %q, want both frozen cards", signalled)
	}
	if len(*waits) != 1 || (*waits)[0].Backoff != 30*time.Minute || !(*waits)[0].Until.Equal(oct(14, 9, 30)) {
		t.Fatalf("provider waits = %+v, want the one hold to the 09:30 reset", *waits)
	}
	if len(h.escalated) != 0 {
		t.Fatalf("escalations = %q, want none", h.escalated)
	}
	if kinds := rowKinds(store); !slices.Equal(kinds, []string{"class_frozen", "class_resumed"}) {
		t.Fatalf("events = %q, want the arm then one class_resumed", kinds)
	}
	if closed, _, _ := store.Task(ctx, card.ID); closed.Status != "done" {
		t.Fatalf("the freeze card stands %s after the press, want closed at once", closed.Status)
	}
	if !slices.Contains(fj.closed, card.IssueNumber) {
		t.Errorf("issue #%d stands open after the press", card.IssueNumber)
	}
	// the success keeps the card closed
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := store.Task(ctx, card.ID); kept.Status != "done" {
		t.Fatalf("the success reopened the freeze card: %s", kept.Status)
	}

	// the provider refuses again, a new freeze: the same card re-arms
	g.arm("big", "2026-10-14T10:20:00Z", words)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if rearmed, _, _ := store.Task(ctx, card.ID); rearmed.Status != "waiting-human" || len(fj.issues) != 1 {
		t.Fatalf("the re-arm stands %s over %d issues, want the same card waiting again", rearmed.Status, len(fj.issues))
	}

	// a retry whose next call is a dated refusal naming a new reset: the
	// hold re-arms, a fresh class_frozen row, and the cards re-freeze at
	// the new reset with no human question
	h2, began2, waits2 := providerLimitHarness(t, limitRefused(oct(14, 10, 20), words), limitRefused(oct(14, 11, 5), words), pushed("s3", "c1"))
	h2.onChecks(ChecksResult{State: ChecksSuccess})
	h2.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	signalled = nil
	p = press(h2, &signalled)
	h2.env.RegisterDelayedCallback(func() {
		if err := p.RetryFreeze(ctx, "t1", "big"); err != nil {
			t.Errorf("RetryFreeze: %v", err)
		}
	}, 20*time.Minute)
	if final := h2.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(*began2, []time.Time{oct(14, 9, 0), oct(14, 9, 20), oct(14, 11, 5)}) || len(h2.sessions) != 3 {
		t.Fatalf("sessions began at %v, want the probe at the press 09:20 and the re-freeze to 11:05", *began2)
	}
	if !slices.Equal(signalled, []string{"t1", "t2"}) {
		t.Fatalf("the second press signalled %q, want both frozen cards again", signalled)
	}
	for i, want := range []time.Duration{80 * time.Minute, 105 * time.Minute} {
		w := (*waits2)[i]
		if w.Backoff != want {
			t.Errorf("wait %d = %+v, want backoff %v to the reset", i+1, w, want)
		}
	}
	if len(h2.escalated) != 0 {
		t.Fatalf("escalations = %q, want none: the re-freeze asks no one", h2.escalated)
	}
	if len(h2.sessions[2].Answers) != 0 {
		t.Fatalf("the re-frozen session carries answers %q, want none", h2.sessions[2].Answers)
	}
	// the first answer re-armed the hold at the new reset: a fresh
	// class_frozen row, and the same card takes the new freeze
	g.arm("big", "2026-10-14T11:05:00Z", words)
	now = oct(14, 9, 20)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := rowKinds(store); !slices.Equal(kinds, []string{"class_frozen", "class_resumed", "class_frozen", "class_resumed", "class_frozen"}) {
		t.Fatalf("events = %q, want the fresh class_frozen of the re-armed hold", kinds)
	}
	refrozen, _, _ := store.Task(ctx, card.ID)
	if refrozen.Status != "waiting-human" || !strings.Contains(refrozen.Objective, "in 1h 45m") || len(fj.issues) != 1 {
		t.Fatalf("the refrozen card stands %s over %d issues:\n%s", refrozen.Status, len(fj.issues), refrozen.Objective)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("a question was asked: %+v", e)
		}
	}
	if cleared := g.clearedClasses(); !slices.Equal(cleared, []string{"big", "big"}) {
		t.Fatalf("cleared = %q, want the class at each press", cleared)
	}

	// with no hold armed and no frozen cards the press is a no-op success
	before := len(store.rows)
	if err := p.RetryFreeze(ctx, "t1", "small"); err != nil {
		t.Fatalf("the empty press: %v", err)
	}
	if len(store.rows) != before {
		t.Fatalf("the empty press wrote rows: %q", rowKinds(store))
	}
}

// rowKinds reads the rows the gateway wrote, in order.
func rowKinds(store *freezeStore) []string {
	var out []string
	for _, ev := range store.rows {
		out = append(out, ev.Kind)
	}
	return out
}

func (g *retryGateway) clearedClasses() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.cleared...)
}
