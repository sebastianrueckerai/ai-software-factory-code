package console

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	nethtml "golang.org/x/net/html"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// cardPages are the opened card id of project at each page of its
// carousel, in order, by its arrows.
func cardPages(t *testing.T, r *rig, ck *http.Cookie, id, project string) []string {
	t.Helper()
	var out []string
	for path := "/cards/" + id + "/drawer?tab=overview&project=" + project; ; {
		card := getAs(r, ck, path, false).Body.String()
		out = append(out, card)
		next := regexp.MustCompile(`class="next" href="([^"]+)"`).FindStringSubmatch(card)
		if next == nil || len(out) > 8 {
			return out
		}
		path = html.UnescapeString(next[1]) + "&project=" + project
	}
}

// askIn is the ask of an opened card: its question and the labels of its
// answers, a label the viewer may not press marked " (disabled)".
func askIn(t *testing.T, card string) (string, []string) {
	question, labels, _ := askParts(t, card)
	return question, labels
}

// askParts is the ask of an opened card: its question, the labels of its
// answers, and its words outside the answers' buttons and fields.
func askParts(t *testing.T, card string) (string, []string, string) {
	t.Helper()
	doc, err := nethtml.Parse(strings.NewReader(card))
	if err != nil {
		t.Fatal(err)
	}
	var question string
	var labels, words []string
	var walk func(*nethtml.Node)
	walk = func(m *nethtml.Node) {
		switch {
		case m.Type == nethtml.TextNode:
			words = append(words, m.Data)
		case hasClass(m, "o-question"):
			question = nodeText(m)
			words = append(words, question)
			return
		case m.Type == nethtml.ElementNode && m.Data == "button" && hasClass(m, "pick"):
			label := nodeText(m)
			if slices.ContainsFunc(m.Attr, func(a nethtml.Attribute) bool { return a.Key == "disabled" }) {
				label += " (disabled)"
			}
			labels = append(labels, label)
			return
		case m.Type == nethtml.ElementNode && (m.Data == "button" || m.Data == "fieldset"):
			return
		}
		for c := range m.ChildNodes() {
			walk(c)
		}
	}
	for n := range doc.Descendants() {
		if n.Type == nethtml.ElementNode && n.Data == "section" && hasClass(n, "o-ask") {
			walk(n)
			break
		}
	}
	return question, labels, strings.Join(strings.Fields(strings.Join(words, " ")), " ")
}

// Each state of a merge renders through the register with the answers of
// its row: green checks merge, return or close; red checks on an agent's
// pull request return or close, as only a pull request factoryd opened
// takes a re-run; a factory pull request merges or closes on green, and
// re-runs or closes on red, its re-run posted to its request, which its
// workflow waits on; a writer who is no approver sees Merge it and may
// not press it; and a plan's last card asks each human promise on a page
// of its own before its merge page. No other action repeats a merge's
// answer.
func TestEveryMergeStateHasItsFace(t *testing.T) {
	const red = `{"state":"failure","statuses":[{"context":"ci / test","status":"failure"}]}`
	for _, tc := range []struct {
		name, spec, checks string
		writer             bool
		ask                string
		answers            []string
	}{
		{"green checks", `{"risk_class":"high","head_sha":"abc123","review":{"verdict":"approve","summary":"adds the column safely"}}`, "", false,
			"Merge “Add the size column”?", []string{"Merge it", "Send it back", "Reject and close it"}},
		{"red checks", `{"risk_class":"high","head_sha":"abc123","red_checks":true,"review":{"verdict":"approve"}}`, red, false,
			"The checks of “Add the size column” are red. Send it back to its agent?", []string{"Send it back", "Reject and close it"}},
		{"a factory pull request", `{"risk_class":"standard","head_sha":"abc123","factory":true}`, "", false,
			"Merge “Add the size column”?", []string{"Merge it", "Reject and close it"}},
		{"a factory pull request with red checks", `{"risk_class":"standard","head_sha":"abc123","factory":true,"red_checks":true}`, red, false,
			"The checks of “Add the size column” are red. Run them again, or close it?", []string{"Run the failed checks again", "Reject and close it"}},
		{"a viewer who may not approve", `{"risk_class":"high","head_sha":"abc123","review":{"verdict":"approve","summary":"adds the column safely"}}`, "", true,
			"Merge “Add the size column”?", []string{"Merge it (disabled)", "Send it back", "Reject and close it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code",
				IssueNumber: 9, PRNumber: 9, Objective: "Add the size column\n\nThe sizes table holds each card's size."}})
			r.approvals.rows[1].Spec = json.RawMessage(tc.spec)
			if tc.checks != "" {
				r.fg.statuses = map[string]string{"abc123": tc.checks}
			}
			seatRequestCases(r)
			if tc.writer {
				r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
				r.forgetWriters()
			}
			ck, _ := r.loggedIn(t)
			card := drawer(t, r, ck, "t-m", "overview")
			ask, answers := askIn(t, card)
			if ask != tc.ask || !slices.Equal(answers, tc.answers) {
				t.Errorf("asks %q with %q, want %q with %q", ask, answers, tc.ask, tc.answers)
			}
			for _, label := range []string{"Merge it", "Send it back", "Reject and close it", "Run the failed checks again"} {
				if strings.Contains(otherActionsOf(t, card), ">"+label+"<") {
					t.Errorf("the other actions hold the merge's %q", label)
				}
			}
			if rerun := strings.Contains(card, `action="/approvals/4/rerun"`); rerun != slices.Contains(tc.answers, "Run the failed checks again") {
				t.Errorf("a re-run posts to the request's door: %t", rerun)
			}
		})
	}
	t.Run("open human promises", func(t *testing.T) {
		r := newRig(t)
		humanRig(r)
		ck, _ := r.loggedIn(t)
		pages := cardPages(t, r, ck, "t-int", "ai-software-factory-code")
		if len(pages) != 3 {
			t.Fatalf("%d pages, want one per human promise and the merge's", len(pages))
		}
		for i, want := range []struct {
			ask     string
			story   string
			answers []string
		}{
			{"Is this promise of the plan met?", "<li>The plan promised: The pages read well.</li><li>What to look at: Open /sizes and /doors at 390 px.</li>",
				[]string{"It is met", "It is not met", "Drop this promise"}},
			{"Is this promise of the plan met?", "<li>The plan promised: Every page links home.</li><li>What to look at: the links on /help.</li>",
				[]string{"It is met", "It is not met", "Drop this promise"}},
			{"Merge this card&#39;s pull request?", "", []string{"Merge it (disabled)", "Send it back", "Reject and close it"}},
		} {
			ask, answers := askIn(t, pages[i])
			if html.UnescapeString(want.ask) != ask || !slices.Equal(answers, want.answers) || !strings.Contains(pages[i], want.story) {
				t.Errorf("page %d asks %q with %q, want %q with %q and the story %s:\n%s", i+1, ask, answers, want.ask, want.answers, want.story, askBlockOf(t, pages[i]))
			}
		}
		if note := `Merge it opens once the 2 promises on the pages before this one have a verdict.`; !strings.Contains(pages[2], note) {
			t.Errorf("the merge page does not say what Merge it waits on:\n%s", askBlockOf(t, pages[2]))
		}
	})
}

// A plan whose round waits on three open questions renders its plan
// request once: the first question asks on the card, and neither the ask
// nor the full brief holds a second copy of the request's confirmation.
func TestPlanApprovalRendersOnce(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	card := drawer(t, r, ck, "t-plan", "overview")
	if n := strings.Count(card, `<section class="o-ask"`); n != 1 {
		t.Fatalf("%d asks on the plan's card, want 1:\n%s", n, card)
	}
	ask, _ := askIn(t, card)
	if ask != "How often does the checker probe a site?" {
		t.Errorf("the plan asks %q, want its first open question", ask)
	}
	for _, copy := range []string{"Confirm the plan", "/approvals/", "Confirmation of the spec"} {
		if strings.Contains(card, copy) {
			t.Errorf("the plan's card holds a second copy of its request, %q:\n%s", copy, card)
		}
	}
}

// The never-judged request of a plan whose last card closed without a
// merge asks each of its 4 open promises on a page of its own: the head
// counts them, the why says what stays blocked, and each page says what is
// true of its promise. The two a run of the factory can check say why no
// test ran and offer exactly Run the tests now and Drop this promise; the
// one no test exists for says so and offers the missing part; the human's
// is the human's to judge. Once sebastian's run stopped, a page says when
// and why, and offers the run again.
func TestCriteriaNeverJudgedTellsWhy(t *testing.T) {
	r := newRig(t)
	fx := slices.IndexFunc(facefixtures.All(), func(f facefixtures.Fixture) bool {
		return f.Group == facefixtures.GroupAction && f.Kind == factoryd.ActRunCriteria
	})
	never := facefixtures.All()[fx]
	s := galleryScene()
	never.Case = nil
	s.seat(never)
	r.seedScene(s)
	seatRequestCases(r)
	ck, _ := r.loggedIn(t)
	pages := cardPages(t, r, ck, never.Task.ID, labCodeProject.Name)
	if len(pages) != 4 {
		t.Fatalf("%d pages, want one per open promise", len(pages))
	}
	const head = "Plan “divide and conquer” finished, but no test checked its 4 promises. Settle each one."
	const why = `<p class="o-askwhy">Until all 4 are settled, the plan stays open and holds production deploys.</p>`
	notRun := fmt.Sprintf("The last card, #%d, closed without a merge, so the factory never ran the plan's tests.", never.Task.IssueNumber+1)
	run, build, human := []string{"Run the tests now", "Drop this promise"}, []string{"Build the missing part", "Drop this promise"},
		[]string{"It is met", "It is not met", "Drop this promise"}
	for i, want := range []struct {
		answers []string
		story   string
	}{
		{run, notRun + " No test checked it."},
		{run, notRun + " No test checked it."},
		{build, notRun + " No test exists for it yet."},
		{human, "No human judged it yet."},
	} {
		c := never.Plan.Criteria[i]
		ask, answers, words := askParts(t, pages[i])
		if ask != head || !strings.Contains(pages[i], why) || !slices.Equal(answers, want.answers) || !strings.Contains(words, "The plan promised: "+c.Promise+" "+want.story) {
			t.Errorf("page %d asks %q with %q, want %q with %q and the story %s: %s", i+1, ask, answers, head, want.answers, want.story, words)
		}
	}
	r.events.mu.Lock()
	r.events.rows = append(r.events.rows, factoryd.Event{TaskID: never.Task.ID, Kind: factoryd.EventCriteriaGateRun, At: time.Date(2026, 11, 17, 14, 2, 0, 0, time.UTC),
		Detail: map[string]any{"task": facefixtures.TaskID(never.Task.IssueNumber + 1), "outcome": "cut", "by": "sebastian", "why": "its sandbox did not run"}})
	r.events.mu.Unlock()
	_, answers, words := askParts(t, cardPages(t, r, ck, never.Task.ID, labCodeProject.Name)[0])
	if story := "The tests sebastian asked for at 2026-11-17 14:02 UTC stopped before they finished: its sandbox did not run. No test checked it."; !slices.Equal(answers, run) || !strings.Contains(words, story) {
		t.Errorf("after the cut the first page offers %q with %s, want %q and %q", answers, words, run, story)
	}
}

// No face of an approval kind names an answer it does not offer: neither
// its ask, its why line, its story nor its note holds the label of a
// vocabulary entry outside its answers and its other actions.
func TestNoFaceNamesAnAnswerItLacks(t *testing.T) {
	kinds := factoryd.ApprovalKinds()
	for _, fx := range facefixtures.All() {
		if fx.Approval == nil || !slices.Contains(kinds, fx.Approval.Kind) {
			continue
		}
		t.Run(fx.Group+"/"+fx.Kind, func(t *testing.T) {
			r := newRig(t)
			s := galleryScene()
			s.seat(fx)
			r.seedScene(s)
			ck, _ := r.loggedIn(t)
			for i, card := range cardPages(t, r, ck, fx.Task.ID, labCodeProject.Name) {
				_, offered, words := askParts(t, card)
				for _, a := range factoryd.Vocabulary {
					if slices.ContainsFunc(offered, func(l string) bool { return strings.TrimSuffix(l, " (disabled)") == a.Label }) {
						continue
					}
					if strings.Contains(words, a.Label) {
						t.Errorf("page %d names %q, which it does not offer: %s", i+1, a.Label, words)
					}
				}
			}
		})
	}
}

// heldPlanRig seats the plan a human closed while two of its promises were
// open, with its ask to close it or keep it open; the plan's Close answers
// as factoryd does, calls in closes.
func heldPlanRig(t *testing.T, r *rig) (facefixtures.Fixture, *[]string) {
	t.Helper()
	i := slices.IndexFunc(facefixtures.All(), func(f facefixtures.Fixture) bool {
		return f.Group == facefixtures.GroupCase && f.Kind == string(factoryd.CasePlanHeld)
	})
	fx := facefixtures.All()[i]
	s := galleryScene()
	s.seat(fx)
	r.seedScene(s)
	var closes []string
	r.c.cfg.CheckClosePlan = func(_ context.Context, id, note string, approver bool) error {
		if !approver {
			return factoryd.ErrNotApprover
		}
		return nil
	}
	r.c.cfg.CloseHeldPlan = func(_ context.Context, id, by, note string, approver bool) (int, string, bool, error) {
		closes = append(closes, fmt.Sprintf("%s by %s: %q", id, by, note))
		return 2, factoryd.PlanCloseWords(by, note, true), true, nil
	}
	return fx, &closes
}

// A plan a human closed while promises were open asks on the board to
// close it and drop them, or to keep it open: the ask, what stays blocked,
// the promises, and the two answers. Drop them and close is the plan's own
// Close with the reason as each waiver's; Keep the plan open settles the
// ask through Act. A writer who is no approver sees the drop and may not
// press it.
func TestAHeldPlanAsksToCloseOrStayOpen(t *testing.T) {
	r := newRig(t)
	fx, closes := heldPlanRig(t, r)
	ck, s := r.loggedIn(t)
	card := drawer(t, r, ck, fx.Task.ID, "overview")
	ask, answers, words := askParts(t, card)
	if ask != "Close plan “divide and conquer” and drop its 2 open promises?" || !slices.Equal(answers, []string{"Drop them and close", "Keep the plan open"}) ||
		!strings.Contains(words, "Until it closes, the plan holds production deploys. The plan promised: The new panels keep the page's window and its old figures. A retried session spends money once. A human closed the plan's card while these promises were open.") {
		t.Fatalf("the held plan asks %q with %q: %s", ask, answers, words)
	}
	// its answers are its whole say on the plan's close and its hold: no
	// other action closes the plan or settles the ask beside them
	for _, again := range []string{factoryd.ActClose, factoryd.ActSettle} {
		if strings.Contains(card, `data-answer="`+again+`"`) {
			t.Errorf("the held plan's other actions repeat %s beside its answers", again)
		}
	}
	drop := answerForm(t, card, factoryd.AnswerDropAndEnd)
	if drop.Action != "/tasks/"+fx.Task.ID+"/resolve" || drop.Values.Get("action") != factoryd.ActClose || drop.Values.Has("case_id") {
		t.Fatalf("Drop them and close posts %s %v, want the plan's own Close", drop.Action, drop.Values)
	}
	drop.Values.Set("close.note", "the panels moved to the next plan")
	drop.Values.Set("csrf", s.csrf)
	if rec := r.do(post(drop.Action, drop.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the drop = %d: %s", rec.Code, rec.Body)
	}
	equal(t, "the plan's close", *closes, []string{fx.Task.ID + ` by sebastian: "the panels moved to the next plan"`})
	keep := answerForm(t, card, factoryd.ActSettle+"/keep_open")
	keep.Values.Set("csrf", s.csrf)
	if rec := r.do(post(keep.Action, keep.Values), ck); rec.Code != http.StatusSeeOther || len(r.acted) != 1 || r.acted[0].Res.Action != factoryd.ActSettle ||
		r.acted[0].CaseID != fx.Case.ID {
		t.Fatalf("keep = %d %s, Act took %+v; want one settle of case %d", rec.Code, rec.Body, r.acted, fx.Case.ID)
	}
	r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
	r.forgetWriters()
	ck, _ = r.loggedIn(t)
	if _, answers, _ := askParts(t, drawer(t, r, ck, fx.Task.ID, "overview")); !slices.Equal(answers, []string{"Drop them and close (disabled)", "Keep the plan open"}) {
		t.Errorf("a writer's held plan offers %q", answers)
	}
}

// Run the tests now is an approver's press, as every other answer on a
// never-judged plan's page is: a writer who is no approver is refused on
// the server, and an approver's press reaches Act as run_criteria.
func TestRunTheTestsIsAnApproversPress(t *testing.T) {
	r := newRig(t)
	i := slices.IndexFunc(facefixtures.All(), func(f facefixtures.Fixture) bool {
		return f.Group == facefixtures.GroupAction && f.Kind == factoryd.ActRunCriteria
	})
	never := facefixtures.All()[i]
	s := galleryScene()
	never.Case = nil
	s.seat(never)
	r.seedScene(s)
	seatRequestCases(r)
	ck, sess := r.loggedIn(t)
	run := answerForm(t, cardPages(t, r, ck, never.Task.ID, labCodeProject.Name)[0], factoryd.ActRunCriteria)
	run.Values.Set("csrf", sess.csrf)
	if rec := r.do(post(run.Action, run.Values), ck); rec.Code != http.StatusSeeOther || len(r.acted) != 1 || r.acted[0].Res.Action != factoryd.ActRunCriteria {
		t.Fatalf("an approver's run = %d %s, Act took %+v", rec.Code, rec.Body, r.acted)
	}
	r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
	r.forgetWriters()
	ck, sess = r.loggedIn(t)
	run.Values.Set("csrf", sess.csrf)
	if rec := r.do(post(run.Action, run.Values), ck); rec.Code != http.StatusForbidden || len(r.acted) != 1 {
		t.Fatalf("a writer's run = %d %s with %d Act calls; want 403 and none", rec.Code, rec.Body, len(r.acted)-1)
	}
}

// A plan stored before promises were asked names each in its claim: one
// too long for the story stands whole in a fold of its own on its page,
// "The promise in the plan's words", and the story says it is below. No
// claim is cut, and no placeholder stands in its place.
func TestAnOldPlansLongPromiseStandsWholeInAFold(t *testing.T) {
	r := newRig(t)
	i := slices.IndexFunc(facefixtures.All(), func(f facefixtures.Fixture) bool {
		return f.Group == facefixtures.GroupApproval && f.Kind == factoryd.ApprovalCriteriaRerun
	})
	old := facefixtures.All()[i]
	s := galleryScene()
	old.Case = nil
	s.seat(old)
	r.seedScene(s)
	seatRequestCases(r)
	ck, _ := r.loggedIn(t)
	page := cardPages(t, r, ck, old.Task.ID, labCodeProject.Name)[0]
	claim := old.Plan.Criteria[0].Claim
	_, _, words := askParts(t, page)
	if !strings.Contains(words, "The plan's promise is below, in its own words.") || strings.Contains(page, "too long to show") {
		t.Errorf("the story: %s", words)
	}
	fold := `<details class="fold" data-sec="words"><summary>The promise in the plan&#39;s words`
	if !strings.Contains(page, fold) || !strings.Contains(html.UnescapeString(page), claim) {
		t.Errorf("the page holds no fold with the claim whole:\n%s", askBlockOf(t, page))
	}
}
