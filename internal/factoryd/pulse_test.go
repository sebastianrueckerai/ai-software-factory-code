package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// fakePulseStore is the pulse's store in memory; Record publishes on bus
// as the PGStore does; spent makes the chat task's budget spent.
type fakePulseStore struct {
	mu     sync.Mutex
	bus    *EventBus
	events []Event
	pulses []Pulse
	spent  bool
}

func (s *fakePulseStore) Projects(context.Context) ([]Project, error) {
	return []Project{{Name: "linkshort", Repository: "linkshort/linkshort", File: true, Objects: policy.ObjectsSeed, Agents: policy.AgentsCoder},
		{Name: "infra", Repository: "sf/infra", File: true, Objects: policy.ObjectsSeed, Agents: policy.AgentsNone}}, nil
}

func (s *fakePulseStore) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{}, true, nil
}

func (s *fakePulseStore) EnsureChatTask(_ context.Context, name, repository string, budget float64, at time.Time) (Task, error) {
	t := Task{ID: ChatTaskID(name, at), Kind: KindChat, Status: "running", BudgetEUR: budget, Repository: repository}
	if s.spent {
		t.SpentEUR = budget
	}
	return t, nil
}

func (s *fakePulseStore) Record(ctx context.Context, e Event) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	e.ID, e.At = int64(len(s.events)+1), time.Now()
	s.events = append(s.events, e)
	s.mu.Unlock()
	s.bus.Publish(e)
	return e.ID, nil
}

// ProjectEvents is the newest event, which the watch reads at its start.
func (s *fakePulseStore) ProjectEvents(context.Context, string, int64, int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events[max(len(s.events)-1, 0):]), nil
}

func (s *fakePulseStore) AddPulse(_ context.Context, p Pulse) (Pulse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID, p.At = int64(len(s.pulses)+1), time.Now()
	s.pulses = append(s.pulses, p)
	return p, nil
}

func (s *fakePulseStore) NewestPulse(context.Context, string) (Pulse, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pulses) == 0 {
		return Pulse{}, false, nil
	}
	return s.pulses[len(s.pulses)-1], true, nil
}

// pulseGateway is the gateway in memory: each call streams the next of
// answers as the model's text, the last one again once they run out,
// unless statuses refuses it with its status, or hang holds it until it
// is cancelled.
type pulseGateway struct {
	mu       sync.Mutex
	answers  []string
	statuses []int
	hang     bool
	calls    []pulseCall
}

type pulseCall struct {
	at      time.Time
	header  http.Header
	request map[string]any
}

func (g *pulseGateway) RoundTrip(r *http.Request) (*http.Response, error) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	g.mu.Lock()
	n, answer := len(g.calls), g.answers[min(len(g.calls), len(g.answers)-1)]
	g.calls = append(g.calls, pulseCall{time.Now(), r.Header.Clone(), req})
	g.mu.Unlock()
	if g.hang {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if n < len(g.statuses) {
		return &http.Response{StatusCode: g.statuses[n], Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)), Request: r}, nil
	}
	chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": answer}}}})
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: " + string(chunk) + "\n\ndata: [DONE]\n\n")), Request: r}, nil
}

const goodPulse = `{"sentence": "#5 Redirect-stats needs your answer.", "bullets": ["Needs you: #5 (a question)", "Running: #4 Links", ` +
	`"Done since 16:20: #2", "Spend: €1.10 of €10"], "prominent": true}`

// pulseScript is a Pulser's run in a bubble: the store as seed leaves it
// at the start, a card_moved event of linkshort at each of events, the
// agent's text on the bus at each of said after them, the model answering answers in turn, a card waiting on a human while
// waiting says so (nil: never), stopped at end.
type pulseScript struct {
	seed     func(s *fakePulseStore, start time.Time)
	events   []time.Duration
	said     []time.Duration
	answers  []string
	statuses []int
	hang     bool
	waiting  func(since time.Duration) bool
	end      time.Duration
}

// run plays sc and answers the store, the calls, and their times since
// the start.
func (sc pulseScript) run(t *testing.T) (store *fakePulseStore, g *pulseGateway, at []time.Duration) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		store, g = &fakePulseStore{bus: NewEventBus()}, &pulseGateway{answers: sc.answers, statuses: sc.statuses, hang: sc.hang}
		if g.answers == nil {
			g.answers = []string{goodPulse}
		}
		if sc.seed != nil {
			sc.seed(store, start)
		}
		ctx, cancel := context.WithCancel(context.Background())
		pl := &Pulser{Store: store, Bus: store.bus,
			Model: &OperatorModel{GatewayURL: "http://gateway.test", Token: "gw-token", HTTP: &http.Client{Transport: g}},
			Context: func(_ context.Context, p Project) (string, bool, error) {
				return "The project now:\nStatus: working; " + p.Name, sc.waiting != nil && sc.waiting(time.Since(start)), nil
			}}
		done := make(chan struct{})
		go func() { pl.Run(ctx); close(done) }()
		synctest.Wait()
		for i, d := range sc.events {
			time.Sleep(time.Until(start.Add(d)))
			store.Record(context.Background(), Event{Repository: "linkshort/linkshort", TaskID: "t-5", IssueNumber: int64(i + 1),
				Kind: EventCardMoved, Actor: "sebastian", Via: ViaButton})
		}
		for _, d := range sc.said {
			time.Sleep(time.Until(start.Add(d)))
			store.bus.Publish(Event{Repository: "linkshort/linkshort", TaskID: "t-5", Kind: EventSessionSaid, Actor: FactoryDecider, Via: ViaFactoryd})
		}
		time.Sleep(time.Until(start.Add(sc.end)))
		synctest.Wait()
		cancel()
		<-done
		for _, c := range g.calls {
			at = append(at, c.at.Sub(start))
		}
	})
	return store, g, at
}

func seconds(s ...float64) []time.Duration {
	var out []time.Duration
	for _, x := range s {
		out = append(out, time.Duration(x*float64(time.Second)))
	}
	return out
}

// When the pulse computes: 2 s after an event, its own events starting
// none; five events within the
// 10 s after a computation make one more, 10 s after it; events 1.5 s
// apart one 2 s after the last, but no later than 10 s after the first.
// Without events, every 60 s while the newest event is under an hour old:
// one 10 min old and a pulse at the start make one call each minute and
// 49 in all (the 50th would fall at the hour). A project idle for two
// hours keeps its pulse, also through the agent's text of a session, one
// without events never gets one, and one with an event newer than its
// pulse gets one at the start.
func TestPulseSchedule(t *testing.T) {
	aged := func(event, pulse time.Duration) func(*fakePulseStore, time.Time) {
		return func(s *fakePulseStore, start time.Time) {
			s.events = []Event{{ID: 1, At: start.Add(-event), Repository: "linkshort/linkshort", Kind: EventCardMoved}}
			s.pulses = []Pulse{{ID: 1, Repository: "linkshort/linkshort", At: start.Add(-pulse), Sentence: "Old."}}
		}
	}
	var everyMinute []time.Duration
	for k := 1; k <= 49; k++ {
		everyMinute = append(everyMinute, time.Duration(k)*time.Minute)
	}
	for name, tc := range map[string]struct {
		sc   pulseScript
		want []time.Duration
	}{
		"one event":                     {pulseScript{events: seconds(0), end: 30 * time.Second}, seconds(2)},
		"five events after a pulse":     {pulseScript{events: seconds(0, 3, 4, 5, 6, 7), end: 30 * time.Second}, seconds(2, 12)},
		"five events 1.5 s apart":       {pulseScript{events: seconds(0, 1.5, 3, 4.5, 6), end: 30 * time.Second}, seconds(8)},
		"events that keep coming":       {pulseScript{events: seconds(0, 1.5, 3, 4.5, 6, 7.5, 9, 10.5, 12, 13.5), end: 30 * time.Second}, seconds(10, 20)},
		"active, no event since":        {pulseScript{seed: aged(10*time.Minute, 0), end: 2 * time.Hour}, everyMinute},
		"idle two hours":                {pulseScript{seed: aged(2*time.Hour, 2*time.Hour), end: 2 * time.Hour}, nil},
		"the agent's text only":         {pulseScript{seed: aged(2*time.Hour, 2*time.Hour), said: seconds(0, 5), end: time.Minute}, nil},
		"no event ever":                 {pulseScript{end: 2 * time.Hour}, nil},
		"an event newer than its pulse": {pulseScript{seed: aged(10*time.Second, 30*time.Second), end: 20 * time.Second}, seconds(0)},
	} {
		if _, _, got := tc.sc.run(t); !slices.Equal(got, tc.want) {
			t.Errorf("%s: calls at %v, want %v", name, got, tc.want)
		}
	}
}

// A computation calls the gateway on the month's chat task with purpose
// and alias pulse, no tools and the project's context, files the pulse,
// and records pulse_computing and pulse with it.
func TestPulseCall(t *testing.T) {
	store, g, _ := pulseScript{events: seconds(0), end: 5 * time.Second}.run(t)
	call := g.calls[0]
	msgs := call.request["messages"].([]any)
	if call.header.Get(gateway.HeaderFactoryPurpose) != "pulse" || call.header.Get(gateway.HeaderFactoryTask) != "t-chat-linkshort-2000-01" ||
		call.request["model"] != "pulse" || call.request["max_tokens"] != float64(1024) || call.request["tools"] != nil || len(msgs) != 2 ||
		msgs[1].(map[string]any)["content"] != "The project now:\nStatus: working; linkshort" ||
		!strings.Contains(msgs[0].(map[string]any)["content"].(string), "one sentence of at most 140 characters") {
		t.Fatalf("call = %v %v", call.header, call.request)
	}
	row := Pulse{ID: 1, Repository: "linkshort/linkshort", Sentence: "#5 Redirect-stats needs your answer.",
		Bullets: []string{"Needs you: #5 (a question)", "Running: #4 Links", "Done since 16:20: #2", "Spend: €1.10 of €10"}}
	if len(store.pulses) != 1 || !store.pulses[0].At.Equal(time.Date(2000, 1, 1, 0, 0, 2, 0, time.UTC)) {
		t.Fatalf("pulses = %+v, want one at 2 s", store.pulses)
	}
	if store.pulses[0].At = (time.Time{}); !reflect.DeepEqual(store.pulses[0], row) {
		t.Fatalf("pulse = %+v, want %+v", store.pulses[0], row)
	}
	var kinds []string
	for _, e := range store.events {
		kinds = append(kinds, e.Kind)
	}
	last := store.events[len(store.events)-1]
	if !slices.Equal(kinds, []string{"card_moved", "pulse_computing", "pulse"}) || last.Actor != FactoryDecider || last.Via != ViaFactoryd ||
		!reflect.DeepEqual(last.Detail, map[string]any{"sentence": row.Sentence, "bullets": row.Bullets, "prominent": false}) {
		t.Fatalf("events %v, the last %+v", kinds, last)
	}
}

// A spent chat budget refuses the computation — the pulse stands — and
// the pulser asks the inbox of it once an hour: repeats within the hour
// count on the poke's row instead of writing it anew, and an hour past
// the ask repeats once.
func TestPulseAsksOnceAnHourOfASpentChatBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakePulseStore{bus: NewEventBus(), spent: true}
		pl := &Pulser{Store: store, Bus: store.bus,
			Context: func(context.Context, Project) (string, bool, error) { return "", false, nil }}
		p := Project{Name: "linkshort", Repository: "linkshort/linkshort"}
		asks := func() []Event {
			var out []Event
			store.mu.Lock()
			for _, e := range store.events {
				if e.Kind == EventChatBudgetSpent {
					out = append(out, e)
				}
			}
			store.mu.Unlock()
			return out
		}
		for i := 0; i < 2; i++ {
			err := pl.compute(context.Background(), p)
			if err == nil || !strings.Contains(err.Error(), "budget of €") {
				t.Fatalf("computation %d: error %v", i+1, err)
			}
		}
		if got := len(asks()); got != 1 {
			t.Fatalf("two computations: %d asks, want 1", got)
		}
		pl.askedMu.Lock()
		pl.asked[ChatTaskID(p.Name, time.Now())] = time.Now().Add(-pulseAskWait - time.Minute)
		pl.askedMu.Unlock()
		if err := pl.compute(context.Background(), p); err == nil {
			t.Fatal("the third computation ran")
		}
		got := asks()
		if len(got) != 2 {
			t.Fatalf("an hour past: %d asks, want 2", len(got))
		}
		for _, e := range got {
			if e.Repository != "linkshort/linkshort" || e.Actor != FactoryDecider || e.Via != ViaFactoryd {
				t.Fatalf("the ask: %+v", e)
			}
			if budget, ok := e.Detail["budget_eur"].(float64); !ok || budget != 5 {
				t.Fatalf("the ask's detail: %v", e.Detail)
			}
		}
	})
}

// An answer that is not the pulse's JSON files no row: the newest stays,
// and the pulse event that ends the computation carries it.
func TestPulseKeepsThePreviousOnANonJSONAnswer(t *testing.T) {
	old := Pulse{ID: 1, Repository: "linkshort/linkshort", Sentence: "All quiet.", Bullets: []string{"a", "b", "c"}}
	store, g, _ := pulseScript{seed: func(s *fakePulseStore, _ time.Time) { s.pulses = []Pulse{old} }, events: seconds(0),
		answers: []string{"Sure! Here is the pulse: all is well."}, end: 5 * time.Second}.run(t)
	if len(g.calls) != 1 || !reflect.DeepEqual(store.pulses, []Pulse{old}) || len(store.events) != 3 ||
		!reflect.DeepEqual(store.events[2].Detail, map[string]any{"sentence": "All quiet.", "bullets": old.Bullets, "prominent": false}) {
		t.Fatalf("%d calls, pulses %+v, events %+v: want the old pulse kept and sent", len(g.calls), store.pulses, store.events)
	}
}

// A failing gateway backs off: three 429s then a 200 make calls 2 s after
// the event, then 60, 120 and 240 s after each failed one, whatever events
// come meanwhile; the first failure and the recovery log one line each.
func TestPulseBacksOffAFailingGateway(t *testing.T) {
	var buf strings.Builder
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)
	_, _, at := pulseScript{events: seconds(0, 70, 100, 200), statuses: []int{429, 429, 429}, end: 450 * time.Second}.run(t)
	log.SetOutput(os.Stderr)
	if want := seconds(2, 62, 182, 422); !slices.Equal(at, want) {
		t.Errorf("calls at %v, want %v", at, want)
	}
	var lines []string
	for line := range strings.Lines(buf.String()) {
		if _, after, ok := strings.Cut(line, "pulse of linkshort: "); ok {
			lines = append(lines, strings.TrimSpace(after))
		}
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "status 429: ") || !strings.HasSuffix(lines[0], "; the last pulse stays, the next tries back off") ||
		lines[1] != "computed again after 3 failed tries" {
		t.Errorf("log lines = %q", lines)
	}
}

// A computation cut off by the shutdown still records its closing pulse
// event, so no browser keeps the typing indicator.
func TestPulseClosesACutOffComputation(t *testing.T) {
	store, g, _ := pulseScript{events: seconds(0), hang: true, end: 5 * time.Second}.run(t)
	var kinds []string
	for _, e := range store.events {
		kinds = append(kinds, e.Kind)
	}
	if len(g.calls) != 1 || !slices.Equal(kinds, []string{"card_moved", "pulse_computing", "pulse"}) {
		t.Fatalf("%d calls, events %v: want the pulse event after the cut-off call", len(g.calls), kinds)
	}
}

// prominent is whether a card waits on a human, whatever the model says.
func TestPulseProminentFollowsTheWaitingCard(t *testing.T) {
	store, _, _ := pulseScript{events: seconds(0, 10), end: 20 * time.Second, waiting: func(d time.Duration) bool { return d < 5*time.Second },
		answers: []string{`{"sentence": "#5 waits on your answer.", "bullets": ["a", "b", "c"], "prominent": false}`,
			`{"sentence": "#5 is done.", "bullets": ["a", "b", "c"], "prominent": true}`}}.run(t)
	var got []bool
	for _, p := range store.pulses {
		got = append(got, p.Prominent)
	}
	if !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("prominent = %v, want true while the card waits, then false", got)
	}
}

func TestPulseAnswer(t *testing.T) {
	long := strings.Repeat("é", 150)
	for answer, want := range map[string]*Pulse{
		`{"sentence": " All  quiet.\n", "bullets": ["a", "b", "c", "d"], "prominent": false}`: {Sentence: "All quiet.", Bullets: []string{"a", "b", "c", "d"}},
		"```json\n{\"sentence\": \"All quiet.\", \"bullets\": [\"a\", \"b\", \"c\"]}\n```":    {Sentence: "All quiet.", Bullets: []string{"a", "b", "c"}},
		`{"sentence": "` + long + `", "bullets": ["` + long + `", "b", "c"]}`: {Sentence: strings.Repeat("é", 139) + "…",
			Bullets: []string{strings.Repeat("é", 99) + "…", "b", "c"}},
		"All quiet.": nil,
		`{"sentence": "All quiet.", "bullets": ["a", "b"]}`:                nil,
		`{"sentence": "All quiet.", "bullets": ["a", "b", "c", "d", "e"]}`: nil,
		`{"sentence": "All quiet.", "bullets": ["a", " ", "c"]}`:           nil,
		`{"bullets": ["a", "b", "c"]}`:                                     nil,
	} {
		if got, ok := pulseAnswer(answer); ok != (want != nil) || ok && !reflect.DeepEqual(got, *want) {
			t.Errorf("%q: %+v, %v; want %+v", answer, got, ok, want)
		}
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): the newest
// pulse is a project's last row, and a project's events leave the
// pulse's own out.
func TestPGStorePulses(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pulse"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	ctx := t.Context()
	if _, ok, err := s.NewestPulse(ctx, "linkshort/linkshort"); err != nil || ok {
		t.Fatalf("NewestPulse before any = %v, %v", ok, err)
	}
	for _, p := range []Pulse{{Repository: "linkshort/linkshort", Sentence: "First."},
		{Repository: "linkshort/linkshort", Sentence: "#5 needs your answer.", Bullets: []string{"x", "y", "z", "w"}, Prominent: true},
		{Repository: "sf/code", Sentence: "Other."}} {
		if _, err := s.AddPulse(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.NewestPulse(ctx, "linkshort/linkshort")
	if err != nil || !ok || got.ID != 2 || got.At.IsZero() || got.Sentence != "#5 needs your answer." ||
		!slices.Equal(got.Bullets, []string{"x", "y", "z", "w"}) || !got.Prominent {
		t.Fatalf("NewestPulse = %+v, %v, %v", got, ok, err)
	}
	for _, kind := range []string{EventCardMoved, EventPulseComputing, EventPulse} {
		if _, err := s.Record(ctx, Event{Repository: "linkshort/linkshort", Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd}); err != nil {
			t.Fatal(err)
		}
	}
	if events, err := s.ProjectEvents(ctx, "linkshort/linkshort", 0, 10); err != nil || len(events) != 1 || events[0].Kind != EventCardMoved {
		t.Fatalf("ProjectEvents = %+v, %v: want the card_moved only", events, err)
	}
	// a new pulse deletes its project's pulses and pulse events older than a day
	old := time.Now().Add(-25 * time.Hour)
	for _, kind := range []string{EventCardMoved, EventPulseComputing, EventPulse} {
		if _, err := s.Record(ctx, Event{At: old, Repository: "linkshort/linkshort", Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE pulses SET at = $1 WHERE id IN (1, 3)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPulse(ctx, Pulse{Repository: "linkshort/linkshort", Sentence: "Fourth."}); err != nil {
		t.Fatal(err)
	}
	var pulses, kept []string
	rows, err := s.db.QueryContext(ctx, `SELECT sentence FROM pulses ORDER BY id`)
	for err == nil && rows.Next() {
		var p string
		err = rows.Scan(&p)
		pulses = append(pulses, p)
	}
	events, err2 := s.db.QueryContext(ctx, `SELECT kind || CASE WHEN at < now() - interval '1 day' THEN ' old' ELSE '' END FROM events ORDER BY id`)
	for err2 == nil && events.Next() {
		var e string
		err2 = events.Scan(&e)
		kept = append(kept, e)
	}
	if err != nil || err2 != nil || !slices.Equal(pulses, []string{"#5 needs your answer.", "Other.", "Fourth."}) ||
		!slices.Equal(kept, []string{"card_moved", "pulse_computing", "pulse", "card_moved old"}) {
		t.Fatalf("pulses %v, events %v (%v, %v): want the old first pulse and the old pulse events gone", pulses, kept, err, err2)
	}
}
