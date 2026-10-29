package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The tracks of the shipped processes: maintenance builds every card
// directly, spec plans too.
var (
	maintenanceTracks = policy.Tracks{Direct: policy.Track{Class: policy.ClassCoder}}
	specTracks        = policy.Tracks{Direct: policy.Track{Class: policy.ClassCoder},
		Plan: &policy.Track{Class: "planner", Plan: &policy.PlanShape{MinCards: 1, MaxCards: 12, Foundation: true, Integration: true}}}
)

// trackIntake is a test intake whose code repository's project follows a
// process of tracks.
func trackIntake(t *testing.T, process string, tracks policy.Tracks) (*Intake, *fakeStore, *[]string) {
	t.Helper()
	in, store, requests := newTestIntake(t)
	p := seedProject("sf/ai-software-factory-code")
	p.Process, p.Tracks = process, tracks
	store.projects = []Project{p}
	return in, store, requests
}

// labelEvent is Forgejo's issue_label delivery for issue number, filed by
// poster, now labelled labels, by sender.
func labelEvent(number int64, poster string, labels []string, sender string) []byte {
	var p map[string]any
	json.Unmarshal(issueOpenedBody(number, poster, labels, "A card", "body"), &p) // #nosec G104 -- the test's own JSON
	p["action"], p["sender"] = "label_updated", map[string]string{"login": sender}
	b, _ := json.Marshal(p)
	return b
}

func post(t *testing.T, in *Intake, event string, body []byte) {
	t.Helper()
	if rr := postWebhook(in, event, body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, body = %s", event, rr.Code, rr.Body)
	}
}

// comments are the comments the intake stub took.
func comments(requests []string) []string {
	var out []string
	for _, r := range requests {
		if _, body, ok := strings.Cut(r, "/comments: "); ok && strings.HasPrefix(r, "comment ") {
			out = append(out, body)
		}
	}
	return out
}

// setLabels puts labels on issue number in the stub, as Forgejo holds them.
func setLabels(t *testing.T, in *Intake, number int64, labels []string) {
	t.Helper()
	if err := in.Forgejo.ReplaceIssueLabels(context.Background(), "sf", "ai-software-factory-code", number, labels); err != nil {
		t.Fatal(err)
	}
}

func card(store *fakeStore, number int64) Task {
	got, _, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", number)
	return got
}

// Intake's track rule, the same for every process: human wins, then agent
// or bug asks for direct, then plan for plan where the process has a plan
// track; backlog, improvement and a plan label on a process without one
// file no card; no track label asks for direct.
func TestIntakeTrackRule(t *testing.T) {
	for _, tc := range []struct {
		labels            []string
		maintenance, spec string
	}{
		{nil, "direct", "direct"},
		{[]string{"question", "priority/p1"}, "direct", "direct"},
		{[]string{"bug"}, "direct", "direct"},
		{[]string{"agent"}, "direct", "direct"},
		{[]string{"plan"}, "", "plan"},
		{[]string{"backlog"}, "", ""},
		{[]string{"improvement"}, "", ""},
		{[]string{"improvement", "bug"}, "direct", "direct"},
		{[]string{"backlog", "agent"}, "direct", "direct"},
		{[]string{"plan", "agent"}, "direct", "direct"},
		{[]string{"plan", "backlog"}, "", "plan"},
		{[]string{"human"}, "human", "human"},
		{[]string{"human", "agent"}, "human", "human"},
		{[]string{"human", "plan"}, "human", "human"},
		{[]string{"state/queued"}, "direct", "direct"},
	} {
		if got := IssueTrack(tc.labels, maintenanceTracks); got != tc.maintenance {
			t.Errorf("maintenance %q = %q, want %q", tc.labels, got, tc.maintenance)
		}
		if got := IssueTrack(tc.labels, specTracks); got != tc.spec {
			t.Errorf("spec %q = %q, want %q", tc.labels, got, tc.spec)
		}
	}
}

// A writer's issue with no track label is a direct card: queued at main's
// head with the direct track's class, its process recorded, its workflow
// started under the work bound and its card_created naming the track; no
// comment, no label written.
func TestDirectIssueQueuesAndStarts(t *testing.T) {
	in, store, requests := trackIntake(t, "spec", specTracks)
	objects := &fakeObjectStore{}
	in.Timeline = &Timeline{Objects: objects}
	post(t, in, "issues", issueOpenedBody(5, "sebastian", []string{"priority/p1"}, "Fix it", "body"))
	got := card(store, 5)
	if got.Status != "queued" || got.Track != "direct" || got.Process != "spec" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) ||
		got.SHA != "sha-main-head" || got.Gate != GateIntake {
		t.Fatalf("card = %+v, want a queued direct card of process spec at main's head", got)
	}
	if !slices.Equal(workflowsOf(in).started, []string{got.ID}) || len(comments(*requests)) != 0 {
		t.Fatalf("started %v, comments %q", workflowsOf(in).started, comments(*requests))
	}
	if ev := events(t, objects, got.ID); !slices.Equal(ev, []string{"card_created direct sebastian"}) {
		t.Fatalf("events = %q", ev)
	}
}

// A writer's plan label on a spec project files a plan card: the
// planner's class, started at once. An outside account's plan label is
// off its new issue, which is a direct card.
func TestPlanLabelFilesAPlanCardFromAWriterOnly(t *testing.T) {
	in, store, _ := trackIntake(t, "spec", specTracks)
	setLabels(t, in, 16, []string{"plan"})
	post(t, in, "issues", issueOpenedBody(16, "sebastian", []string{"plan"}, "Build it", "body"))
	if got := card(store, 16); got.Track != "plan" || got.Status != "queued" || !slices.Equal(got.RequiredCapabilities, []string{"planner"}) {
		t.Fatalf("writer's plan card = %+v, want queued on the plan track as planner", got)
	}
	setLabels(t, in, 15, []string{"plan"})
	post(t, in, "issues", issueOpenedBody(15, "outsider", []string{"plan"}, "Build it", "body"))
	if got := card(store, 15); got.Track != "direct" || got.Status != "queued" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) {
		t.Fatalf("outsider's card = %+v, want a direct card", got)
	}
	if got := issueLabels(t, in.Forgejo, 15); slices.Contains(got, "plan") {
		t.Fatalf("labels = %v, want plan off", got)
	}
}

// An issue whose labels file no card stays an interest: backlog,
// improvement, and plan on a process without a plan track leave no row,
// no comment and no start. A writer's agent label on it later files the
// card through the label event, its author the poster.
func TestNoTrackFilesNoCardUntilAWriterLabels(t *testing.T) {
	in, store, requests := trackIntake(t, "maintenance", maintenanceTracks)
	// a writer's plan label: an outside account's is off the issue first
	for n, filed := range map[int64]struct {
		poster string
		labels []string
	}{23: {"sebastian", []string{"plan"}}, 24: {"alice", []string{"backlog"}}, 25: {"alice", []string{"improvement"}}} {
		setLabels(t, in, n, filed.labels)
		post(t, in, "issues", issueOpenedBody(n, filed.poster, filed.labels, "Later", "body"))
	}
	if len(store.tasks) != 0 || len(workflowsOf(in).started) != 0 || len(comments(*requests)) != 0 {
		t.Fatalf("tasks %v, started %v, comments %q: want none", store.tasks, workflowsOf(in).started, comments(*requests))
	}
	labels := []string{"improvement", "agent"}
	setLabels(t, in, 25, labels)
	post(t, in, "issue_label", labelEvent(25, "alice", labels, "sebastian"))
	if got := card(store, 25); got.Track != "direct" || got.Status != "queued" || got.Author != "alice" || got.AuthorWriter {
		t.Fatalf("card = %+v, want alice's direct card", got)
	}
}

// A label event on a card filed already moves nothing: its track is the
// filer's, and only a queued card's lost workflow start is retried.
func TestLabelOnAFiledCardMovesNothing(t *testing.T) {
	in, store, requests := trackIntake(t, "spec", specTracks)
	store.tasks["t-c"] = Task{ID: "t-c", Kind: "agent", Status: "running", Track: "direct", Process: "spec", Repository: "sf/ai-software-factory-code",
		IssueNumber: 20, Parent: "t-plan", Source: SourcePlan}
	store.tasks["t-h"] = Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 22, Source: SourceAlert}
	before := len(*requests)
	for _, n := range []int64{20, 22} {
		labels := []string{"plan", "agent"}
		setLabels(t, in, n, labels)
		post(t, in, "issue_label", labelEvent(n, "factory-agent", labels, "sebastian"))
	}
	if got := store.tasks["t-c"]; got.Track != "direct" || got.Status != "running" {
		t.Fatalf("card = %+v", got)
	}
	if got := store.tasks["t-h"]; got.Kind != "human" || got.Status != "waiting-human" {
		t.Fatalf("person's card = %+v", got)
	}
	if len(workflowsOf(in).started) != 0 || len(*requests) != before+2 {
		t.Fatalf("started %v, forgejo calls %v: want the two label writes only", workflowsOf(in).started, (*requests)[before:])
	}
	store.tasks["t-q"] = Task{ID: "t-q", Kind: "agent", Status: "queued", Track: "direct", Repository: "sf/ai-software-factory-code", IssueNumber: 26}
	post(t, in, "issue_label", labelEvent(26, "sebastian", []string{"agent"}, "sebastian"))
	if !slices.Equal(workflowsOf(in).started, []string{"t-q"}) {
		t.Fatalf("started %v, want the queued card's start retried", workflowsOf(in).started)
	}
}

// An issue whose reproduction command breaks the sandbox rule files no
// agent card: a person's card asks the poster to file it again, once,
// and nothing starts.
func TestBadReproFilesAPersonsCard(t *testing.T) {
	in, store, _ := trackIntake(t, "maintenance", maintenanceTracks)
	body := "It breaks\n\n```repro\ncd /tmp/x && make repro\n```"
	post(t, in, "issues", issueOpenedBody(27, "sebastian", []string{"bug"}, "It breaks", body))
	got := card(store, 27)
	if got.Kind != "human" || got.Status != "waiting-human" || got.Track != "" || got.ReproCommand != "" {
		t.Fatalf("card = %+v, want a person's card", got)
	}
	if len(workflowsOf(in).started) != 0 {
		t.Fatalf("started %v", workflowsOf(in).started)
	}
	var asks []HumanQuestion
	for _, e := range store.intakeEvents {
		if e.TaskID == got.ID && e.Kind == IntakeEscalation && e.Question != nil {
			asks = append(asks, *e.Question)
		}
	}
	if len(asks) != 1 || !strings.HasPrefix(asks[0].Why, "filed by sebastian: the reproduction command ") || !strings.Contains(asks[0].Info, "File the issue again") {
		t.Fatalf("asks = %+v, want the one repro ask", asks)
	}
}

// fileStub is a Forgejo that files issue 40 and keeps its labels.
func fileStub(t *testing.T) (*Forgejo, *[]string) {
	t.Helper()
	var labels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":40,"html_url":"https://forgejo/sf/r/issues/40"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/issues/40/labels"):
			var p struct {
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&p) // #nosec G104 -- test stub
			labels = p.Labels
			io.WriteString(w, `[]`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}, &labels
}

// FileAgentTask's cards (the API's, SigNoz alerts') are queued on the
// direct track of the project's process with its class and started; a
// project with no direct track files none.
func TestFileTaskEntersDirect(t *testing.T) {
	ctx := context.Background()
	p := seedProject("sf/r")
	f, labels := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	got, ref, err := FileAgentTask(ctx, store, f, wf, nil, p, "Fix it", "body", []string{"agent"}, Task{ID: "t-api", Source: "api", Author: "factory-agent"})
	if err != nil || ref.Number != 40 {
		t.Fatal(ref, err)
	}
	if got.Kind != "agent" || got.Status != "queued" || got.Track != "direct" || got.Process != "maintenance" ||
		!slices.Equal(got.RequiredCapabilities, []string{"coder"}) || !slices.Equal(store.tasks["t-api"].RequiredCapabilities, []string{"coder"}) {
		t.Fatalf("task = %+v", got)
	}
	if !slices.Equal(*labels, []string{"agent", "state/queued"}) || !slices.Equal(wf.started, []string{"t-api"}) {
		t.Fatalf("labels %v, started %v", *labels, wf.started)
	}
	p.Tracks = policy.Tracks{}
	if _, _, err := FileAgentTask(ctx, store, f, wf, nil, p, "x", "y", nil, Task{ID: "t-none"}); err == nil ||
		err.Error() != "project sf-r follows no process with a direct track" {
		t.Fatalf("no direct track: %v", err)
	}
}

// A roll runs the direct track's class with no process of its own, outside
// the work bound; a human task has neither track nor process.
func TestRollRunsOutsideTheBound(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	got, _, err := FileAgentTask(ctx, store, f, wf, nil, seedProject("sf/r"), "roll abc", "body", nil, Task{ID: "t-roll", Source: SourceRoll})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || got.Track != "direct" || got.Process != "" || !got.Unbound() || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) ||
		!slices.Equal(*labels, []string{"state/queued"}) || !slices.Equal(wf.started, []string{"t-roll"}) {
		t.Fatalf("roll = %+v, labels %v, started %v", got, *labels, wf.started)
	}
	got, _, err = FileTask(ctx, store, f, Repo{Owner: "sf", Name: "r"}, "config", "body", []string{"human"}, Task{ID: "t-h", Kind: "human", Source: SourceConfig})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting-human" || got.Track != "" || got.Process != "" || len(got.RequiredCapabilities) != 0 || len(wf.started) != 1 {
		t.Fatalf("human task = %+v, started %v", got, wf.started)
	}
}

// The schema refuses an agent card without a track: no source can file
// one undecided.
func TestAnAgentCardWithoutATrackIsRefused(t *testing.T) {
	store := newFakeStore()
	if err := store.CreateTask(context.Background(), Task{ID: "t-u", Kind: "agent", Status: "queued"}); err == nil ||
		!strings.Contains(err.Error(), "tasks_agent_track") {
		t.Fatalf("CreateTask = %v, want the track refusal", err)
	}
}

// A report task is told by its source: a plan's card has a parent too and
// runs the lifecycle, and RunReport refuses it.
func TestChildReportDetection(t *testing.T) {
	for _, tc := range []struct {
		task Task
		want bool
	}{
		{Task{Parent: "t-p", Source: SourceScreening}, true},
		{Task{Parent: "t-plan", Source: "plan"}, false},
		{Task{Source: "webhook"}, false},
	} {
		if got := tc.task.Report(); got != tc.want {
			t.Errorf("%+v: Report = %v, want %v", tc.task, got, tc.want)
		}
	}
	child := Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", IssueNumber: 3, Parent: "t-plan", Source: "plan",
		Track: "direct", Process: "maintenance", RequiredCapabilities: []string{policy.ClassCoder}}
	r, _, _ := newActivityRunner(t, child, &prForgejo{})
	_, err := execActivity(t, r, ActivityRunReport, "t1", "m", int64(0))
	if err == nil || !strings.Contains(err.Error(), "task t1 is no report task") {
		t.Fatalf("RunReport on a plan's card: %v", err)
	}
}

// labelsForgejo is a repository's labels: it answers the labels step's
// reads and records every write in order.
type labelsForgejo struct {
	labels []RepoLabel
	calls  []string // "METHOD path body"
}

func (lf *labelsForgejo) srv(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		const base = "/api/v1/repos/sf/r"
		if r.Method == http.MethodGet && r.URL.Path == base+"/labels" {
			items := make([]string, len(lf.labels))
			for i, l := range lf.labels {
				b, _ := json.Marshal(l)
				items[i] = string(b)
			}
			servePage(w, r, 50, items...)
			return
		}
		lf.calls = append(lf.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, base)+" "+strings.TrimSpace(string(body)))
		if r.Method != http.MethodPost || r.URL.Path != base+"/labels" {
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			return
		}
		var l RepoLabel
		json.Unmarshal(body, &l) // #nosec G104 -- test stub
		l.ID = int64(100 + len(lf.labels))
		lf.labels = append(lf.labels, l)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// The labels step creates BootLabels and the track labels of the
// project's process where missing: plan only where the process has a
// plan track.
func TestProjectLabelsFollowTheTracks(t *testing.T) {
	ctx := context.Background()
	all := func(extra ...string) []RepoLabel {
		var out []RepoLabel
		for i, name := range slices.Sorted(maps.Keys(BootLabels)) {
			out = append(out, RepoLabel{ID: int64(10 + i), Name: name})
		}
		for i, name := range extra {
			out = append(out, RepoLabel{ID: int64(5 + i), Name: name})
		}
		return out
	}
	lf := &labelsForgejo{labels: all("backlog")}
	change, err := lf.srv(t).EnsureProjectLabels(ctx, "sf", "r", specTracks)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`POST /labels {"color":"#5319e7","name":"plan"}`}; !slices.Equal(lf.calls, want) || change != "created plan" {
		t.Fatalf("calls = %q, change %q", lf.calls, change)
	}
	lf = &labelsForgejo{labels: all("backlog")}
	if change, err = lf.srv(t).EnsureProjectLabels(ctx, "sf", "r", maintenanceTracks); err != nil || change != Unchanged || len(lf.calls) != 0 {
		t.Fatalf("maintenance: %q, %v, calls %q", change, err, lf.calls)
	}
}

// events are the event lines of task id's timeline: the event and its
// track and by where it has them.
func events(t *testing.T, objects *fakeObjectStore, id string) []string {
	t.Helper()
	b, err := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e["step"] != StepEvent {
			continue
		}
		fields := []string{e["event"].(string)}
		for _, k := range []string{"track", "by", "state", "pr", "cards"} {
			if v, ok := e[k]; ok {
				fields = append(fields, fmt.Sprint(v))
			}
		}
		out = append(out, strings.Join(fields, " "))
	}
	return out
}
