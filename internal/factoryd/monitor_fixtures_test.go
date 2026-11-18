package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// monitorAt is the monitor rig's clock: 12:00 on 2026-11-04.
var monitorAt = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// monitorRig is a monitor on Postgres over demo-app/demo-app at monitorAt,
// under the project tree's policy, whose queue bound is 2h. Forgejo keeps
// the comments by issue, answers an issue's state and closes it, and the
// object store fails the read of one key. The workflows record every
// signal.
type monitorRig struct {
	m       *Monitor
	s       *PGStore
	forge   *monitorForge
	objects *unreadableObjects
	signals *caseSignals
}

// unreadableObjects is a fake object store whose Get of key unreadable
// fails, as S3 does when it is down.
type unreadableObjects struct {
	fakeObjectStore
	unreadable string
}

func (o *unreadableObjects) Get(ctx context.Context, key string) ([]byte, error) {
	if key == o.unreadable {
		return nil, errors.New("the object store is down")
	}
	return o.fakeObjectStore.Get(ctx, key)
}

// monitorForge is a Forgejo for the issues of demo-app/demo-app.
type monitorForge struct {
	mu       sync.Mutex
	comments map[int64][]string
	posted   int
	closed   map[int64]bool
	closes   []int64
	// failComments fails that many comment posts, and failClose every
	// close
	failComments int
	failClose    bool
}

func (f *monitorForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/issues/")
		if !ok {
			rest, ok = strings.CutPrefix(r.URL.Path, "/api/v1/repos/sf/r/issues/")
		}
		number, sub, _ := strings.Cut(rest, "/")
		n, err := strconv.ParseInt(number, 10, 64)
		switch {
		case !ok || err != nil:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && sub == "comments" && f.failComments > 0:
			f.failComments--
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && sub == "comments":
			var c struct{ Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &c)
			f.comments[n] = append(f.comments[n], c.Body)
			f.posted++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+f.posted)
		case strings.HasPrefix(sub, "labels"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && sub == "":
			state := "open"
			if f.closed[n] {
				state = "closed"
			}
			fmt.Fprintf(w, `{"number": %d, "state": %q}`, n, state)
		case r.Method == http.MethodPatch && sub == "" && f.failClose:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPatch && sub == "":
			f.closed[n] = true
			f.closes = append(f.closes, n)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		default:
			// an issue's dependencies among them: none
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// monitorStore is a Postgres store with no cards but cardCaseStore's
// t1410 on sf/r, no events and no monitor cursor.
func monitorStore(t *testing.T) *PGStore {
	t.Helper()
	s := cardCaseStore(t, "monitor")
	if _, err := s.db.ExecContext(context.Background(), `TRUNCATE monitor_cursor`); err != nil {
		t.Fatal(err)
	}
	return s
}

func newMonitorRig(t *testing.T) monitorRig {
	t.Helper()
	s := monitorStore(t)
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	signals := &caseSignals{}
	cfg.Workflows = signals
	forge := &monitorForge{comments: map[int64][]string{}, closed: map[int64]bool{}}
	objects := &unreadableObjects{}
	now := func() time.Time { return monitorAt }
	r := &Runner{store: s, objects: objects, cfg: cfg, timeline: &Timeline{Objects: objects, Events: s, Store: s},
		forgejo: &Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"}, now: now}
	m := &Monitor{Store: s, Runner: r, Policy: func() policy.Tree { return v.Tree }, Now: now}
	return monitorRig{m: m, s: s, forge: forge, objects: objects, signals: signals}
}

// wakeCases runs the case-wake pass once.
func (g monitorRig) wakeCases(t *testing.T) {
	t.Helper()
	if err := g.m.WakeCases(context.Background()); err != nil {
		t.Fatalf("WakeCases: %v", err)
	}
}

// event records an event of kind on card id, on issue, at at.
func (g monitorRig) event(t *testing.T, id string, issue int64, kind string, detail map[string]any, at time.Time) int64 {
	t.Helper()
	n, err := g.s.Record(context.Background(), Event{Repository: "demo-app/demo-app", TaskID: id, IssueNumber: issue, Kind: kind,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail, At: at})
	if err != nil || n == 0 {
		t.Fatalf("Record %s = %d, %v", kind, n, err)
	}
	return n
}

// filed records card id's card_created row with its budget of 3 EUR, so
// the budget moved by no event.
func (g monitorRig) filed(t *testing.T, id string, issue int64) int64 {
	t.Helper()
	return g.event(t, id, issue, EventCardCreated, map[string]any{"budget_eur": 3.0}, monitorAt.Add(-4*time.Hour))
}

// flagCases are card id's monitor_flag cases by state key, one case per
// key in any status.
func (g monitorRig) flagCases(t *testing.T, id string) map[string]Case {
	t.Helper()
	cases, err := g.s.CasesOf(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Case{}
	for _, c := range cases {
		if c.Kind != CaseMonitorFlag {
			continue
		}
		if have, ok := out[c.StateKey]; ok {
			t.Fatalf("%s has two flag cases on %s: %d (%s) and %d (%s)", id, c.StateKey, have.ID, have.Status, c.ID, c.Status)
		}
		out[c.StateKey] = c
	}
	return out
}

// A queued card held three hours by the count since 09:00, the filing at
// 08:00, against the 2h bound: queued_over_bound's case, which
// wait_over_bound on the same wait leaves alone.
const (
	queuedSince    = "2026-11-04T09:00:00Z"
	queuedKey      = "queued_over_bound|queued|" + queuedSince
	queuedLine     = "Monitor flag: queued_over_bound(work bound 3 of 3 for 3h)"
	monitorHandsUp = "no resolver fits"
)

// assertQueueCase checks that card id carries the queue's flag case alone,
// at the human with rung 0's hand-up, rung 2's and the human's step, its
// facts naming the flag, its state and the wait, beside the digest, and that the card
// stays queued on its reason with no ask of its own.
func (g monitorRig) assertQueueCase(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	cases := g.flagCases(t, id)
	c, ok := cases[queuedKey]
	if len(cases) != 1 || !ok || c.Status != CaseAtHuman || c.Problem != queuedLine {
		t.Fatalf("%s's flag cases = %v, want %s alone at the human on %q", id, slices.Sorted(maps.Keys(cases)), queuedKey, queuedLine)
	}
	digest, _ := c.Facts["digest"].(string)
	if c.Facts["flag"] != "queued_over_bound" || c.Facts["detail"] != "work bound 3 of 3 for 3h" || c.Facts["state"] != "queued|"+queuedSince ||
		c.Facts["wait"] != "queued" || c.Facts["since"] != queuedSince || c.Facts["bound"] != "2h" ||
		!strings.Contains(digest, "flags: queued_over_bound(work bound 3 of 3 for 3h), wait_over_bound(queued since +1:00)") {
		t.Fatalf("%s's case facts = %v, want the flag, its detail, state queued|%s, the queued wait since then against 2h and the digest", id, c.Facts, queuedSince)
	}
	steps := caseStepsOf(t, g.s, c.ID)
	if len(steps) != 3 || steps[0].Rung != 0 || steps[0].Found != monitorHandsUp || steps[0].Outcome != StepHandedUp ||
		steps[1].Rung != 2 || steps[1].Outcome != StepHandedUp || steps[2].Rung != 3 || steps[2].Outcome != StepInProgress {
		t.Fatalf("%s's case steps = %+v, want rung 0's hand-up, rung 2's and the human's step", id, steps)
	}
	task, _, err := g.s.Task(ctx, id)
	if err != nil || task.Status != "queued" || task.QueueReason != "work bound 3 of 3" {
		t.Fatalf("%s = %s on %q, %v; want it queued on its reason", id, task.Status, task.QueueReason, err)
	}
	if intake, err := g.s.IntakeEvents(ctx, id); err != nil || len(intake) != 0 {
		t.Fatalf("%s's intake rows = %+v, %v; want none: the card asked nothing", id, intake, err)
	}
}

// logs captures the log lines a test writes.
func logs(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	was := log.Writer()
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(was) })
	return &b
}

// The newest ask on a card is the run's: a monitor case beside it, newer
// and at the human, is answered by its id alone.
func TestNewestAskCaseLeavesAMonitorCaseOut(t *testing.T) {
	for name, open := range monitorStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			run, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap", Status: CaseAtHuman, Problem: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag, StateKey: "checkin_cut|+0:20",
				Status: CaseAtHuman, Problem: "Monitor flag: checkin_cut(+0:20)"}); err != nil {
				t.Fatal(err)
			}
			if got, err := s.NewestAskCase(ctx, "t1"); err != nil || got.ID != run.ID {
				t.Fatalf("NewestAskCase = case %d (%s), %v; want the run's case %d", got.ID, got.Kind, err, run.ID)
			}
		})
	}
}

// monitorStores are the stores whose monitor reads must agree; Postgres runs
// in internal/factoryd/pg, so the fake runs here.
var monitorStores = map[string]func(*testing.T) Store{
	"fake": func(*testing.T) Store { return newFakeStore() },
}

// The first read plants the cursor at the newest event and answers
// nothing: no history raises a flag. Later reads answer the monitor's
// kinds past the cursor and past after, oldest first, up to the limit.
// The cursor moves forward only.
func TestMonitorPlantsTheCursorAtTheNewestEvent(t *testing.T) {
	for name, open := range monitorStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			record := func(kind string) int64 {
				t.Helper()
				id, err := s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd})
				if err != nil || id == 0 {
					t.Fatalf("Record %s = %d, %v", kind, id, err)
				}
				return id
			}
			read := func(after int64, limit int) []int64 {
				t.Helper()
				events, err := s.MonitorEvents(ctx, after, monitorKinds, limit)
				if err != nil {
					t.Fatal(err)
				}
				ids := make([]int64, len(events))
				for i, e := range events {
					ids[i] = e.ID
				}
				return ids
			}
			record(EventTaskStatus)
			record(EventSessionEnd)
			if got := read(0, 100); len(got) != 0 {
				t.Fatalf("the first read = %v, want nothing: the cursor starts at the newest event", got)
			}
			review := record(EventModelReview)
			record(EventSessionStarted)
			audit := record(EventGateAudit)
			if got := read(0, 100); !slices.Equal(got, []int64{review, audit}) {
				t.Fatalf("read = %v, want the review %d and the audit %d, not the session's start", got, review, audit)
			}
			if got := read(0, 1); !slices.Equal(got, []int64{review}) {
				t.Fatalf("read of one = %v, want the review %d", got, review)
			}
			if got := read(review, 100); !slices.Equal(got, []int64{audit}) {
				t.Fatalf("read past the review = %v, want the audit %d", got, audit)
			}
			for _, id := range []int64{audit, review} {
				if err := s.SetMonitorCursor(ctx, id); err != nil {
					t.Fatal(err)
				}
				if got := read(0, 100); len(got) != 0 {
					t.Fatalf("read after the cursor moved to %d = %v, want nothing: the cursor stays at the audit %d", id, got, audit)
				}
			}
		})
	}
}
