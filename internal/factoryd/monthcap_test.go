package factoryd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/mocks"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const gatewayRefusal = "the month cap budget.month_eur is reached: spent 49.98 EUR of 50.00 EUR"

const proxyRefusal = "litellm proxy budget exceeded (litellm_settings.max_budget, the proxy-wide monthly stop behind budget.month_eur): Budget has been exceeded! Current cost: 55.0123, Max budget: 55.0"

// refusedAt is when the tests' gateway refusal was made.
var refusedAt = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// monthPolicy is a policy.yaml whose month_eur is on line 7.
const monthPolicy = "# yaml-language-server: $schema=schema/policy.schema.json\nversion: 1\n\nbudget:\n  task_default_eur: 2\n  day_eur: 20\n  month_eur: 50\n"

func monthAlarm(t *testing.T, store *fakeStore, monthEUR float64) (*MonthAlarm, *alertForgejo) {
	forgejo, fj := infraForgejo(t, map[string][]byte{"policy.yaml": []byte(monthPolicy)})
	repo := Repo{Owner: "sf", Name: "ai-software-factory-infra"}
	return &MonthAlarm{Store: store, Forgejo: forgejo, Repo: repo, Dir: "factory",
		Config: &ForgejoConfig{Forgejo: forgejo, Token: "cfg-tok", Repo: repo, Branch: "main", Dir: "factory"},
		Current: func() policy.Version {
			return policy.Version{ID: 4, Commit: "c9", Tree: policy.Tree{Document: policy.Document{Budget: policy.Budget{MonthEUR: monthEUR}}}}
		}}, fj
}

func monthAlerts(store *fakeStore) []string {
	var out []string
	for _, a := range store.alertRows() {
		if a.Kind == AlertMonthCap {
			out = append(out, a.Fingerprint)
		}
	}
	return out
}

// Hand-derived against a cap of 50 EUR: 25 is 50 percent, 40 is 80, and
// the cap is reached at 50 spent or at the gateway's first month cap
// refusal. Each alert is raised once per month; the cap's also files one
// human-lane card linking the month_eur line (line 7).
func TestMonthAlarm(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	step := func(spend MonthSpend, want ...string) {
		t.Helper()
		store.monthSpend = spend
		if err := alarm.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if got := monthAlerts(store); !slices.Equal(got, want) {
			t.Fatalf("after %+v: month alerts %v, want %v", spend, got, want)
		}
	}
	step(MonthSpend{Month: "2026-09", EUR: 24.99})
	step(MonthSpend{Month: "2026-09", EUR: 25}, "2026-09/50")
	step(MonthSpend{Month: "2026-09", EUR: 39.99}, "2026-09/50")
	step(MonthSpend{Month: "2026-09", EUR: 40}, "2026-09/50", "2026-09/80")
	step(MonthSpend{Month: "2026-09", EUR: 49.9}, "2026-09/50", "2026-09/80")
	if len(fj.issues) != 0 {
		t.Fatalf("issues before the cap: %q", fj.issues)
	}
	// a failed filing is filed at the next check, once
	fj.failNext = 1
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 49.98, Refusal: gatewayRefusal, RefusedAt: refusedAt}
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed filing raised nothing")
	}
	step(MonthSpend{Month: "2026-09", EUR: 49.98, Refusal: gatewayRefusal, RefusedAt: refusedAt}, "2026-09/50", "2026-09/80", "2026-09/100/50")
	step(MonthSpend{Month: "2026-09", EUR: 49.99, Refusal: gatewayRefusal, RefusedAt: refusedAt}, "2026-09/50", "2026-09/80", "2026-09/100/50")
	if len(fj.issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(fj.issues))
	}
	for _, want := range []string{"This month's spending reached the factory's limit of 50.00 EUR\n\n",
		"49.98 EUR of the cap of 50.00 EUR",
		"https://git.example/sf/ai-software-factory-infra/src/commit/c9/factory/policy.yaml#L7",
		"The gateway refuses every further call until the month ends, a human resets the month's count in the console's status panel, or the cap is raised by a commit.", "The newest refusal:\n\n```\n" + gatewayRefusal + "\n```"} {
		if !strings.Contains(fj.issues[0], want) {
			t.Errorf("issue lacks %q:\n%s", want, fj.issues[0])
		}
	}
	if got := fj.labels[41]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	var task Task
	for _, task = range store.tasks {
	}
	if len(store.tasks) != 1 || task.Kind != "human" || task.Status != "waiting-human" || task.Source != SourceMonthCap ||
		task.Repository != "sf/ai-software-factory-infra" || task.IssueNumber != 41 {
		t.Fatalf("tasks = %+v", store.tasks)
	}
	for _, a := range store.alertRows() {
		switch {
		case a.Kind != AlertMonthCap:
		case a.Fingerprint == "2026-09/100/50" && (a.TaskID != task.ID || a.Detail["issue"] != int64(41)):
			t.Errorf("the cap's alert = %+v, want it on task %s", a, task.ID)
		case a.Verdict != VerdictReal || a.ResolvedBy != "factoryd" || a.Detail["month"] != "2026-09" || a.Detail["cap_eur"] != 50.0:
			t.Errorf("alert = %+v", a)
		}
	}
	// a new month starts over; a jump past every threshold raises each
	step(MonthSpend{Month: "2026-10", EUR: 50}, "2026-09/50", "2026-09/80", "2026-09/100/50", "2026-10/50", "2026-10/80", "2026-10/100/50")
	if len(fj.issues) != 2 || !strings.Contains(fj.issues[1], "spend in 2026-10 (UTC") {
		t.Fatalf("issues = %q, want the October card", fj.issues)
	}
}

// failingTasks fails the next task row creation, as a database error
// right after the issue was filed.
type failingTasks struct {
	*fakeStore
	failNext int
}

func (f *failingTasks) CreateHumanCard(ctx context.Context, t Task, ask HumanAsk) (Task, bool, error) {
	if f.failNext > 0 {
		f.failNext--
		return Task{}, false, errors.New("database gone")
	}
	return f.fakeStore.CreateHumanCard(ctx, t, ask)
}

func (f *failingTasks) CreateTask(ctx context.Context, t Task) error {
	if f.failNext > 0 {
		f.failNext--
		return errors.New("database gone")
	}
	return f.fakeStore.CreateTask(ctx, t)
}

// The cap's issue is filed once even when the task row after it fails: the
// issue stays open, and the next check finds it by its title and takes it on.
func TestMonthAlarmFilesTheCapIssueOnce(t *testing.T) {
	ctx := context.Background()
	captureLog(t)
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	book := &failingTasks{fakeStore: store, failNext: 1}
	alarm.Store = book
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 50.2}
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed task row raised nothing")
	}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 || len(store.tasks) != 1 {
		t.Fatalf("issues %d, tasks %d, want 1 and 1", len(fj.issues), len(store.tasks))
	}
	var task Task
	for _, task = range store.tasks {
	}
	a, ok, _ := store.AlertByFingerprint(ctx, AlertMonthCap, "2026-09/100/50")
	if !ok || task.IssueNumber != 41 || task.Source != SourceMonthCap || task.Status != "waiting-human" || a.TaskID != task.ID || a.Detail["issue"] != int64(41) {
		t.Fatalf("task %+v, alert %+v", task, a)
	}
	if got := fj.labels[41]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
}

// A month cap of 0 stops all spend by design: nothing to alert on.
func TestMonthAlarmZeroCap(t *testing.T) {
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 0)
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 3, Refusal: gatewayRefusal, RefusedAt: refusedAt}
	if err := alarm.Check(context.Background()); err != nil || len(store.alertRows()) != 0 || len(fj.issues) != 0 {
		t.Fatalf("check under a cap of 0 = %v, alerts %+v, issues %q", err, store.alertRows(), fj.issues)
	}
}

// A human card filed from a question anchors the ask by its intake row
// and its question_asked row, filed fresh or adopted from the open issue
// a failed task row left behind.
func TestFileHumanTaskRecordsTheAsk(t *testing.T) {
	ctx := context.Background()
	q := HumanQuestion{Ask: "Raise the cap?", Why: "the cap is reached", Info: "a raise is a commit",
		Choices: []QuestionChoice{{Value: "raise", Label: "Raise it"}, {Value: AnswerPark, Label: "Park"}}}
	for _, adopt := range []bool{false, true} {
		store, rows := newFakeStore(), &fakeEventRecorder{}
		alarm, fj := monthAlarm(t, store, 50)
		book := &failingTasks{fakeStore: store}
		if adopt {
			captureLog(t)
			book.failNext = 1
			if _, _, err := fileHumanTask(ctx, book, alarm.Forgejo, nil, alarm.Repo, q, nil, Task{ID: "t-0", Kind: "human", Source: SourceMonthCap}, ""); err == nil {
				t.Fatal("a failed task row raised nothing")
			}
		}
		tl := &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: teeEvents{rows, store}}
		task, number, err := fileHumanTask(ctx, book, alarm.Forgejo, tl, alarm.Repo, q, nil, Task{ID: "t-1", Kind: "human", Source: SourceMonthCap}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(fj.issues) != 1 || !strings.HasPrefix(fj.issues[0], "Raise the cap?\n\nWhy: the cap is reached\n\n---\n\n```\nRaise the cap?\n```\n\nInfo: a raise is a commit\n\nChoices:\n- Raise it\n- Park\n\nThe factory asks. ") {
			t.Fatalf("adopt=%v issues = %q", adopt, fj.issues)
		}
		events := store.askedRows(task.ID)
		if len(events) != 1 || events[0].Question == nil ||
			!reflect.DeepEqual(*events[0].Question, q) || !slices.Equal(events[0].Choices, []string{"Raise it", "Park"}) {
			t.Fatalf("adopt=%v asks = %+v", adopt, events)
		}
		var asked []Event
		for _, e := range store.events {
			if e.Kind == EventQuestionAsked {
				asked = append(asked, e)
			}
		}
		if len(asked) != 1 || asked[0].Via != ViaFactoryd || asked[0].Detail["ask"] != "Raise the cap?" || asked[0].Detail["by"] != SourceMonthCap ||
			asked[0].Detail["source"] != SourceMonthCap || number != 41 {
			t.Fatalf("adopt=%v question_asked rows = %+v, issue %d", adopt, asked, number)
		}
	}
}

// capWaitStore is a store of cards by id with their cases: a month wait at
// the human (m1), a day wait at the human (d1), an agent's ask at the human
// (a1), a month wait rung 2 holds on a running card (r1), a month wait a
// human answered before the agent asked (l1), a month wait withdrawn when
// it ended on its own (w1), a card with two live cap waits, the
// project's rung 0 holds and the month's at the human (b1), and a done
// card whose month wait is still live (x1).
func capWaitStore(t *testing.T) *fakeStore {
	t.Helper()
	store, ctx := newFakeStore(), context.Background()
	month := CapWaitReason(policy.RefusedMonthCap, capGateway(policy.RefusedMonthCap), "spent", novFirst)
	day := CapWaitReason(policy.RefusedDayCap, capGateway(policy.RefusedDayCap), "spent", oct(15, 0, 0))
	for id, status := range map[string]string{"m1": "waiting-human", "d1": "waiting-human", "a1": "waiting-human", "r1": "running",
		"l1": "waiting-human", "w1": "running", "b1": "waiting-human", "x1": "done"} {
		if err := store.CreateTask(ctx, Task{ID: id, Status: status, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
	}
	open := func(id string, kind CaseKind) int64 {
		c, opened, err := store.OpenCase(ctx, Case{TaskID: id, Kind: kind, StateKey: "k", Problem: "p"})
		if err != nil || !opened {
			t.Fatalf("OpenCase %s %s = %v, %v", id, kind, opened, err)
		}
		return c.ID
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	store.face("m1", 1, CaseMonthCap, 101, month, oct31(21, 0, 0))
	store.face("d1", 2, CaseDayCap, 102, day, oct(14, 20, 43))
	store.face("a1", 3, CaseAgentEscalate, 103, "The agent asks: which one?", oct31(21, 0, 0))
	due := oct31(21, 35, 0)
	_, err := store.StartRungStep(ctx, CaseStep{CaseID: open("r1", CaseMonthCap), Rung: 2, Actor: FactoryDecider, DueAt: &due})
	must(err)
	store.face("l1", 5, CaseMonthCap, 105, month, oct31(21, 0, 0))
	must(store.ResolveCase(ctx, 5, CaseResolution{By: "sebastian", Rung: 3, Action: ActContinue}, nil, nil, nil))
	store.face("l1", 6, CaseAgentEscalate, 106, "The agent asks: which one?", oct31(22, 0, 0))
	must(store.WithdrawCase(ctx, open("w1", CaseMonthCap), "the wait ended on its own: the cap no longer binds a session"))
	open("b1", CaseProjectCap)
	store.face("b1", 9, CaseMonthCap, 109, month, oct31(21, 0, 0))
	store.face("x1", 10, CaseMonthCap, 110, month, oct31(21, 0, 0))
	return store
}

// A wake signals every card a live cap wait holds, whatever rung holds
// it, oldest case first and each card once: the month's (m1) and the
// day's (d1) at the human, the month's that rung 2 holds (r1), and the
// card with two (b1). It signals no agent's ask (a1), no answered wait
// (l1), no withdrawn one (w1) and no ended card (x1), and its log line
// says what it ran for. A signal that fails leaves the others signalled and Wake failing.
func TestCapWaitsWake(t *testing.T) {
	var sent []string
	var broken string
	w := &CapWaits{Store: capWaitStore(t), Signal: func(_ context.Context, id string) error {
		if id == broken {
			return errors.New("frontend down")
		}
		sent = append(sent, id)
		return nil
	}}
	lines := logs(t)
	if err := w.Wake(context.Background(), "console: alice reset the month cap's count (reset 4)"); err != nil || !slices.Equal(sent, []string{"m1", "d1", "r1", "b1"}) {
		t.Fatalf("Wake = %v, signalled %v; want m1, d1, r1 and b1", err, sent)
	}
	if want := "console: alice reset the month cap's count (reset 4), 4 cap waits woken\n"; !strings.HasSuffix(lines.String(), want) {
		t.Fatalf("Wake logged %q, want it to end %q", lines.String(), want)
	}
	sent, broken = nil, "r1"
	err := w.Wake(context.Background(), "config: version 8 loaded")
	if err == nil || !strings.Contains(err.Error(), "task r1") || !slices.Equal(sent, []string{"m1", "d1", "b1"}) {
		t.Fatalf("Wake with r1 failing = %v, signalled %v; want m1, d1 and b1 signalled and r1 named", err, sent)
	}
	if want := "config: version 8 loaded, 3 cap waits woken\n"; !strings.HasSuffix(lines.String(), want) {
		t.Fatalf("Wake logged %q, want it to end %q", lines.String(), want)
	}
}

// A card whose workflow ended has no wait to wake: SignalRunning counts
// the frontend's NotFound as delivered and starts nothing, so with the
// waker and the loader the other waits are signalled and the head settles.
// A frontend that is down fails the signal, retried once, and the head
// stays unsettled for the next poll.
func TestPolicyLoadWakesCapWaitsPastAnEndedWorkflow(t *testing.T) {
	ctx := context.Background()
	c := &mocks.Client{}
	c.On("SignalWorkflow", mock.Anything, "task-d1", "", SignalPolicyLoaded, TaskSignal{Author: "factoryd"}).
		Return(serviceerror.NewNotFound("workflow execution not found")).Once()
	for _, id := range []string{"task-m1", "task-r1", "task-b1"} {
		c.On("SignalWorkflow", mock.Anything, id, "", SignalPolicyLoaded, TaskSignal{Author: "factoryd"}).Return(nil).Once()
	}
	inner := TemporalWorkflows{Client: c}
	waits := &CapWaits{Store: capWaitStore(t), Signal: func(ctx context.Context, id string) error {
		return inner.SignalRunning(ctx, id, SignalPolicyLoaded)
	}}
	src := &memSource{}
	l := &PolicyLoader{Source: src, Store: &memPolicy{}, Wake: waits.Wake}
	src.push("c1", testFiles(testPolicyYAML))
	if err := l.Load(ctx); err != nil {
		t.Fatalf("Load = %v, want the head settled", err)
	}
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	c.AssertNumberOfCalls(t, "SignalWorkflow", 4)
	c.AssertNotCalled(t, "SignalWithStartWorkflow", mock.Anything)

	down := &mocks.Client{}
	down.On("SignalWorkflow", mock.Anything, mock.Anything, "", SignalPolicyLoaded, mock.Anything).Return(serviceerror.NewUnavailable("down"))
	if err := (TemporalWorkflows{Client: down}).SignalRunning(ctx, "m1", SignalPolicyLoaded); err == nil {
		t.Fatal("SignalRunning against a frontend that is down = nil, want the failure")
	}
	down.AssertNumberOfCalls(t, "SignalWorkflow", 2)
}

// closeComments are the comments of task id's factory-close events.
func closeComments(t *testing.T, store *fakeStore, id string) []string {
	t.Helper()
	events, err := store.IntakeEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == IntakeFactoryClosed {
			out = append(out, e.Body)
		}
	}
	return out
}

func taskOf(t *testing.T, store *fakeStore, alert string) Task {
	t.Helper()
	a, ok, _ := store.AlertByFingerprint(context.Background(), AlertMonthCap, alert)
	task, found, _ := store.Task(context.Background(), a.TaskID)
	if !ok || !found {
		t.Fatalf("no card for alert %s", alert)
	}
	return task
}

// Hand-derived: the cap of 544 is reached at 544 spent (version 4): one
// card and the alert 2026-10/100/544. Version 5 raises it to 600, activated
// a minute ago, with 543.90 spent (below 600) and the newest refusal made
// 5 minutes before the activation: nothing is filed, no early alert comes
// again (50 and 80 are once a month), and the first card closes. At 600
// spent a second card and the alert 2026-10/100/600 come.
func TestMonthAlarmClosesTheCardARaiseLifted(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 544)
	now := time.Now()
	version := func(id int64, limit float64, activated time.Time) {
		alarm.Current = func() policy.Version {
			return policy.Version{ID: id, Commit: "c9", ActivatedAt: activated,
				Tree: policy.Tree{Document: policy.Document{Budget: policy.Budget{MonthEUR: limit}}}}
		}
	}
	version(4, 544, now.Add(-48*time.Hour))
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-10/50", "2026-10/80", "2026-10/100/544"}
	if got := monthAlerts(store); !slices.Equal(got, want) || len(fj.issues) != 1 {
		t.Fatalf("alerts %v, issues %d, want %v and 1", got, len(fj.issues), want)
	}
	first := taskOf(t, store, "2026-10/100/544")
	activated := now.Add(-time.Minute)
	version(5, 600, activated)
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 543.90, Refusal: gatewayRefusal, RefusedAt: activated.Add(-5 * time.Minute)}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := monthAlerts(store); !slices.Equal(got, want) || len(fj.issues) != 1 {
		t.Fatalf("after the raise: alerts %v, issues %d, want %v and 1", got, len(fj.issues), want)
	}
	if got := taskOf(t, store, "2026-10/100/544"); got.Status != "done" {
		t.Fatalf("first card = %s, want done", got.Status)
	}
	if got := closeComments(t, store, first.ID); !slices.Equal(got, []string{"The month cap is 600.00 EUR in policy version 5, so the cap card closes."}) {
		t.Fatalf("close comments = %q", got)
	}
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 600}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want = append(want, "2026-10/100/600")
	if got := monthAlerts(store); !slices.Equal(got, want) || len(fj.issues) != 2 {
		t.Fatalf("at 600: alerts %v, issues %d, want %v and 2", got, len(fj.issues), want)
	}
	if got := taskOf(t, store, "2026-10/100/600"); got.Status != "waiting-human" || got.ID == first.ID {
		t.Fatalf("second card = %+v", got)
	}
}

// Hand-derived: the cap of 544 is reached at 544 spent, with one card and
// the alerts 2026-10/50, /80 and /100/544. A human resets the month's
// count, 0.40 EUR is counted since and 544.40 in all: the card closes, as
// on a raise, and no alert comes. 544 counted since the reset, 1088.40 in
// all, raises the count's own alerts, each marked reset-7, and a second
// card that states the whole month, the count since the reset and the
// reset, and whose why names the reset and the month's turn.
func TestMonthAlarmFollowsTheReset(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 544)
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544, AllEUR: 544}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	first := taskOf(t, store, "2026-10/100/544")
	pressed := time.Now().UTC().Truncate(time.Minute)
	reset := &policy.CapReset{ID: 7, Cap: policy.CapMonth, At: pressed, Login: "sebastian", BeforeEUR: 544}
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 0.40, AllEUR: 544.40, Reset: reset}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-10/50", "2026-10/80", "2026-10/100/544"}
	if got := monthAlerts(store); !slices.Equal(got, want) || len(fj.issues) != 1 {
		t.Fatalf("after the reset: alerts %v, issues %d, want %v and 1", got, len(fj.issues), want)
	}
	if got := taskOf(t, store, "2026-10/100/544"); got.Status != "done" {
		t.Fatalf("first card = %s, want done", got.Status)
	}
	hhmm := pressed.Format("15:04")
	if got, want := closeComments(t, store, first.ID), []string{"sebastian reset the month's count at " + hhmm + " UTC, so the cap card closes."}; !slices.Equal(got, want) {
		t.Fatalf("close comments = %q, want %q", got, want)
	}
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544, AllEUR: 1088.40, Reset: reset}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want = append(want, "2026-10/50/reset-7", "2026-10/80/reset-7", "2026-10/100/544/reset-7")
	if got := monthAlerts(store); !slices.Equal(got, want) || len(fj.issues) != 2 {
		t.Fatalf("at the cap again: alerts %v, issues %d, want %v and 2", got, len(fj.issues), want)
	}
	if got := taskOf(t, store, "2026-10/100/544/reset-7"); got.Status != "waiting-human" || got.ID == first.ID {
		t.Fatalf("second card = %+v", got)
	}
	for _, want := range []string{
		"Why: The gateway refuses every further model call until a human resets the month's count in the status panel or the UTC month turns.",
		"is 1088.40 EUR; counted since " + hhmm + " UTC, when sebastian reset the month's count: 544.00 EUR of the cap of 544.00 EUR (`budget.month_eur`).",
	} {
		if !strings.Contains(fj.issues[1], want) {
			t.Errorf("the second card lacks %q:\n%s", want, fj.issues[1])
		}
	}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskOf(t, store, "2026-10/100/544/reset-7"); got.Status != "waiting-human" || len(fj.issues) != 2 {
		t.Fatalf("the next check: second card %s, %d issues, want it open and no third", got.Status, len(fj.issues))
	}
}

// LiteLLM's outer guard counts the whole month, which a reset does not
// move: its card, filed at 300 of 544 EUR, stays open after a reset.
func TestMonthAlarmKeepsTheOuterGuardsCardOnAReset(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 544)
	refusal := gateway.ProxyBudgetRefusal + "Budget has been exceeded!"
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 300, AllEUR: 300, Refusal: refusal, RefusedAt: refusedAt}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "LiteLLM's outer guard refused model calls at 300.00 EUR") {
		t.Fatalf("issues = %q, want the outer guard's card", fj.issues)
	}
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 0.40, AllEUR: 300.40,
		Reset: &policy.CapReset{ID: 7, Cap: policy.CapMonth, At: time.Now().UTC(), Login: "sebastian", BeforeEUR: 300}}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskOf(t, store, "2026-10/100/544"); got.Status != "waiting-human" {
		t.Fatalf("the outer guard's card = %s after a reset, want it open", got.Status)
	}
}

// Hand-derived from a raise past the cap card's 544 EUR to 600: the cap
// card closes, and the webhook of factoryd's own close ends the human card
// before the alarm moves its row. The pass ends with no error, the card
// done and its close said once.
func TestMonthAlarmCloseOutrunByItsWebhook(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, _ := monthAlarm(t, store, 544)
	now := time.Now()
	alarm.Current = func() policy.Version {
		return policy.Version{ID: 4, Commit: "c9", ActivatedAt: now.Add(-48 * time.Hour), Tree: policy.Tree{Document: policy.Document{Budget: policy.Budget{MonthEUR: 544}}}}
	}
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card := taskOf(t, store, "2026-10/100/544")
	alarm.Current = func() policy.Version {
		return policy.Version{ID: 5, Commit: "c9", ActivatedAt: now.Add(-time.Minute), Tree: policy.Tree{Document: policy.Document{Budget: policy.Budget{MonthEUR: 600}}}}
	}
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 543.90}
	store.webhookOnClose = true
	if err := alarm.Check(ctx); err != nil {
		t.Fatalf("the close the webhook outran = %v, want it done", err)
	}
	if got := taskOf(t, store, "2026-10/100/544"); got.Status != "done" {
		t.Fatalf("the cap card stands %s, want done", got.Status)
	}
	if got := closeComments(t, store, card.ID); !slices.Equal(got, []string{"The month cap is 600.00 EUR in policy version 5, so the cap card closes."}) {
		t.Fatalf("close comments = %q", got)
	}
}

// A refusal counts toward the cap in force only once the gateway enforced
// it: with the raise activated at T, one made at T+10s was under 544 and
// does not count toward 600; one made at T+40s (PolicyTTL is 30 s) does,
// the spend being 543.90 still.
func TestMonthAlarmCountsARefusalAfterTheTTL(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, _ := monthAlarm(t, store, 600)
	activated := time.Now().Add(-time.Hour)
	alarm.Current = func() policy.Version {
		return policy.Version{ID: 5, Commit: "c9", ActivatedAt: activated,
			Tree: policy.Tree{Document: policy.Document{Budget: policy.Budget{MonthEUR: 600}}}}
	}
	for _, c := range []struct {
		after time.Duration
		want  []string
	}{
		{10 * time.Second, []string{"2026-10/50", "2026-10/80"}},
		{40 * time.Second, []string{"2026-10/50", "2026-10/80", "2026-10/100/600"}},
	} {
		store.monthSpend = MonthSpend{Month: "2026-10", EUR: 543.90, Refusal: gatewayRefusal, RefusedAt: activated.Add(c.after)}
		if err := alarm.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if got := monthAlerts(store); !slices.Equal(got, c.want) {
			t.Fatalf("refusal %s after the activation: alerts %v, want %v", c.after, got, c.want)
		}
	}
}

// A cap card of September still open in October closes with the month's
// comment, whatever the cap in force.
func TestMonthAlarmClosesLastMonthsCard(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, _ := monthAlarm(t, store, 50)
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 50}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card := taskOf(t, store, "2026-09/100/50")
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 1}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskOf(t, store, "2026-09/100/50"); got.Status != "done" {
		t.Fatalf("September's card = %s, want done", got.Status)
	}
	if got := closeComments(t, store, card.ID); !slices.Equal(got, []string{"The UTC month 2026-09 ended, so the cap card closes."}) {
		t.Fatalf("close comments = %q", got)
	}
}

func versionAt(id int64, limit float64, activated time.Time) func() policy.Version {
	return func() policy.Version {
		return policy.Version{ID: id, Commit: "c9", ActivatedAt: activated,
			Tree: policy.Tree{Document: policy.Document{Budget: policy.Budget{MonthEUR: limit}}}}
	}
}

// Hand-derived: a September card (cap 50) and an October card (cap 544) are
// open; the October card's filing check had September's close fail. At 600
// with 605 spent, a close fails for one of the two cards: the other still
// closes, the 600 card and alert are filed, and Check returns the error.
func TestMonthAlarmCloseFailureStopsNothing(t *testing.T) {
	ctx := context.Background()
	captureLog(t)
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 50}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	sept := taskOf(t, store, "2026-09/100/50")
	alarm.Current = versionAt(4, 544, time.Now().Add(-time.Hour))
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544}
	fj.failClose = 1
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed close returned no error")
	}
	oct := taskOf(t, store, "2026-10/100/544")
	if oct.Status != "waiting-human" || taskOf(t, store, "2026-09/100/50").Status != "waiting-human" {
		t.Fatalf("after the failed close: September %s, October %s, want both waiting", taskOf(t, store, "2026-09/100/50").Status, oct.Status)
	}
	alarm.Current = versionAt(5, 600, time.Now().Add(-time.Minute))
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 605}
	fj.failClose = 1
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed close returned no error")
	}
	done := 0
	for _, id := range []string{sept.ID, oct.ID} {
		if tk, _, _ := store.Task(ctx, id); tk.Status == "done" {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("%d of the two old cards closed, want 1", done)
	}
	if got := taskOf(t, store, "2026-10/100/600"); got.Status != "waiting-human" {
		t.Fatalf("600 card = %+v", got)
	}
	if _, ok, _ := store.AlertByFingerprint(ctx, AlertMonthCap, "2026-10/100/600"); !ok {
		t.Fatal("no 600 alert")
	}
}

// A raise to 600 with the spend already at 605 closes the 544 card and
// files the 600 card in the same check.
func TestMonthAlarmClosesTheOldCardWhenTheNewCapIsPassed(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 544)
	alarm.Current = versionAt(4, 544, time.Now().Add(-time.Hour))
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	alarm.Current = versionAt(5, 600, time.Now().Add(-time.Minute))
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 605}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if taskOf(t, store, "2026-10/100/544").Status != "done" || taskOf(t, store, "2026-10/100/600").Status != "waiting-human" || len(fj.issues) != 2 {
		t.Fatalf("old %s, new %s, issues %d", taskOf(t, store, "2026-10/100/544").Status, taskOf(t, store, "2026-10/100/600").Status, len(fj.issues))
	}
}

// Under a cap of 0 nothing is raised, but a card of an ended month closes.
func TestMonthAlarmClosesAnEndedMonthsCardUnderACapOfZero(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, _ := monthAlarm(t, store, 50)
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 50}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	alarm.Current = versionAt(6, 0, time.Now().Add(-time.Hour))
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 3}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskOf(t, store, "2026-09/100/50"); got.Status != "done" || len(monthAlerts(store)) != 3 {
		t.Fatalf("September's card = %s, alerts %v", got.Status, monthAlerts(store))
	}
}

// A ledger of 500.00 under a cap of 600.00 and a newest refusal from LiteLLM's
// outer guard: the card tells the human to raise max_budget, not the cap, and
// shows the refusal with its credentials masked.
func TestMonthAlarmNamesTheOuterGuard(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 600)
	alarm.Scrub = redact.New("hunter2hunter2")
	refusal := gateway.ProxyBudgetRefusal + "Budget has been exceeded! https://user:pass@litellm.example/v1 Authorization: Bearer abc123def456 key hunter2hunter2"
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 500, Refusal: refusal, RefusedAt: refusedAt}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(fj.issues))
	}
	got := fj.issues[0]
	for _, want := range []string{"LiteLLM's outer guard refused model calls at 500.00 EUR, below the month cap of 600.00 EUR\n\n", "Raise `max_budget` in k8s/platform/litellm/helmrelease.yaml",
		"600.00 EUR (`budget.month_eur`)", "1.25 times", "scripts/budget-backstop-check", "USD counter ran ahead of the ledger",
		"https://git.example/sf/ai-software-factory-infra/src/commit/c9/k8s/platform/litellm/helmrelease.yaml",
		"litellm proxy budget exceeded"} {
		if !strings.Contains(got, want) {
			t.Errorf("issue lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"raise it", "pass@", "abc123def456", "hunter2hunter2"} {
		if strings.Contains(got, bad) {
			t.Errorf("issue holds %q:\n%s", bad, got)
		}
	}
	for _, a := range store.alertRows() {
		if d, _ := a.Detail["refusal"].(string); strings.Contains(d, "hunter2hunter2") || strings.Contains(d, "pass@") || strings.Contains(d, "abc123def456") {
			t.Errorf("alert %s stores the refusal unmasked: %q", a.Fingerprint, d)
		}
	}
}

// The ledger at the cap of 600.00 is the cap reached, whoever refused last.
func TestMonthAlarmOuterGuardRefusalAtTheCapIsTheCapCard(t *testing.T) {
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 600)
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 600, Refusal: proxyRefusal, RefusedAt: refusedAt}
	if err := alarm.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "This month's spending reached the factory's limit of 600.00 EUR") ||
		strings.Contains(fj.issues[0], "Raise `max_budget`") {
		t.Fatalf("issues = %q", fj.issues)
	}
}

// The newest refusal decides the card's text: a filing that failed after an
// outer-guard refusal is filed at the next check with the refusal then newest.
func TestMonthAlarmNewestRefusalDecidesTheCardText(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 600)
	fj.failNext = 1
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 500, Refusal: proxyRefusal, RefusedAt: refusedAt}
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed filing raised nothing")
	}
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 500, Refusal: gatewayRefusal, RefusedAt: refusedAt}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "This month's spending reached the factory's limit") || strings.Contains(fj.issues[0], "max_budget") ||
		!strings.Contains(fj.issues[0], gatewayRefusal) {
		t.Fatalf("issues = %q, want the cap card of the gateway's refusal", fj.issues)
	}
}

// The ledger at the cap of 600.00 and the gateway's own refusal: the cap card.
func TestMonthAlarmAtTheCapAsksToRaiseIt(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 600)
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 600, Refusal: gatewayRefusal, RefusedAt: refusedAt}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "This month's spending reached the factory's limit of 600.00 EUR\n\n") ||
		strings.Contains(fj.issues[0], "max_budget") {
		t.Fatalf("issues = %q", fj.issues)
	}
}

// withdrawnRows are the question_withdrawn rows of card id.
func withdrawnRows(store *fakeStore, id string) []Event {
	var out []Event
	for _, e := range store.events {
		if e.Kind == EventQuestionWithdrawn && e.TaskID == id {
			out = append(out, e)
		}
	}
	return out
}

// requireWithdrawn fails unless card id's one question_withdrawn row, by
// the factory, carries reason and the moments said, and the live ask it
// names ended withdrawn with that reason.
func requireWithdrawn(t *testing.T, store *fakeStore, id, reason string, said Moments) {
	t.Helper()
	rows := withdrawnRows(store, id)
	if len(rows) != 1 {
		t.Fatalf("card %s has %d question_withdrawn rows, want 1: %+v", id, len(rows), rows)
	}
	row := rows[0]
	if row.Actor != FactoryDecider || row.Via != ViaFactoryd || row.Detail["reason"] != reason {
		t.Fatalf("withdrawn row = %+v, want the factory's with reason %q", row, reason)
	}
	got := MomentsOf(row.Detail)
	if len(got) != len(said) {
		t.Fatalf("row moments = %+v, want %+v", got, said)
	}
	for i := range said {
		if !got[i].At.Equal(said[i].At) || got[i].Layout != said[i].Layout {
			t.Fatalf("row moments = %+v, want %+v", got, said)
		}
	}
	caseID, _ := toInt(row.Detail["case_id"])
	c, err := store.CaseByID(context.Background(), int64(caseID))
	if err != nil || c.TaskID != id || c.Status != CaseWithdrawn || c.Resolution == nil || c.Resolution.Reason != reason {
		t.Fatalf("the row's case = %+v (%v), want card %s's, withdrawn with %q", c, err, id, reason)
	}
}

// Hand-derived: a cap of 544 is reached at 544 spent. A raise to 600, a
// reset at today's 07:30:32 UTC and the turn of the month each close the
// card, and each first withdraws its live ask with the comment's first
// sentence as a clause; only the reset names a time.
func TestMonthAlarmWithdrawsTheAskWithTheCloseReason(t *testing.T) {
	ctx := context.Background()
	resetAt := time.Now().UTC().Truncate(24 * time.Hour).Add(7*time.Hour + 30*time.Minute + 32*time.Second)
	for _, tc := range []struct {
		name   string
		after  MonthSpend
		next   func() policy.Version
		reason string
		said   Moments
	}{
		{"a raise", MonthSpend{Month: "2026-10", EUR: 543.90}, versionAt(5, 600, time.Now().Add(-time.Minute)),
			"the month cap is 600.00 EUR in policy version 5, so the cap card closes", nil},
		{"a reset", MonthSpend{Month: "2026-10", EUR: 0.40, AllEUR: 544.40, Reset: &policy.CapReset{ID: 7, Cap: policy.CapMonth, At: resetAt, Login: "sebastian", BeforeEUR: 544}}, nil,
			"sebastian reset the month's count at 07:30 UTC, so the cap card closes", Moments{{At: resetAt, Layout: LayoutClock}}},
		{"the month's turn", MonthSpend{Month: "2026-11", EUR: 1}, nil,
			"the UTC month 2026-10 ended, so the cap card closes", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			alarm, _ := monthAlarm(t, store, 544)
			alarm.Timeline = &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: store}
			store.monthSpend = MonthSpend{Month: "2026-10", EUR: 544}
			if err := alarm.Check(ctx); err != nil {
				t.Fatal(err)
			}
			card := taskOf(t, store, "2026-10/100/544")
			if tc.next != nil {
				alarm.Current = tc.next
			}
			store.monthSpend = tc.after
			if err := alarm.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if got := taskOf(t, store, "2026-10/100/544"); got.Status != "done" {
				t.Fatalf("card = %s, want done", got.Status)
			}
			requireWithdrawn(t, store, card.ID, tc.reason, tc.said)
		})
	}
}

// Hand-derived: September's card (cap 50) closes when October starts. The
// close of its issue fails after the withdrawal's row: the next pass finds
// the case ended, writes no second row and closes the card.
func TestACapCardWhoseCloseFailedWritesOneRow(t *testing.T) {
	ctx := context.Background()
	captureLog(t)
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	alarm.Timeline = &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: store}
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 50}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card := taskOf(t, store, "2026-09/100/50")
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 1}
	fj.failClose = 1
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed close returned no error")
	}
	if got := taskOf(t, store, "2026-09/100/50"); got.Status != "waiting-human" {
		t.Fatalf("after the failed close the card = %s, want waiting-human", got.Status)
	}
	if err := alarm.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskOf(t, store, "2026-09/100/50"); got.Status != "done" {
		t.Fatalf("after the next pass the card = %s, want done", got.Status)
	}
	requireWithdrawn(t, store, card.ID, "the UTC month 2026-09 ended, so the cap card closes", nil)
}
