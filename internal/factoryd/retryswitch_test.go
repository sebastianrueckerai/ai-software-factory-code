package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var _ RetrySwitchStore = (*freezeStore)(nil)

// ListFrozenCards lists the fake's running cards of the class in every
// repository, in id order, as the store's read does.
func (s *freezeStore) ListFrozenCards(ctx context.Context, class string) ([]Task, error) {
	var out []Task
	for _, id := range slices.Sorted(maps.Keys(s.tasks)) {
		if t := s.tasks[id]; t.Status == "running" && slices.Contains(t.RequiredCapabilities, class) {
			out = append(out, t)
		}
	}
	return out, nil
}

// retryGateway is the gateway's class holds behind its hold-clear door:
// the door takes the factoryd token on a named task alone, and an arm or a
// clear writes the class_frozen or class_resumed row the freeze watcher
// reads, as the gateway does.
type retryGateway struct {
	repo    string
	store   *freezeStore
	armed   map[string]string // class -> the reset it holds
	words   string
	nextID  int64
	cleared []string // the classes the door cleared, in press order
}

func (g *retryGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/hold/clear" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer gateway-token" || r.Header.Get(gateway.HeaderFactoryTask) == "" {
		http.Error(w, "the hold-clear door takes the factoryd shared token", http.StatusUnauthorized)
		return
	}
	var req struct {
		Class string `json:"class"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Class == "" {
		http.Error(w, "no class", http.StatusBadRequest)
		return
	}
	if reset, ok := g.armed[req.Class]; ok {
		delete(g.armed, req.Class)
		g.row("class_resumed", req.Class, reset)
	}
	g.cleared = append(g.cleared, req.Class)
	fmt.Fprintf(w, `{"class":%q}`, req.Class)
}

// arm arms class's hold at reset and writes the class_frozen row, as the
// gateway's first dated refusal does.
func (g *retryGateway) arm(class, reset string) {
	g.armed[class] = reset
	g.row("class_frozen", class, reset)
}

func (g *retryGateway) row(kind, class, reset string) {
	g.nextID++
	g.store.rows = append(g.store.rows, FreezeEvent{ID: g.nextID, Repository: g.repo, At: time.Now(), Kind: kind,
		Class: class, Reset: reset, Words: g.words})
}

// handlerTransport serves a client's requests in-process, on the caller's
// goroutine.
type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func rowKinds(store *freezeStore) []string {
	var out []string
	for _, ev := range store.rows {
		out = append(out, ev.Kind)
	}
	return out
}

// TestRetrySwitchEndsFreeze: two frozen cards of the big class (t1 runs in
// the workflow rig, t2 beside it) and an armed hold. A press the gateway
// refuses wakes nothing and closes nothing. A press clears the hold through
// POST /v1/hold/clear (one class_resumed row), closes the freeze card at
// once through CloseOnRetry before it wakes both cards with provider_retry,
// and leaves the small card and the queued big card alone; a succeeding
// probe keeps the card closed. A second press whose probe meets a dated
// refusal naming a new reset sees the hold re-arm (a fresh class_frozen
// row) and the cards re-freeze at the new reset with no human question. A
// press on a class with no hold and no frozen card is a quiet success.
func TestRetrySwitchEndsFreeze(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	const words = "rate limit exceeded until 2026-10-14 18:04:50 (Asia/Singapore)"
	g := &retryGateway{repo: repo.String(), store: store, armed: map[string]string{}, words: words}
	now := oct(14, 9, 0)
	w := freezeWatcher(store, forgejo, &now)
	for _, c := range []struct{ id, status, class string }{
		{"t1", "running", "big"}, {"t2", "running", "big"}, {"t3", "running", "small"}, {"t4", "queued", "big"},
	} {
		if err := store.CreateTask(ctx, Task{ID: c.id, Kind: "agent", Track: policy.TrackDirect, Status: c.status,
			Repository: repo.String(), RequiredCapabilities: []string{c.class}}); err != nil {
			t.Fatal(err)
		}
	}
	g.arm("big", "2026-10-14T09:30:00Z")
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
	cardStatus := func() string {
		c, _, _ := store.Task(ctx, card.ID)
		return c.Status
	}

	var signalled []string
	press := func(h *workflowHarness, token string) *RetrySwitch {
		return &RetrySwitch{Store: store, Freeze: w, GatewayURL: "http://gateway", Token: token, HTTP: &http.Client{Transport: handlerTransport{g}},
			Signal: func(_ context.Context, id string) error {
				if s := cardStatus(); s != "done" {
					t.Errorf("%s woken while the freeze card stands %s, want it closed first", id, s)
				}
				signalled = append(signalled, id)
				if id == "t1" && h != nil {
					h.env.SignalWorkflow(SignalProviderRetry, TaskSignal{Author: "sebastian"})
				}
				return nil
			}}
	}

	// a press without the factoryd token: the hold stays, nothing moves
	if err := press(nil, "sandbox-token").RetryFreeze(ctx, "t1", "big"); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("the refused press = %v, want the door's 401", err)
	}
	if len(signalled) != 0 || cardStatus() != "waiting-human" || g.armed["big"] == "" || len(store.rows) != 1 {
		t.Fatalf("after the refused press: signalled %q, card %s, armed %q, rows %q; want nothing moved", signalled, cardStatus(), g.armed, rowKinds(store))
	}

	// a press whose probe succeeds: the freeze ends for all
	h, began, waits := providerLimitHarness(t, limitRefused(oct(14, 9, 30), words), pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	p := press(h, "gateway-token")
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
		t.Fatalf("the press woke %q, want both frozen cards", signalled)
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
	if cardStatus() != "done" || !slices.Contains(fj.closed, card.IssueNumber) {
		t.Fatalf("the freeze card stands %s (closed issues %v) after the press, want closed at once", cardStatus(), fj.closed)
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if cardStatus() != "done" {
		t.Fatalf("the class_resumed reopened the freeze card: %s", cardStatus())
	}

	// the provider refuses again, a new freeze: the same card re-arms
	g.arm("big", "2026-10-14T10:20:00Z")
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if cardStatus() != "waiting-human" || len(fj.issues) != 1 {
		t.Fatalf("the re-arm stands %s over %d issues, want the same card waiting again", cardStatus(), len(fj.issues))
	}

	// a press whose probe meets a dated refusal naming a new reset
	h2, began2, waits2 := providerLimitHarness(t, limitRefused(oct(14, 10, 20), words), limitRefused(oct(14, 11, 5), words), pushed("s3", "c1"))
	h2.onChecks(ChecksResult{State: ChecksSuccess})
	h2.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	signalled = nil
	p = press(h2, "gateway-token")
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
		t.Fatalf("the second press woke %q, want both frozen cards again", signalled)
	}
	if len(*waits2) != 2 || (*waits2)[0].Backoff != 80*time.Minute || (*waits2)[1].Backoff != 105*time.Minute ||
		!(*waits2)[1].Until.Equal(oct(14, 11, 5)) {
		t.Fatalf("provider waits = %+v, want 80m to 10:20, then 105m to the new reset 11:05", *waits2)
	}
	if len(h2.escalated) != 0 || len(h2.sessions[2].Answers) != 0 {
		t.Fatalf("escalations %q, answers %q, want none: the re-freeze asks no one", h2.escalated, h2.sessions[2].Answers)
	}
	// the probe's refusal re-arms the gateway's hold at the new reset: a
	// fresh class_frozen row, and the same card takes the new freeze
	g.arm("big", "2026-10-14T11:05:00Z")
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
	if !slices.Equal(g.cleared, []string{"big", "big"}) {
		t.Fatalf("cleared = %q, want the class at each press", g.cleared)
	}

	// no hold armed and no frozen card: a quiet success
	signalled = nil
	if err := p.RetryFreeze(ctx, "t1", "premium"); err != nil {
		t.Fatalf("the empty press: %v", err)
	}
	if len(store.rows) != 5 || len(signalled) != 0 || refrozen.Status != cardStatus() {
		t.Fatalf("the empty press moved something: rows %q, woke %q, card %s", rowKinds(store), signalled, cardStatus())
	}
}
