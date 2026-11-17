package console

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

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
// counts them, the why says what stays blocked, the story says why no
// test ran, and the answers are exactly Run the tests now and Drop this
// promise, but on the human promise's page, which a human judges.
func TestCriteriaNeverJudgedTellsWhy(t *testing.T) {
	r := newRig(t)
	fx := slices.IndexFunc(facefixtures.All(), func(f facefixtures.Fixture) bool {
		return f.Group == facefixtures.GroupVariant && strings.HasPrefix(f.Kind, factoryd.ApprovalCriteriaRerun+" on")
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
	notRun := fmt.Sprintf("The last card, #%d, closed without a merge, so the factory never ran the plan's tests. No test checked it.", never.Task.IssueNumber+1)
	for i, c := range never.Plan.Criteria {
		ask, answers, words := askParts(t, pages[i])
		want, story := []string{"Run the tests now", "Drop this promise"}, notRun
		if c.Kind == factoryd.CriterionHuman {
			want, story = []string{"It is met", "It is not met", "Drop this promise"}, "No human judged it yet."
		}
		if ask != head || !strings.Contains(pages[i], why) || !slices.Equal(answers, want) || !strings.Contains(words, "The plan promised: "+c.Promise+" "+story) {
			t.Errorf("page %d asks %q with %q, want %q with %q and the story %s: %s", i+1, ask, answers, head, want, story, words)
		}
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
