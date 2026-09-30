package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The lanes of the shipped processes (design section 0), and a variant of
// maintenance whose ready lane the approvers move.
var (
	maintenanceLanes = []policy.Lane{
		{Name: policy.LaneBacklog, WriteUp: policy.ClassAnalyzer},
		{Name: policy.LaneReady, Class: policy.ClassCoder, MovedBy: policy.MovedByWriters},
	}
	specLanes = []policy.Lane{
		{Name: policy.LaneBacklog},
		{Name: policy.LanePlan, Class: "planner", MovedBy: policy.MovedByWriters, Plan: &policy.PlanShape{MaxCards: 12, Foundation: true, Integration: true}},
		{Name: policy.LaneReady, Class: policy.ClassCoder, MovedBy: policy.MovedByWriters},
	}
	approversLanes = []policy.Lane{
		{Name: policy.LaneBacklog},
		{Name: policy.LaneReady, Class: policy.ClassCoder, MovedBy: policy.MovedByApprovers},
	}
)

// laneIntake is a test intake whose code repository's project follows a
// process of lanes.
func laneIntake(t *testing.T, process string, lanes []policy.Lane) (*Intake, *fakeStore, *[]string) {
	t.Helper()
	in, store, requests := newTestIntake(t)
	p := seedProject("sf/ai-software-factory-code")
	p.Process, p.Lanes = process, lanes
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

// Intake's lane rule, the same for every process: human wins, then agent
// or bug asks for ready, then plan asks for plan where the process has a
// plan lane (backlog elsewhere), then backlog or improvement, and no lane
// label asks for ready.
func TestIntakeLaneRule(t *testing.T) {
	for _, tc := range []struct {
		labels            []string
		maintenance, spec string
	}{
		{nil, "ready", "ready"},
		{[]string{"question", "priority/p1"}, "ready", "ready"},
		{[]string{"bug"}, "ready", "ready"},
		{[]string{"agent"}, "ready", "ready"},
		{[]string{"plan"}, "backlog", "plan"},
		{[]string{"backlog"}, "backlog", "backlog"},
		{[]string{"improvement"}, "backlog", "backlog"},
		{[]string{"improvement", "bug"}, "ready", "ready"},
		{[]string{"backlog", "agent"}, "ready", "ready"},
		{[]string{"plan", "agent"}, "ready", "ready"},
		{[]string{"plan", "backlog"}, "backlog", "plan"},
		{[]string{"human"}, "human", "human"},
		{[]string{"human", "agent"}, "human", "human"},
		{[]string{"human", "plan"}, "human", "human"},
		{[]string{"state/backlog"}, "ready", "ready"},
	} {
		if got := IssueLane(tc.labels, maintenanceLanes); got != tc.maintenance {
			t.Errorf("maintenance %q = %s, want %s", tc.labels, got, tc.maintenance)
		}
		if got := IssueLane(tc.labels, specLanes); got != tc.spec {
			t.Errorf("spec %q = %s, want %s", tc.labels, got, tc.spec)
		}
	}
}

// A writer who is no approver files an agent issue where the approvers
// move ready: the card lands in the backlog, agent off and backlog on,
// with one comment why, and nothing starts.
func TestCreateIntoApproversLaneLandsInBacklog(t *testing.T) {
	in, store, requests := laneIntake(t, "gated", approversLanes)
	setLabels(t, in, 5, []string{"agent", "priority/p1"})
	post(t, in, "issues", issueOpenedBody(5, "sebastian", []string{"agent", "priority/p1"}, "Fix it", "body"))
	got := card(store, 5)
	if got.Status != "backlog" || got.Lane != "backlog" || got.Process != "gated" || len(got.RequiredCapabilities) != 0 || got.SHA != "" || got.Gate != "" {
		t.Fatalf("card = %+v, want a backlog card of process gated with no class, base or gate", got)
	}
	if len(workflowsOf(in).started) != 0 || len(store.tasks) != 1 {
		t.Fatalf("started %v, %d tasks: want nothing started and no write-up", workflowsOf(in).started, len(store.tasks))
	}
	if labels := issueLabels(t, in.Forgejo, 5); !slices.Equal(labels, []string{"priority/p1", "backlog", "state/backlog"}) {
		t.Fatalf("labels = %v, want priority/p1, backlog, state/backlog", labels)
	}
	want := "factoryd: lane ready takes a card from the approvers; sebastian is none. The card stays in backlog."
	if got := comments(*requests); !slices.Equal(got, []string{want}) {
		t.Fatalf("comments = %q, want %q", got, want)
	}
}

// The default entry is judged against the poster too: where the approvers
// move ready, an outside account's bug or unlabelled issue lands in the
// backlog.
func TestDefaultEntryApproversReadyLandsInBacklog(t *testing.T) {
	for _, labels := range [][]string{{"bug"}, nil} {
		in, store, requests := laneIntake(t, "gated", approversLanes)
		setLabels(t, in, 6, labels)
		post(t, in, "issues", issueOpenedBody(6, "outsider", labels, "It breaks", "body"))
		if got := card(store, 6); got.Status != "backlog" || got.Lane != "backlog" || got.Author != "outsider" {
			t.Fatalf("%v: card = %+v, want the outsider's card in the backlog", labels, got)
		}
		if len(workflowsOf(in).started) != 0 {
			t.Fatalf("%v: started %v", labels, workflowsOf(in).started)
		}
		if got := issueLabels(t, in.Forgejo, 6); !slices.Equal(got, []string{"backlog", "state/backlog"}) {
			t.Fatalf("%v: labels = %v, want bug off, backlog on", labels, got)
		}
		want := "factoryd: lane ready takes a card from the approvers; outsider is none. The card stays in backlog."
		if got := comments(*requests); !slices.Equal(got, []string{want}) {
			t.Fatalf("%v: comments = %q", labels, got)
		}
	}
}

// An approver's issue enters ready by the default entry, even from an
// account that cannot write the repository.
func TestDefaultEntryApproverPosterEntersReady(t *testing.T) {
	in, store, requests := laneIntake(t, "gated", approversLanes)
	post(t, in, "issues", issueOpenedBody(7, "alice", nil, "It breaks", "body"))
	got := card(store, 7)
	if got.Status != "queued" || got.Lane != "ready" || got.Process != "gated" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) ||
		got.SHA != "sha-main-head" || got.AuthorWriter {
		t.Fatalf("card = %+v, want alice's card queued in ready as coder", got)
	}
	if !slices.Equal(workflowsOf(in).started, []string{got.ID}) || len(comments(*requests)) != 0 {
		t.Fatalf("started %v, comments %q", workflowsOf(in).started, comments(*requests))
	}
	// under writers the same default entry takes anyone's issue
	in, store, _ = laneIntake(t, "maintenance", maintenanceLanes)
	post(t, in, "issues", issueOpenedBody(8, "outsider", []string{"bug"}, "It breaks", "body"))
	if got := card(store, 8); got.Status != "queued" || got.Lane != "ready" || got.Process != "maintenance" {
		t.Fatalf("maintenance: card = %+v, want queued in ready", got)
	}
}

// Taking backlog off a card leaves it no lane label, which asks for ready:
// the sender is judged; an outsider's removal is refused, a writer's
// starts the card.
func TestLabelRemovalIntoReadyJudged(t *testing.T) {
	in, store, requests := laneIntake(t, "spec", specLanes)
	store.tasks["t-c"] = Task{ID: "t-c", Kind: "agent", Status: "backlog", Lane: "backlog", Process: "spec", Repository: "sf/ai-software-factory-code",
		IssueNumber: 9, Author: "sebastian", AuthorWriter: true}
	setLabels(t, in, 9, []string{"state/backlog"})
	post(t, in, "issue_label", labelEvent(9, "sebastian", []string{"state/backlog"}, "outsider"))
	if got := store.tasks["t-c"]; got.Status != "backlog" || got.Lane != "backlog" {
		t.Fatalf("after the outsider: %+v", got)
	}
	if got := issueLabels(t, in.Forgejo, 9); !slices.Equal(got, []string{"state/backlog", "backlog"}) {
		t.Fatalf("labels = %v, want backlog restored", got)
	}
	want := "factoryd: lane ready takes a card from writers of sf/ai-software-factory-code; outsider is none. The card stays in backlog."
	if got := comments(*requests); !slices.Equal(got, []string{want}) {
		t.Fatalf("comments = %q", got)
	}
	post(t, in, "issue_label", labelEvent(9, "sebastian", []string{"state/backlog"}, "sebastian"))
	if got := store.tasks["t-c"]; got.Status != "queued" || got.Lane != "ready" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) ||
		got.SHA != "sha-main-head" || got.Gate != GateIntake {
		t.Fatalf("after the writer: %+v, want queued in ready", got)
	}
	if !slices.Equal(workflowsOf(in).started, []string{"t-c"}) {
		t.Fatalf("started %v", workflowsOf(in).started)
	}
}

// A refused label event restores the backlog label as the bot and comments
// once; the bot's own label event that follows judges nothing.
func TestRefusedEntryRestoresBacklog(t *testing.T) {
	in, store, requests := laneIntake(t, "maintenance", maintenanceLanes)
	store.tasks["t-w"] = Task{ID: "t-w", Kind: "agent", Status: "backlog", Lane: "backlog", Process: "maintenance", Repository: "sf/ai-software-factory-code",
		IssueNumber: 12, Author: "outsider"}
	store.tasks["t-w-presort"] = Task{ID: "t-w-presort", Kind: "agent", Status: "done", Parent: "t-w", Source: SourcePresort, Repository: "sf/ai-software-factory-code"}
	labels := []string{"improvement", "agent", "state/backlog"}
	setLabels(t, in, 12, labels)
	post(t, in, "issue_label", labelEvent(12, "outsider", labels, "outsider"))
	restored := issueLabels(t, in.Forgejo, 12)
	if !slices.Equal(restored, []string{"improvement", "state/backlog", "backlog"}) {
		t.Fatalf("labels = %v, want agent off and backlog on", restored)
	}
	post(t, in, "issue_label", labelEvent(12, "outsider", restored, "factory-agent"))
	want := "factoryd: lane ready takes a card from writers of sf/ai-software-factory-code; outsider is none. The card stays in backlog."
	if got := comments(*requests); !slices.Equal(got, []string{want}) {
		t.Fatalf("comments = %q, want one", got)
	}
	if got := store.tasks["t-w"]; got.Status != "backlog" || len(workflowsOf(in).started) != 0 {
		t.Fatalf("card = %+v, started %v", got, workflowsOf(in).started)
	}
}

// A non-writer's agent label moves nothing, and neither does a label event
// with no sender (the heal pass) or the bot's.
func TestMoveRefusedForNonWriter(t *testing.T) {
	in, store, _ := laneIntake(t, "maintenance", maintenanceLanes)
	store.tasks["t-w"] = Task{ID: "t-w", Kind: "agent", Status: "backlog", Lane: "backlog", Process: "maintenance", Repository: "sf/ai-software-factory-code",
		IssueNumber: 12, Author: "outsider"}
	store.tasks["t-w-presort"] = Task{ID: "t-w-presort", Kind: "agent", Status: "done", Parent: "t-w", Source: SourcePresort, Repository: "sf/ai-software-factory-code"}
	labels := []string{"improvement", "agent"}
	for _, sender := range []string{"outsider", "", "factory-agent"} {
		setLabels(t, in, 12, labels)
		post(t, in, "issue_label", labelEvent(12, "outsider", labels, sender))
		if got := store.tasks["t-w"]; got.Status != "backlog" || got.Lane != "backlog" || got.SHA != "" {
			t.Fatalf("sender %q: card = %+v", sender, got)
		}
	}
	if err := in.intakeIssue(context.Background(), "heal pass", "sf/ai-software-factory-code", "", webhookIssue{Index: 12,
		Poster: webhookUser{UserName: "outsider"}, Labels: []webhookLabel{{Name: "improvement"}, {Name: "agent"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if got := store.tasks["t-w"]; got.Status != "backlog" || len(workflowsOf(in).started) != 0 {
		t.Fatalf("card = %+v, started %v", got, workflowsOf(in).started)
	}
	post(t, in, "issue_label", labelEvent(12, "outsider", labels, "sebastian"))
	if got := store.tasks["t-w"]; got.Status != "queued" || got.Lane != "ready" || !slices.Equal(workflowsOf(in).started, []string{"t-w"}) {
		t.Fatalf("after the writer: %+v, started %v", got, workflowsOf(in).started)
	}
}

// Where the approvers move ready, a writer's move is refused and an
// approver's starts the card.
func TestMoveApproversLane(t *testing.T) {
	in, store, requests := laneIntake(t, "gated", approversLanes)
	store.tasks["t-a"] = Task{ID: "t-a", Kind: "agent", Status: "backlog", Lane: "backlog", Process: "gated", Repository: "sf/ai-software-factory-code",
		IssueNumber: 14, Author: "outsider"}
	labels := []string{"backlog", "agent"}
	setLabels(t, in, 14, labels)
	post(t, in, "issue_label", labelEvent(14, "outsider", labels, "sebastian"))
	if got := store.tasks["t-a"]; got.Status != "backlog" {
		t.Fatalf("after the writer: %+v", got)
	}
	want := "factoryd: lane ready takes a card from the approvers; sebastian is none. The card stays in backlog."
	if got := comments(*requests); !slices.Equal(got, []string{want}) {
		t.Fatalf("comments = %q", got)
	}
	post(t, in, "issue_label", labelEvent(14, "outsider", labels, "alice"))
	if got := store.tasks["t-a"]; got.Status != "queued" || got.Lane != "ready" || !slices.Equal(workflowsOf(in).started, []string{"t-a"}) {
		t.Fatalf("after the approver: %+v, started %v", got, workflowsOf(in).started)
	}
}

// An outside account's plan label is off its new issue, which then takes
// the default entry into ready.
func TestOutsiderPlanLabelStripped(t *testing.T) {
	in, store, _ := laneIntake(t, "spec", specLanes)
	setLabels(t, in, 15, []string{"plan"})
	post(t, in, "issues", issueOpenedBody(15, "outsider", []string{"plan"}, "Build it", "body"))
	if got := card(store, 15); got.Lane != "ready" || got.Status != "queued" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) {
		t.Fatalf("card = %+v, want the default entry into ready", got)
	}
	if got := issueLabels(t, in.Forgejo, 15); slices.Contains(got, "plan") {
		t.Fatalf("labels = %v, want plan off", got)
	}
	// a writer's plan label starts the planner's lane
	setLabels(t, in, 16, []string{"plan"})
	post(t, in, "issues", issueOpenedBody(16, "sebastian", []string{"plan"}, "Build it", "body"))
	if got := card(store, 16); got.Lane != "plan" || got.Status != "queued" || !slices.Equal(got.RequiredCapabilities, []string{"planner"}) {
		t.Fatalf("writer's plan card = %+v, want queued in plan as planner", got)
	}
}

// A plan's card is the bot's issue with a task row: a writer's label moves
// it into ready, and never into plan (plans do not nest).
func TestLabelOnBotIssueMovesChild(t *testing.T) {
	in, store, requests := laneIntake(t, "spec", specLanes)
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "done", Lane: "plan", Process: "spec", Repository: "sf/ai-software-factory-code", IssueNumber: 1}
	store.tasks["t-child"] = Task{ID: "t-child", Kind: "agent", Status: "backlog", Lane: "backlog", Process: "spec", Repository: "sf/ai-software-factory-code",
		IssueNumber: 20, Parent: "t-plan", Source: "plan", Author: "alice", AuthorWriter: true}
	labels := []string{"backlog", "state/backlog", "plan"}
	setLabels(t, in, 20, labels)
	post(t, in, "issue_label", labelEvent(20, "factory-agent", labels, "sebastian"))
	if got := store.tasks["t-child"]; got.Status != "backlog" {
		t.Fatalf("into plan: %+v", got)
	}
	want := "factoryd: lane plan takes no card of a plan: plans do not nest. The card stays in backlog."
	if got := comments(*requests); !slices.Equal(got, []string{want}) {
		t.Fatalf("comments = %q", got)
	}
	labels = []string{"backlog", "state/backlog", "agent"}
	setLabels(t, in, 20, labels)
	post(t, in, "issue_label", labelEvent(20, "factory-agent", labels, "sebastian"))
	if got := store.tasks["t-child"]; got.Status != "queued" || got.Lane != "ready" || !slices.Equal(workflowsOf(in).started, []string{"t-child"}) {
		t.Fatalf("into ready: %+v, started %v", got, workflowsOf(in).started)
	}
	// an unlinked bot issue is still ignored
	post(t, in, "issues", issueOpenedBody(21, "factory-agent", []string{"agent"}, "self-filed", "body"))
	if _, ok, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", 21); ok {
		t.Fatal("an unlinked bot issue became a card")
	}
}

// A bot issue of the human lane (an alert's on agents none, the loader's)
// has a row no label changes.
func TestLabelOnHumanKindBotIssueChangesNothing(t *testing.T) {
	in, store, requests := laneIntake(t, "maintenance", maintenanceLanes)
	store.tasks["t-h"] = Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 22, Source: SourceAlert}
	before := len(*requests)
	post(t, in, "issue_label", labelEvent(22, "factory-agent", []string{"human", "agent"}, "sebastian"))
	post(t, in, "issue_label", labelEvent(22, "factory-agent", []string{"agent"}, "sebastian"))
	if got := store.tasks["t-h"]; got.Kind != "human" || got.Status != "waiting-human" || got.Lane != "" {
		t.Fatalf("card = %+v", got)
	}
	if len(workflowsOf(in).started) != 0 || len(*requests) != before {
		t.Fatalf("started %v, forgejo calls %v", workflowsOf(in).started, (*requests)[before:])
	}
}

// A plan label on a maintenance project reads as backlog: the card waits
// with its write-up, and no entry was refused.
func TestPlanLabelOnMaintenanceIsBacklog(t *testing.T) {
	in, store, requests := laneIntake(t, "maintenance", maintenanceLanes)
	setLabels(t, in, 23, []string{"plan"})
	post(t, in, "issues", issueOpenedBody(23, "sebastian", []string{"plan"}, "Build it", "body"))
	got := card(store, 23)
	if got.Status != "backlog" || got.Lane != "backlog" || got.Process != "maintenance" {
		t.Fatalf("card = %+v, want a backlog card", got)
	}
	writeUp, ok, _ := store.ReportOf(context.Background(), got.ID, SourcePresort)
	if !ok || !slices.Equal(writeUp.RequiredCapabilities, []string{"analyzer"}) || !slices.Equal(workflowsOf(in).started, []string{writeUp.ID}) {
		t.Fatalf("write-up %+v %v, started %v: want the analyzer's", writeUp, ok, workflowsOf(in).started)
	}
	if len(comments(*requests)) != 0 || !slices.Contains(issueLabels(t, in.Forgejo, 23), "plan") {
		t.Fatalf("comments %q, labels %v: nothing refused", comments(*requests), issueLabels(t, in.Forgejo, 23))
	}
	// a spec backlog, which names no write_up, opens none
	in, store, _ = laneIntake(t, "spec", specLanes)
	post(t, in, "issues", issueOpenedBody(24, "sebastian", []string{"backlog"}, "Later", "body"))
	if got := card(store, 24); got.Status != "backlog" || len(store.tasks) != 1 || len(workflowsOf(in).started) != 0 {
		t.Fatalf("spec backlog: %+v, %d tasks", got, len(store.tasks))
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

// FileTask's agent tasks (the API's, SigNoz alerts') take the default
// entry into ready of the project's process with ready's class; where the
// approvers move ready, the bot's card waits in the backlog.
func TestFileTaskEntersReady(t *testing.T) {
	ctx := context.Background()
	p := seedProject("sf/r")
	f, labels := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	got, ref, err := FileAgentTask(ctx, store, f, wf, p, "Fix it", "body", []string{"agent"}, Task{ID: "t-api", Source: "api", Author: "factory-agent"})
	if err != nil || ref.Number != 40 {
		t.Fatal(ref, err)
	}
	if got.Kind != "agent" || got.Status != "queued" || got.Lane != "ready" || got.Process != "maintenance" ||
		!slices.Equal(got.RequiredCapabilities, []string{"coder"}) || !slices.Equal(store.tasks["t-api"].RequiredCapabilities, []string{"coder"}) {
		t.Fatalf("task = %+v", got)
	}
	if !slices.Equal(*labels, []string{"agent", "state/queued"}) || !slices.Equal(wf.started, []string{"t-api"}) {
		t.Fatalf("labels %v, started %v", *labels, wf.started)
	}
	p.Process, p.Lanes = "gated", approversLanes
	store, wf = newFakeStore(), &fakeWorkflows{}
	got, _, err = FileAgentTask(ctx, store, f, wf, p, "It fires", "body", []string{"bug"}, Task{ID: "t-alert", Source: SourceAlert})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "backlog" || got.Lane != "backlog" || got.Process != "gated" || len(got.RequiredCapabilities) != 0 || store.tasks["t-alert"].Gate != "" {
		t.Fatalf("task = %+v, want a backlog card", got)
	}
	if !slices.Equal(*labels, []string{"backlog", "state/backlog"}) || len(wf.started) != 0 {
		t.Fatalf("labels %v, started %v", *labels, wf.started)
	}
	p.Lanes = nil
	if _, _, err := FileAgentTask(ctx, store, f, wf, p, "x", "y", nil, Task{ID: "t-none"}); err == nil ||
		err.Error() != "project sf-r follows no process with a lane ready" {
		t.Fatalf("no ready lane: %v", err)
	}
}

// A roll runs ready's class with no lane and no process; a human task has
// neither either.
func TestRollHasNoLane(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	got, _, err := FileAgentTask(ctx, store, f, wf, seedProject("sf/r"), "roll abc", "body", nil, Task{ID: "t-roll", Source: SourceRoll})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || got.Lane != "" || got.Process != "" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) ||
		!slices.Equal(*labels, []string{"state/queued"}) || !slices.Equal(wf.started, []string{"t-roll"}) {
		t.Fatalf("roll = %+v, labels %v, started %v", got, *labels, wf.started)
	}
	got, _, err = FileTask(ctx, store, f, wf, Repo{Owner: "sf", Name: "r"}, "config", "body", []string{"human"}, Task{ID: "t-h", Kind: "human", Source: SourceConfig})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting-human" || got.Lane != "" || got.Process != "" || len(got.RequiredCapabilities) != 0 || len(wf.started) != 1 {
		t.Fatalf("human task = %+v, started %v", got, wf.started)
	}
}

// The heal pass knows no sender: a backlog card with no lane label, which
// reads as ready, stays where it is.
func TestHealNoLaneLabelIsNoOp(t *testing.T) {
	in, store, requests := laneIntake(t, "spec", specLanes)
	store.tasks["t-c"] = Task{ID: "t-c", Kind: "agent", Status: "backlog", Lane: "backlog", Process: "spec", Repository: "sf/ai-software-factory-code",
		IssueNumber: 30, Parent: "t-plan", Source: "plan"}
	before := len(*requests)
	if err := in.intakeIssue(context.Background(), "heal pass", "sf/ai-software-factory-code", "", webhookIssue{Index: 30,
		Poster: webhookUser{UserName: "factory-agent"}, Labels: []webhookLabel{{Name: "state/backlog"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if got := store.tasks["t-c"]; got.Status != "backlog" || len(workflowsOf(in).started) != 0 || len(*requests) != before {
		t.Fatalf("card = %+v, started %v, calls %v", got, workflowsOf(in).started, (*requests)[before:])
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
		{Task{Parent: "t-p", Source: SourcePresort}, true},
		{Task{Parent: "t-plan", Source: "plan"}, false},
		{Task{Source: "webhook"}, false},
	} {
		if got := tc.task.Report(); got != tc.want {
			t.Errorf("%+v: Report = %v, want %v", tc.task, got, tc.want)
		}
	}
	child := Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", IssueNumber: 3, Parent: "t-plan", Source: "plan",
		Lane: "ready", Process: "maintenance", RequiredCapabilities: []string{policy.ClassCoder}}
	r, _, _ := newActivityRunner(t, child, &prForgejo{})
	_, err := execActivity(t, r, ActivityRunReport, "t1", "m", int64(0))
	if err == nil || !strings.Contains(err.Error(), "task t1 is no report task") {
		t.Fatalf("RunReport on a plan's card: %v", err)
	}
}

// labelsForgejo is a repository's labels and open issues: it answers the
// labels step's reads and records every write in order.
type labelsForgejo struct {
	labels []RepoLabel
	issues []string // open issues, as Forgejo lists them
	calls  []string // "METHOD path body"
}

func (lf *labelsForgejo) srv(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		const base = "/api/v1/repos/sf/r"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base+"/labels":
			items := make([]string, len(lf.labels))
			for i, l := range lf.labels {
				b, _ := json.Marshal(l)
				items[i] = string(b)
			}
			servePage(w, r, 50, items...)
			return
		case r.Method == http.MethodGet && r.URL.Path == base+"/issues":
			servePage(w, r, 50, lf.issues...)
			return
		}
		lf.calls = append(lf.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, base)+" "+strings.TrimSpace(string(body)))
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/labels/"):
			var p struct {
				Name string `json:"name"`
			}
			json.Unmarshal(body, &p) // #nosec G104 -- test stub
			for i := range lf.labels {
				if r.URL.Path == base+"/labels/"+strconv.FormatInt(lf.labels[i].ID, 10) {
					lf.labels[i].Name = p.Name
				}
			}
			io.WriteString(w, `{}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/labels/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == base+"/labels":
			var p RepoLabel
			json.Unmarshal(body, &p) // #nosec G104 -- test stub
			p.ID = int64(100 + len(lf.labels))
			lf.labels = append(lf.labels, p)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/labels"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// The labels step renames state/suggested to state/backlog in place before
// it creates anything, so every issue carrying it follows; after a
// half-done run with both, the old label's open issues move to the new one
// and the old is deleted. The lane labels of the project's process join
// BootLabels: plan only where the process has a plan lane.
func TestStateSuggestedRenamed(t *testing.T) {
	ctx := context.Background()
	all := func(extra ...string) []RepoLabel {
		var out []RepoLabel
		for i, name := range slices.Sorted(maps.Keys(BootLabels)) {
			if name != "state/backlog" {
				out = append(out, RepoLabel{ID: int64(10 + i), Name: name})
			}
		}
		for i, name := range extra {
			out = append(out, RepoLabel{ID: int64(5 + i), Name: name})
		}
		return out
	}

	lf := &labelsForgejo{labels: all("state/suggested", "backlog")}
	change, err := lf.srv(t).EnsureProjectLabels(ctx, "sf", "r", specLanes)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`PATCH /labels/5 {"name":"state/backlog"}`, `POST /labels {"color":"#5319e7","name":"plan"}`}; !slices.Equal(lf.calls, want) {
		t.Fatalf("calls = %q, want %q", lf.calls, want)
	}
	if change != "renamed state/suggested to state/backlog; created plan" {
		t.Fatalf("change = %q", change)
	}

	lf = &labelsForgejo{labels: all("state/suggested", "state/backlog", "backlog"), issues: []string{
		`{"number":3,"labels":[{"name":"bug"},{"name":"state/suggested"}]}`,
		`{"number":4,"labels":[{"name":"state/backlog"}]}`,
		`{"number":7,"labels":[{"name":"state/suggested"},{"name":"state/backlog"}]}`,
	}}
	change, err = lf.srv(t).EnsureProjectLabels(ctx, "sf", "r", maintenanceLanes)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`PUT /issues/3/labels {"labels":["bug","state/backlog"]}`, `PUT /issues/7/labels {"labels":["state/backlog"]}`, `DELETE /labels/5 `}; !slices.Equal(lf.calls, want) {
		t.Fatalf("calls = %q, want %q", lf.calls, want)
	}
	if change != "moved 2 issues from state/suggested to state/backlog and deleted state/suggested" {
		t.Fatalf("change = %q", change)
	}

	lf = &labelsForgejo{labels: append(all("backlog"), RepoLabel{ID: 9, Name: "state/backlog"})}
	if change, err = lf.srv(t).EnsureProjectLabels(ctx, "sf", "r", maintenanceLanes); err != nil || change != Unchanged || len(lf.calls) != 0 {
		t.Fatalf("old absent: %q, %v, calls %q", change, err, lf.calls)
	}
}
