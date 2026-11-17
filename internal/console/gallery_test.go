package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"golang.org/x/net/html"
)

// galleryEntry is one face of the gallery: its group and its kind, and
// the seat that puts it on a fresh rig and answers what the gallery opens.
type galleryEntry struct {
	Group, Kind string
	Seat        func(*rig) string
}

// galleryBoard is the group of the board's own items no fixture holds:
// a card with three cases, the plain cards of Needs you, the routine's
// card and the status strip's actions.
const galleryBoard = "board"

// The targets a seat answers besides a card's task id: a routine's ghost
// and its drawer, "routine:<project>/<name>", and the status strip.
const (
	galleryRoutine = "routine:"
	galleryStatus  = "status"
)

// galleryEntries are the gallery's faces: every fixture of facefixtures,
// then the board's own items.
func galleryEntries() []galleryEntry {
	all := facefixtures.All()
	var out []galleryEntry
	for _, fx := range all {
		out = append(out, galleryEntry{fx.Group, fx.Kind, func(r *rig) string { return r.seedScene(fixtureScene(fx, all)) }})
	}
	return append(out, boardEntries(all)...)
}

// galleryScene is an empty moment of the code project: no card, and no
// case a fake would make up for a waiting card.
func galleryScene() labScene {
	return labScene{spend: 61.40, chatSpend: 3.85, objects: map[string]string{}, cases: map[string][]factoryd.Case{}, asks: map[int64]factoryd.CaseAsk{},
		plans: map[string][]factoryd.PlanRevision{}, questions: map[string][]factoryd.PlanQuestion{}}
}

// fixtureScene is the code project holding fixture fx, and the plan a
// plan's card belongs to, so its chip and its drawer name the plan.
func fixtureScene(fx facefixtures.Fixture, all []facefixtures.Fixture) labScene {
	s := galleryScene()
	s.drawer = fx.Task.ID
	s.seat(fx)
	if i := slices.IndexFunc(all, func(p facefixtures.Fixture) bool { return p.Task.ID == fx.Task.Parent && p.Plan != nil }); i >= 0 {
		s.seat(all[i])
	}
	return s
}

// seat adds fixture fx to s as the store holds it today: the board's row
// of its card reads its live ask from its case and the case's newest ask
// row (BoardAsk), and a plan card's revision holds its questions.
func (s *labScene) seat(fx facefixtures.Fixture) {
	bt := factoryd.BoardTask{Task: fx.Task}
	if c := fx.Case; c != nil {
		var row map[string]any
		bt.EscalatedAt = c.OpenedAt
		if fx.Ask != nil {
			row, bt.EscalatedAt = fx.Ask.Detail, fx.Ask.At
			s.asks[c.ID] = *fx.Ask
		}
		bt.AskCase, bt.QuestionComment = c.ID, c.CommentID
		bt.Escalation, bt.Choices, bt.Question = factoryd.BoardAsk(*c, row)
		s.cases[fx.Task.ID] = append(s.cases[fx.Task.ID], *c)
	}
	if fx.Approval != nil {
		s.approvals = append(s.approvals, *fx.Approval)
	}
	if p := fx.Plan; p != nil {
		revision := 0
		if len(p.Questions) > 0 {
			revision = 1
		}
		plan, err := json.Marshal(p)
		if err != nil {
			panic(err)
		}
		s.plans[fx.Task.ID] = []factoryd.PlanRevision{{TaskID: fx.Task.ID, Revision: revision, Plan: plan, SHA256: facefixtures.PlanSHA, CreatedAt: fx.Task.CreatedAt}}
		s.questions[fmt.Sprintf("%s %d", fx.Task.ID, revision)] = p.Questions
	}
	s.tasks = append(s.tasks, bt)
}

// boardEntries are the board's own items: a card holding three cases at
// the human, a human card with no question, a card waiting on its author,
// a card whose title and why line hold words longer than a line, the
// routine's card, and the status strip at each of its actions.
func boardEntries(all []facefixtures.Fixture) []galleryEntry {
	fixture := func(group, kind string) facefixtures.Fixture {
		i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool { return fx.Group == group && fx.Kind == kind })
		if i < 0 {
			panic("gallery: no fixture " + group + "/" + kind)
		}
		return all[i]
	}
	plain := func(t factoryd.Task) func(*rig) string {
		return func(r *rig) string {
			s := galleryScene()
			s.drawer, s.tasks = t.ID, []factoryd.BoardTask{{Task: t}}
			return r.seedScene(s)
		}
	}
	now := time.Now()
	return []galleryEntry{
		{galleryBoard, "three open cases", func(r *rig) string {
			fx := fixture(facefixtures.GroupCase, string(factoryd.CaseAgentEscalate))
			s := fixtureScene(fx, all)
			for i, kind := range []factoryd.CaseKind{factoryd.CaseSessionCap, factoryd.CaseGateStop} {
				c := *fixture(facefixtures.GroupCase, string(kind)).Case
				c.ID, c.TaskID = fx.Case.ID+int64(i+1), fx.Task.ID
				s.cases[fx.Task.ID] = append(s.cases[fx.Task.ID], c)
				s.asks[c.ID] = factoryd.CaseAsk{At: c.OpenedAt, Detail: maps.Clone(c.Facts)}
			}
			return r.seedScene(s)
		}},
		{galleryBoard, "human card with no question", plain(factoryd.Task{ID: facefixtures.TaskID(2400), Kind: "human", Status: "waiting-human",
			Repository: facefixtures.Repository, IssueNumber: 2400, Source: factoryd.SourceWebhook, Author: "sebastian", AuthorWriter: true, CreatedAt: now.Add(-20 * time.Minute),
			Objective: "Renew the TLS certificate of git.example\n\nThe certificate expires on 2026-12-02. Renew it at the registrar and load it into the ingress's secret."})},
		{galleryBoard, "author wait", plain(factoryd.Task{ID: facefixtures.TaskID(2401), Kind: "agent", Status: "waiting-poster", Track: policy.TrackDirect,
			Process: "spec", Repository: facefixtures.Repository, IssueNumber: 2401, Source: factoryd.SourceWebhook, Author: "alice", CreatedAt: now.Add(-6 * time.Hour),
			Gate: factoryd.GateIntake, GateSince: new(now.Add(-5 * time.Hour)), BudgetEUR: 2,
			Objective: "The export drops archived cards\n\nAn export of a project with archived cards lists none of them. Expected: every card, archived ones marked."})},
		{galleryBoard, "long words", plain(factoryd.Task{ID: facefixtures.TaskID(2402), Kind: "agent", Status: "queued", Track: policy.TrackDirect,
			Repository: facefixtures.Repository, IssueNumber: 2402, Source: factoryd.SourceAPI, Author: "factory-chat", ChatFiled: true, CreatedAt: now.Add(-2 * time.Hour),
			Objective: "Fix TestWallClockStopShipsTheWorkAcrossEveryRetryAndHandover\n\nThe test fails on a retry.",
			Why:       "The page https://git.example/sf/code/wiki/Wall-clock-stops-on-the-runner-and-its-retries says why the run stops before it ships."})},
		{galleryBoard, "routine card", func(r *rig) string {
			s := galleryScene()
			s.addRoutines(now, false)
			r.seedScene(s)
			return galleryRoutine + labCodeProject.Name + "/" + labBump.Name
		}},
		{galleryBoard, "status: Fund the chat", func(r *rig) string {
			s := galleryScene()
			chat := factoryd.Task{ID: factoryd.ChatTaskID(labCodeProject.Name, now), Kind: factoryd.KindChat, Status: "running", Repository: facefixtures.Repository,
				BudgetEUR: 5, SpentEUR: 5.02, Source: factoryd.SourceChat, CreatedAt: now.Add(-10 * 24 * time.Hour)}
			s.tasks = []factoryd.BoardTask{{Task: chat}}
			s.events = []factoryd.Event{{At: now.Add(-30 * time.Minute), TaskID: chat.ID, Kind: factoryd.EventChatBudgetSpent, Actor: factoryd.FactoryDecider,
				Via: factoryd.ViaFactoryd, Detail: map[string]any{"budget_eur": 5.0, "spent_eur": 5.02, "suggest_eur": 2.0}}}
			r.seedScene(s)
			return galleryStatus
		}},
		{galleryBoard, "status: Deploy now", func(r *rig) string {
			r.seedScene(galleryScene())
			return galleryStatus
		}},
	}
}

// galleryFace is one entry as the gallery page shows it: the small card
// as the board's column holds it, the opened card beside it, and a
// problem when the entry did not render. Strip: the faces are the status
// strip and its panel, not a card and its drawer.
type galleryFace struct {
	ID, Group, Kind, Problem string
	Small, Open              template.HTML
	Strip                    bool
}

// galleryFaces seats e on a fresh rig and renders its faces: the board's
// columns holding the card alone and the card's drawer at its overview, a
// routine's ghost and its drawer, or the status strip shut and open.
func galleryFaces(t *testing.T, e galleryEntry) (galleryFace, error) {
	f := galleryFace{ID: "g-" + gallerySlug(e.Group+"-"+e.Kind), Group: e.Group, Kind: e.Kind}
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	target := e.Seat(r)
	get := func(path string) (string, error) {
		rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck)
		if rec.Code != http.StatusOK {
			line, _, _ := strings.Cut(rec.Body.String(), "\n")
			return "", fmt.Errorf("GET %s = %d: %s", path, rec.Code, line)
		}
		return rec.Body.String(), nil
	}
	page, err := get("/?project=" + labCodeProject.Name)
	if err != nil {
		return f, err
	}
	var open string
	switch {
	case target == galleryStatus:
		f.Strip = true
		f.Small, f.Open, err = statusFaces(page)
		return f, err
	case strings.HasPrefix(target, galleryRoutine):
		name := strings.TrimPrefix(target, galleryRoutine)
		if f.Small, err = boardItems(page, "data-ghost", name); err == nil {
			open, err = get("/routines/" + name + "/drawer")
		}
	default:
		if f.Small, err = boardItems(page, "data-card", target); err == nil {
			open, err = get("/cards/" + target + "/drawer?tab=overview")
		}
	}
	f.Open = template.HTML(open)
	return f, err
}

// gallerySlug is s with every run of characters but letters and digits one
// dash, lower case.
func gallerySlug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// boardItems are the board's columns that show the items whose attr is
// value, each holding those items alone.
func boardItems(page, attr, value string) (template.HTML, error) {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "", err
	}
	var columns []*html.Node
	for n := range doc.Descendants() {
		if n.Type == html.ElementNode && n.Data == "section" && slices.Contains(strings.Fields(htmlAttr(n, "class")), "column") {
			columns = append(columns, n)
		}
	}
	var b strings.Builder
	for _, col := range columns {
		var cards *html.Node
		for c := range col.ChildNodes() {
			if c.Type == html.ElementNode && htmlAttr(c, "class") == "cards" {
				cards = c
			}
		}
		if cards == nil {
			continue
		}
		var drop []*html.Node
		shown := false
		for c := range cards.ChildNodes() {
			if c.Type == html.ElementNode && htmlAttr(c, attr) == value {
				shown = true
			} else {
				drop = append(drop, c)
			}
		}
		if !shown {
			continue
		}
		for _, c := range drop {
			cards.RemoveChild(c)
		}
		if err := html.Render(&b, col); err != nil {
			return "", err
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("the board shows no item whose %s is %q", attr, value)
	}
	return template.HTML(b.String()), nil
}

// statusFaces are the page's status strip shut, and open on its panel.
func statusFaces(page string) (shut, open template.HTML, err error) {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "", "", err
	}
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode || htmlAttr(n, "id") != "status-line" {
			continue
		}
		var b strings.Builder
		if err := html.Render(&b, n); err != nil {
			return "", "", err
		}
		n.Attr = append(n.Attr, html.Attribute{Key: "open"})
		var o strings.Builder
		if err := html.Render(&o, n); err != nil {
			return "", "", err
		}
		return template.HTML(b.String()), template.HTML(o.String()), nil
	}
	return "", "", errors.New("the page has no status strip")
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// galleryPage renders every entry of the gallery, an entry that fails
// with its problem in place of its faces.
func galleryPage(t *testing.T) []galleryFace {
	var out []galleryFace
	for _, e := range galleryEntries() {
		f, err := galleryFaces(t, e)
		if err != nil {
			f.Problem = err.Error()
		}
		out = append(out, f)
	}
	return out
}

// censusCardKinds are the card kinds of the board-cards census
// (docs/board-cards-problem-2026-11-16.md, appendix A), a combined row split
// into one kind per name it holds.
var censusCardKinds = []string{
	"direct work card", "plan card", "plan child: foundation", "plan child: feature", "plan child: integration", "delta card",
	"retry replacement", "successor", "delegated card", "helper task: screening", "helper task: sizing", "helper task: prover",
	"tool request", "roll", "main fix", "revert", "group revert", "chain repair (agent)", "alert (agent)", "routine (agent)",
	"template update", "schema copy", "GitHub sync", "image removal",
	"human issue", "reproduction-rule card", "workflow-files card", "card moved to a human", "factory pull request on a project without agents",
	"chat escalation", "alert (human)", "chain stall", "chain repair (human)", "roll refused", "roll failed", "rerun", "still red",
	"config rejected", "month cap", "judgment budget", "production held", "routine for a human",
}

// humanCardSources are the card sources that file a human card alone,
// which stands in Needs you; every other source's card stands in each
// column.
var humanCardSources = []string{
	factoryd.SourceRollRefused, factoryd.SourceRollFailed, factoryd.SourceRerun, factoryd.SourceStillRed, factoryd.SourceChainStall,
	factoryd.SourceChat, factoryd.SourceChatBudget, factoryd.SourceMonthCap, factoryd.SourceConfig, factoryd.SourceProductionHeld,
}

// galleryColumns are the board's columns as a source entry's kind names
// them, with the title the board heads each with.
var galleryColumns = map[string]string{"needs-you": "Needs you", "queued": "Queued", "in-progress": "In progress", "done": "Done"}

// The gallery holds an entry for every case kind, human action, approval
// kind and monitor flag, for every card source in each column it stands
// in, for every card kind of the census and for the board's own items;
// each entry's seat renders its small card and its opened card, and a
// source's small card stands in the column its kind names.
func TestGalleryHoldsEveryKind(t *testing.T) {
	entries := galleryEntries()
	holds := func(group string, match func(kind string) bool) bool {
		return slices.ContainsFunc(entries, func(e galleryEntry) bool { return e.Group == group && match(e.Kind) })
	}
	named := func(want string) func(string) bool { return func(k string) bool { return k == want } }
	need := func(group string, kinds []string) {
		for _, k := range kinds {
			if !holds(group, named(k)) {
				t.Errorf("the gallery holds no entry of %s %q", group, k)
			}
		}
	}
	var cases []string
	for _, k := range factoryd.CaseKinds() {
		cases = append(cases, string(k))
	}
	need(facefixtures.GroupCase, cases)
	need(facefixtures.GroupAction, factoryd.HumanActions())
	need(facefixtures.GroupApproval, factoryd.ApprovalKinds())
	need(facefixtures.GroupFlag, factoryd.MonitorFlags())
	need(facefixtures.GroupCard, censusCardKinds)
	for _, s := range factoryd.CardSources() {
		columns := slices.Collect(maps.Keys(galleryColumns))
		if slices.Contains(humanCardSources, s) {
			columns = []string{"needs-you"}
		}
		for _, col := range columns {
			need(facefixtures.GroupSource, []string{s + "/" + col})
		}
	}
	need(galleryBoard, []string{"three open cases", "human card with no question", "author wait", "long words", "routine card", "status: Fund the chat", "status: Deploy now"})

	for _, e := range entries {
		t.Run(e.Group+"/"+e.Kind, func(t *testing.T) {
			f, err := galleryFaces(t, e)
			if err != nil {
				t.Fatal(err)
			}
			if action, ok := strings.CutPrefix(e.Kind, "status: "); ok && !strings.Contains(string(f.Open), ">"+action+"</button>") {
				t.Errorf("the status panel offers no %s:\n%s", action, f.Open)
			}
			if _, col, ok := strings.Cut(e.Kind, "/"); ok && e.Group == facefixtures.GroupSource {
				small := string(f.Small)
				if n := strings.Count(small, `<section class="column`); n != 1 || !strings.Contains(small, ">"+galleryColumns[col]+` <span class="count">`) {
					t.Errorf("the card stands in %d columns, wanted %s alone:\n%s", n, galleryColumns[col], small)
				}
			}
		})
	}
}
