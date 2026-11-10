package factoryd

import (
	"cmp"
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
	"time"
)

// refusalForgejo is a Forgejo holding one open issue, #67 of the code
// repository, with its labels and comments kept across calls.
type refusalForgejo struct {
	mu       sync.Mutex
	body     string
	poster   string
	labels   []string
	comments []fjComment
}

type fjComment struct{ user, body string }

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
			issue, _ := json.Marshal(map[string]any{"number": 67, "title": "Plan", "body": f.body, "labels": ls, "user": map[string]string{"login": cmp.Or(f.poster, "sebastian")}})
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
			var cs []map[string]any
			for _, c := range f.comments {
				cs = append(cs, map[string]any{"body": c.body, "user": map[string]string{"login": c.user}})
			}
			json.NewEncoder(w).Encode(cs)
		case strings.HasSuffix(p, "/comments") && r.Method == http.MethodPost:
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, fjComment{"factory-agent", in.Body})
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, p)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"}
}

func (f *refusalForgejo) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		out = append(out, c.body)
	}
	return out
}

func (f *refusalForgejo) commentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments)
}

const refusalRepo = "sf/ai-software-factory-code"

func refusalIntake(store Store, forgejo *Forgejo) *Intake {
	return &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Policy: testPolicy,
		Projects: projectList{seedProject(refusalRepo)}}
}

func edited(t *testing.T, in *Intake, fj *refusalForgejo) {
	t.Helper()
	body := issueOpenedBody(67, cmp.Or(fj.poster, "sebastian"), fj.labels, "Plan", fj.body)
	if code := postWebhookTo(in, "issues", body); code != http.StatusOK {
		t.Fatalf("edited webhook status = %d", code)
	}
}

// An issue that names a roadmap item an open card builds gets one comment
// naming that card and the way forward, and the state/refused label;
// further heal passes and a restarted intake over the same store and
// Forgejo add none. Once the holder closes, an edit without the refile
// line is refused again, now told how to refile; an edit with the line
// files the card and replaces the label.
func TestRefusedIssueIsToldOnceAndLeftBe(t *testing.T) {
	fj := &refusalForgejo{body: "Build 66.\n\nRoadmap: #66", labels: []string{"agent"}}
	forgejo := fj.serve(t)
	store := newFakeStore()
	store.tasks["t-holder"] = Task{ID: "t-holder", Repository: refusalRepo, Status: "running", Kind: "agent", Track: "direct", RoadmapItem: 66, IssueNumber: 1547}
	in := refusalIntake(store, forgejo)
	for range 3 {
		if err := in.ResyncIssues(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	open := "The factory made no card for this issue: card t-holder already builds roadmap item 66 of sf/ai-software-factory-code. " +
		"Wait for it to end, or close it and add the line \"Refile of #1547\" to this issue, then edit this issue."
	if got := fj.bodies(); len(got) != 1 || !strings.HasPrefix(got[0], open+"\n\n<!-- factoryd refusal") {
		t.Fatalf("comments = %q, want exactly one: %s", got, open)
	}
	if !slices.Contains(fj.labels, StateRefused) || !slices.Contains(fj.labels, "agent") {
		t.Errorf("labels = %v, want agent and %s", fj.labels, StateRefused)
	}
	if len(store.tasks) != 1 {
		t.Errorf("tasks = %d, want the holder only", len(store.tasks))
	}
	// a restart and an edit under the same reason add no comment
	in = refusalIntake(store, forgejo)
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	edited(t, in, fj)
	if fj.commentCount() != 1 {
		t.Errorf("comments after a restart and an edit = %q, want still one", fj.bodies())
	}
	// the holder closes: the heal pass leaves the labelled issue be, an edit
	// without the refile line is refused again with the new instruction
	h := store.tasks["t-holder"]
	h.Status = "closed"
	store.tasks["t-holder"] = h
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("the heal pass filed a card for the refused issue")
	}
	edited(t, in, fj)
	ended := "The factory made no card for this issue: card t-holder already builds roadmap item 66 of sf/ai-software-factory-code. " +
		"Add the line \"Refile of #1547\" to this issue, then edit this issue."
	if got := fj.bodies(); len(store.tasks) != 1 || len(got) != 2 || !strings.HasPrefix(got[1], ended+"\n\n") {
		t.Fatalf("after the close: %d cards, comments %q; want the holder only and a second comment: %s", len(store.tasks), got, ended)
	}
	// the refile line files the card, and the state label follows it
	fj.body += "\nRefile of #1547"
	edited(t, in, fj)
	if len(store.tasks) != 2 {
		t.Fatalf("the refile filed %d cards, want 2", len(store.tasks))
	}
	if slices.Contains(fj.labels, StateRefused) || !slices.Contains(fj.labels, "state/queued") {
		t.Errorf("labels after the card = %v, want state/queued in place of %s", fj.labels, StateRefused)
	}
	if fj.commentCount() != 2 {
		t.Errorf("the refile added a comment: %q", fj.bodies())
	}
}

// Each holder state names its own way forward, whole.
func TestRefusalTextByHolderState(t *testing.T) {
	in := &Intake{}
	head := "The factory made no card for this issue: card t-h already builds roadmap item 66 of sf/r. "
	for status, want := range map[string]string{
		"running": head + "Wait for it to end, or close it and add the line \"Refile of #1547\" to this issue, then edit this issue.",
		"closed":  head + "Add the line \"Refile of #1547\" to this issue, then edit this issue.",
		"failed":  head + "Add the line \"Refile of #1547\" to this issue, then edit this issue.",
		"done":    head + "That item is built. Cite a new roadmap item or remove the Roadmap line, then edit this issue.",
	} {
		r := &Refusal{Kind: RefusalRoadmap, Holder: "t-h", Status: status, HolderIssue: 1547, Item: 66, Repository: "sf/r"}
		if got := in.refusalText(r); got != want {
			t.Errorf("%s: text %q, want %q", status, got, want)
		}
	}
}

// A refusal posted but not yet recorded (a crash between the two) is not
// posted again; a marker planted by another account does not stand for
// the factory's comment.
func TestRefusalCrashBetweenPostAndRecord(t *testing.T) {
	fj := &refusalForgejo{body: "Build 66.\n\nRoadmap: #66", labels: []string{"agent"}}
	forgejo := fj.serve(t)
	store := newFakeStore()
	store.tasks["t-holder"] = Task{ID: "t-holder", Repository: refusalRepo, Status: "running", Kind: "agent", Track: "direct", RoadmapItem: 66, IssueNumber: 1547}
	r := &Refusal{Kind: RefusalRoadmap, Holder: "t-holder", Status: "running"}
	marker := "\n\n<!-- factoryd refusal " + r.Key() + " -->"
	fj.comments = []fjComment{{"mallory", "> " + marker}}
	in := refusalIntake(store, forgejo)
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fj.commentCount() != 2 {
		t.Fatalf("a planted marker suppressed the comment: %q", fj.bodies())
	}
	// the factory's own comment stands, its row is missing
	store.refusals = nil
	fj.labels = []string{"agent"}
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fj.commentCount() != 2 {
		t.Errorf("a second comment after the crash: %q", fj.bodies())
	}
	if got, _ := store.IntakeRefusals(context.Background(), refusalRepo, 67); len(got) != 1 {
		t.Errorf("the row was not recorded after the crash: %+v", got)
	}
}

// The heal pass leaves a refused issue for a day, and takes it up
// sooner when its text changed since the refusal (a lost edit event).
func TestHealPassRereadsARefusedIssue(t *testing.T) {
	fj := &refusalForgejo{body: "Build 66.\n\nRoadmap: #66", labels: []string{"agent"}}
	forgejo := fj.serve(t)
	store := newFakeStore()
	store.tasks["t-holder"] = Task{ID: "t-holder", Repository: refusalRepo, Status: "closed", Kind: "agent", Track: "direct", RoadmapItem: 66, IssueNumber: 1547}
	in := refusalIntake(store, forgejo)
	heal := func() {
		t.Helper()
		if err := in.ResyncIssues(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	heal()
	delete(store.tasks, "t-holder")
	heal()
	if len(store.tasks) != 0 {
		t.Fatalf("the heal pass took up a fresh refusal")
	}
	k := refusalRepo + " 67"
	store.refusals[k][0].At = time.Now().Add(-25 * time.Hour)
	heal()
	if len(store.tasks) != 1 {
		t.Fatalf("the heal pass left a day-old refusal: %d cards", len(store.tasks))
	}

	// a changed text is taken up at once
	fj2 := &refusalForgejo{body: "Build 66.\n\nRoadmap: #66", labels: []string{"agent"}}
	store2 := newFakeStore()
	store2.tasks["t-holder"] = Task{ID: "t-holder", Repository: refusalRepo, Status: "closed", Kind: "agent", Track: "direct", RoadmapItem: 66, IssueNumber: 1547}
	in = refusalIntake(store2, fj2.serve(t))
	heal()
	fj2.body += "\nRefile of #1547"
	heal()
	if len(store2.tasks) != 2 {
		t.Fatalf("the heal pass missed the edit: %d cards", len(store2.tasks))
	}
}

// The delivery that loses the one-card-per-issue race has no refusal to
// tell: the issue's card exists, and its labels stay the card's.
func TestIssueRaceTellsNothing(t *testing.T) {
	fj := &refusalForgejo{body: "Do it.", labels: []string{"agent"}}
	forgejo := fj.serve(t)
	store := &issueRaceStore{fakeStore: newFakeStore()}
	in := refusalIntake(store, forgejo)
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fj.commentCount() != 0 || slices.Contains(fj.labels, StateRefused) {
		t.Errorf("comments %q, labels %v: the race lost told the issue", fj.bodies(), fj.labels)
	}
	if w := in.Workflows.(*fakeWorkflows); !slices.Equal(w.started, []string{"t-winner"}) {
		t.Errorf("started = %v, want the winner's card", w.started)
	}
}

// issueRaceStore makes the winner's card at the loser's insert.
type issueRaceStore struct{ *fakeStore }

func (s *issueRaceStore) CreateTask(ctx context.Context, t Task) error {
	s.tasks["t-winner"] = Task{ID: "t-winner", Repository: t.Repository, Status: "queued", Kind: "agent", Track: "direct", IssueNumber: t.IssueNumber}
	return &Refusal{Kind: RefusalIssue, Holder: "t-winner", Status: "queued", IssueNo: t.IssueNumber, Repository: t.Repository}
}

// The Roadmap and refile lines count from a writer only: an outside
// account's card cites no item and frees no closed card's.
func TestOutsiderRoadmapAndRefileLinesAreIgnored(t *testing.T) {
	fj := &refusalForgejo{poster: "outsider", body: "Build 66.\n\nRoadmap: #66\nRefile of #1547", labels: nil}
	forgejo := fj.serve(t)
	store := newFakeStore()
	store.tasks["t-closed"] = Task{ID: "t-closed", Repository: refusalRepo, Status: "closed", Kind: "agent", Track: "direct", RoadmapItem: 66, IssueNumber: 1547}
	in := refusalIntake(store, forgejo)
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.tasks) != 2 {
		t.Fatalf("tasks = %d, want the outsider's card filed", len(store.tasks))
	}
	for id, task := range store.tasks {
		if id != "t-closed" && task.RoadmapItem != 0 {
			t.Errorf("the outsider's card cites roadmap item %d", task.RoadmapItem)
		}
	}
	if store.released["t-closed"] || store.tasks["t-closed"].RoadmapItem != 66 {
		t.Errorf("the closed card lost its item to an outsider")
	}
}

// A second reason on the same issue is told too: the key is the reason.
func TestRefusalKeyDiffersByHolder(t *testing.T) {
	a := &Refusal{Kind: RefusalRoadmap, Holder: "t-a", Status: "running", Item: 1}
	for _, b := range []*Refusal{{Kind: RefusalRoadmap, Holder: "t-b", Status: "running"}, {Kind: RefusalRoadmap, Holder: "t-a", Status: "closed"}} {
		if a.Key() == b.Key() {
			t.Errorf("keys %q and %q are one reason", a.Key(), b.Key())
		}
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
		"> Refile of #1547":                  0,
		"- Refile of #1547":                  1547,
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
	if h, ok, err := s.RoadmapHolder(ctx, repo, 66, 0); err != nil || !ok || h.ID != "t-closed" || h.Status != "closed" || h.IssueNumber != 1547 {
		t.Fatalf("RoadmapHolder without a refile = %+v %v %v", h, ok, err)
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
	for id, want := range map[string]int{"t-closed": 66, "t-failed": 342, "t-1667": 66, "t-1675": 342} {
		got, _, err := s.Task(ctx, id)
		if err != nil || got.RoadmapItem != want {
			t.Errorf("%s: roadmap item %d, %v; want %d", id, got.RoadmapItem, err, want)
		}
	}
	// the replaced cards keep the item they built and no longer hold it
	if h, ok, err := s.RoadmapHolder(ctx, repo, 66, 0); err != nil || !ok || h.ID != "t-1667" {
		t.Fatalf("RoadmapHolder after the takeover = %+v %v %v, want the refile", h, ok, err)
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
	for _, r := range [][3]string{{"roadmap t-a", "first", "h1"}, {"roadmap t-b", "second", "h1"}, {"roadmap t-a", "again", "h2"}} {
		if err := s.RecordRefusal(ctx, "sf/r", 5, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.IntakeRefusals(ctx, "sf/r", 5)
	if err != nil || len(got) != 2 || got[0].Key != "roadmap t-b" || got[1].Key != "roadmap t-a" ||
		got[1].Reason != "first" || got[1].BodySHA != "h2" || got[0].BodySHA != "h1" || got[1].At.Before(got[0].At) {
		t.Fatalf("refusals = %+v, %v; want t-b then t-a (refused again last), t-a keeping its first reason and taking the new hash", got, err)
	}
	if got, err := s.IntakeRefusals(ctx, "sf/r", 6); err != nil || len(got) != 0 {
		t.Fatalf("another issue's refusals = %+v, %v", got, err)
	}
}
