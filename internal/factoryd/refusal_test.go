package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// refusalForgejo is a Forgejo holding one open issue, #67 of the code
// repository, with its labels and comments kept across calls.
type refusalForgejo struct {
	mu       sync.Mutex
	body     string
	labels   []string
	comments []string
}

func (f *refusalForgejo) serve(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case isWriterRead(p):
			serveWriters(t, w, r)
		case p == "/api/v1/repos/sf/ai-software-factory-code":
			io.WriteString(w, `{"default_branch":"main"}`)
		case strings.HasSuffix(p, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"sha"}}`)
		case p == "/api/v1/repos/sf/ai-software-factory-code/issues" && r.Method == http.MethodGet:
			var ls []map[string]string
			for _, l := range f.labels {
				ls = append(ls, map[string]string{"name": l})
			}
			issue, _ := json.Marshal(map[string]any{"number": 67, "title": "Plan", "body": f.body, "labels": ls, "user": map[string]string{"login": "sebastian"}})
			servePage(w, r, 50, string(issue))
		case strings.HasSuffix(p, "/labels") && r.Method == http.MethodGet:
			var ls []map[string]string
			for _, l := range f.labels {
				ls = append(ls, map[string]string{"name": l})
			}
			json.NewEncoder(w).Encode(ls)
		case strings.HasSuffix(p, "/labels") && r.Method == http.MethodPut:
			var in struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&in)
			f.labels = in.Labels
			io.WriteString(w, `[]`)
		case strings.HasSuffix(p, "/comments") && r.Method == http.MethodGet:
			var cs []map[string]string
			for _, c := range f.comments {
				cs = append(cs, map[string]string{"body": c})
			}
			json.NewEncoder(w).Encode(cs)
		case strings.HasSuffix(p, "/comments") && r.Method == http.MethodPost:
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, in.Body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, p)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"}
}

func (f *refusalForgejo) commentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments)
}

// An issue that names a roadmap item an open card builds gets one comment
// naming that card and the state/refused label; further heal passes and a
// restarted intake over the same store and Forgejo add none, and the
// heal pass no longer reads the issue.
func TestRefusedIssueIsToldOnceAndLeftBe(t *testing.T) {
	fj := &refusalForgejo{body: "Build 66.\n\nRoadmap: #66", labels: []string{"agent"}}
	forgejo := fj.serve(t)
	store := newFakeStore()
	store.tasks["t-holder"] = Task{ID: "t-holder", Repository: "sf/ai-software-factory-code", Status: "running", Kind: "agent", Track: "direct", RoadmapItem: 66, IssueNumber: 1547}
	intake := func() *Intake {
		return &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Policy: testPolicy,
			Projects: projectList{seedProject("sf/ai-software-factory-code")}}
	}
	in := intake()
	for range 3 {
		if err := in.ResyncIssues(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fj.commentCount() != 1 {
		t.Fatalf("comments = %q, want exactly one", fj.comments)
	}
	if c := fj.comments[0]; !strings.Contains(c, "card t-holder already builds roadmap item 66") {
		t.Errorf("comment = %q, want it to name card t-holder and item 66", c)
	}
	if !slices.Contains(fj.labels, StateRefused) || !slices.Contains(fj.labels, "agent") {
		t.Errorf("labels = %v, want agent and %s", fj.labels, StateRefused)
	}
	if len(store.tasks) != 1 {
		t.Errorf("tasks = %d, want the holder only", len(store.tasks))
	}
	if err := intake().ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a webhook for the edited issue tries again: the same reason, no second comment
	body := issueOpenedBody(67, "sebastian", fj.labels, "Plan", fj.body)
	if rr := postWebhookTo(intake(), "issues", body); rr != http.StatusOK {
		t.Fatalf("edited webhook status = %d", rr)
	}
	if fj.commentCount() != 1 {
		t.Errorf("comments after a restart and an edit = %q, want still one", fj.comments)
	}
	// the holder goes: the heal pass still leaves the labelled issue be,
	// and an edit files its card and clears the label
	delete(store.tasks, "t-holder")
	if err := intake().ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("the heal pass filed %d cards for the refused issue", len(store.tasks))
	}
	if code := postWebhookTo(intake(), "issues", body); code != http.StatusOK || len(store.tasks) != 1 {
		t.Fatalf("the edit: status %d, %d cards; want a card", code, len(store.tasks))
	}
	if slices.Contains(fj.labels, StateRefused) || !slices.Contains(fj.labels, "state/queued") {
		t.Errorf("labels after the card = %v, want state/queued in place of %s", fj.labels, StateRefused)
	}
	got, err := store.IntakeRefusals(context.Background(), "sf/ai-software-factory-code", 67)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Reason, "t-holder") {
		t.Errorf("recorded refusals = %+v, %v; want one naming t-holder", got, err)
	}
}

// A second reason on the same issue is told too: the key is the reason.
func TestRefusalKeyDiffersByHolder(t *testing.T) {
	a := &Refusal{Kind: RefusalRoadmap, Holder: "t-a", Item: 1}
	b := &Refusal{Kind: RefusalRoadmap, Holder: "t-b", Item: 1}
	if a.Key() == b.Key() {
		t.Errorf("keys %q and %q are one reason", a.Key(), b.Key())
	}
}

// The comment is text a person reads: a credential in the refusal's
// message is masked.
func TestRefusalTextMasksCredentials(t *testing.T) {
	in := &Intake{}
	r := &Refusal{Kind: RefusalRoadmap, Holder: "https://user:pass@host/x Authorization: Bearer abcdef0123456789abcdef", Item: 66, Repository: "sf/r"}
	text := in.refusalText(r)
	for _, leak := range []string{"user:pass", "abcdef0123456789abcdef"} {
		if strings.Contains(text, leak) {
			t.Errorf("text %q leaks %q", text, leak)
		}
	}
	if !strings.Contains(text, "[redacted]@host") || !strings.Contains(text, "Bearer [redacted]") {
		t.Errorf("text = %q, want the userinfo and the bearer value masked", text)
	}
}

func TestAskedRefile(t *testing.T) {
	for body, want := range map[string]int64{
		"Roadmap: #66\nRefile of #1547":      1547,
		"Roadmap: #66\n\nrefile of #1646.":   1646,
		"nothing here":                       0,
		"a refile of #12 in the middle line": 0,
	} {
		if got := AskedRefile(body); got != want {
			t.Errorf("AskedRefile(%q) = %d, want %d", body, got, want)
		}
	}
}

func postWebhookTo(in *Intake, eventType string, body []byte) int {
	in.Secret = webhookSecret
	return postWebhook(in, eventType, body, sign(webhookSecret, body)).Code
}

// A closed or failed card frees its roadmap item for the issue that names
// it as its refile, and the refile takes the item over; any other card,
// open or done, and any other issue, still hold it.
func TestPGStoreRefileTakesTheRoadmapItemOver(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	const repo = "sf/ai-software-factory-code"
	card := func(id, status string, issue int64, item int, body string) Task {
		return Task{ID: id, Status: status, Repository: repo, Kind: "agent", Source: "webhook", Track: "direct", RoadmapItem: item, IssueNumber: issue, Objective: body}
	}
	for _, c := range []Task{
		card("t-closed", "closed", 1547, 66, "plan"),
		card("t-failed", "failed", 1646, 342, "plan"),
		card("t-done", "done", 1700, 400, "plan"),
		card("t-open", "running", 1701, 401, "plan"),
	} {
		if err := s.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(err error, holder string) bool {
		var r *Refusal
		return errors.As(err, &r) && r.Kind == RefusalRoadmap && r.Holder == holder
	}
	// no refile line: the closed card still holds item 66
	if err := s.CreateTask(ctx, card("t-a", "queued", 1667, 66, "plan\n\nRoadmap: #66")); !refused(err, "t-closed") {
		t.Fatalf("a filing without a refile line: %v, want the closed card named", err)
	}
	// a refile line naming another issue frees nothing
	if err := s.CreateTask(ctx, card("t-b", "queued", 1667, 66, "plan\n\nRefile of #1646")); !refused(err, "t-closed") {
		t.Fatalf("a refile of another issue: %v, want the closed card named", err)
	}
	// RoadmapHolder, the console's check, agrees
	if h, ok, err := s.RoadmapHolder(ctx, repo, 66, 0); err != nil || !ok || h != "t-closed" {
		t.Fatalf("RoadmapHolder without a refile = %q %v %v", h, ok, err)
	}
	if _, ok, err := s.RoadmapHolder(ctx, repo, 66, 1547); err != nil || ok {
		t.Fatalf("RoadmapHolder of its refile = %v %v, want the item free", ok, err)
	}
	// done and open cards are not freed by a refile line
	if err := s.CreateTask(ctx, card("t-c", "queued", 1800, 400, "Refile of #1700")); !refused(err, "t-done") {
		t.Fatalf("refile of a done card: %v", err)
	}
	if err := s.CreateTask(ctx, card("t-d", "queued", 1801, 401, "Refile of #1701")); !refused(err, "t-open") {
		t.Fatalf("refile of an open card: %v", err)
	}
	// the refiles of the closed and the failed card file and take their items
	if err := s.CreateTask(ctx, card("t-1667", "queued", 1667, 66, "plan\n\nRoadmap: #66\nRefile of #1547")); err != nil {
		t.Fatalf("the refile of the closed card: %v", err)
	}
	if err := s.CreateTask(ctx, card("t-1675", "queued", 1675, 342, "plan\n\nRoadmap: #342\nrefile of #1646")); err != nil {
		t.Fatalf("the refile of the failed card: %v", err)
	}
	for id, want := range map[string]int{"t-closed": 0, "t-failed": 0, "t-1667": 66, "t-1675": 342} {
		got, _, err := s.Task(ctx, id)
		if err != nil || got.RoadmapItem != want {
			t.Errorf("%s: roadmap item %d, %v; want %d", id, got.RoadmapItem, err, want)
		}
	}
	// the item is held again: by the refile
	if err := s.CreateTask(ctx, card("t-e", "queued", 1900, 66, "plan")); !refused(err, "t-1667") {
		t.Fatalf("a third filing of item 66: %v, want the refile named", err)
	}
}

// The refusals of an issue are kept per reason, oldest first, and a
// second record of a reason changes nothing.
func TestPGStoreIntakeRefusals(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE intake_refusals`); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]string{{"roadmap t-a", "first"}, {"roadmap t-a", "again"}, {"roadmap t-b", "second"}} {
		if err := s.RecordRefusal(ctx, "sf/r", 5, r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.IntakeRefusals(ctx, "sf/r", 5)
	want := []IntakeRefusal{{"roadmap t-a", "first"}, {"roadmap t-b", "second"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("refusals = %+v, %v; want %+v", got, err, want)
	}
	if got, err := s.IntakeRefusals(ctx, "sf/r", 6); err != nil || len(got) != 0 {
		t.Fatalf("another issue's refusals = %+v, %v", got, err)
	}
}
