package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// switchGateway is the real gateway (gateway.NewServer) at
// http://gateway, standing on the freeze store as it stands on the shared
// tables: its task rows are the store's cards, and its class_frozen and
// class_resumed rows land where the watcher reads them. It takes the
// factoryd token gateway-token, and a sandbox's bearer sandbox-<id> names
// card id; its holds and its key cost nothing. The provider behind it
// answers every call with answer, zai's dated refusal, or a success when
// answer is "". The class holds live in the server: restart stands a new
// one on the same store.
type switchGateway struct {
	store   *freezeStore
	version policy.Version
	answer  string
	server  http.Handler
}

func newSwitchGateway(store *freezeStore, version policy.Version) *switchGateway {
	g := &switchGateway{store: store, version: version}
	g.restart()
	return g
}

func (g *switchGateway) restart() {
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.answer == "" {
			io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, g.answer)
	})
	g.server = gateway.NewServer(gateway.ServerOptions{Auth: g, Store: g, Minter: g, Prices: g, LiteLLMURL: "http://litellm",
		HTTP: &http.Client{Transport: handlerTransport{provider}}, FactorydToken: "gateway-token"}).Routes("test")
}

func (g *switchGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.server.ServeHTTP(w, r) }

// client reaches the gateway in-process, on the caller's goroutine.
func (g *switchGateway) client() *http.Client { return &http.Client{Transport: handlerTransport{g}} }

func (g *switchGateway) Authenticate(r *http.Request) (gateway.Identity, error) {
	id, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer sandbox-")
	if !ok {
		return gateway.Identity{}, errors.New("no sandbox token")
	}
	return gateway.Identity{Subject: "system:serviceaccount:factory:task-" + id, Namespace: "factory", TaskID: id, SAUID: "sa-" + id}, nil
}

func (g *switchGateway) Task(ctx context.Context, id string) (gateway.Task, bool, error) {
	t, ok, err := g.store.Task(ctx, id)
	return gateway.Task{ID: t.ID, BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR, Status: t.Status, Classes: t.RequiredCapabilities,
		Repository: t.Repository, Source: t.Source, ModelAlias: t.ModelAlias}, ok, err
}

func (g *switchGateway) TaskCosts(context.Context, string) (gateway.TaskCosts, bool, error) {
	return gateway.TaskCosts{}, true, nil
}

func (g *switchGateway) InsertAudit(context.Context, gateway.AuditRow) error { return nil }

func (g *switchGateway) InsertEvent(_ context.Context, row gateway.EventRow) error {
	detail := func(key string) string { s, _ := row.Detail[key].(string); return s }
	g.store.rows = append(g.store.rows, FreezeEvent{ID: int64(len(g.store.rows) + 1), Repository: row.Repository, TaskID: row.TaskID,
		At: time.Now(), Kind: row.Kind, Class: detail("class"), Reset: detail("reset"), Words: detail("words")})
	return nil
}

func (g *switchGateway) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return g.version, true, nil
}

func (g *switchGateway) TakeHold(_ context.Context, r policy.HoldRequest) (policy.Hold, error) {
	return policy.Hold{ID: 1, TaskID: r.TaskID, Kind: r.Kind, Ref: r.Ref, MaxOut: 1024}, nil
}

func (g *switchGateway) ReleaseHold(context.Context, policy.Hold, *float64, policy.Usage) (policy.Booking, error) {
	return policy.Booking{}, nil
}

func (g *switchGateway) Mint(_ context.Context, _, taskID string, _ []string) (string, error) {
	return "sk-" + taskID, nil
}

func (g *switchGateway) Block(context.Context, string) error { return nil }

func (g *switchGateway) Price(context.Context, string) (policy.Price, error) {
	return policy.Price{In: 1e-6, Out: 4e-6, MaxOutput: 32768}, nil
}

// call is card id's model call through the gateway, as its session's
// sandbox makes it: the answer's status and the reset a named refusal
// holds the class to. Any other answer fails the test.
func (g *switchGateway) call(t *testing.T, id string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"go on"}],"model":"big"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sandbox-"+id)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	var refused struct{ Reset string }
	switch {
	case rec.Code == http.StatusOK:
	case rec.Code == http.StatusTooManyRequests && rec.Header().Get(gateway.HeaderDecision) == gateway.RefusedProviderLimit:
		// call runs on the workflow rig's activity goroutine too, where
		// t.Fatalf may not stop the test
		if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
			t.Errorf("%s's named refusal %s does not decode: %v", id, rec.Body.String(), err)
		}
	default:
		t.Errorf("%s's call = %d %q: %s, want served or the named refusal", id, rec.Code, rec.Header().Get(gateway.HeaderDecision), rec.Body.String())
	}
	return rec.Code, refused.Reset
}

// session is a session of card id whose one model call is its probe: a
// served call pushes, the named refusal ends the session on the provider
// limit, to the reset the gateway answered.
func (g *switchGateway) session(t *testing.T, id string) SessionResult {
	code, reset := g.call(t, id)
	if code == http.StatusOK {
		return pushed("s1", "c1")
	}
	until, _ := time.Parse(time.RFC3339, reset)
	return limitRefused(until, "Usage limit reached for 5 hour")
}

// zaiWords are zai's words on a refusal whose reset the gateway answers as
// reset: zai names the minute before it, in Singapore time.
func zaiWords(reset time.Time) string {
	sg, _ := time.LoadLocation("Asia/Singapore")
	return "Usage limit reached for 5 hour. Your limit will reset at " + reset.Add(-time.Minute).In(sg).Format(time.DateTime)
}

// zaiLimit is zai's dated refusal of a call, its words zaiWords(reset).
func zaiLimit(reset time.Time) string {
	return fmt.Sprintf(`{"error":{"message":%q,"type":"rate_limit_error","param":null,"code":"429"}}`, zaiWords(reset))
}

// switchVersion is the runner's policy version with the classes of
// freezeWatcher's: big and small name the zai provider, which reads
// Asia/Singapore, and plain names none; big's look calls vision.
func switchVersion() policy.Version {
	v := testPolicyVersion()
	doc := v.Tree.Document
	doc.Classes = maps.Clone(doc.Classes)
	doc.Classes["big"] = policy.Class{Models: []string{"big"}, VisionModel: "vision", Provider: "zai"}
	doc.Classes["small"] = policy.Class{Models: []string{"small"}, Provider: "zai"}
	doc.Classes["plain"] = policy.Class{Models: []string{"small"}}
	doc.ProviderLimits = map[string]policy.ProviderLimit{"zai": {TZ: "Asia/Singapore"}}
	v.Tree.Document = doc
	return v
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

// TestRetrySwitchEndsFreeze proves the retry switch from both of its entry
// points, on the real gateway and the real watcher: the freeze card's
// Retry now, the console route's Config.RetryFreeze, which cmd/factoryd
// wires to the switch's RetryFreeze, and the chat's retry of a frozen
// card, Act's ActRetry on t2 by the project agent, which reaches the
// switch through Runner.RetryFreeze. In each, sf/other's t5 meets zai's
// dated refusal first, to 09:45; the gateway restarts, which forgets the
// hold, and sf/acme's t2 meets it to 09:30: the big class's hold stands
// armed over t1 (in the workflow rig), t2 and t5, and each repository
// holds its open freeze card. A press the gateway refuses moves nothing.
// A press clears the hold, one class_resumed row on the pressing card,
// closes both repositories' freeze cards at once before it wakes the three
// frozen cards, and leaves alone the small card, the queued big card and
// t6, whose first class with a provider is small. t1's probe at the press
// is served, as is every woken card's next call, and the card stays
// closed on class_resumed: the freeze ended for all. A new dated refusal
// re-arms sf/acme's same card. The second press's probe meets zai's
// refusal naming a new reset, 11:05, which re-arms the gateway's hold:
// every frozen card's next call meets it, t1 waits to it, the card takes
// it, and no one is asked. The hold's named refusal reaches the tool error
// path of a session of the frozen class: its digest and its image look,
// whose calls run through the chat's model door on the card's account,
// refuse naming the decision, zai's words, the new reset and the press that
// ends the freeze sooner. The switch
// refuses a work card and a done freeze card by their state, and a press
// with no hold armed and no frozen card is a quiet success.
func TestRetrySwitchEndsFreeze(t *testing.T) {
	for _, entry := range []struct {
		name  string
		press func(sw *RetrySwitch, r *Runner, card string) error
	}{
		{"the freeze card's Retry now", func(sw *RetrySwitch, _ *Runner, card string) error {
			return sw.RetryFreeze(context.Background(), card, "sebastian")
		}},
		{"the chat's retry of a frozen card", func(_ *RetrySwitch, r *Runner, card string) error {
			got, err := r.Act(context.Background(), ActInput{TaskID: "t2", By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActRetry}})
			if err == nil && (len(got.Did) != 1 || got.Did[0].Args["freeze_card"] != card) {
				err = fmt.Errorf("the retry did %+v, want the press on %s", got.Did, card)
			}
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) { retrySwitchEndsFreeze(t, entry.press) })
	}
}

func retrySwitchEndsFreeze(t *testing.T, press func(sw *RetrySwitch, r *Runner, card string) error) {
	ctx := context.Background()
	const acme, other = "sf/acme", "sf/other"
	// tomorrow's 09:00 UTC: the gateway's holds run on the wall clock, so
	// every reset lies ahead of it
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(33 * time.Hour)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	version := switchVersion()
	work := func(id, repo, status string, classes ...string) Task {
		return Task{ID: id, Kind: "agent", Track: policy.TrackDirect, Status: status, Repository: repo,
			RequiredCapabilities: classes, ModelAlias: "big"}
	}
	frozen := func(id, repo string) Task {
		t := work(id, repo, "running", "big")
		t.Gate = GateProvider
		return t
	}
	// the lookEnv rig's session pod and its worker serve a session's
	// workspace reads
	look := newLookEnv(t)
	r, _, runnerStore := newTestRunner(t, frozen("t1", acme), look.runner.kube.(*fakeKube), &fakeObjectStore{})
	r.cfg.Policy = func() policy.Version { return version }
	r.workerPort = look.runner.workerPort
	store := &freezeStore{fakeStore: runnerStore}
	forgejo, fj := newAlertForgejo(t)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	w.Current = r.cfg.Policy
	for _, c := range []Task{frozen("t2", acme), frozen("t5", other), work("t3", acme, "running", "small"),
		work("t4", acme, "queued", "big"), work("t6", acme, "running", "small", "big")} {
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	gw := newSwitchGateway(store, version)
	gw.answer = zaiLimit(at(45 * time.Minute))
	if _, reset := gw.call(t, "t5"); reset != stamp(at(45*time.Minute)) {
		t.Fatalf("t5's call armed the hold to %q, want 09:45's %s", reset, stamp(at(45*time.Minute)))
	}
	gw.restart()
	gw.answer = zaiLimit(at(30 * time.Minute))
	if _, reset := gw.call(t, "t2"); reset != stamp(at(30*time.Minute)) {
		t.Fatalf("t2's call armed the hold to %q, want 09:30's %s", reset, stamp(at(30*time.Minute)))
	}
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
	held := func(want time.Time) {
		t.Helper()
		for _, id := range []string{"t2", "t5"} {
			if code, reset := gw.call(t, id); code != http.StatusTooManyRequests || reset != stamp(want) {
				t.Fatalf("%s's next call = %d to %q, want the hold's named refusal to %s", id, code, reset, stamp(want))
			}
		}
	}

	var signalled []string
	var current *workflowHarness
	sw := &RetrySwitch{Freeze: w, GatewayURL: "http://gateway", Token: "gateway-token", HTTP: gw.client(),
		Signal: func(_ context.Context, id string) error {
			if a, o := cardStatus(acme), cardStatus(other); a == "waiting-human" || o == "waiting-human" {
				t.Errorf("%s woken while the freeze cards stand %s and %s, want them closed first", id, a, o)
			}
			signalled = append(signalled, id)
			if id == "t1" && current != nil {
				current.env.SignalWorkflow(SignalProviderRetry, TaskSignal{Author: "sebastian"})
			}
			return nil
		}}
	r.RetryFreeze = sw.RetryFreeze

	// a press on a card that holds no open freeze names its state and the
	// action that fits, and moves nothing
	var no ErrNoFreeze
	if err := sw.RetryFreeze(ctx, "t1", "sebastian"); !errors.As(err, &no) ||
		err.Error() != "t1 is no freeze card: the retry switch ends a provider freeze from the class's open freeze card, and Retry files a failed card again" {
		t.Fatalf("the press on a work card = %v, want the no-freeze refusal", err)
	}
	// a press without the factoryd token: the hold stays, nothing moves
	sandbox := *sw
	sandbox.Token = "sandbox-t1"
	if err := sandbox.RetryFreeze(ctx, cards[acme].ID, "sebastian"); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("the refused press = %v, want the door's 401", err)
	}
	if len(signalled) != 0 || cardStatus(acme) != "waiting-human" || cardStatus(other) != "waiting-human" || len(store.rows) != 2 {
		t.Fatalf("after the refused press: signalled %q, cards %s and %s, rows %q; want nothing moved",
			signalled, cardStatus(acme), cardStatus(other), rowKinds(store))
	}
	held(at(30 * time.Minute))

	// a press whose probe is served: the freeze ends for all
	gw.answer = ""
	probe := func() SessionResult { return gw.session(t, "t1") }
	h, began, waits := providerLimitHarness(t, probe)
	h.env.SetStartTime(base)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	current = h
	h.env.RegisterDelayedCallback(func() {
		if err := press(sw, r, cards[acme].ID); err != nil {
			t.Errorf("the press: %v", err)
		}
	}, 20*time.Minute)
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(*began, []time.Time{base, at(20 * time.Minute)}) || len(h.sessions) != 2 {
		t.Fatalf("sessions began at %v, want the probe at the press 09:20", *began)
	}
	if !slices.Equal(signalled, []string{"t1", "t2", "t5"}) {
		t.Fatalf("the press woke %q, want the three frozen cards of both repositories", signalled)
	}
	if len(*waits) != 1 || (*waits)[0].Backoff != 30*time.Minute || !(*waits)[0].Until.Equal(at(30*time.Minute)) {
		t.Fatalf("provider waits = %+v, want the one hold to the 09:30 reset", *waits)
	}
	if len(h.escalated) != 0 {
		t.Fatalf("escalations = %q, want none", h.escalated)
	}
	if kinds := rowKinds(store); !slices.Equal(kinds, []string{"class_frozen", "class_frozen", "class_resumed"}) ||
		store.rows[2].Repository != acme || store.rows[2].TaskID != cards[acme].ID {
		t.Fatalf("events = %+v, want the two arms then one class_resumed on the pressing card of sf/acme", store.rows)
	}
	for _, repo := range []string{acme, other} {
		if cardStatus(repo) != "done" || !slices.Contains(fj.closed, cards[repo].IssueNumber) {
			t.Fatalf("%s's freeze card stands %s (closed issues %v) after the press, want closed at once", repo, cardStatus(repo), fj.closed)
		}
	}
	for _, id := range []string{"t2", "t5"} {
		if code, _ := gw.call(t, id); code != http.StatusOK {
			t.Fatalf("%s's next call after the press = %d, want served", id, code)
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
	gw.answer = zaiLimit(at(80 * time.Minute))
	held(at(80 * time.Minute))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if cardStatus(acme) != "waiting-human" || cardStatus(other) != "done" || len(fj.issues) != 2 {
		t.Fatalf("the re-arm stands %s and %s over %d issues, want sf/acme's same card waiting again", cardStatus(acme), cardStatus(other), len(fj.issues))
	}

	// a press whose probe meets zai's refusal naming a new reset; the
	// gateway's hold lapses at that reset on the wall clock, which the test
	// does not wait for, so the session after it pushes as scripted
	gw.answer = zaiLimit(at(125 * time.Minute))
	h2, began2, waits2 := providerLimitHarness(t, probe, probe, func() SessionResult { return pushed("s3", "c1") })
	h2.env.SetStartTime(base)
	h2.onChecks(ChecksResult{State: ChecksSuccess})
	h2.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	signalled, current = nil, h2
	h2.env.RegisterDelayedCallback(func() {
		if err := press(sw, r, cards[acme].ID); err != nil {
			t.Errorf("the press: %v", err)
		}
	}, 20*time.Minute)
	if final := h2.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(*began2, []time.Time{base, at(20 * time.Minute), at(125 * time.Minute)}) || len(h2.sessions) != 3 {
		t.Fatalf("sessions began at %v, want the probe at the press 09:20 and the re-freeze to 11:05", *began2)
	}
	if !slices.Equal(signalled, []string{"t1", "t2", "t5"}) {
		t.Fatalf("the second press woke %q, want the three frozen cards again", signalled)
	}
	if len(*waits2) != 2 || (*waits2)[0].Backoff != 80*time.Minute || (*waits2)[1].Backoff != 105*time.Minute ||
		!(*waits2)[1].Until.Equal(at(125*time.Minute)) {
		t.Fatalf("provider waits = %+v, want 80m to 10:20, then 105m to the new reset 11:05", *waits2)
	}
	if len(h2.escalated) != 0 || len(h2.sessions[2].Answers) != 0 {
		t.Fatalf("escalations %q, answers %q, want none: the re-freeze asks no one", h2.escalated, h2.sessions[2].Answers)
	}
	// the probe's refusal re-armed the gateway's hold at the new reset: a
	// fresh class_frozen row, every frozen card's next call meets it, and
	// the same card takes the new freeze
	held(at(125 * time.Minute))
	now = at(20 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := rowKinds(store); !slices.Equal(kinds, []string{"class_frozen", "class_frozen", "class_resumed", "class_frozen", "class_resumed", "class_frozen"}) ||
		store.rows[4].TaskID != cards[acme].ID || store.rows[5].Reset != stamp(at(125*time.Minute)) {
		t.Fatalf("events = %+v, want the press's class_resumed, then the fresh class_frozen of the re-armed hold", store.rows)
	}
	refrozen, _, _ := store.Task(ctx, cards[acme].ID)
	if refrozen.Status != "waiting-human" || !strings.Contains(refrozen.Objective, "in 1h 45m") || len(fj.issues) != 2 {
		t.Fatalf("the refrozen card stands %s over %d issues:\n%s", refrozen.Status, len(fj.issues), refrozen.Objective)
	}
	freezeAsks(t, store)
	// a session of the frozen class meets the hold in its tools whose calls
	// run through the chat's model door on the card's account, the digest
	// and the image look: each refusal names the decision, zai's words, the
	// new reset the class is frozen until and the press that ends it sooner
	if err := store.PutLiveSession(ctx, LiveSession{TaskID: "t2", SessionID: "s9", Namespace: "factory-agents-x", Pod: "p1", MessageToken: "msg-tok"}); err != nil {
		t.Fatal(err)
	}
	op := &OperatorModel{GatewayURL: "http://gateway", Token: "gateway-token", HTTP: gw.client()}
	frozenUntil := " was refused (refused_provider_limit): " + zaiWords(at(125*time.Minute)) + "; the class is frozen until " + stamp(at(125*time.Minute)) +
		", when the work resumes by itself, or sooner if an approver presses Retry now on the class's freeze card or retries a frozen case in the chat"
	for _, tool := range []struct {
		serve      http.HandlerFunc
		body, want string
	}{
		{r.DigestHandler(op), `{"task_id":"t2","path":"docs/plan.md"}`, "the digest of docs/plan.md" + frozenUntil},
		{r.ImageLookHandler(op), `{"task_id":"t2","path":"shots/home.png"}`, "the look at shots/home.png" + frozenUntil},
	} {
		rec := httptest.NewRecorder()
		tool.serve(rec, httptest.NewRequest(http.MethodPost, "/internal/tool", strings.NewReader(tool.body)))
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusConflict || got != tool.want {
			t.Fatalf("%s inside the freeze = %d %q, want %q", tool.body, rec.Code, got, tool.want)
		}
	}

	// the done card of sf/other: its freeze ended, which the press names
	if err := sw.RetryFreeze(ctx, cards[other].ID, "sebastian"); !errors.As(err, &no) ||
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
	if err := sw.RetryFreeze(ctx, premium.ID, "sebastian"); err != nil {
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
		Repository: "sf/acme", RequiredCapabilities: []string{"big"}, ModelAlias: "big"}); err != nil {
		t.Fatal(err)
	}
	gw := newSwitchGateway(store, switchVersion())
	reset := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Minute)
	gw.answer = zaiLimit(reset)
	if _, armed := gw.call(t, "t1"); armed != stamp(reset) {
		t.Fatalf("t1's call armed the hold to %q, want %s", armed, stamp(reset))
	}
	if err := w.Check(ctx); !errors.Is(err, errNoPolicyReader) || len(fj.issues) != 0 {
		t.Fatalf("Check = %v over %d issues, want the missing policy reader named and no card", err, len(fj.issues))
	}
	card := Task{ID: "fc", Kind: "human", Status: "waiting-human", Source: SourceFreezeCard, Repository: "sf/acme",
		Objective: freezeCardTitle("big") + "\n\n" + freezeClassMarker("big")}
	if err := store.CreateTask(ctx, card); err != nil {
		t.Fatal(err)
	}
	var signalled []string
	p := &RetrySwitch{Freeze: w, GatewayURL: "http://gateway", Token: "gateway-token", HTTP: gw.client(),
		Signal: func(_ context.Context, id string) error { signalled = append(signalled, id); return nil }}
	if err := p.RetryFreeze(ctx, card.ID, "sebastian"); !errors.Is(err, errNoPolicyReader) {
		t.Fatalf("RetryFreeze = %v, want the missing policy reader named", err)
	}
	if code, held := gw.call(t, "t1"); held != stamp(reset) || len(signalled) != 0 || len(store.rows) != 1 {
		t.Fatalf("the refused press moved something: t1's next call %d to %q, woke %q, rows %q", code, held, signalled, rowKinds(store))
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
		!strings.HasPrefix(err.Error(), "rung 0 ends no provider freeze, which a human's press or the factory agent's ends: `hand_up` with `retry` as the recommendation") {
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
	// a press that cleared the hold and failed part after it reaches the
	// answer's caller as it is, for the caller's own sentence
	r.RetryFreeze = func(context.Context, string, string) error {
		return &PartialPressError{Err: errors.New("wake t-z: temporal unavailable")}
	}
	var part *PartialPressError
	if _, err := r.Act(ctx, ActInput{TaskID: "t-z", By: "sebastian", Rung: 3, Res: Resolution{Action: ActRetry}}); !errors.As(err, &part) {
		t.Fatalf("a press that failed past the clear = %v, want its PartialPressError", err)
	}
}

// The freeze card asks as a human card, and Retry now is its own answer: a
// human's press on its case runs the retry switch on the card and ends the
// case, rung 1's is refused as any answer of a human card is, and
// the switch's refusal comes back as Act's with the case still open.
func TestTheFreezeCardsRetryNowRunsTheRetrySwitch(t *testing.T) {
	ctx := context.Background()
	g := newFactsRig(t)
	card := Task{ID: "t-fz", Kind: "human", Status: "waiting-human", Source: SourceFreezeCard, Repository: "demo-app/demo-app", IssueNumber: 8,
		Objective: freezeCardTitle("coder") + "\n\n" + freezeClassMarker("coder")}
	g.store.tasks[card.ID] = card
	g.store.cases = []Case{{ID: 9, TaskID: card.ID, Repository: card.Repository, Kind: CaseHumanCard, Status: CaseAtHuman}}
	asked := freezeAsk(FreezeQuestion(FreezeEvent{Class: "coder", Reset: "2026-11-21T17:00:00Z"}, 2, time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))).Event(card)
	asked.Detail["case_id"] = 9
	g.store.events = append(g.store.events, asked)
	var pressed []string
	var refusal error
	g.r.RetryFreeze = func(_ context.Context, cardID, by string) error {
		pressed = append(pressed, cardID+" by "+by)
		return refusal
	}
	retry := ActInput{TaskID: card.ID, CaseID: 9, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActRetry, Note: "Retry now"}}
	var refused ErrActRefused
	checkin := retry
	checkin.Rung = 1
	if _, err := g.r.Act(ctx, checkin); !errors.As(err, &refused) || err.Error() != "the check-in may not answer a human card: rung 2 and the human may" || len(pressed) != 0 {
		t.Fatalf("rung 1's retry = %v, pressed %q; want the human card's refusal and no press", err, pressed)
	}
	refusal = ErrNoFreeze("the freeze of #8 ended, the card is done: no hold waits on a retry, and the class's next freeze opens its card again")
	if _, err := g.r.Act(ctx, retry); !errors.As(err, &refused) || err.Error() != refusal.Error() || g.store.cases[0].Status != CaseAtHuman {
		t.Fatalf("a press the switch refuses = %v, case %s; want its refusal and the case open", err, g.store.cases[0].Status)
	}
	refusal = nil
	got, err := g.r.Act(ctx, retry)
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"freeze_card": "t-fz"},
		Result: "Ended the provider freeze: its hold cleared, its freeze card closed, and its frozen cards call the provider again."}}
	if err != nil || !reflect.DeepEqual(got.Did, want) || g.store.cases[0].Status != CaseResolved {
		t.Fatalf("the human's Retry now = %+v, %v, case %s; want the press and the case ended", got.Did, err, g.store.cases[0].Status)
	}
	if !slices.Equal(pressed, []string{"t-fz by sebastian", "t-fz by sebastian"}) {
		t.Fatalf("pressed %q, want the refused press and the taken one, both on the freeze card by the human", pressed)
	}
}
