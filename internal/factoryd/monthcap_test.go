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
	for _, want := range []string{"The month cap of 50.00 EUR is reached (2026-09): raise it, or let the factory stand until the month ends?\n\n",
		"49.98 EUR of the cap of 50.00 EUR",
		"https://git.example/sf/ai-software-factory-infra/src/commit/c9/factory/policy.yaml#L7",
		"refused_month_cap", "The newest refusal:\n\n```\n" + gatewayRefusal + "\n```"} {
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
	if len(fj.issues) != 2 || !strings.Contains(fj.issues[1], "(2026-10)") {
		t.Fatalf("issues = %q, want the October card", fj.issues)
	}
}

// failingTasks fails the next task row creation, as a database error
// right after the issue was filed.
type failingTasks struct {
	*fakeStore
	failNext int
}

func (f *failingTasks) CreateTask(ctx context.Context, t Task) error {
	if f.failNext > 0 {
		f.failNext--
		return errors.New("database gone")
	}
	return f.fakeStore.CreateTask(ctx, t)
}

// The cap's issue is filed once even when the task row after it fails and
// the issue's close fails too: the next check finds the open issue by its
// title and takes it on.
func TestMonthAlarmFilesTheCapIssueOnce(t *testing.T) {
	ctx := context.Background()
	captureLog(t)
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	fj.failClose = 1
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
// a failed task row left behind, its close failed too.
func TestFileHumanTaskRecordsTheAsk(t *testing.T) {
	ctx := context.Background()
	q := HumanQuestion{Ask: "Raise the cap?", Why: "the cap is reached", Info: "a raise is a commit",
		Choices: []QuestionChoice{{Value: AnswerGrant, Label: "Raise it"}, {Value: AnswerPark, Label: "Park"}}}
	for _, adopt := range []bool{false, true} {
		store, rows := newFakeStore(), &fakeEventRecorder{}
		alarm, fj := monthAlarm(t, store, 50)
		book := &failingTasks{fakeStore: store}
		if adopt {
			captureLog(t)
			book.failNext, fj.failClose = 1, 1
			if _, _, err := fileHumanTask(ctx, book, alarm.Forgejo, nil, alarm.Repo, q, nil, Task{ID: "t-0", Kind: "human", Source: SourceMonthCap}, ""); err == nil {
				t.Fatal("a failed task row raised nothing")
			}
		}
		tl := &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: rows}
		task, number, err := fileHumanTask(ctx, book, alarm.Forgejo, tl, alarm.Repo, q, nil, Task{ID: "t-1", Kind: "human", Source: SourceMonthCap}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(fj.issues) != 1 || !strings.HasPrefix(fj.issues[0], "Raise the cap?\n\n```\nRaise the cap?\n```\n\nWhy: the cap is reached\n\nInfo: a raise is a commit\n\nChoices:\n- Raise it\n- Park\n\nfactoryd asks (month_cap). ") {
			t.Fatalf("adopt=%v issues = %q", adopt, fj.issues)
		}
		events, err := store.IntakeEvents(ctx, task.ID)
		if err != nil || len(events) != 1 || events[0].Kind != IntakeEscalation || events[0].Question == nil ||
			!reflect.DeepEqual(*events[0].Question, q) || !slices.Equal(events[0].Choices, []string{"Raise it", "Park"}) {
			t.Fatalf("adopt=%v intake events = %+v, %v", adopt, events, err)
		}
		var asked []Event
		for _, e := range rows.rows {
			if e.Kind == EventQuestionAsked {
				asked = append(asked, e)
			}
		}
		if len(asked) != 1 || asked[0].Via != ViaFactoryd || asked[0].Detail["ask"] != "Raise the cap?" || asked[0].Detail["by"] != "factoryd" ||
			asked[0].Detail["source"] != SourceMonthCap || number != 41 {
			t.Fatalf("adopt=%v question_asked rows = %+v, issue %d", adopt, asked, number)
		}
	}
}

// capWaitStore is a store of waiting-human cards by id with the escalation
// each one's last: a month wait (m1), a day wait (d1), an agent's ask (a1)
// and a month wait followed by an ask (l1); a card done (x1) is not
// waiting.
func capWaitStore(t *testing.T) *fakeStore {
	t.Helper()
	store, ctx := newFakeStore(), context.Background()
	month := capWaitReason(policy.RefusedMonthCap, capGateway(policy.RefusedMonthCap), "spent", novFirst)
	day := capWaitReason(policy.RefusedDayCap, capGateway(policy.RefusedDayCap), "spent", oct(15, 0, 0))
	esc := func(id, body string, at time.Time) {
		if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: id, Kind: IntakeEscalation, Author: "factoryd", Body: body, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for id, status := range map[string]string{"m1": "waiting-human", "d1": "waiting-human", "a1": "waiting-human", "l1": "waiting-human", "x1": "done"} {
		if err := store.CreateTask(ctx, Task{ID: id, Status: status, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
	}
	esc("m1", month, oct31(21, 0, 0))
	esc("d1", day, oct(14, 20, 43))
	esc("a1", "The agent asks: which one?", oct31(21, 0, 0))
	esc("l1", month, oct31(21, 0, 0))
	esc("l1", "The agent asks: which one?", oct31(22, 0, 0))
	esc("x1", month, oct31(21, 0, 0))
	return store
}

// A policy load signals the waiting cards whose last escalation is a cap
// wait, the month's (m1) and the day's (d1), and no other: not an agent's
// ask (a1), not a cap wait a later ask followed (l1), not a done card
// (x1). A signal that fails leaves the others signalled and Wake failing.
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
	if err := w.Wake(context.Background(), policy.Version{ID: 8}); err != nil || !slices.Equal(sent, []string{"d1", "m1"}) {
		t.Fatalf("Wake = %v, signalled %v; want d1 and m1", err, sent)
	}
	sent, broken = nil, "d1"
	err := w.Wake(context.Background(), policy.Version{ID: 8})
	if err == nil || !strings.Contains(err.Error(), "task d1") || !slices.Equal(sent, []string{"m1"}) {
		t.Fatalf("Wake with d1 failing = %v, signalled %v; want m1 signalled and d1 named", err, sent)
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
	c.On("SignalWorkflow", mock.Anything, "task-m1", "", SignalPolicyLoaded, TaskSignal{Author: "factoryd"}).Return(nil).Once()
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
	c.AssertNumberOfCalls(t, "SignalWorkflow", 2)
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
	if got := closeComments(t, store, first.ID); !slices.Equal(got, []string{"budget.month_eur is 600 in policy version 5: the cap card closes."}) {
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
	if got := closeComments(t, store, card.ID); !slices.Equal(got, []string{"The UTC month 2026-09 ended: the cap card closes."}) {
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
	for _, want := range []string{"LiteLLM's outer guard refused calls at 500.00 EUR, below the month cap of 600.00 EUR: raise max_budget in k8s/platform/litellm/helmrelease.yaml",
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
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "The month cap of 600.00 EUR is reached (2026-09): raise it") ||
		strings.Contains(fj.issues[0], "raise max_budget") {
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
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "raise it") || strings.Contains(fj.issues[0], "max_budget") ||
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
	if len(fj.issues) != 1 || !strings.Contains(fj.issues[0], "The month cap of 600.00 EUR is reached (2026-09): raise it, or let the factory stand until the month ends?") ||
		strings.Contains(fj.issues[0], "max_budget") {
		t.Fatalf("issues = %q", fj.issues)
	}
}
