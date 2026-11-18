package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
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
	h := DelegationHandler(DelegationDeps{Store: store, Forgejo: fj, Workflows: &fakeWorkflows{}, Tries: &BriefTally{},
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
				if rr.Header().Get(gateway.HeaderDecision) != gateway.RefusedBrief {
					t.Errorf("the refusal's decision = %q, want the door's", rr.Header().Get(gateway.HeaderDecision))
				}
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
	// a session's delegates count together, whatever each title: a
	// rewrite is the same writer's next try
	post, delegates = delegateRig(t)
	titled := func(n int) BriefText {
		b := liveBrief
		b.Title = fmt.Sprintf("The doClose refusal narrowed, rewrite %d: the review finding answered on branch card/2084", n)
		return b
	}
	for try := 1; try < BriefTries; try++ {
		if rr := post(titled(try)); rr.Code != http.StatusBadRequest {
			t.Fatalf("delegate titled anew, try %d = %d %s", try, rr.Code, rr.Body)
		}
	}
	if child := delegated(t, post(titled(BriefTries)), delegates); !child.BriefUnchecked || CardTitle(child) != titled(BriefTries).Title {
		t.Fatalf("the third title's try filed %+v, want it marked unchecked", child)
	}
}

// A planner card's section is a path of the document repository with a
// heading: one whose heading the document lacks, whose document is
// missing or matches two, or whose path is no markdown document, is
// refused on every try, each in one sentence; a section that resolves
// files. A section label is refused in the title, the why line and the
// summary, not in the body, which may number its own steps.
func TestSectionMustResolve(t *testing.T) {
	docs := map[string]string{"docs/plan.md": "# Plan\n\n## Phase B\n\nThe board.\n", "docs/notes.md": "# Notes\n", "guide/notes.md": "# Notes\n"}
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
		{"docs/plan.md#phase-c", "plan: card links-api: The document docs/plan.md has no heading phase-c. Drop the section, or name a heading that exists."},
		{"docs/none.md#phase-b", "plan: card links-api: There is no document docs/none.md. Drop the section, or name a document that exists."},
		{"docs/none.md", "plan: card links-api: There is no document docs/none.md. Drop the section, or name a document that exists."},
		{"notes.md#notes", "plan: card links-api: The section notes.md matches 2 documents (docs/notes.md, guide/notes.md): drop the section, or name one path from the repository's root."},
		{"internal/links/links.go#redirect", "plan: card links-api: The section internal/links/links.go#redirect is in no markdown document: drop the section, or name a heading of a document."},
		{"#phase-b", "plan: card links-api: The section #phase-b is in no markdown document: drop the section, or name a heading of a document."},
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
// the opening sentence of the body's first paragraph, its wrapped lines
// joined, when that has at most 160 characters; none when the body opens
// with a heading, a list, a quote, a table or a fence. Its text is the
// issue's.
func TestHumanIssueWhyIsItsOpeningSentence(t *testing.T) {
	long := strings.Repeat("word ", 33) + "ends here."
	for _, tc := range []struct{ title, body, why string }{
		{"board: cache drops on reload", "The board drops its cache on every reload. It costs a second a page.\n\nMore.", "The board drops its cache on every reload."},
		{"Board cache", "One line without a stop", "One line without a stop"},
		{"Board cache", "The board drops its cache\non every reload. It costs\na second a page.", "The board drops its cache on every reload."},
		{"Board cache", "The board drops its cache\r\non every reload.\r\n\r\nMore.", "The board drops its cache on every reload."},
		{"Board cache", "# Cache\n\nThe board drops its cache.", ""},
		{"Board cache", "### Cache\n\nThe board drops its cache.", ""},
		{"Board cache", "#12 is back after the reload. It costs a second.", "#12 is back after the reload."},
		{"Board cache", "- drops the cache\n- costs a second", ""},
		{"Board cache", "1. reload\n2. reload again", ""},
		{"Board cache", "> the cache is gone\n\nIt drops.", ""},
		{"Board cache", "```\npanic: cache\n```", ""},
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
// replacement and a human card handed to the factory. Each keeps the tool
// image the old card adds, so the open one takes the next ask for it,
// and a retry and a hand-over keep the old card's writer, the chat.
func TestSuccessorCarriesTheBrief(t *testing.T) {
	ctx := context.Background()
	const continues = "Continues #5.\n\n"
	head := "Why: " + acceptedBrief.Why + "\n\nSummary: " + acceptedBrief.Summary + "\n\n---\n\n"
	briefed := func(store *fakeStore, id string) {
		c := store.tasks[id]
		c.Why, c.Summary, c.ToolImage, c.ChatFiled = acceptedBrief.Why, acceptedBrief.Summary, "video", true
		store.tasks[id] = c
	}
	carried := func(name string, n Task, issue string, chat bool) {
		t.Helper()
		_, text, _ := strings.Cut(n.Objective, "\n\n")
		if n.Why != acceptedBrief.Why || n.Summary != acceptedBrief.Summary || !strings.HasPrefix(text, continues) || !strings.HasPrefix(issue, head+continues) {
			t.Errorf("%s: why %q, summary %q, text %q, issue %q", name, n.Why, n.Summary, text, issue)
		}
		if n.ToolImage != "video" || n.ChatFiled != chat {
			t.Errorf("%s: tool image %q, chat filed %v; want video, %v", name, n.ToolImage, n.ChatFiled, chat)
		}
	}

	r, store, _, fj, _ := newRetryRig(t)
	briefed(store, "t-a")
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"})
	if err != nil {
		t.Fatal(err)
	}
	carried("the successor", store.tasks[id], fj.issues[store.tasks[id].IssueNumber].body, false)

	r, store, _, fj, _ = newRetryRig(t)
	briefed(store, "t-a")
	n, err := r.RetryCard(ctx, "t-a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range store.tasks {
		if c.IssueNumber == n && c.ID != "t-a" {
			carried("the retry's replacement", c, fj.issues[n].body, true)
		}
	}

	r, store, issues, _ := routineRunner(t, routineTree(t))
	if err := store.CreateTask(ctx, Task{ID: "t-back", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 5,
		Source: SourceWebhook, RiskClass: "standard", Objective: "Tidy the docs\n\nTidy the console's docs.", Why: acceptedBrief.Why, Summary: acceptedBrief.Summary,
		ToolImage: "video", ChatFiled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Go(ctx, "t-back", "sebastian"); err != nil {
		t.Fatal(err)
	}
	for _, c := range store.tasks {
		if c.ID != "t-back" {
			_, text, _ := strings.Cut(issues.issues[0], "\n\n")
			carried("the hand-over", c, text, true)
		}
	}
}

// The issue mirror opens with the why line and the summary when set, then
// the full text after a rule, and reads back as it was written; a body of
// any other shape reads as all text.
func TestIssueTextRoundTrips(t *testing.T) {
	for _, b := range []BriefText{
		{Why: "So it loads.", Summary: "It caches. A test reads it.", Body: "The text.\n\n---\n\nwith its own rule"},
		{Why: "So it loads.", Body: "The text."},
		{Summary: "It caches. A test reads it.", Body: "The text."},
		{Body: "The text alone."},
	} {
		text := IssueText(b)
		why, summary, body := ParseIssueText(text)
		if why != b.Why || summary != b.Summary || body != b.Body {
			t.Errorf("%q reads back as %q, %q, %q", text, why, summary, body)
		}
	}
	for _, c := range []struct {
		b    BriefText
		want string
	}{
		{BriefText{Why: "So it\nloads.", Summary: "It caches.\n\nA test\nreads it.", Body: "The text."}, "Why: So it loads.\n\nSummary: It caches. A test reads it.\n\n---\n\nThe text."},
		{BriefText{Summary: "It caches.", Body: "The text."}, "Summary: It caches.\n\n---\n\nThe text."},
		{BriefText{Body: "The text."}, "The text."},
	} {
		if got := IssueText(c.b); got != c.want {
			t.Errorf("the mirror of %+v = %q, want %q", c.b, got, c.want)
		}
	}
	// a mirror a human saved in Forgejo's web editor holds \r\n line ends
	if why, summary, body := ParseIssueText("Why: So it loads.\r\n\r\nSummary: It caches.\r\n\r\n---\r\n\r\nThe text.\r\nIts second line."); why != "So it loads." ||
		summary != "It caches." || body != "The text.\nIts second line." {
		t.Errorf("the web editor's mirror reads as %q, %q, %q", why, summary, body)
	}
	// a human's own text of another shape stays the human's text
	for _, text := range []string{
		"Summary: the board drops its cache\n\nSize: large\n\nSteps: reload twice\n\n---\n\nEnvironment: Firefox",
		"Why: two\nlines\n\n---\n\nthe text",
		"Why: the cache\n\nSteps: reload\n\n---\n\nthe text",
		"Why: \n\n---\n\nthe text",
		"Summary: \n\n---\n\nthe text",
		"Summary: two\nlines\n\n---\n\nthe text",
		"Summary: first\n\nWhy: second\n\n---\n\nthe text",
	} {
		if why, summary, body := ParseIssueText(text); why != "" || summary != "" || body != text {
			t.Errorf("%q reads as %q, %q, %q; want all text", text, why, summary, body)
		}
	}
}

// Each shape of a human's brief, its why line and summary set or not,
// files through its issue's mirror to the stored card: a summary without
// a why line stays, and the why line a human left out is the text's
// opening sentence.
func TestIntakeStoresEveryBriefShape(t *testing.T) {
	const text = "The cache drops entries on reload. It must keep them."
	for _, c := range []struct {
		b            BriefText
		why, summary string
	}{
		{BriefText{Why: "So the board loads.", Summary: "It caches. A test reads it.", Body: text}, "So the board loads.", "It caches. A test reads it."},
		{BriefText{Why: "So the board loads.", Body: text}, "So the board loads.", ""},
		{BriefText{Summary: "It caches. A test reads it.", Body: text}, "The cache drops entries on reload.", "It caches. A test reads it."},
		{BriefText{Body: text}, "The cache drops entries on reload.", ""},
	} {
		in, store, _ := newTestIntake(t)
		body := issueOpenedBody(7, "sebastian", []string{"agent"}, "Keep the cache", IssueText(c.b))
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
		}
		if task := onlyTask(t, store); task.Why != c.why || task.Summary != c.summary || task.Objective != "Keep the cache\n\n"+text {
			t.Errorf("brief %+v: stored why %q, summary %q, objective %q; want %q, %q and the text", c.b, task.Why, task.Summary, task.Objective, c.why, c.summary)
		}
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

// A chat filing's card_filed row names the project agent as the writer of
// the card's text, whoever posted the issue: the project agent's account
// when it decided, the human's own when the human confirmed its proposal.
// The intake stores the door's unchecked mark from the row, not the one
// a human's issue would take (its title names a branch), the why line and
// the summary the issue mirrors, and the card as the agent's, so the door
// checks its later edits as the agent's work though the human authors it.
func TestIntakeStoresAChatFilingsMark(t *testing.T) {
	for _, poster := range []string{ChatUser, "sebastian"} {
		for _, unchecked := range []bool{true, false} {
			in, store, _ := newTestIntake(t)
			in.Filings = store
			store.events = append(store.events, chatFiling(7, unchecked))
			body := issueOpenedBody(7, poster, []string{"agent"}, title2100, IssueText(BriefText{Why: liveBrief.Why, Summary: liveBrief.Summary, Body: "Narrow it."}))
			if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, body = %s", poster, rr.Code, rr.Body)
			}
			if task := onlyTask(t, store); task.BriefUnchecked != unchecked || !task.ChatFiled || task.Author != "sebastian" || task.Why != liveBrief.Why ||
				task.Summary != liveBrief.Summary || task.Objective != title2100+"\n\nNarrow it." || BriefJobOf(task) != BriefWork {
				t.Errorf("posted by %s, row unchecked %v: task = %+v, want the agent's card with the row's mark, its why line and summary", poster, unchecked, task)
			}
		}
	}
}

// A human's confirm posts the agent's card under the human's account, and
// the console records its row after the post: the intake that read no row
// yet makes a human's card, marked unchecked for the branch its title
// names and with its text's opening sentence as the why line the agent
// left out. The row turns it into the agent's card with the door's mark
// and the filing's own why line, none, as the intake that read the row
// first stores it, whether the row lands while the intake files it or
// after.
func TestAChatFilingsRowAfterItsIssueMarksTheCard(t *testing.T) {
	body := issueOpenedBody(7, "sebastian", []string{"agent"}, title2100, IssueText(BriefText{Summary: liveBrief.Summary, Body: "Narrow it."}))
	for _, during := range []bool{true, false} {
		in, store, _ := newTestIntake(t)
		late := &lateFilings{fakeStore: store}
		in.Filings = late
		if during {
			late.row = func() { store.events = append(store.events, chatFiling(7, false)) }
		}
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
		}
		if !during {
			if task := onlyTask(t, store); task.ChatFiled || !task.BriefUnchecked || task.Why != "Narrow it." {
				t.Fatalf("before the row: task = %+v, want a human's card, marked unchecked, its why line the text's", task)
			}
			store.events = append(store.events, chatFiling(7, false))
			if err := store.MarkChatFiled(t.Context(), "sf/ai-software-factory-code", 7, ""); err != nil {
				t.Fatal(err)
			}
		}
		if task := onlyTask(t, store); !task.ChatFiled || task.BriefUnchecked || task.Why != "" || task.Summary != liveBrief.Summary || BriefJobOf(task) != BriefWork {
			t.Errorf("row during the intake %v: task = %+v, want the agent's card, checked as the door filed it, with no why line", during, task)
		}
	}
}

// chatFiling is the console's card_filed row of a chat filing of issue n
// for the human sebastian, which the door filed unchecked or not.
func chatFiling(n int64, unchecked bool) Event {
	detail := map[string]any{"title": title2100, "track": "direct", "decider": "member"}
	if unchecked {
		detail["brief_unchecked"] = true
	}
	return Event{Repository: "sf/ai-software-factory-code", IssueNumber: n, Kind: EventCardFiled, Actor: "sebastian", Via: ViaChat, Detail: detail}
}

// lateFilings answers its first FiledFor before the console's row: row,
// when set, records it right after.
type lateFilings struct {
	*fakeStore
	asked bool
	row   func()
}

func (f *lateFilings) FiledFor(ctx context.Context, repository string, issue int64) (Filing, bool, error) {
	if f.asked {
		return f.fakeStore.FiledFor(ctx, repository, issue)
	}
	f.asked = true
	if f.row != nil {
		defer f.row()
	}
	return Filing{}, false, nil
}

// A new session of a card starts its agent's refused tries anew: the
// tries its escalate and its delegate counted in the session before do not
// carry its next filing to the last try.
func TestANewSessionStartsTheTriesAnew(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "add a README line", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2,
		Track: policy.TrackDirect}
	r, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, r)
	for range BriefTries - 1 {
		if err := r.AdmitAsk(context.Background(), task, AgentAsk{Question: title2100, Reason: acceptedBrief.Why}); err == nil {
			t.Fatal("the live title passed")
		}
		r.Briefs().Refused(delegateBriefKey(task.ID))
	}
	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "README.md"}}) + "\n" +
		pushRequestLine("c0ffee") + "\n" + tokenLogs + pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t1 at c0ffee")
	if _, err := runSession(t, nil, r, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil, logs); err != nil {
		t.Fatal(err)
	}
	if escalate, delegate := r.Briefs().Try(escalateBriefKey(task.ID)), r.Briefs().Try(delegateBriefKey(task.ID)); escalate != 1 || delegate != 1 {
		t.Fatalf("after the new session: the escalate's try %d, the delegate's %d; want 1 and 1", escalate, delegate)
	}
}

// A planner card without a title is refused once, by the door, on every
// try: no issue carries an empty title.
func TestAnUntitledPlanCardIsRefusedOnce(t *testing.T) {
	r, store := plannerRig(t, nil)
	untitled := acceptedBrief
	untitled.Title = ""
	for try := 1; try <= BriefTries; try++ {
		if refusal, _ := filePlanCard(t, r, store, untitled); refusal != "plan: card links-api: The title is empty. Write it in 1 to 70 characters." {
			t.Fatalf("try %d: findings %q", try, refusal)
		}
	}
}
