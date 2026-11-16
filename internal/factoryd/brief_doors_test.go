package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// liveBrief is card #2100's live title with the accepted why line and
// summary: the door refuses the title alone.
var liveBrief = BriefText{Title: title2100, Why: acceptedBrief.Why, Summary: acceptedBrief.Summary}

// carriesCheck fails t unless refusal carries every line CheckBrief writes
// for b as job, so the writer reads the door's own words.
func carriesCheck(t *testing.T, refusal string, b BriefText, job BriefJob) {
	t.Helper()
	lines := CheckBrief(b, job, briefRefs{})
	if len(lines) == 0 {
		t.Fatalf("CheckBrief passes %+v", b)
	}
	for _, l := range lines {
		if !strings.Contains(refusal, l) {
			t.Errorf("the refusal %q lacks %q", refusal, l)
		}
	}
}

// plannerRig is plan t-plan running on linkshort before its first
// revision, its spec confirmed, filing under policy version 7; the
// project's documents are sf/docs, which docs serves with Forgejo's API.
func plannerRig(t *testing.T, docs map[string]string) (*Runner, *fakeStore) {
	t.Helper()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, Priority: 2, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	fj := &planForgejo{issues: map[int64]*planIssue{1: {title: "Link shortener", poster: "alice", state: "open"}}, next: 1}
	plans := fj.srv(t).Config.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.Path, "/api/v1/repos/sf/docs/")
		switch {
		case !ok:
			plans.ServeHTTP(w, r)
		case strings.HasPrefix(path, "git/trees/"):
			var tree []TreeEntry
			for p := range docs {
				tree = append(tree, TreeEntry{Path: p, Type: "blob"})
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(tree), "tree": tree})
		case strings.HasPrefix(path, "contents/"):
			md, found := docs[strings.TrimPrefix(path, "contents/")]
			if !found {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(md)), "encoding": "base64", "sha": "b1"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	r, _, store := newTestRunner(t, plan, newFakeKube(), &fakeObjectStore{})
	r.forgejo = &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	r.completion.Forgejo = r.forgejo
	pinned := fixtureVersion(t)
	pinned.Document.Chat = &policy.ChatConfig{DocRepo: "sf/docs"}
	r.cfg.Policy = func() policy.Version { return pinned }
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	r.cfg.Workflows = &fakeWorkflows{}
	confirmSpec(t, store)
	return r, store
}

// withBrief is testPlan with its links-api card under brief b.
func withBrief(b BriefText) Plan {
	p := testPlan()
	c := &p.Cards[1]
	c.Title, c.Why, c.Summary = b.Title, b.Why, b.Summary
	return p
}

// filePlanCard files testPlan with its links-api card under brief b, as
// the planner's file_plan does: the findings of a refusal, else the card
// the plan's first round files.
func filePlanCard(t *testing.T, r *Runner, store *fakeStore, b BriefText) (string, Task) {
	t.Helper()
	res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, withBrief(b)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision == 0 {
		return strings.Join(res.Findings, "\n"), Task{}
	}
	requestPlan(t, r)
	return "", cardsByKey(t, store)["links-api"]
}

// deltaAnswer is a delta round's answer of the card fix-stats under b.
func deltaAnswer(t *testing.T, b BriefText) string {
	t.Helper()
	card, err := json.Marshal(map[string]any{"key": "fix-stats", "title": b.Title, "why": b.Why, "summary": b.Summary, "brief": "Count each redirect once.",
		"depends_on": []string{"stats"}, "size": "small",
		"acceptance": []map[string]string{{"criterion": "redirect_302", "text": "TestStats in internal/stats: two redirects: clicks 2"}}})
	if err != nil {
		t.Fatal(err)
	}
	return `{"card": ` + string(card) + `}`
}

// delegateRig is card t-parent running on sf/ai-software-factory-code
// and the delegate hand-over, whose tries count per parent.
func delegateRig(t *testing.T) (func(b BriefText) *httptest.ResponseRecorder, *fakeStore) {
	t.Helper()
	rf := &rollForgejo{}
	fj, _ := rf.srv(t)
	store := newFakeStore()
	store.projects = []Project{seedProject("sf/ai-software-factory-code")}
	if err := store.CreateTask(context.Background(), Task{ID: "t-parent", Status: "running", Repository: "sf/ai-software-factory-code", IssueNumber: 41,
		Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	h := DelegationHandler(DelegationDeps{Store: store, Forgejo: fj, Workflows: &fakeWorkflows{},
		Budget: func() policy.Budget {
			return policy.Budget{TaskDefaultEUR: 5, TaskMaxEUR: 15, DayEUR: 20, MonthEUR: 50}
		}})
	return func(b BriefText) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"task_id": "t-parent", "title": b.Title, "why": b.Why, "summary": b.Summary,
			"brief": "In store.go add an index on the priority column; a test reads it."})
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodPost, "/internal/delegations", strings.NewReader(string(body))))
		return rr
	}, store
}

// delegated is the card a 201 of the hand-over filed.
func delegated(t *testing.T, rr *httptest.ResponseRecorder, store *fakeStore) Task {
	t.Helper()
	var res struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil || rr.Code != http.StatusCreated {
		t.Fatalf("the hand-over = %d %s", rr.Code, rr.Body)
	}
	return store.tasks[res.TaskID]
}

// Every filing door of factoryd checks the brief its writer wrote: the
// live title of card #2100 comes back as the door's refusal with
// CheckBrief's words, and the accepted brief files with its why line and
// summary stored. An agent's ask files no card: it passes.
func TestEveryFilingDoorChecksTheBrief(t *testing.T) {
	ctx := context.Background()
	for _, door := range []struct {
		name string
		job  BriefJob
		file func(t *testing.T, b BriefText) (string, Task)
	}{
		{"planner card", BriefWork, func(t *testing.T, b BriefText) (string, Task) {
			r, store := plannerRig(t, nil)
			return filePlanCard(t, r, store, b)
		}},
		{"delta card", BriefWork, func(t *testing.T, b BriefText) (string, Task) {
			model := &fakeLiteLLM{content: deltaAnswer(t, b)}
			store, _ := deltaRoundOn(t, model)
			if len(model.chats) > 1 {
				var in struct{ Refused []deltaRefusal }
				if err := json.Unmarshal([]byte(userMessage(model.chats[1])), &in); err != nil || len(in.Refused) != 1 {
					t.Fatalf("the second call's input: %v, %+v", err, in)
				}
				return in.Refused[0].Why, Task{}
			}
			card, _ := deltaCard(t, store)
			return "", card
		}},
		{"delegate", BriefWork, func(t *testing.T, b BriefText) (string, Task) {
			post, store := delegateRig(t)
			rr := post(b)
			if rr.Code != http.StatusCreated {
				return rr.Body.String(), Task{}
			}
			return "", delegated(t, rr, store)
		}},
		{"chat escalate", BriefAsk, func(t *testing.T, b BriefText) (string, Task) {
			f, _ := fileStub(t)
			card, err := EscalateToHuman(WithBriefTry(ctx, 1), newFakeStore(), f, Repo{Owner: "sf", Name: "r"},
				HumanQuestion{Ask: b.Title, Why: b.Why, Info: "i", Recommendation: "r", Choices: []QuestionChoice{{Value: "a", Label: "a"}, {Value: "b", Label: "b"}}},
				"sebastian", Task{Source: SourceChat})
			var no *BriefRefusal
			if errors.As(err, &no) {
				return no.Error(), Task{}
			}
			if err != nil {
				t.Fatal(err)
			}
			return "", card
		}},
		{"agent escalate", BriefAsk, func(t *testing.T, b BriefText) (string, Task) {
			task := Task{ID: "t-a", Kind: "agent", Status: "running", Track: policy.TrackDirect, Repository: "sf/ai-software-factory-code", IssueNumber: 5}
			r, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
			if err := r.AdmitAsk(ctx, task, AgentAsk{Kind: "question", Question: b.Title, Reason: b.Why}); err != nil {
				return err.Error(), Task{}
			}
			return "", Task{Why: b.Why}
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			refusal, filed := door.file(t, liveBrief)
			if filed.ID != "" {
				t.Fatalf("the live title filed %+v", filed)
			}
			carriesCheck(t, refusal, liveBrief, door.job)

			refusal, filed = door.file(t, acceptedBrief)
			summary := acceptedBrief.Summary
			if door.job == BriefAsk {
				summary = ""
			}
			if refusal != "" || filed.Why != acceptedBrief.Why || filed.Summary != summary || filed.BriefUnchecked {
				t.Fatalf("the accepted brief: refusal %q, stored why %q, summary %q, unchecked %v", refusal, filed.Why, filed.Summary, filed.BriefUnchecked)
			}
		})
	}
}

// A writer the door refuses three times files its last text on the third
// try, marked unchecked, each door counting the tries of one filing: the
// planner's plan (FilePlan) and the delegate hand-over of one parent.
func TestTheThirdRefusedTryFilesMarkedUnchecked(t *testing.T) {
	r, store := plannerRig(t, nil)
	for try := 1; try < BriefTries; try++ {
		if refusal, _ := filePlanCard(t, r, store, liveBrief); refusal == "" {
			t.Fatalf("plan try %d filed", try)
		}
	}
	refusal, card := filePlanCard(t, r, store, liveBrief)
	if refusal != "" || card.ID == "" || !card.BriefUnchecked || CardTitle(card) != title2100 || card.Why != liveBrief.Why {
		t.Fatalf("the third plan try: refusal %q, card %+v", refusal, card)
	}
	if rev, _, _ := store.PlanRevision(context.Background(), "t-plan", 0); !strings.Contains(string(rev.Plan), `"brief_unchecked":true`) {
		t.Fatalf("the stored revision lacks the card's mark: %s", rev.Plan)
	}

	post, delegates := delegateRig(t)
	for try := 1; try < BriefTries; try++ {
		if rr := post(liveBrief); rr.Code != http.StatusBadRequest {
			t.Fatalf("delegate try %d = %d %s", try, rr.Code, rr.Body)
		}
	}
	if child := delegated(t, post(liveBrief), delegates); !child.BriefUnchecked || CardTitle(child) != title2100 {
		t.Fatalf("the third delegate try filed %+v", child)
	}
	if rr := post(liveBrief); rr.Code != http.StatusBadRequest {
		t.Fatalf("the next filing's first try = %d, want its own count", rr.Code)
	}
}

// A planner card's section is a path of the document repository with a
// heading: one whose heading the document lacks, or whose document is
// missing, is refused on every try; a section that resolves files. A
// section label is refused in the title, the why line and the summary,
// not in the body, which may number its own steps.
func TestSectionMustResolve(t *testing.T) {
	docs := map[string]string{"docs/plan.md": "# Plan\n\n## Phase B\n\nThe board.\n"}
	section := func(r *Runner, store *fakeStore, s string) []string {
		p := withBrief(acceptedBrief)
		p.Cards[1].Section = s
		res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, p))
		if err != nil {
			t.Fatal(err)
		}
		return res.Findings
	}
	r, store := plannerRig(t, docs)
	for _, tc := range []struct{ section, want string }{
		{"docs/plan.md#phase-c", "plan: card links-api: The document docs/plan.md has no heading phase-c."},
		{"docs/none.md#phase-b", "plan: card links-api: The document docs/none.md has no heading phase-b."},
		{"docs/none.md", "plan: card links-api: There is no document docs/none.md."},
	} {
		for try := 1; try <= BriefTries; try++ {
			if got := section(r, store, tc.section); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("section %s, try %d: findings %q, want %q", tc.section, try, got, tc.want)
			}
		}
	}
	if got := section(r, store, "docs/plan.md#phase-b"); len(got) != 0 {
		t.Fatalf("a section that resolves: findings %q", got)
	}
	labelled := acceptedBrief
	labelled.Summary = "It narrows the refusal, as phase B of the plan says. A test closes a plan with none."
	labelled.Body = "Step 1 reads the plan. Step 2 closes it."
	want := []string{"'phase B' names a part of a document. Link it as [words](path#heading), or say what it is."}
	if lines := CheckBrief(labelled, BriefWork, briefRefs{}); !slices.Equal(lines, want) {
		t.Fatalf("a section label in the summary and steps in the body: %q, want %q", lines, want)
	}
}

// An issue a human opens keeps its title as written, and its why line is
// the body's opening sentence when that has at most 160 characters, else
// none; its text is the issue's.
func TestHumanIssueWhyIsItsOpeningSentence(t *testing.T) {
	long := strings.Repeat("word ", 33) + "ends here."
	for _, tc := range []struct{ title, body, why string }{
		{"board: cache drops on reload", "The board drops its cache on every reload. It costs a second a page.\n\nMore.", "The board drops its cache on every reload."},
		{"Board cache", "One line without a stop", "One line without a stop"},
		{"Board cache", long + " Then more.", ""},
		{"Board cache", "", ""},
	} {
		in, store, _ := newTestIntake(t)
		body := issueOpenedBody(7, "sebastian", []string{"agent"}, tc.title, tc.body)
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body)
		}
		var task Task
		for _, task = range store.tasks {
		}
		if CardTitle(task) != tc.title || task.Why != tc.why || task.Summary != "" || task.Objective != IssueObjective(tc.title, tc.body) || task.BriefUnchecked {
			t.Errorf("issue %q: title %q, why %q, summary %q, objective %q, unchecked %v; want the why %q",
				tc.body, CardTitle(task), task.Why, task.Summary, task.Objective, task.BriefUnchecked, tc.why)
		}
	}
}

// A card that continues another stores the old card's why line and
// summary, and its text opens with "Continues #N.": a successor, a retry's
// replacement and a human card handed to the factory.
func TestSuccessorCarriesTheBrief(t *testing.T) {
	ctx := context.Background()
	const continues = "Continues #5.\n\n"
	head := "Why: " + acceptedBrief.Why + "\n\nSummary: " + acceptedBrief.Summary + "\n\n---\n\n"
	briefed := func(store *fakeStore, id string) {
		c := store.tasks[id]
		c.Why, c.Summary = acceptedBrief.Why, acceptedBrief.Summary
		store.tasks[id] = c
	}
	carried := func(name string, n Task, issue string) {
		t.Helper()
		_, text, _ := strings.Cut(n.Objective, "\n\n")
		if n.Why != acceptedBrief.Why || n.Summary != acceptedBrief.Summary || !strings.HasPrefix(text, continues) || !strings.HasPrefix(issue, head+continues) {
			t.Errorf("%s: why %q, summary %q, text %q, issue %q", name, n.Why, n.Summary, text, issue)
		}
	}

	r, store, _, fj, _ := newRetryRig(t)
	briefed(store, "t-a")
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"})
	if err != nil {
		t.Fatal(err)
	}
	carried("the successor", store.tasks[id], fj.issues[store.tasks[id].IssueNumber].body)

	r, store, _, fj, _ = newRetryRig(t)
	briefed(store, "t-a")
	n, err := r.RetryCard(ctx, "t-a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range store.tasks {
		if c.IssueNumber == n && c.ID != "t-a" {
			carried("the retry's replacement", c, fj.issues[n].body)
		}
	}

	r, store, issues, _ := routineRunner(t, routineTree(t))
	if err := store.CreateTask(ctx, Task{ID: "t-back", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 5,
		Source: SourceWebhook, RiskClass: "standard", Objective: "Tidy the docs\n\nTidy the console's docs.", Why: acceptedBrief.Why, Summary: acceptedBrief.Summary}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Go(ctx, "t-back", "sebastian"); err != nil {
		t.Fatal(err)
	}
	for _, c := range store.tasks {
		if c.ID != "t-back" {
			_, text, _ := strings.Cut(issues.issues[0], "\n\n")
			carried("the hand-over", c, text)
		}
	}
}

// The issue mirror opens with the why line and the summary, each when
// set, then the full text after a rule, and reads back as it was written.
func TestIssueTextRoundTrips(t *testing.T) {
	for _, b := range []BriefText{
		{Why: "So it loads.", Summary: "It caches. A test reads it.", Body: "The text.\n\n---\n\nwith its own rule"},
		{Why: "So it loads.", Body: "The text."},
		{Body: "The text alone."},
	} {
		text := IssueText(b)
		why, summary, body := ParseIssueText(text)
		if why != b.Why || summary != b.Summary || body != b.Body {
			t.Errorf("%q reads back as %q, %q, %q", text, why, summary, body)
		}
	}
	if got := IssueText(BriefText{Why: "So it loads.", Summary: "It caches.", Body: "The text."}); got != "Why: So it loads.\n\nSummary: It caches.\n\n---\n\nThe text." {
		t.Errorf("the mirror = %q", got)
	}
}

// A writer's filing through fileOnIssue (delegate, chat escalate, the
// task API) checks its document links against the document repository
// WithBriefDocs names for its project, before any issue is filed.
func TestAWritersFilingChecksItsDocumentLinks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/docs/git/trees/") {
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "tree": []TreeEntry{{Path: "docs/plan.md", Type: "blob"}}})
	}))
	t.Cleanup(srv.Close)
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Chat: &policy.ChatConfig{DocRepo: "sf/docs"}}}}
	ctx, err := WithBriefDocs(WithBriefTry(context.Background(), 1), tree, "sf/code")
	if err != nil {
		t.Fatal(err)
	}
	b := acceptedBrief
	b.Body = "Builds [the plan](docs/plan.md) and [the notes](docs/none.md)."
	_, _, err = fileOnIssue(ctx, &Forgejo{BaseURL: srv.URL}, Repo{Owner: "sf", Name: "code"}, b, nil, Task{Kind: "agent"}, 0, nil)
	var no *BriefRefusal
	if !errors.As(err, &no) || no.Error() != "There is no document docs/none.md." {
		t.Fatalf("fileOnIssue = %v, want the missing document refused", err)
	}
}
