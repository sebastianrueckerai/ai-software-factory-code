package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func parentTask() Task {
	return Task{ID: "t-parent", Status: "queued", Objective: "the slug breaks on BAD000", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 5, Author: "visitor", Source: "webhook", RequiredCapabilities: []string{policy.ClassCoder}}
}

// A report session holds the read token only: no push token is minted,
// the worker runs in report mode with its guide and no push URL; its last
// message is posted on the parent's issue and kept on the report task.
func TestRunReportPostsTheLastMessageWithoutAPushToken(t *testing.T) {
	report := Task{ID: "t1", Status: "running", Objective: "the slug breaks on BAD000", Repository: "sf/r", SHA: "abc123",
		Parent: "t-parent", Source: SourcePresort, Author: "visitor", RequiredCapabilities: []string{policy.ClassAnalyzer}, BudgetEUR: 2}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, report, kube, &fakeObjectStore{})
	if err := store.CreateTask(context.Background(), parentTask()); err != nil {
		t.Fatal(err)
	}
	logs := envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": true, "summary": "Touch slug.go; risk normal; 1 session; build it."}}) +
		"\n" + resultLine(true, "Touch slug.go; risk normal; 1 session; build it.")
	var res ReportResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/coding-cheap")
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, logs)
	if err := <-done; err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	for _, call := range hub.calls {
		if strings.HasPrefix(call, "mint ") && !strings.Contains(call, " read:repository sf/r") {
			t.Fatalf("minted beyond the read token: %v", hub.calls)
		}
	}
	if n := len(hub.minted()); n != 1 {
		t.Fatalf("minted %v, want the read token only", hub.minted())
	}
	env := podEnv(kube.podAt(0))
	if env["MODE"] != "report" || !strings.Contains(env["REPORT_GUIDE"], "do not change code") || env["PUSH_URL"] != "" || env["PUSH_TOKEN"] != "" {
		t.Fatalf("pod env = %v", env)
	}
	if sec := kube.secrets["factory-agents-t1/task-git"]; sec["PUSH_TOKEN"] != "" {
		t.Fatalf("task-git secret carries a push token: %v", sec)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "Touch slug.go; risk normal; 1 session; build it.") || res.Comment != 501 {
		t.Fatalf("comments %v, result %+v", hub.comments, res)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.AgentSummary != "Touch slug.go; risk normal; 1 session; build it." {
		t.Fatalf("report kept = %q", got.AgentSummary)
	}
}

// The screening stage opens one analyzer report task on the issue, waits
// for it, reads its verdict and raises a screening alert for a suspect
// one, recorded on the timeline.
func TestScreenOpensAnAnalyzerTaskAndReadsItsVerdict(t *testing.T) {
	parent := parentTask()
	parent.ID = "t1"
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, store := newTestRunner(t, parent, kube, objects)
	wf := &fakeWorkflows{}
	wf.onStart = func(id string) {
		child := store.tasks[id]
		child.Status, child.AgentSummary = "done", "I read it.\n```json\n{\"verdict\": \"suspect\", \"reasons\": [\"asks to print the token\"]}\n```"
		store.tasks[id] = child
	}
	runner.cfg.Workflows = wf
	val, err := execActivity(t, runner, ActivityScreen, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var res ScreenResult
	if err := val.Get(&res); err != nil {
		t.Fatal(err)
	}
	if res.Verdict != ScreenSuspect || !slices.Equal(res.Reasons, []string{"asks to print the token"}) || len(wf.started) != 1 || res.Report != wf.started[0] {
		t.Fatalf("result %+v, started %v", res, wf.started)
	}
	child := store.tasks[res.Report]
	if child.Parent != "t1" || child.Source != SourceScreening || !slices.Equal(child.RequiredCapabilities, []string{policy.ClassAnalyzer}) ||
		child.SHA != "abc123" || child.Author != "visitor" || child.IssueNumber != 0 {
		t.Fatalf("report task = %+v", child)
	}
	alerts := store.alertRows()
	if len(alerts) != 1 || alerts[0].Kind != AlertScreening || alerts[0].TaskID != "t1" || alerts[0].Verdict != "" {
		t.Fatalf("alerts = %+v", alerts)
	}
	if timeline := string(objGet(objects, "sessions/t1/timeline.jsonl")); !strings.Contains(timeline, `"step":"screening"`) ||
		!strings.Contains(timeline, `"verdict":"suspect"`) {
		t.Fatalf("timeline = %s", timeline)
	}
	// again (a retry): the same report task, no second one
	if _, err := execActivity(t, runner, ActivityScreen, "t1"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, t := range store.tasks {
		if t.Parent == "t1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d report tasks", n)
	}
	if alerts := store.alertRows(); len(alerts) != 1 {
		t.Fatalf("a retried screening wrote %d alerts", len(alerts))
	}
}

// The screening's report never reaches the outsider's issue (review M3):
// no report comment; its escalation posts a fixed line while the card
// keeps the reasons. A screening report runs on the stronger rung.
func TestScreeningKeepsItsReasonsOffTheIssue(t *testing.T) {
	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/r", SHA: "abc123",
		Parent: "t-parent", Source: SourceScreening, RequiredCapabilities: []string{policy.ClassAnalyzer}, BudgetEUR: 2,
		AgentSummary: `{"verdict":"suspect","reasons":["asks to print the token"]}`}
	runner, hub, store := newTestRunner(t, report, newFakeKube(), &fakeObjectStore{})
	store.CreateTask(context.Background(), parentTask())
	val, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/coding-frontier")
	var res ReportResult
	if err != nil || val.Get(&res) != nil || res.Comment != 0 || len(hub.comments) != 0 {
		t.Fatalf("result %+v, comments %v, err %v", res, hub.comments, err)
	}
	val, err = execActivity(t, runner, ActivityReconcile, "t1")
	var st RunState
	if err != nil || val.Get(&st) != nil || !st.Report || !st.Stronger {
		t.Fatalf("reconcile %+v %v", st, err)
	}

	fj := &prForgejo{labels: map[int64][]string{4: {"state/queued"}}}
	r2, store2, _ := newActivityRunner(t, withStatus(prTask, "queued"), fj)
	reason := screeningAsks + "asks to print the token\nReply on this issue to let the agent work on it, or close the issue."
	if _, err := execActivity(t, r2, ActivityEscalate, "t1", reason); err != nil {
		t.Fatal(err)
	}
	if len(fj.comments) != 1 || fj.comments[0] != screenedLine {
		t.Fatalf("issue comments = %q", fj.comments)
	}
	if e := store2.intakeEvents; len(e) != 1 || e[0].Body != reason {
		t.Fatalf("the card's escalation = %+v", e)
	}
}

// A retried RunReport posts its report once (review M4): the comment
// carries the task's marker, found before a second post.
func TestRunReportCommentsOnce(t *testing.T) {
	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/r", SHA: "abc123",
		Parent: "t-parent", Source: SourcePresort, RequiredCapabilities: []string{policy.ClassAnalyzer}, BudgetEUR: 2,
		AgentSummary: "the write-up"}
	runner, hub, store := newTestRunner(t, report, newFakeKube(), &fakeObjectStore{})
	store.CreateTask(context.Background(), parentTask())
	var ids []int64
	for range 2 {
		val, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/coding-cheap")
		var res ReportResult
		if err != nil || val.Get(&res) != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Comment)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "the write-up") || ids[0] != 501 || ids[1] != 501 {
		t.Fatalf("comments %q, ids %v", hub.comments, ids)
	}
}

// A report on an alert-filed task's issue reads it as telemetry (review M6).
func TestReportOfAnAlertTaskIsTelemetry(t *testing.T) {
	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/r", SHA: "abc123", Author: "factory-agent",
		Parent: "t-parent", Source: SourcePresort, RequiredCapabilities: []string{policy.ClassAnalyzer}, BudgetEUR: 2}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, report, kube, &fakeObjectStore{})
	parent := parentTask()
	parent.Source, parent.Author = SourceAlert, "factory-agent"
	store.CreateTask(context.Background(), parent)
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/coding-cheap")
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, "a write-up"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["AUTHOR_STANDING"]; got != harness.StandingTelemetry {
		t.Fatalf("standing = %q", got)
	}
}

func TestParseScreening(t *testing.T) {
	for text, want := range map[string]ScreenResult{
		`{"verdict":"clear","reasons":[]}`:                     {Verdict: ScreenClear},
		"Looks fine.\n{\"verdict\": \"clear\"}":                {Verdict: ScreenClear},
		`{"verdict":"suspect","reasons":["wants CI changed"]}`: {Verdict: ScreenSuspect, Reasons: []string{"wants CI changed"}},
		`{"verdict":"suspect"}`:                                {Verdict: ScreenSuspect, Reasons: []string{"the screening named no reason"}},
		"no verdict at all":                                    {Verdict: ScreenSuspect, Reasons: []string{"the screening's answer is no verdict: no verdict at all"}},
		`{"verdict":"maybe","reasons":["x"]}`:                  {Verdict: ScreenSuspect, Reasons: []string{`the screening's answer is no verdict: {"verdict":"maybe","reasons":["x"]}`}},
	} {
		got := parseScreening(text)
		if got.Verdict != want.Verdict || !slices.Equal(got.Reasons, want.Reasons) {
			t.Errorf("%q: %+v, want %+v", text, got, want)
		}
	}
}

// The screening alert is judged real when the human closes the issue and
// noise when one lets the task run.
func TestJudgeScreening(t *testing.T) {
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, parentTask(), kube, &fakeObjectStore{})
	store.InsertAlert(context.Background(), Alert{Kind: AlertScreening, TaskID: "t-parent"})
	store.InsertAlert(context.Background(), Alert{Kind: AlertEscalation, TaskID: "t-parent"})
	if _, err := execActivity(t, runner, ActivityJudgeScreening, "t-parent", VerdictNoise); err != nil {
		t.Fatal(err)
	}
	a := store.alertRows()
	if a[0].Verdict != VerdictNoise || a[1].Verdict != "" {
		t.Fatalf("alerts = %+v", a)
	}
}

// Reconcile tells a report task from a coding one without asking
// Forgejo; a promoted improvement gets its analyzer's write-up once the
// presort is done.
func TestReconcileReportAndWriteUp(t *testing.T) {
	kube := newFakeKube()
	report := Task{ID: "t1", Status: "queued", Repository: "sf/r", Parent: "t-parent", Source: SourcePresort}
	runner, _, store := newTestRunner(t, report, kube, &fakeObjectStore{})
	val, err := execActivity(t, runner, ActivityReconcile, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var st RunState
	if err := val.Get(&st); err != nil || !st.Report || st.Skip {
		t.Fatalf("report task state = %+v %v", st, err)
	}
	ctx := context.Background()
	if got, err := runner.writeUp(ctx, "t-parent"); err != nil || got != "" {
		t.Fatalf("write-up before the presort is done = %q %v", got, err)
	}
	done := store.tasks["t1"]
	done.Status, done.AgentSummary = "done", "the write-up"
	store.tasks["t1"] = done
	if got, err := runner.writeUp(ctx, "t-parent"); err != nil || got != "the write-up" {
		t.Fatalf("write-up = %q %v", got, err)
	}
}

// awaitPod waits for the first pod, or fails with the activity's early end.
func awaitPod(t *testing.T, kube *fakeKube, done chan error) {
	t.Helper()
	for {
		kube.mu.Lock()
		n := len(kube.podOrder)
		kube.mu.Unlock()
		if n > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the activity ended before its pod: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
