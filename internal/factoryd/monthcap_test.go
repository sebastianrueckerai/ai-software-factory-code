package factoryd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const proxyRefusal = "litellm proxy budget exceeded (litellm_settings.max_budget, the proxy-wide monthly stop behind budget.month_eur): Budget has been exceeded! Current cost: 55.0123, Max budget: 55.0"

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
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 49.98, Refusal: proxyRefusal}
	if err := alarm.Check(ctx); err == nil {
		t.Fatal("a failed filing raised nothing")
	}
	step(MonthSpend{Month: "2026-09", EUR: 49.98, Refusal: proxyRefusal}, "2026-09/50", "2026-09/80", "2026-09/100")
	step(MonthSpend{Month: "2026-09", EUR: 49.99, Refusal: proxyRefusal}, "2026-09/50", "2026-09/80", "2026-09/100")
	if len(fj.issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(fj.issues))
	}
	for _, want := range []string{"The month cap of 50.00 EUR is reached (2026-09): raise it, or let the factory stand until the month ends?\n\n",
		"49.98 EUR of the cap of 50.00 EUR",
		"https://git.example/sf/ai-software-factory-infra/src/commit/c9/factory/policy.yaml#L7",
		"refused_month_cap", "The first refusal:\n\n```\n" + proxyRefusal + "\n```"} {
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
		case a.Fingerprint == "2026-09/100" && (a.TaskID != task.ID || a.Detail["issue"] != int64(41)):
			t.Errorf("the cap's alert = %+v, want it on task %s", a, task.ID)
		case a.Verdict != VerdictReal || a.ResolvedBy != "factoryd" || a.Detail["month"] != "2026-09" || a.Detail["cap_eur"] != 50.0:
			t.Errorf("alert = %+v", a)
		}
	}
	// a new month starts over; a jump past every threshold raises each
	step(MonthSpend{Month: "2026-10", EUR: 50}, "2026-09/50", "2026-09/80", "2026-09/100", "2026-10/50", "2026-10/80", "2026-10/100")
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

// The cap's issue is filed once even when the task row after it fails:
// the next check finds the open issue by its title and takes it on.
func TestMonthAlarmFilesTheCapIssueOnce(t *testing.T) {
	ctx := context.Background()
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
	a, ok, _ := store.AlertByFingerprint(ctx, AlertMonthCap, "2026-09/100")
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
	store.monthSpend = MonthSpend{Month: "2026-09", EUR: 3, Refusal: proxyRefusal}
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
		Choices: []QuestionChoice{{Value: AnswerGrant, Label: "Raise it"}, {Value: AnswerPark, Label: "Park"}}}
	for _, adopt := range []bool{false, true} {
		store, rows := newFakeStore(), &fakeEventRecorder{}
		alarm, fj := monthAlarm(t, store, 50)
		book := &failingTasks{fakeStore: store}
		if adopt {
			book.failNext = 1
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
