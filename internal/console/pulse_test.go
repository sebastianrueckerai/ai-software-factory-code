package console

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakePulses is the newest pulse of each repository.
type fakePulses map[string]factoryd.Pulse

func (f fakePulses) NewestPulse(_ context.Context, repository string) (factoryd.Pulse, bool, error) {
	p, ok := f[repository]
	return p, ok, nil
}

var calm = factoryd.Pulse{ID: 7, Repository: "sf/code", At: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC),
	Sentence: "#11 Speed up the board is running; #4 merged at 09:12.",
	Bullets:  []string{"Needs you: nothing", "Running: #11 Speed up the board", "Since 08:30: #4 done", "Spend: €5.50 of the €50 cap"}}

// pulseBlock is the pulse's markup on the board of query.
func pulseBlock(t *testing.T, r *rig, ck *http.Cookie, query string) string {
	t.Helper()
	body := r.do(httptest.NewRequest(http.MethodGet, "/"+query, nil), ck).Body.String()
	start := strings.Index(body, `<section id="pulse"`)
	if start < 0 || !strings.Contains(body[start:], `<div class="pulse-row">`) {
		return ""
	}
	return body[start : start+strings.Index(body[start:], "</p></div>")+len("</p></div>")]
}

// The pulse band stands above the screen with its sentence first: muted
// words when all is well, the warning sign and the prominent class when a
// card waits on a human. Its bullets are in the markup, folded behind the
// band's disclosure; a project without a pulse has a band of the strip
// alone.
func TestPulseLineSubduedAndProminent(t *testing.T) {
	r, ck := screenRig(t)
	r.c.cfg.Pulses = fakePulses{"sf/code": calm}
	const tail = `<span class="typing pulse-typing" hidden title="the pulse recomputes"><i></i><i></i><i></i></span></p></div>`
	const bullets = `<ul class="pulse-bullets" id="pulse-bullets" hidden><li>Needs you: nothing</li><li>Running: #11 Speed up the board</li><li>Since 08:30: #4 done</li><li>Spend: €5.50 of the €50 cap</li></ul>`
	for _, query := range []string{"", "?view=wide"} {
		if got, want := pulseBlock(t, r, ck, query), `<section id="pulse" class="status" aria-label="Pulse">
<div class="pulse-row"><span class="pulse-sign" hidden aria-hidden="true">⚠</span><p class="pulse-text"><span class="pulse-say">#11 Speed up the board is running; #4 merged at 09:12.</span>`+tail; got != want {
			t.Errorf("board%s: the calm pulse\n%s\nwant\n%s", query, got, want)
		}
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	band, screen := strings.Index(body, `<section id="pulse"`), strings.Index(body, `<div id="screen"`)
	if band < 0 || screen < 0 || band > screen || !strings.Contains(body[band:screen], bullets) || !strings.Contains(body[band:screen], `<div id="status-line">`) {
		t.Errorf("the pulse band with its strip and folded bullets does not stand above the screen")
	}
	waits := calm
	waits.Sentence, waits.Prominent = "#13 Use a replica? needs your answer.", true
	r.c.cfg.Pulses = fakePulses{"sf/code": waits}
	if got, want := pulseBlock(t, r, ck, ""), `<section id="pulse" class="status prominent" aria-label="Pulse">
<div class="pulse-row"><span class="pulse-sign" aria-hidden="true">⚠</span><p class="pulse-text"><span class="pulse-say">#13 Use a replica? needs your answer.</span>`+tail; got != want {
		t.Errorf("the prominent pulse\n%s\nwant\n%s", got, want)
	}
	r.c.cfg.Pulses = fakePulses{}
	if got := pulseBlock(t, r, ck, ""); got != "" {
		t.Errorf("a project without a pulse shows %s", got)
	}
}

// The strip carries the pulse's age once it is two minutes old, marked
// warn when the pulse is stale on a project with an event in the last
// hour — stopped computing, not quiet: without such an event the age
// stays plain until one lands, and a pulse under two minutes carries no
// cell at all.
func TestStatusStripCarriesThePulseAge(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	stale := calm
	stale.At = time.Now().Add(-25 * time.Minute)
	r.c.cfg.Pulses = fakePulses{"sf/code": stale}
	r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", Kind: factoryd.EventCardMoved, TaskID: "t-2"})
	fresh := `<span class="cell warn" title="the age of the project's newest pulse">pulse 25 min old</span>`
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, fresh) {
		t.Errorf("a stale pulse on a live project:\n%s", body)
	}
	r.events = &fakeEvents{bus: factoryd.NewEventBus()} // no event of the last hour
	r.c.cfg.Events = r.events
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, fresh) {
		t.Error("a quiet project marks its pulse's age warn")
	}
	r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", Kind: factoryd.EventCardMoved, TaskID: "t-2"})
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, fresh) {
		t.Errorf("a fresh event does not warn again:\n%s", body)
	}
	r.c.cfg.Pulses = fakePulses{"sf/code": {ID: 8, Repository: "sf/code", At: time.Now().Add(-90 * time.Second), Sentence: "All quiet."}}
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "the age of the project's newest pulse") {
		t.Error("a pulse under two minutes carries an age cell")
	}
}

// readerForgejo shows the member repository sf/code, as Forgejo does a
// repository they read.
type readerForgejo struct{ http.RoundTripper }

func (f readerForgejo) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code" {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"full_name":"sf/code"}`)), Request: r}, nil
	}
	return f.RoundTripper.RoundTrip(r)
}

// GET /projects/{name}/pulse answers the newest pulse to a writer and a
// reader of the project, 403 to a member who does neither, and 404 for no
// pulse yet or no such project; the board shows it under the same rule.
func TestPulseGet(t *testing.T) {
	const want = `{"id":7,"repository":"sf/code","at":"2026-10-11T09:30:00Z","sentence":"#11 Speed up the board is running; #4 merged at 09:12.",` +
		`"bullets":["Needs you: nothing","Running: #11 Speed up the board","Since 08:30: #4 done","Spend: €5.50 of the €50 cap"],"prominent":false}`
	for name, tc := range map[string]struct {
		writes, reads bool
		pulses        fakePulses
		path          string
		code          int
		body          string
	}{
		"a writer":      {writes: true, pulses: fakePulses{"sf/code": calm}, code: 200, body: want},
		"a reader":      {reads: true, pulses: fakePulses{"sf/code": calm}, code: 200, body: want},
		"a non-member":  {pulses: fakePulses{"sf/code": calm}, code: 403, body: "you read no project ai-software-factory-code"},
		"no pulse yet":  {writes: true, pulses: fakePulses{}, code: 404, body: "the project has no pulse yet"},
		"no such thing": {writes: true, pulses: fakePulses{"sf/code": calm}, path: "/projects/nothing/pulse", code: 404, body: "404 page not found"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.c.cfg.Pulses = tc.pulses
			if !tc.writes {
				r.repoWriters["sf/code"] = nil
			}
			if tc.reads {
				r.c.cfg.HTTP = &http.Client{Transport: readerForgejo{http.DefaultTransport}}
			}
			ck, _ := r.loggedIn(t)
			rec := r.do(httptest.NewRequest(http.MethodGet, cmp.Or(tc.path, "/projects/ai-software-factory-code/pulse"), nil), ck)
			if got := strings.TrimSpace(rec.Body.String()); rec.Code != tc.code || got != tc.body {
				t.Fatalf("%d %s, want %d %s", rec.Code, got, tc.code, tc.body)
			}
			if shown := pulseBlock(t, r, ck, "?project=ai-software-factory-code") != ""; tc.path == "" && shown != (tc.code == 200) {
				t.Fatalf("the board shows the pulse: %v, want %v", shown, tc.code == 200)
			}
		})
	}
}

// The pulse reads the project as the project agent does, seen by no
// member: no writer check runs and its own last pulse is not in it; the
// agent of a chat turn gets the pulse line under its status line.
func TestPulseContextIsTheAgentsSnapshot(t *testing.T) {
	r, _, g := newChatRig(t)
	r.c.cfg.Pulses = fakePulses{"sf/code": calm}
	ctx := t.Context()
	r.events.Record(ctx, factoryd.Event{At: time.Date(2026, 10, 11, 9, 12, 0, 0, time.UTC), Repository: "sf/code", TaskID: "t-r", IssueNumber: 11,
		Kind: factoryd.EventCardMoved, Actor: "sebastian", Via: factoryd.ViaButton})
	text, waiting, err := r.c.PulseContext(ctx, projectRows()[0])
	want := "The project now:\nStatus: waiting on you; 1 working; 3 need you; spend this month €0.00 of €50.00: the cards €0.00, the chat €0.00\n" +
		boardText +
		"\n\nIts last events, oldest first:\n2026-10-11 09:12 #11 t-r card_moved by sebastian via button"
	if err != nil || text != want || !waiting {
		t.Fatalf("PulseContext = %v, %v\n%s\nwant waiting and\n%s", waiting, err, text, want)
	}
	if len(r.mayAsked) != 0 {
		t.Fatalf("the pulse's view asked who writes: %v", r.mayAsked)
	}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("All calm.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what matters now?"}})
	if system := messages(t, g.calls()[0])[0]["content"].(string); !strings.HasSuffix(system, "\nPulse: "+calm.Sentence) {
		t.Fatalf("the agent's status lacks the pulse:\n%s", system)
	}
}

// For the pulse, a card waiting on its author waits on a person, whoever
// the author is.
func TestPulseContextCountsACardWaitingOnItsAuthor(t *testing.T) {
	r := newRig(t)
	r.approvals.rows = nil
	for status, want := range map[string]bool{"waiting-poster": true, "queued": false} {
		r.tasks.set("", factoryd.Task{ID: "t-ask", Kind: "agent", Status: status, Author: "alice", Repository: "sf/code", IssueNumber: 3, Objective: "Ask."})
		if _, waiting, err := r.c.PulseContext(t.Context(), projectRows()[0]); err != nil || waiting != want {
			t.Errorf("%s: waiting %v, %v; want %v", status, waiting, err, want)
		}
	}
}

// PulseState answers the heartbeat's two questions from the same
// snapshot: work sits open, and a card waits on a human.
func TestPulseStateReadsTheBoard(t *testing.T) {
	r := newRig(t)
	r.approvals.rows = nil
	for status, want := range map[string][2]bool{
		"running": {true, false}, "queued": {false, false}, "waiting-human": {false, true},
	} {
		r.tasks.set("", factoryd.Task{ID: "t-hb", Kind: "agent", Status: status, Author: "alice", Repository: "sf/code", IssueNumber: 3, Objective: "Beat."})
		open, waiting, err := r.c.PulseState(t.Context(), projectRows()[0])
		if err != nil || open != want[0] || waiting != want[1] {
			t.Errorf("%s: PulseState = %v, %v, %v; want %v, %v", status, open, waiting, err, want[0], want[1])
		}
	}
}
