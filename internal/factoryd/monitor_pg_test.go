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
}

func (f *monitorForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/issues/")
		number, sub, _ := strings.Cut(rest, "/")
		n, err := strconv.ParseInt(number, 10, 64)
		switch {
		case !ok || err != nil:
			w.WriteHeader(http.StatusNotFound)
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

// card files agent card id on issue of demo-app/demo-app in status at
// 08:00, four hours before the rig's clock.
func (g monitorRig) card(t *testing.T, id string, issue int64, status string) {
	t.Helper()
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: status, Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: issue, Track: policy.TrackDirect, Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET created_at = $2 WHERE id = $1`, id, monitorAt.Add(-4*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// queued files queued card id on issue, held by the count since 09:00:
// three hours against the queue's bound of 2h.
func (g monitorRig) queued(t *testing.T, id string, issue int64) {
	t.Helper()
	g.card(t, id, issue, "queued")
	if err := g.s.SetQueueReason(context.Background(), id, "work bound 3 of 3", monitorAt.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
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

func (g monitorRig) pass(t *testing.T) {
	t.Helper()
	if err := g.m.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
}

func (g monitorRig) cursor(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := g.s.db.QueryRowContext(context.Background(), `SELECT event_id FROM monitor_cursor`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
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

// The queue's two flags on a card held three hours by the count since
// 09:00, the filing at 08:00, against the 2h bound.
const (
	queuedSince    = "2026-11-04T09:00:00Z"
	queuedKey      = "queued_over_bound|queued|" + queuedSince
	waitKey        = "wait_over_bound|queued|" + queuedSince
	queuedProblem  = "Monitor flag: queued_over_bound(work bound 3 of 3 since +1:00)"
	waitedProblem  = "Monitor flag: wait_over_bound(queued since +1:00)"
	monitorHandsUp = "no resolver fits"
)

// assertQueueCases checks that card id carries the queue's two flag cases
// alone, each at the human with rung 0's hand-up and the human's step,
// its facts naming the flag, its state and the wait, beside the digest.
func (g monitorRig) assertQueueCases(t *testing.T, id string) {
	t.Helper()
	cases := g.flagCases(t, id)
	if len(cases) != 2 {
		t.Fatalf("%s's flag cases = %v, want %s and %s", id, slices.Sorted(maps.Keys(cases)), queuedKey, waitKey)
	}
	for key, want := range map[string]struct{ flag, detail, problem string }{
		queuedKey: {"queued_over_bound", "work bound 3 of 3 since +1:00", queuedProblem},
		waitKey:   {"wait_over_bound", "queued since +1:00", waitedProblem},
	} {
		c, ok := cases[key]
		if !ok || c.Status != CaseAtHuman || c.Problem != want.problem {
			t.Fatalf("%s's case %s = %+v (found %v), want it at the human on %q", id, key, c, ok, want.problem)
		}
		digest, _ := c.Facts["digest"].(string)
		if c.Facts["flag"] != want.flag || c.Facts["detail"] != want.detail || c.Facts["state"] != "queued|"+queuedSince ||
			c.Facts["wait"] != "queued" || c.Facts["since"] != queuedSince || c.Facts["bound"] != "2h" ||
			!strings.Contains(digest, "flags: queued_over_bound(work bound 3 of 3 since +1:00), wait_over_bound(queued since +1:00)") {
			t.Fatalf("%s's case %s facts = %v, want flag %s, detail %q, state queued|%s, the queued wait since then against 2h and the digest",
				id, key, c.Facts, want.flag, want.detail, queuedSince)
		}
		steps := caseStepsOf(t, g.s, c.ID)
		if len(steps) != 2 || steps[0].Rung != 0 || steps[0].Found != monitorHandsUp || steps[0].Outcome != StepHandedUp ||
			steps[1].Rung != 3 || steps[1].Outcome != StepInProgress {
			t.Fatalf("%s's case %s steps = %+v, want rung 0's hand-up and the human's step", id, key, steps)
		}
	}
}

// An event on a queued card past its bound opens one monitor_flag case per
// flag, keyed by the flag and the wait's kind and start, and the human is
// asked on each. The card back in the queue on the same wait and another
// event: the flags stand, and nothing is opened or asked again. Each pass
// moves the cursor past the events it read, the first one planted at the
// newest event, which it reads nothing past.
func TestMonitorPassOpensOneCasePerNewFlag(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.queued(t, "t1", 12)
	g.pass(t)
	if len(g.flagCases(t, "t1")) != 0 {
		t.Fatal("the first pass read history")
	}
	filed := g.filed(t, "t1", 12)
	g.pass(t)
	g.assertQueueCases(t, "t1")
	if task, _, _ := g.s.Task(ctx, "t1"); task.Status != "waiting-human" || len(g.forge.comments[12]) != 2 {
		t.Fatalf("t1 %s with comments %q, want it waiting on the human with one face per case", task.Status, g.forge.comments[12])
	}
	if got := g.cursor(t); got != filed {
		t.Fatalf("cursor = %d, want at the card_created row %d it read", got, filed)
	}

	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'queued' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	again := g.event(t, "t1", 12, EventTaskStatus, map[string]any{"status": "queued"}, monitorAt.Add(-time.Minute))
	g.pass(t)
	g.assertQueueCases(t, "t1")
	if len(g.forge.comments[12]) != 2 {
		t.Fatalf("comments = %q, want the two faces of the first pass alone", g.forge.comments[12])
	}
	if got := g.cursor(t); got != again {
		t.Fatalf("cursor = %d, want at the task_status row %d, the newest it read", got, again)
	}
}

// A card whose timeline cannot be read is logged and skipped: the next
// card's cases open and the cursor moves past both events, since the
// minute sweep reads the card again.
func TestMonitorPassGoesOnPastACardItCannotRead(t *testing.T) {
	g := newMonitorRig(t)
	g.card(t, "t1", 12, "running")
	g.queued(t, "t2", 13)
	g.objects.unreadable = "sessions/t1/timeline.jsonl"
	g.pass(t)
	g.filed(t, "t1", 12)
	last := g.filed(t, "t2", 13)
	g.pass(t)
	if cases := g.flagCases(t, "t1"); len(cases) != 0 {
		t.Fatalf("t1's flag cases = %v, want none", cases)
	}
	g.assertQueueCases(t, "t2")
	if got := g.cursor(t); got != last {
		t.Fatalf("cursor = %d, want %d past both cards' events", got, last)
	}
}

// The sweep needs no event. It opens the flags of every live card of the
// project's repository: the queued card past its bound. It wakes the case
// whose wake passed: rung 0 runs on it again through OpenTrip's repeat
// path and hands it up, the human is asked on it, and its wake is gone.
// And it expires a rung's step past its bound, on a card of no project.
func TestMonitorSweepWakesDueCasesAndExpiresSteps(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.queued(t, "t1", 12)
	g.filed(t, "t1", 12)
	g.card(t, "t2", 13, "waiting-human")
	g.filed(t, "t2", 13)
	const cut = "Monitor flag: checkin_cut(+0:20)"
	due, _, err := g.s.OpenCase(ctx, Case{TaskID: "t2", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag, StateKey: "checkin_cut|",
		Status: CaseOpen, Problem: cut, Facts: map[string]any{"reason": cut, "flag": "checkin_cut", "state": ""}})
	if err != nil {
		t.Fatal(err)
	}
	wake := monitorAt.Add(-time.Minute)
	if err := g.s.SetCaseWake(ctx, due.ID, 0, nil, &wake); err != nil {
		t.Fatal(err)
	}
	elsewhere, _, err := g.s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "p"})
	if err != nil {
		t.Fatal(err)
	}
	stepAt := monitorAt.Add(-5 * time.Minute)
	step, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: elsewhere.ID, Rung: 1, Actor: "rung 1", DueAt: &stepAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = $2 WHERE id = $1`, step, monitorAt.Add(-40*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := g.m.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	g.assertQueueCases(t, "t1")
	woken := caseByID(t, g.s, due.ID)
	steps := caseStepsOf(t, g.s, due.ID)
	if woken.Status != CaseAtHuman || woken.NextWakeAt != nil || woken.OwnerRung != 3 || len(steps) != 2 || steps[0].Rung != 0 ||
		steps[0].Found != monitorHandsUp || steps[0].Outcome != StepHandedUp || steps[1].Rung != 3 {
		t.Fatalf("woken case = %+v, steps %+v; want it at the human, owned by the human with no wake, rung 0's hand-up and the human's step", woken, steps)
	}
	if cases := g.flagCases(t, "t2"); len(cases) != 1 || len(g.forge.comments[13]) != 1 {
		t.Fatalf("t2's flag cases = %v, comments %q; want the woken case alone and its one face", cases, g.forge.comments[13])
	}
	expired := caseStepsOf(t, g.s, elsewhere.ID)
	if len(expired) != 1 || expired[0].Outcome != StepHandedUp || expired[0].Found != "the rung did not answer within 35m" {
		t.Fatalf("the step past its bound = %+v, want it handed up as unanswered within 35m", expired)
	}
}

// An ended card with a live case raises issue_open_after_end, and rung 0
// closes what it left open through Act, the human asked nothing and no run
// signalled: closed card t1's open issue closes with factoryd's comment,
// and done card t2's closed issue is left alone. Another event on t1 opens
// no second case: the flag's case for that state ended, and one is all.
func TestMonitorRungZeroClosesTheIssueOfAnEndedCard(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.card(t, "t1", 12, "closed")
	g.card(t, "t2", 13, "done")
	g.forge.closed[13] = true
	for _, id := range []string{"t1", "t2"} {
		if _, _, err := g.s.OpenCase(ctx, Case{TaskID: id, Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap",
			Status: CaseAtHuman, Problem: "Card used its sessions"}); err != nil {
			t.Fatal(err)
		}
	}
	g.pass(t)
	g.event(t, "t1", 12, EventTaskEnded, map[string]any{"state": "closed"}, monitorAt.Add(-time.Minute))
	g.event(t, "t2", 13, EventTaskEnded, map[string]any{"state": "done"}, monitorAt.Add(-time.Minute))
	g.pass(t)

	for id, result := range map[string]string{"t1": "Closed #12.", "t2": "#13 was closed already."} {
		cases := g.flagCases(t, id)
		c, ok := cases["issue_open_after_end|"]
		if len(cases) != 1 || !ok || c.Status != CaseResolved ||
			!reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActClose}) {
			t.Fatalf("%s's flag cases = %v, want issue_open_after_end resolved by factoryd on rung 0 with close", id, cases)
		}
		steps := caseStepsOf(t, g.s, c.ID)
		if len(steps) != 1 || steps[0].Rung != 0 || steps[0].Outcome != StepResolved ||
			!reflect.DeepEqual(steps[0].Did, []CaseAction{{Action: ActClose, Result: result}}) {
			t.Fatalf("%s's steps = %+v, want rung 0's close: %q", id, steps, result)
		}
	}
	if !slices.Equal(g.forge.closes, []int64{12}) || !slices.Equal(g.forge.comments[12], []string{"factoryd: this card ended closed, so its issue closes."}) ||
		len(g.forge.comments[13]) != 0 {
		t.Fatalf("closes %v, comments %v; want #12 closed with factoryd's comment and #13 untouched", g.forge.closes, g.forge.comments)
	}
	if asks := (actRig{s: g.s}).rows(t, EventQuestionAsked); len(asks) != 0 || len(g.signals.got) != 0 {
		t.Fatalf("asks %d, signals %+v; want none", len(asks), g.signals.got)
	}

	g.event(t, "t1", 12, EventTaskStatus, map[string]any{"status": "closed"}, monitorAt)
	g.pass(t)
	if cases := g.flagCases(t, "t1"); len(cases) != 1 || len(g.forge.closes) != 1 {
		t.Fatalf("t1's flag cases = %v, closes %v; want the one case and the one close", cases, g.forge.closes)
	}
}

// A rung's step past its due on a live case raises wait_over_bound on
// rung_answer, from the step's start at 11:20, the filing at 08:00. Rung
// 0 ends the step as ExpireSteps ends it, 35m after its start, and hands
// the flag's case up: no resolver takes it.
func TestMonitorRungZeroEndsAStepPastItsBound(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.card(t, "t1", 12, "waiting-human")
	held, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "p"})
	if err != nil {
		t.Fatal(err)
	}
	due := monitorAt.Add(-5 * time.Minute)
	step, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: held.ID, Rung: 1, Actor: "rung 1", DueAt: &due})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = $2 WHERE id = $1`, step, monitorAt.Add(-40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	g.pass(t)
	g.filed(t, "t1", 12)
	g.pass(t)

	cases := g.flagCases(t, "t1")
	c, ok := cases["wait_over_bound|rung_answer|2026-11-04T11:20:00Z"]
	if len(cases) != 1 || !ok || c.Status != CaseAtHuman || c.Problem != "Monitor flag: wait_over_bound(rung_answer since +3:20)" ||
		c.Facts["wait"] != "rung_answer" || c.Facts["owner_rung"] != 1.0 {
		t.Fatalf("t1's flag cases = %v, want wait_over_bound on rung 1's answer since 11:20 at the human", cases)
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) != 2 || steps[0].Rung != 0 || steps[0].Found != monitorHandsUp || steps[0].Outcome != StepHandedUp {
		t.Fatalf("the flag's steps = %+v, want rung 0's hand-up and the human's step", steps)
	}
	if ended := caseStepsOf(t, g.s, held.ID); len(ended) != 1 || ended[0].Outcome != StepHandedUp || ended[0].Found != "the rung did not answer within 35m" {
		t.Fatalf("rung 1's step = %+v, want it handed up as unanswered within 35m", ended)
	}
}

// monitorCursorStores are the stores whose monitor cursor must agree:
// Postgres, and the fake.
var monitorCursorStores = map[string]func(*testing.T) Store{
	"postgres": func(t *testing.T) Store { return monitorStore(t) },
	"fake":     func(*testing.T) Store { return newFakeStore() },
}

// The first read plants the cursor at the newest event and answers
// nothing: no history raises a flag. Later reads answer the monitor's
// kinds past the cursor and past after, oldest first, up to the limit.
// The cursor moves forward only.
func TestMonitorPlantsTheCursorAtTheNewestEvent(t *testing.T) {
	for name, open := range monitorCursorStores {
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
