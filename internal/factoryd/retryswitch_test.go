package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// retryGateway is the gateway's class holds behind its hold-clear door:
// the door takes the factoryd token on a named task alone, an arm writes
// the class_frozen row on the arming card's repository and a clear the
// class_resumed row on the pressing card's, as the gateway does.
type retryGateway struct {
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
	task, ok := g.store.tasks[r.Header.Get(gateway.HeaderFactoryTask)]
	if r.Header.Get("Authorization") != "Bearer gateway-token" || !ok {
		http.Error(w, "the hold-clear door takes the factoryd shared token on a known task", http.StatusUnauthorized)
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
		g.row(task.Repository, "class_resumed", req.Class, reset)
	}
	g.cleared = append(g.cleared, req.Class)
	fmt.Fprintf(w, `{"class":%q}`, req.Class)
}

// arm arms class's hold at reset and writes the class_frozen row on repo,
// as a dated refusal of one of repo's cards does.
func (g *retryGateway) arm(repo, class, reset string) {
	g.armed[class] = reset
	g.row(repo, "class_frozen", class, reset)
}

func (g *retryGateway) row(repo, kind, class, reset string) {
	g.nextID++
	g.store.rows = append(g.store.rows, FreezeEvent{ID: g.nextID, Repository: repo, At: time.Now(), Kind: kind,
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

// TestRetrySwitchEndsFreeze: an armed hold on the big class freezes two
// cards of sf/acme (t1 runs in the workflow rig, t2 beside it) and t5 of
// sf/other, and each repository holds its open freeze card. A press the
// gateway refuses wakes nothing and closes nothing. A press clears the hold
// through POST /v1/hold/clear (one class_resumed row), closes both
// repositories' freeze cards at once through CloseOnRetry before it wakes
// the frozen cards with provider_retry, and leaves alone the small card,
// the queued big card and t6, whose first class with a provider is small,
// as the gateway reads it; a succeeding probe keeps the cards closed. A
// second press whose probe meets a dated refusal naming a new reset sees
// the hold re-arm (a fresh class_frozen row) and the cards re-freeze at
// the new reset with no human question. A press on a class with no hold
// and no frozen card is a quiet success.
func TestRetrySwitchEndsFreeze(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	const acme, other = "sf/acme", "sf/other"
	const words = "rate limit exceeded until 2026-10-14 18:04:50 (Asia/Singapore)"
	g := &retryGateway{store: store, armed: map[string]string{}, words: words}
	now := oct(14, 9, 0)
	w := freezeWatcher(store, forgejo, &now)
	for _, c := range []struct {
		id, repo, status string
		classes          []string
	}{
		{"t1", acme, "running", []string{"big"}}, {"t2", acme, "running", []string{"big"}}, {"t5", other, "running", []string{"big"}},
		{"t3", acme, "running", []string{"small"}}, {"t4", acme, "queued", []string{"big"}}, {"t6", acme, "running", []string{"small", "big"}},
	} {
		if err := store.CreateTask(ctx, Task{ID: c.id, Kind: "agent", Track: policy.TrackDirect, Status: c.status,
			Repository: c.repo, RequiredCapabilities: c.classes}); err != nil {
			t.Fatal(err)
		}
	}
	g.arm(acme, "big", "2026-10-14T09:30:00Z")
	g.arm(other, "big", "2026-10-14T09:45:00Z")
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cards := map[string]Task{}
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard {
			cards[c.Repository] = c
		}
	}
	if len(cards) != 2 || cards[acme].Status != "waiting-human" || cards[other].Status != "waiting-human" {
		t.Fatalf("the freeze cards = %+v, want one waiting in each repository", cards)
	}
	// sf/acme's face counts t1, t2 and the queued t4, not t6, which the
	// hold of small holds; sf/other's counts t5
	if a, o := cards[acme].Objective, cards[other].Objective; !strings.Contains(a, "3 cards of the class wait") || !strings.Contains(o, "1 cards of the class wait") {
		t.Fatalf("the faces count wrong:\n%s\n\n%s", a, o)
	}
	cardStatus := func(repo string) string {
		c, _, _ := store.Task(ctx, cards[repo].ID)
		return c.Status
	}

	var signalled []string
	press := func(h *workflowHarness, token string) *RetrySwitch {
		return &RetrySwitch{Freeze: w, GatewayURL: "http://gateway", Token: token, HTTP: &http.Client{Transport: handlerTransport{g}},
			Signal: func(_ context.Context, id string) error {
				if a, o := cardStatus(acme), cardStatus(other); a == "waiting-human" || o == "waiting-human" {
					t.Errorf("%s woken while the freeze cards stand %s and %s, want them closed first", id, a, o)
				}
				signalled = append(signalled, id)
				if id == "t1" && h != nil {
					h.env.SignalWorkflow(SignalProviderRetry, TaskSignal{Author: "sebastian"})
				}
				return nil
			}}
	}

	// a press on a card that holds no open freeze names its state and the
	// action that fits, and moves nothing
	var no ErrNoFreeze
	if err := press(nil, "gateway-token").RetryFreeze(ctx, "t1", "sebastian"); !errors.As(err, &no) ||
		err.Error() != "t1 is no freeze card: the retry switch ends a provider freeze from the class's open freeze card, and Retry files a failed card again" {
		t.Fatalf("the press on a work card = %v, want the no-freeze refusal", err)
	}
	// a press without the factoryd token: the hold stays, nothing moves
	if err := press(nil, "sandbox-token").RetryFreeze(ctx, cards[acme].ID, "sebastian"); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("the refused press = %v, want the door's 401", err)
	}
	if len(signalled) != 0 || cardStatus(acme) != "waiting-human" || cardStatus(other) != "waiting-human" || g.armed["big"] == "" || len(store.rows) != 2 {
		t.Fatalf("after the refused press: signalled %q, cards %s and %s, armed %q, rows %q; want nothing moved",
			signalled, cardStatus(acme), cardStatus(other), g.armed, rowKinds(store))
	}

	// a press whose probe succeeds: the freeze ends for all
	h, began, waits := providerLimitHarness(t, limitRefused(oct(14, 9, 30), words), pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	p := press(h, "gateway-token")
	h.env.RegisterDelayedCallback(func() {
		if err := p.RetryFreeze(ctx, cards[acme].ID, "sebastian"); err != nil {
			t.Errorf("RetryFreeze: %v", err)
		}
	}, 20*time.Minute)
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(*began, []time.Time{oct(14, 9, 0), oct(14, 9, 20)}) || len(h.sessions) != 2 {
		t.Fatalf("sessions began at %v, want the probe at the press 09:20", *began)
	}
	if !slices.Equal(signalled, []string{"t1", "t2", "t5"}) {
		t.Fatalf("the press woke %q, want the three frozen cards of both repositories", signalled)
	}
	if len(*waits) != 1 || (*waits)[0].Backoff != 30*time.Minute || !(*waits)[0].Until.Equal(oct(14, 9, 30)) {
		t.Fatalf("provider waits = %+v, want the one hold to the 09:30 reset", *waits)
	}
	if len(h.escalated) != 0 {
		t.Fatalf("escalations = %q, want none", h.escalated)
	}
	if kinds := rowKinds(store); !slices.Equal(kinds, []string{"class_frozen", "class_frozen", "class_resumed"}) || store.rows[2].Repository != acme {
		t.Fatalf("events = %+v, want the two arms then one class_resumed on the pressing card's sf/acme", store.rows)
	}
	for _, repo := range []string{acme, other} {
		if cardStatus(repo) != "done" || !slices.Contains(fj.closed, cards[repo].IssueNumber) {
			t.Fatalf("%s's freeze card stands %s (closed issues %v) after the press, want closed at once", repo, cardStatus(repo), fj.closed)
		}
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if cardStatus(acme) != "done" || cardStatus(other) != "done" {
		t.Fatalf("the class_resumed reopened a freeze card: %s, %s", cardStatus(acme), cardStatus(other))
	}

	// the provider refuses one of sf/acme's cards again, a new freeze: the
	// same card re-arms
	g.arm(acme, "big", "2026-10-14T10:20:00Z")
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if cardStatus(acme) != "waiting-human" || cardStatus(other) != "done" || len(fj.issues) != 2 {
		t.Fatalf("the re-arm stands %s and %s over %d issues, want sf/acme's same card waiting again", cardStatus(acme), cardStatus(other), len(fj.issues))
	}

	// a press whose probe meets a dated refusal naming a new reset
	h2, began2, waits2 := providerLimitHarness(t, limitRefused(oct(14, 10, 20), words), limitRefused(oct(14, 11, 5), words), pushed("s3", "c1"))
	h2.onChecks(ChecksResult{State: ChecksSuccess})
	h2.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	signalled = nil
	p = press(h2, "gateway-token")
	h2.env.RegisterDelayedCallback(func() {
		if err := p.RetryFreeze(ctx, cards[acme].ID, "sebastian"); err != nil {
			t.Errorf("RetryFreeze: %v", err)
		}
	}, 20*time.Minute)
	if final := h2.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(*began2, []time.Time{oct(14, 9, 0), oct(14, 9, 20), oct(14, 11, 5)}) || len(h2.sessions) != 3 {
		t.Fatalf("sessions began at %v, want the probe at the press 09:20 and the re-freeze to 11:05", *began2)
	}
	if !slices.Equal(signalled, []string{"t1", "t2", "t5"}) {
		t.Fatalf("the second press woke %q, want the three frozen cards again", signalled)
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
	g.arm(acme, "big", "2026-10-14T11:05:00Z")
	now = oct(14, 9, 20)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := rowKinds(store); !slices.Equal(kinds, []string{"class_frozen", "class_frozen", "class_resumed", "class_frozen", "class_resumed", "class_frozen"}) {
		t.Fatalf("events = %q, want the fresh class_frozen of the re-armed hold", kinds)
	}
	refrozen, _, _ := store.Task(ctx, cards[acme].ID)
	if refrozen.Status != "waiting-human" || !strings.Contains(refrozen.Objective, "in 1h 45m") || len(fj.issues) != 2 {
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

	// the done card of sf/other: its freeze ended, which the press names
	if err := p.RetryFreeze(ctx, cards[other].ID, "sebastian"); !errors.As(err, &no) ||
		err.Error() != fmt.Sprintf("the freeze of #%d ended, the card is done: no hold waits on a retry, and the class's next freeze opens its card again", cards[other].IssueNumber) {
		t.Fatalf("the press on a done freeze card = %v, want the ended-freeze refusal", err)
	}

	// the premium class's open card with no hold armed and no frozen card:
	// a quiet success that closes the card alone
	premium := Task{ID: "fc-premium", Kind: "human", Status: "waiting-human", Source: SourceFreezeCard, Repository: acme, IssueNumber: 90,
		Objective: freezeCardTitle("premium") + "\n\n" + freezeClassMarker("premium")}
	if err := store.CreateTask(ctx, premium); err != nil {
		t.Fatal(err)
	}
	signalled = nil
	if err := p.RetryFreeze(ctx, premium.ID, "sebastian"); err != nil {
		t.Fatalf("the empty press: %v", err)
	}
	if got, _, _ := store.Task(ctx, premium.ID); len(store.rows) != 6 || len(signalled) != 0 || cardStatus(acme) != "waiting-human" || got.Status != "done" {
		t.Fatalf("the empty press: rows %q, woke %q, sf/acme's card %s, premium's %s; want only premium's card closed", rowKinds(store), signalled, cardStatus(acme), got.Status)
	}
	if !slices.ContainsFunc(fj.comments, func(c string) bool {
		return strings.Contains(c, "sebastian's retry press cleared the premium class's hold")
	}) {
		t.Fatalf("comments %q, want the close naming who pressed", fj.comments)
	}
}

// A retry press signalled before a session starts is that session's probe:
// the start drops it (retry-press-drop), so the card's next dated wait runs
// to its reset. Each session takes two minutes. s1 meets a plain refusal
// (the two-minute ladder step, which takes no press), s2 a dated one to
// 10:30, s3 pushes. A press mid-s1 is dropped at s2's start: s3 starts at
// the reset. A press mid-s2 came after its start: it ends the wait at once.
// A run from before the change keeps the mid-s1 press for the wait too.
func TestRetryPressBeforeASessionEndsNoLaterWait(t *testing.T) {
	const words = "rate limit exceeded until 2026-10-14 18:30:00 (Asia/Singapore)"
	for name, tc := range map[string]struct {
		press  time.Duration
		before bool
		ended  []time.Time
	}{
		"a press mid-s1":                    {time.Minute, false, []time.Time{oct(14, 9, 2), oct(14, 9, 6), oct(14, 10, 32)}},
		"a press mid-s2":                    {5 * time.Minute, false, []time.Time{oct(14, 9, 2), oct(14, 9, 6), oct(14, 9, 8)}},
		"a press mid-s1, a run from before": {time.Minute, true, []time.Time{oct(14, 9, 2), oct(14, 9, 6), oct(14, 9, 8)}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			if tc.before {
				h.env.OnGetVersion("retry-press-drop", workflow.DefaultVersion, versionRetryPressDrop).Return(workflow.DefaultVersion)
			}
			results := []SessionResult{refusedSession, limitRefused(oct(14, 10, 30), words), pushed("s3", "c1")}
			var ended []time.Time
			h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(2 * time.Minute).Return(
				func(ctx context.Context, in SessionInput) (SessionResult, error) {
					h.sessions, ended = append(h.sessions, in), append(ended, h.env.Now())
					return results[min(len(h.sessions), len(results))-1], nil
				})
			var waits []ProviderWait
			h.env.OnActivity(ActivityProviderWait, mock.Anything, "t1", mock.Anything).Return(
				func(ctx context.Context, taskID string, w ProviderWait) error {
					waits = append(waits, w)
					return nil
				})
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(tc.press, SignalProviderRetry, TaskSignal{Author: "sebastian"})
			h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, want done", final)
			}
			if !sameTimes(ended, tc.ended) {
				t.Fatalf("sessions ended at %v, want %v", ended, tc.ended)
			}
			if len(waits) != 2 || waits[0].Backoff != 2*time.Minute || !waits[1].Until.Equal(oct(14, 10, 30)) || waits[1].Backoff != 84*time.Minute {
				t.Fatalf("provider waits = %+v, want the 2m ladder step, then 84m to the 10:30 reset", waits)
			}
		})
	}
}

// A freeze watcher wired without its policy reader cannot tell which cards
// a hold freezes: its check refuses before it files a card, and a press
// through it refuses before it clears the hold, closes or wakes anything.
func TestFreezeCardRefusesAWatcherWithoutItsPolicyReader(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	now := oct(14, 9, 0)
	w := freezeWatcher(store, forgejo, &now)
	w.Current = nil
	if err := store.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Track: policy.TrackDirect, Status: "running",
		Repository: "sf/acme", RequiredCapabilities: []string{"big"}}); err != nil {
		t.Fatal(err)
	}
	g := &retryGateway{store: store, armed: map[string]string{}}
	g.arm("sf/acme", "big", "2026-10-14T09:30:00Z")
	if err := w.Check(ctx); !errors.Is(err, errNoPolicyReader) || len(fj.issues) != 0 {
		t.Fatalf("Check = %v over %d issues, want the missing policy reader named and no card", err, len(fj.issues))
	}
	card := Task{ID: "fc", Kind: "human", Status: "waiting-human", Source: SourceFreezeCard, Repository: "sf/acme",
		Objective: freezeCardTitle("big") + "\n\n" + freezeClassMarker("big")}
	if err := store.CreateTask(ctx, card); err != nil {
		t.Fatal(err)
	}
	var signalled []string
	p := &RetrySwitch{Freeze: w, GatewayURL: "http://gateway", Token: "gateway-token", HTTP: &http.Client{Transport: handlerTransport{g}},
		Signal: func(_ context.Context, id string) error { signalled = append(signalled, id); return nil }}
	if err := p.RetryFreeze(ctx, card.ID, "sebastian"); !errors.Is(err, errNoPolicyReader) {
		t.Fatalf("RetryFreeze = %v, want the missing policy reader named", err)
	}
	if g.armed["big"] == "" || len(g.cleared) != 0 || len(signalled) != 0 || len(store.rows) != 1 {
		t.Fatalf("the refused press moved something: armed %q, cleared %q, woke %q, rows %q", g.armed, g.cleared, signalled, rowKinds(store))
	}
}

// The retry rig's coder class names the zai provider, and its open freeze
// card t-fz (#8) holds t-z (#7), which waits on the provider. A retry with
// no task_id on t-z, a human's on rung 3, and one on t-fz itself, the
// project agent's on rung 2, each press the retry switch on t-fz and
// refuse nothing; the factory's own rung 0 presses nothing. A thawed card
// takes the per-card path: t-b's retry files its failed dependency again,
// while t-y, waiting on the provider under a class that names none, and
// t-x, a coder card in its session, are refused with their state.
func TestActRetryOnAFrozenCaseRunsTheRetrySwitch(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := newRetryRig(t)
	v := r.cfg.Policy()
	doc := v.Document
	doc.Classes = maps.Clone(doc.Classes)
	coder := doc.Classes["coder"]
	coder.Provider = "zai"
	doc.Classes["coder"], doc.Classes["plain"] = coder, policy.Class{}
	doc.ProviderLimits = map[string]policy.ProviderLimit{"zai": {TZ: "Asia/Singapore"}}
	v.Tree.Document = doc
	r.cfg.Policy = func() policy.Version { return v }
	for _, c := range []Task{
		{ID: "t-z", Kind: "agent", Status: "running", Gate: GateProvider, RequiredCapabilities: []string{"coder"}, IssueNumber: 7},
		{ID: "t-y", Kind: "agent", Status: "running", Gate: GateProvider, RequiredCapabilities: []string{"plain"}},
		{ID: "t-x", Kind: "agent", Status: "running", Gate: GateSession, RequiredCapabilities: []string{"coder"}},
		{ID: "t-fz", Kind: "human", Status: "waiting-human", Source: SourceFreezeCard, IssueNumber: 8,
			Objective: freezeCardTitle("coder") + "\n\n" + freezeClassMarker("coder")},
	} {
		c.Track, c.Repository = policy.TrackDirect, "linkshort/linkshort"
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	var pressed []string
	r.RetryFreeze = func(_ context.Context, cardID, by string) error {
		pressed = append(pressed, cardID+" by "+by)
		return nil
	}
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"freeze_card": "t-fz"},
		Result: "Ended the provider freeze: its hold cleared, its freeze card closed, and its frozen cards call the provider again."}}
	for _, in := range []ActInput{
		{TaskID: "t-z", By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActRetry}},
		{TaskID: "t-fz", By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActRetry}},
	} {
		got, err := r.Act(ctx, in)
		if err != nil || !reflect.DeepEqual(got.Did, want) {
			t.Fatalf("%s's retry on %s = %+v, %v; want the press on t-fz", in.By, in.TaskID, got.Did, err)
		}
	}
	if !slices.Equal(pressed, []string{"t-fz by sebastian", "t-fz by " + ChatUser}) {
		t.Fatalf("pressed %q, want t-fz by the human, then by the project agent", pressed)
	}
	var refused ErrActRefused
	if _, err := r.Act(ctx, ActInput{TaskID: "t-z", By: FactoryDecider, Rung: 0, Res: Resolution{Action: ActRetry}}); !errors.As(err, &refused) ||
		!strings.HasPrefix(err.Error(), "rung 0 ends no provider freeze, a human's press or the project agent's: hand it up (hand_up) with retry as the recommendation") {
		t.Fatalf("rung 0's retry = %v, want the refusal naming who presses", err)
	}
	for _, id := range []string{"t-y", "t-x"} {
		if _, err := r.Act(ctx, ActInput{TaskID: id, By: "sebastian", Rung: 3, Res: Resolution{Action: ActRetry}}); !errors.As(err, &refused) ||
			!strings.Contains(err.Error(), "no failed dependency is open to a retry, and no provider freeze holds the card") {
			t.Fatalf("the retry on %s = %v, want the refusal naming the state", id, err)
		}
	}
	got, err := r.Act(ctx, ActInput{TaskID: "t-b", By: "sebastian", Rung: 3, Res: Resolution{Action: ActRetry}})
	if err != nil || len(got.Did) != 1 || got.Did[0].Args["task_id"] != "t-a" || got.Did[0].Args["replacement"] != int64(9) {
		t.Fatalf("t-b's retry = %+v, %v; want #9 filed in place of t-a", got.Did, err)
	}
	if len(pressed) != 2 {
		t.Fatalf("pressed %q, want no press past the two", pressed)
	}
}
