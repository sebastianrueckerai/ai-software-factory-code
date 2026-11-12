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

// card files agent card id on issue of demo-app/demo-app on the direct
// track in status at 08:00, four hours before the rig's clock.
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

// A queued card held three hours by the count since 09:00, the filing at
// 08:00, against the 2h bound: queued_over_bound's case, which
// wait_over_bound on the same wait leaves alone.
const (
	queuedSince    = "2026-11-04T09:00:00Z"
	queuedKey      = "queued_over_bound|queued|" + queuedSince
	queuedLine     = "Monitor flag: queued_over_bound(work bound 3 of 3 for 3h)"
	monitorHandsUp = "no resolver fits"
)

// face is the comment of a monitor case's face on card id for line: the
// card goes on, and the flag is answered in the chat.
func face(id, line string) string {
	return "factoryd: task " + id + " needs a human.\n\n```\n" + line + "\n```\n\n" +
		"The card goes on meanwhile. Answer the flag in the project chat."
}

// assertQueueCase checks that card id carries the queue's flag case alone,
// at the human with rung 0's hand-up and the human's step, its facts naming
// the flag, its state and the wait, beside the digest, and that the card
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
	if len(steps) != 2 || steps[0].Rung != 0 || steps[0].Found != monitorHandsUp || steps[0].Outcome != StepHandedUp ||
		steps[1].Rung != 3 || steps[1].Outcome != StepInProgress {
		t.Fatalf("%s's case steps = %+v, want rung 0's hand-up and the human's step", id, steps)
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

// An event on a queued card past its bound opens queued_over_bound's case,
// keyed by the flag and the wait's kind and start, whose face asks the
// human beside the queue: the card stays queued, and the heal's pass leaves
// the case at the human. Another event opens and asks nothing more. Each
// pass moves the cursor to the newest event it read, the first one planted
// at the newest event, which it reads nothing past.
func TestMonitorPassOpensOneCasePerNewFlag(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.queued(t, "t1", 12)
	g.pass(t)
	if len(g.flagCases(t, "t1")) != 0 {
		t.Fatal("the first pass read history")
	}
	filed := g.filed(t, "t1", 12)
	g.pass(t)
	g.assertQueueCase(t, "t1")
	n := g.flagCases(t, "t1")[queuedKey].ID
	if want := []string{face("t1", queuedLine)}; !slices.Equal(g.forge.comments[12], want) {
		t.Fatalf("comments = %q, want %q", g.forge.comments[12], want)
	}
	if got := g.cursor(t); got != filed {
		t.Fatalf("cursor = %d, want at the card_created row %d it read", got, filed)
	}
	const ask = "The monitor flagged this card (queued_over_bound(work bound 3 of 3 for 3h)): what should happen? Answer it in the project chat."
	if asks := (actRig{s: g.s}).rows(t, EventQuestionAsked); len(asks) != 1 || asks[0].Detail["ask"] != ask || asks[0].Detail["case_id"] != float64(n) ||
		asks[0].Detail["case_kind"] != string(CaseMonitorFlag) || asks[0].Detail["flag"] != "queued_over_bound" || len(asks[0].Detail["choices"].([]any)) != 0 {
		t.Fatalf("asks = %+v, want one on case %d of kind monitor_flag and flag queued_over_bound asking %q with no choices", asks, n, ask)
	}
	if _, err := g.s.ReconcileCases(ctx, "demo-app/demo-app"); err != nil {
		t.Fatal(err)
	}
	g.assertQueueCase(t, "t1")

	again := g.event(t, "t1", 12, EventTaskStatus, map[string]any{"status": "queued"}, monitorAt.Add(-time.Minute))
	g.pass(t)
	g.assertQueueCase(t, "t1")
	if len(g.forge.comments[12]) != 1 {
		t.Fatalf("comments = %q, want the first pass's face alone", g.forge.comments[12])
	}
	if got := g.cursor(t); got != again {
		t.Fatalf("cursor = %d, want at the task_status row %d, the newest it read", got, again)
	}
}

// A card whose timeline cannot be read is logged and skipped: the next
// card's case opens and the cursor moves past both events, since the
// minute sweep reads the card again.
func TestMonitorPassGoesOnPastACardItCannotRead(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	g.card(t, "t1", 12, "running")
	g.queued(t, "t2", 13)
	g.objects.unreadable = "sessions/t1/timeline.jsonl"
	g.pass(t)
	g.filed(t, "t1", 12)
	last := g.filed(t, "t2", 13)
	logged := logs(t)
	g.pass(t)
	if cases := g.flagCases(t, "t1"); len(cases) != 0 {
		t.Fatalf("t1's flag cases = %v, want none", cases)
	}
	g.assertQueueCase(t, "t2")
	if got := g.cursor(t); got != last {
		t.Fatalf("cursor = %d, want %d past both cards' events", got, last)
	}
	if !strings.Contains(logged.String(), "monitor: card t1: read the timeline: the object store is down") {
		t.Fatalf("log = %q, want t1's failure", logged.String())
	}
}

// batchStore counts the monitor's reads, by where they start, and its
// cursor moves.
type batchStore struct {
	*PGStore
	afters, cursors []int64
}

func (s *batchStore) MonitorEvents(ctx context.Context, after int64, kinds []string, limit int) ([]Event, error) {
	s.afters = append(s.afters, after)
	return s.PGStore.MonitorEvents(ctx, after, kinds, limit)
}

func (s *batchStore) SetMonitorCursor(ctx context.Context, eventID int64) error {
	s.cursors = append(s.cursors, eventID)
	return s.PGStore.SetMonitorCursor(ctx, eventID)
}

// 101 events are two batches: the first hundred, of no card, move the
// cursor to the hundredth, and the second, the queued card's filing, opens
// its case and moves the cursor to it.
func TestMonitorPassReadsEveryBatch(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.queued(t, "t1", 12)
	g.pass(t)
	var hundredth int64
	for range 100 {
		id, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", Kind: EventTaskStatus, Actor: FactoryDecider, Via: ViaFactoryd})
		if err != nil {
			t.Fatal(err)
		}
		hundredth = id
	}
	filed := g.filed(t, "t1", 12)
	counted := &batchStore{PGStore: g.s}
	g.m.Store = counted
	g.pass(t)
	if !slices.Equal(counted.afters, []int64{0, hundredth}) || !slices.Equal(counted.cursors, []int64{hundredth, filed}) {
		t.Fatalf("reads after %v, cursor moves %v; want reads after 0 and %d, the cursor to %d then %d",
			counted.afters, counted.cursors, hundredth, hundredth, filed)
	}
	g.assertQueueCase(t, "t1")
}

// The sweep needs no event. It opens the flags of every live card of a
// project: the queued card past its bound. It wakes the case whose wake
// passed: a stop's open case on a waiting card, on which rung 0 runs again
// through OpenTrip's repeat path and hands it up to the human, with no
// wake left. A quiet run's own case at the human already takes the
// repeat's step with no new face, its card left running. A card of no
// project is not read: its step past its bound opens no flag case, and the
// sweep expires it.
func TestMonitorSweepWakesDueCasesAndExpiresSteps(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.queued(t, "t1", 12)
	g.filed(t, "t1", 12)
	g.card(t, "t2", 13, "waiting-human")
	g.filed(t, "t2", 13)
	due, _, err := g.s.OpenCase(ctx, Case{TaskID: "t2", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "stop",
		Status: CaseOpen, Problem: "the stop", Facts: map[string]any{"reason": "the stop"}})
	if err != nil {
		t.Fatal(err)
	}
	wake := monitorAt.Add(-time.Minute)
	if err := g.s.SetCaseWake(ctx, due.ID, 0, nil, &wake); err != nil {
		t.Fatal(err)
	}
	g.card(t, "t3", 14, "running")
	g.filed(t, "t3", 14)
	const quiet = "Monitor flag: wait_over_bound(run_quiet since +0:00)"
	faced, _, err := g.s.OpenCase(ctx, Case{TaskID: "t3", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag,
		StateKey: "wait_over_bound|run_quiet|2026-11-04T08:00:00Z", Status: CaseAtHuman, Problem: quiet,
		Facts: map[string]any{"reason": quiet, "flag": "wait_over_bound", "wait": "run_quiet", "state": "run_quiet|2026-11-04T08:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.s.SetCaseWake(ctx, faced.ID, 3, nil, &wake); err != nil {
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
	g.assertQueueCase(t, "t1")
	woken := caseByID(t, g.s, due.ID)
	steps := caseStepsOf(t, g.s, due.ID)
	if woken.Status != CaseAtHuman || woken.NextWakeAt != nil || woken.OwnerRung != 3 || len(steps) != 2 || steps[0].Rung != 0 ||
		steps[0].Found != monitorHandsUp || steps[0].Outcome != StepHandedUp || steps[1].Rung != 3 {
		t.Fatalf("woken case = %+v, steps %+v; want it at the human, owned by the human with no wake, rung 0's hand-up and the human's step", woken, steps)
	}
	if len(g.forge.comments[13]) != 1 || len(g.flagCases(t, "t2")) != 0 {
		t.Fatalf("t2's comments %q, flag cases %v; want the stop's one face and no flag case", g.forge.comments[13], g.flagCases(t, "t2"))
	}
	repeat := caseStepsOf(t, g.s, faced.ID)
	if task, _, _ := g.s.Task(ctx, "t3"); task.Status != "running" || caseByID(t, g.s, faced.ID).Status != CaseAtHuman ||
		caseByID(t, g.s, faced.ID).NextWakeAt != nil || len(g.forge.comments[14]) != 0 ||
		len(repeat) != 1 || repeat[0].Rung != 0 || repeat[0].Found != "the same stop again: "+quiet {
		t.Fatalf("t3 %s, steps %+v, comments %q; want it running, the case at the human with no wake, the repeat's step and no face", task.Status, repeat, g.forge.comments[14])
	}
	if cases := g.flagCases(t, caseTask); len(cases) != 0 {
		t.Fatalf("the card of no project has flag cases %v, want none", cases)
	}
	expired := caseStepsOf(t, g.s, elsewhere.ID)
	if len(expired) != 2 || expired[0].Outcome != StepHandedUp || expired[0].Found != "the rung did not answer within 35m" || expired[1].Rung != 3 {
		t.Fatalf("the steps of the case = %+v, want the step past its bound handed up as unanswered within 35m, then the human's", expired)
	}
	if c := caseByID(t, g.s, elsewhere.ID); c.Status != CaseAtHuman || len(g.forge.comments[1410]) != 1 {
		t.Fatalf("the case of the expired step is %s with comments %q, want it at the human with one face", c.Status, g.forge.comments[1410])
	}
}

// An ended card with a live case raises issue_open_after_end. Where Finish
// closes the issue, rung 0 closes it through Act and signals no run:
// closed card t1's open issue closes with factoryd's comment, and done card
// t2's closed issue is left alone. Where Finish leaves it open, the monitor
// opens no case: failed t3, whose issue waits for its retry, and done t4
// after its merge. Another event on t1 opens no second case: the flag's
// case for that state ended, and one is all.
func TestMonitorRungZeroClosesTheIssueOfAnEndedCard(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		id     string
		issue  int64
		status string
	}{{"t1", 12, "closed"}, {"t2", 13, "done"}, {"t3", 14, "failed"}, {"t4", 15, "done"}} {
		g.card(t, c.id, c.issue, c.status)
		if _, _, err := g.s.OpenCase(ctx, Case{TaskID: c.id, Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap",
			Status: CaseAtHuman, Problem: "Card used its sessions"}); err != nil {
			t.Fatal(err)
		}
	}
	g.forge.closed[13] = true
	g.pass(t)
	g.event(t, "t4", 15, EventTaskStatus, map[string]any{"status": "merged"}, monitorAt.Add(-time.Hour))
	for _, c := range []struct {
		id    string
		issue int64
		state string
	}{{"t1", 12, "closed"}, {"t2", 13, "done"}, {"t3", 14, "failed"}, {"t4", 15, "done"}} {
		g.event(t, c.id, c.issue, EventTaskEnded, map[string]any{"state": c.state}, monitorAt.Add(-time.Minute))
	}
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
	for id, issue := range map[string]int64{"t3": 14, "t4": 15} {
		if cases := g.flagCases(t, id); len(cases) != 0 || len(g.forge.comments[issue]) != 0 {
			t.Fatalf("%s's flag cases = %v, comments on #%d %q; want none", id, cases, issue, g.forge.comments[issue])
		}
	}
	if !slices.Equal(g.forge.closes, []int64{12}) || !slices.Equal(g.forge.comments[12], []string{"factoryd: this card ended closed, so its issue closes."}) ||
		len(g.forge.comments[13]) != 0 {
		t.Fatalf("closes %v, comments %v; want #12 alone closed with factoryd's comment and #13 untouched", g.forge.closes, g.forge.comments)
	}
	if len(g.signals.got) != 0 {
		t.Fatalf("signals %+v, want none", g.signals.got)
	}

	g.event(t, "t1", 12, EventTaskStatus, map[string]any{"status": "closed"}, monitorAt)
	g.pass(t)
	if cases := g.flagCases(t, "t1"); len(cases) != 1 || len(g.forge.closes) != 1 {
		t.Fatalf("t1's flag cases = %v, closes %v; want the one case and the one close", cases, g.forge.closes)
	}
}

// An ended card's issue that rung 0 failed to close: its
// issue_open_after_end case sits at the human, and its ask stays open on
// the ended card, until the issue's close is a row on the card. The next
// pass withdraws the case as the issue is closed, by a row that names it,
// so the ask reads withdrawn, and opens none again.
func TestMonitorWithdrawsTheIssueCaseOnceTheIssueCloses(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.card(t, "t1", 13, "done")
	if _, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap",
		Status: CaseAtHuman, Problem: "Card used its sessions"}); err != nil {
		t.Fatal(err)
	}
	g.forge.failClose = true
	g.pass(t)
	g.event(t, "t1", 13, EventTaskEnded, map[string]any{"state": "done"}, monitorAt.Add(-time.Minute))
	g.pass(t)
	c, ok := g.flagCases(t, "t1")["issue_open_after_end|"]
	asks := (actRig{s: g.s}).rows(t, EventQuestionAsked)
	if !ok || c.Status != CaseAtHuman || len(asks) != 1 || asks[0].Detail["case_id"] != float64(c.ID) || asks[0].Detail["flag"] != "issue_open_after_end" {
		t.Fatalf("t1's flag case = %+v (found %v), asks %+v; want issue_open_after_end at the human with its ask", c, ok, asks)
	}
	settled := func() PokeSettle {
		t.Helper()
		events, err := g.s.TaskEvents(ctx, "t1", 100)
		if err != nil {
			t.Fatal(err)
		}
		return SettleOf(ChatMessage{Kind: MessageEscalation, BirthKind: EventQuestionAsked, At: asks[0].At}, events, "done", false)
	}
	if s := settled(); !s.Open {
		t.Fatalf("the ask on the ended card settles as %+v, want it open", s)
	}
	// the pass reads the face's own row, so only the close is left to read
	g.pass(t)
	if got := caseByID(t, g.s, c.ID); got.Status != CaseAtHuman {
		t.Fatalf("the case = %s before the issue closed, want it at the human", got.Status)
	}
	g.event(t, "t1", 13, EventIssueClosed, map[string]any{"issue": 13}, monitorAt)
	g.pass(t)
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseWithdrawn || !reflect.DeepEqual(got.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWithdraw, Reason: "the issue is closed"}) {
		t.Fatalf("the case = %s, %+v; want withdrawn by factoryd as the issue is closed", got.Status, got.Resolution)
	}
	if rows := (actRig{s: g.s}).rows(t, EventQuestionWithdrawn); len(rows) != 1 || rows[0].Detail["case_id"] != float64(c.ID) || rows[0].Detail["reason"] != "the issue is closed" {
		t.Fatalf("withdrawal rows = %+v, want one naming case %d as the issue is closed", rows, c.ID)
	}
	if s := settled(); s.Word != "withdrawn" {
		t.Fatalf("the ask settles as %+v, want withdrawn", s)
	}
	if cases := g.flagCases(t, "t1"); len(cases) != 1 {
		t.Fatalf("t1's flag cases = %v, want the one withdrawn", cases)
	}
}

// A human's close of an ended card keeps the human's note: on closed t1's
// open issue as the close's comment, and on done t2's closed issue as a
// comment of its own. No run is signalled.
func TestActKeepsTheNoteOfAHumansCloseOfAnEndedCard(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.card(t, "t1", 12, "closed")
	g.card(t, "t2", 13, "done")
	g.forge.closed[13] = true
	note := "factoryd: sebastian answered with close.\n\n```\nnot worth it\n```"
	for id, result := range map[string]string{"t1": "Closed #12.", "t2": "#13 was closed already."} {
		c, _, err := g.s.OpenCase(ctx, Case{TaskID: id, Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap", Status: CaseAtHuman, Problem: "p"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := g.m.Runner.Act(ctx, ActInput{TaskID: id, CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose, Note: "not worth it"}})
		if err != nil || !reflect.DeepEqual(out.Did, []CaseAction{{Action: ActClose, Result: result}}) {
			t.Fatalf("%s: Act = %+v, %v; want the close: %q", id, out.Did, err, result)
		}
	}
	if !slices.Equal(g.forge.closes, []int64{12}) || !slices.Equal(g.forge.comments[12], []string{note}) || !slices.Equal(g.forge.comments[13], []string{note}) ||
		len(g.signals.got) != 0 {
		t.Fatalf("closes %v, comments %v, signals %+v; want #12 closed, the note on #12 and #13, no signal", g.forge.closes, g.forge.comments, g.signals.got)
	}
}

// A rung's step past its due on a live case raises wait_over_bound on
// rung_answer, from the step's start at 11:20, the filing at 08:00. Rung
// 0 ends that card's steps past their bound, as ExpireSteps ends one, and
// with them the flag's case through Act, with no face for the flag: the
// step's hand-up is the record, and the case whose rung did not answer
// climbs to the human. The next pass opens nothing. Another card's step
// past its bound is left to its own pass.
func TestMonitorRungZeroEndsAStepPastItsBound(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	overdue := func(id string) (Case, int64) {
		t.Helper()
		held, _, err := g.s.OpenCase(ctx, Case{TaskID: id, Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "p"})
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
		return held, step
	}
	g.card(t, "t1", 12, "waiting-human")
	g.card(t, "t2", 13, "waiting-human")
	held, _ := overdue("t1")
	other, _ := overdue("t2")
	g.pass(t)
	g.filed(t, "t1", 12)
	g.pass(t)

	const ended = "ended the overdue step: rung 1 did not answer within 35m"
	cases := g.flagCases(t, "t1")
	c, ok := cases["wait_over_bound|rung_answer|2026-11-04T11:20:00Z"]
	if len(cases) != 1 || !ok || c.Problem != "Monitor flag: wait_over_bound(rung_answer since +3:20)" ||
		c.Facts["wait"] != "rung_answer" || c.Facts["owner_rung"] != 1.0 || c.Status != CaseWithdrawn ||
		!reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWithdraw, Reason: ended}) {
		t.Fatalf("t1's flag cases = %v, want wait_over_bound on rung 1's answer since 11:20 ended by factoryd on rung 0", cases)
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) != 1 || steps[0].Rung != 0 || steps[0].Decision != ActWithdraw ||
		steps[0].Grounds != ended || steps[0].Outcome != StepWithdrawn {
		t.Fatalf("the flag's steps = %+v, want rung 0's one step naming the step it ended", steps)
	}
	if asks := (actRig{s: g.s}).rows(t, EventQuestionAsked); len(g.forge.comments[12]) != 1 || len(asks) != 1 || asks[0].Detail["case_id"] != float64(held.ID) {
		t.Fatalf("comments %q, asks %+v; want the one face of the case whose rung did not answer, and no face for the flag", g.forge.comments[12], asks)
	}
	if ended := caseStepsOf(t, g.s, held.ID); len(ended) != 2 || ended[0].Outcome != StepHandedUp || ended[0].Found != "the rung did not answer within 35m" || ended[1].Rung != 3 {
		t.Fatalf("t1's steps = %+v, want rung 1's step handed up as unanswered within 35m, then the human's", ended)
	}
	if left := caseStepsOf(t, g.s, other.ID); len(left) != 1 || left[0].Outcome != StepInProgress {
		t.Fatalf("t2's rung 1 step = %+v, want it in progress", left)
	}
	g.event(t, "t1", 12, EventMergeDenied, nil, monitorAt)
	g.pass(t)
	if cases := g.flagCases(t, "t1"); len(cases) != 1 {
		t.Fatalf("t1's flag cases after another pass = %v, want the one", cases)
	}
}

// A monitor case's answer signals no run: the run waits on none, and a
// signal would start a queued card's. The human's close of queued t1's
// monitor case is refused, since close ends the card: the case stays at
// the human, the card queued, no issue closes. The settle of the same
// case ends it alone, with its one step, and no signal goes.
func TestActSignalsNoRunForAMonitorCase(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.queued(t, "t1", 12)
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag, StateKey: queuedKey,
		Status: CaseAtHuman, Problem: queuedLine})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.m.Runner.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose, Note: "settled"}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.Contains(string(refused), "settle") || !strings.Contains(string(refused), "close ends the card") {
		t.Fatalf("close of a monitor case = %v, want the refusal that names settle and says close ends the card", err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseAtHuman {
		t.Fatalf("the case after the refused close = %s, want it at the human", got.Status)
	}
	if task, _, _ := g.s.Task(ctx, "t1"); task.Status != "queued" || len(g.forge.closes) != 0 || len(g.signals.got) != 0 {
		t.Fatalf("t1 %s, closes %v, signals %+v; want it queued, nothing closed and no signal", task.Status, g.forge.closes, g.signals.got)
	}
	if _, err := g.m.Runner.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActSettle, Note: "the flag's concern is settled"}}); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Action != ActSettle {
		t.Fatalf("the case after the settle = %s by %v, want it resolved with settle", got.Status, got.Resolution)
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) != 1 || steps[0].Rung != 3 || steps[0].Decision != ActSettle ||
		steps[0].Grounds != "the flag's concern is settled" || steps[0].Outcome != StepResolved {
		t.Fatalf("the case's steps = %+v, want the human's one settle step", steps)
	}
	if task, _, _ := g.s.Task(ctx, "t1"); task.Status != "queued" || len(g.forge.closes) != 0 || len(g.signals.got) != 0 {
		t.Fatalf("t1 %s, closes %v, signals %+v; want it queued still, nothing closed and no signal", task.Status, g.forge.closes, g.signals.got)
	}
}

// A running card quiet since its filing at 08:00, with no line and no model
// call, raises wait_over_bound on run_quiet: one case, faced beside the
// run, which stays running. The face writes no line on the card's
// timeline, so the quiet keeps its start, and another event opens nothing.
func TestMonitorFacesAQuietRunBesideIt(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.card(t, "t1", 12, "running")
	g.pass(t)
	g.filed(t, "t1", 12)
	g.pass(t)
	const line = "Monitor flag: wait_over_bound(run_quiet since +0:00)"
	cases := g.flagCases(t, "t1")
	c, ok := cases["wait_over_bound|run_quiet|2026-11-04T08:00:00Z"]
	if len(cases) != 1 || !ok || c.Status != CaseAtHuman || c.Problem != line {
		t.Fatalf("t1's flag cases = %v, want wait_over_bound on run_quiet since 08:00 at the human", cases)
	}
	if task, _, _ := g.s.Task(ctx, "t1"); task.Status != "running" || !slices.Equal(g.forge.comments[12], []string{face("t1", line)}) {
		t.Fatalf("t1 %s, comments %q; want it running with the case's one face", task.Status, g.forge.comments[12])
	}
	if tl, _ := g.objects.Get(ctx, "sessions/t1/timeline.jsonl"); len(tl) != 0 {
		t.Fatalf("t1's timeline = %q, want no line", tl)
	}
	g.event(t, "t1", 12, EventMergeDenied, nil, monitorAt)
	g.pass(t)
	if cases := g.flagCases(t, "t1"); len(cases) != 1 || len(g.forge.comments[12]) != 1 {
		t.Fatalf("t1's flag cases = %v, comments %q; want the one case and its one face", cases, g.forge.comments[12])
	}
}

// A case whose flag no longer holds is withdrawn on the card's next pass,
// by a question_withdrawn row that names it when it is at the human, so
// its ask in the inbox settles as withdrawn, and with no row when it is
// open, since no face asked anyone. The run's own case at the human
// stays. Two checkin_cut cases on a card with no check-in go as the flag
// no longer holds, while the queue's case opens. Once the card is admitted, the
// queue's case goes as the wait ended, and nothing opens.
func TestMonitorWithdrawsACaseWhoseFlagNoLongerHolds(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.queued(t, "t1", 12)
	run, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseAgentEscalate, StateKey: "run",
		Status: CaseAtHuman, Problem: "Which file?"})
	if err != nil {
		t.Fatal(err)
	}
	g.pass(t)
	const cut = "Monitor flag: checkin_cut(+0:20)"
	stale, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag, StateKey: "checkin_cut|+0:20",
		Status: CaseAtHuman, Problem: cut, Facts: map[string]any{"reason": cut, "flag": "checkin_cut", "state": "+0:20"}})
	if err != nil {
		t.Fatal(err)
	}
	const unasked = "Monitor flag: checkin_cut(+0:30)"
	quiet, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag, StateKey: "checkin_cut|+0:30",
		Status: CaseOpen, Problem: unasked, Facts: map[string]any{"reason": unasked, "flag": "checkin_cut", "state": "+0:30"}})
	if err != nil {
		t.Fatal(err)
	}
	g.filed(t, "t1", 12)
	g.pass(t)
	withdrawn := func(c Case, reason string) {
		t.Helper()
		if c.Status != CaseWithdrawn || !reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWithdraw, Reason: reason}) {
			t.Fatalf("case %s = %s, %+v; want withdrawn by factoryd as %q", c.StateKey, c.Status, c.Resolution, reason)
		}
	}
	withdrawn(caseByID(t, g.s, stale.ID), "the flag no longer holds")
	withdrawn(caseByID(t, g.s, quiet.ID), "the flag no longer holds")
	queue, ok := g.flagCases(t, "t1")[queuedKey]
	if !ok || queue.Status != CaseAtHuman {
		t.Fatalf("t1's queue case = %+v (found %v), want it at the human", queue, ok)
	}
	if err := g.s.SetAdmitted(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	g.event(t, "t1", 12, EventMergeDenied, nil, monitorAt)
	g.pass(t)
	withdrawn(caseByID(t, g.s, queue.ID), "the wait ended")
	if cases := g.flagCases(t, "t1"); len(cases) != 3 || len(g.forge.comments[12]) != 1 {
		t.Fatalf("t1's flag cases = %v, comments %q; want the three withdrawn and the queue's one face", cases, g.forge.comments[12])
	}
	rows := (actRig{s: g.s}).rows(t, EventQuestionWithdrawn)
	if len(rows) != 2 || rows[0].Detail["case_id"] != float64(stale.ID) || rows[0].Detail["reason"] != "the flag no longer holds" ||
		rows[1].Detail["case_id"] != float64(queue.ID) || rows[1].Detail["reason"] != "the wait ended" {
		t.Fatalf("withdrawal rows = %+v, want one naming case %d as the flag no longer holds, then case %d as the wait ended", rows, stale.ID, queue.ID)
	}
	if got := caseByID(t, g.s, run.ID); got.Status != CaseAtHuman {
		t.Fatalf("the run's own case = %s, want it at the human", got.Status)
	}
	asks := (actRig{s: g.s}).rows(t, EventQuestionAsked)
	events, err := g.s.TaskEvents(ctx, "t1", 100)
	if err != nil || len(asks) != 1 {
		t.Fatalf("asks %+v, %v; want the queue's one", asks, err)
	}
	if settled := SettleOf(ChatMessage{Kind: MessageEscalation, BirthKind: EventQuestionAsked, At: asks[0].At}, events, "queued", false); settled.Word != "withdrawn" {
		t.Fatalf("the queue's ask settles as %+v, want withdrawn", settled)
	}
}

// A case whose face failed stays open and is not the flag's case yet: the
// next pass finishes it through OpenTrip, the same case at the human with
// its one face and rung 0's one step.
func TestMonitorFacesACaseWhoseFaceFailed(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	g.queued(t, "t1", 12)
	g.forge.failComments = 1
	g.pass(t)
	g.filed(t, "t1", 12)
	g.pass(t)
	cases := g.flagCases(t, "t1")
	if c, ok := cases[queuedKey]; len(cases) != 1 || !ok || c.Status != CaseOpen || len(g.forge.comments[12]) != 0 {
		t.Fatalf("t1's flag cases = %v, comments %q; want the queue's case open with no face", cases, g.forge.comments[12])
	}
	g.event(t, "t1", 12, EventMergeDenied, nil, monitorAt)
	g.pass(t)
	g.assertQueueCase(t, "t1")
	if !slices.Equal(g.forge.comments[12], []string{face("t1", queuedLine)}) {
		t.Fatalf("comments = %q, want the case's one face", g.forge.comments[12])
	}
}

// A trip that names its case is the case's, whatever its reason: one named
// a monitor flag that carries the restart's reason opens that case, not
// the healed path's.
func TestOpenTripWithAKindTakesNoHealedPath(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	g.card(t, "t1", 12, "waiting-human")
	out, err := g.m.Runner.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: healedReason, Kind: CaseMonitorFlag, StateKey: "checkin_cut|+0:20"}, tripAttempt{})
	if err != nil {
		t.Fatal(err)
	}
	cases, err := g.s.CasesOf(ctx, "t1")
	if err != nil || len(cases) != 1 || cases[0].ID != out.CaseID || cases[0].Kind != CaseMonitorFlag || cases[0].StateKey != "checkin_cut|+0:20" {
		t.Fatalf("t1's cases = %+v, %v; want the named monitor case alone, case %d", cases, err, out.CaseID)
	}
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

// monitorStores are the stores whose monitor reads must agree: Postgres,
// and the fake.
var monitorStores = map[string]func(*testing.T) Store{
	"postgres": func(t *testing.T) Store { return monitorStore(t) },
	"fake":     func(*testing.T) Store { return newFakeStore() },
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

// A plan's child row whose issue never linked reads the queue reason "no
// issue" and holds no slot. The flag fires on a wait longer than its bound,
// so at 2h01m the monitor opens one queued_over_bound case, keyed by the
// wait's start, also after a second pass. Rung 2 is offered close on it, and
// the human's close of it tells the card's run to end, which ends the row. A queued card
// with an issue keeps close refused on its flag case.
func TestAPlanRowWithNoIssueAgesIntoOneQueuedOverBoundCaseThatRung2Closes(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	since := monitorAt.Add(-2*time.Hour - time.Minute)
	g.card(t, "t1", 0, "queued")
	g.card(t, "t2", 13, "queued")
	for id, reason := range map[string]string{"t1": "no issue", "t2": "work bound 3 of 3"} {
		if err := g.s.SetQueueReason(ctx, id, reason, since); err != nil {
			t.Fatal(err)
		}
	}
	g.pass(t)
	g.filed(t, "t1", 0)
	g.filed(t, "t2", 13)
	g.pass(t)
	g.pass(t)
	key := "queued_over_bound|queued|" + since.Format(time.RFC3339)
	offered := func(id, line string) ([]string, Case) {
		cases := g.flagCases(t, id)
		c, ok := cases[key]
		if len(cases) != 1 || !ok || c.Problem != "Monitor flag: queued_over_bound("+line+" for 2h1m)" {
			t.Fatalf("%s's flag cases = %v (%+v), want %s alone with the problem %q", id, slices.Sorted(maps.Keys(cases)), c, key, line)
		}
		task, _, err := g.s.Task(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		f, err := g.m.Runner.factsAtCase(ctx, task, c)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, a := range Offered(f, 2) {
			names = append(names, a.Name)
		}
		return names, c
	}
	names, c := offered("t1", "no issue")
	if !slices.Contains(names, ActClose) {
		t.Fatalf("rung 2 on the row with no issue is offered %v, want close", names)
	}
	if names, _ := offered("t2", "work bound 3 of 3"); slices.Contains(names, ActClose) {
		t.Errorf("rung 2 on a queued card with an issue is offered %v, want no close", names)
	}
	res, err := g.m.Runner.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose}})
	if err != nil || len(res.Did) != 1 || res.Did[0].Result != "Card t1 was told to close." {
		t.Fatalf("the close = %+v, %v; want t1 told to close", res.Did, err)
	}
	if got := g.signals.got; len(got) != 1 || got[0].taskID != "t1" || got[0].signal != SignalIssueClosed {
		t.Fatalf("signals = %+v, want the close sent to t1's run", got)
	}
}

// A card at waiting-deps for 50h behind a plan at waiting-human, whose
// confirmation case has been at the human for 51h, passes the 48h dependency
// bound: three passes open one case, and its face names the plan, its ask and
// since when. The plan's own wait opens no case of the card's.
func TestACardWaitingOnAPlanAtTheHumanOpensOneCaseNamingThePlansAsk(t *testing.T) {
	g := newMonitorRig(t)
	g.rungsOff()
	ctx := context.Background()
	g.card(t, "p1", 31, "waiting-human")
	g.card(t, "t1", 32, "waiting-deps")
	if err := g.s.AddTaskDeps(ctx, "t1", []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET track = 'plan' WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	asked := monitorAt.Add(-51 * time.Hour)
	if _, _, err := g.s.OpenCase(ctx, Case{TaskID: "p1", Repository: "demo-app/demo-app", Kind: CasePlanConfirmation, StateKey: "confirm:1",
		Status: CaseAtHuman, Problem: "Confirm the plan?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE cases SET opened_at = $1 WHERE task_id = 'p1'`, asked); err != nil {
		t.Fatal(err)
	}
	since := monitorAt.Add(-50 * time.Hour)
	g.pass(t)
	g.event(t, "t1", 32, EventCardCreated, map[string]any{"budget_eur": 3.0}, since)
	g.event(t, "t1", 32, EventTaskStatus, map[string]any{"status": "waiting-deps"}, since)
	g.pass(t)
	g.pass(t)
	g.pass(t)
	var flags []Case
	for _, c := range g.flagCases(t, "t1") {
		flags = append(flags, c)
	}
	if len(flags) != 1 || flags[0].Facts["wait"] != "dependency" || flags[0].Facts["flag"] != "wait_over_bound" {
		t.Fatalf("t1's flag cases = %+v, want one wait_over_bound on the dependency wait", flags)
	}
	want := `The card waits on plan #31, which waits on a human to answer "Confirm the plan?" since ` + asked.Format("2006-01-02 15:04") + " UTC."
	if why := CaseFace(flags[0], nil, FactsOf(flags[0]), CaseAsk{}, nil).Why; !strings.HasSuffix(why, want) {
		t.Fatalf("the face's why = %q, want it to end with %q", why, want)
	}
	if got := g.flagCases(t, "p1"); len(got) != 0 {
		t.Errorf("p1's flag cases = %v, want none", slices.Sorted(maps.Keys(got)))
	}
}
