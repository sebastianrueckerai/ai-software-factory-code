package console

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"golang.org/x/net/html"
)

// The words no face shows: a task id, a hash, a branch, the factory's
// own nouns, a snake_case name and a gate's step name.
var refNumber = regexp.MustCompile(`#\d+`)

// smallItem is one board item as a column shows it: the article, the
// column's title, the card it stands for as the board reads it, none for
// a routine, and whether the store holds a card of a number.
type smallItem struct {
	n      *html.Node
	column string
	board  *factoryd.BoardTask
	card   *factoryd.Task
	known  func(n int64) bool
}

// waitWords is what a queued card's wait names, by the fact that holds
// it: the card it waits on, its image, the month cap, its issue, else
// its turn.
func waitWords(b factoryd.BoardTask) string {
	switch r := b.QueueReason; {
	case b.WaitingOn != "" || b.Status == "queued" && len(b.Dependencies) > 0:
		return "waits for #"
	case b.Status == "waiting-image":
		return "waits for its image"
	case r == factoryd.QueueMonthCap:
		return "waits for the project's month cap"
	case strings.HasPrefix(r, factoryd.QueueWaitsOn):
		return "waits for #"
	case r == factoryd.QueueNoIssue:
		return "waits for its issue"
	}
	return "waits for its turn to start"
}

// capturedCards are the cards whose text the gallery keeps as the live
// board held it on 2026-11-16, filed before the filing door checked a
// brief, so the faults named in the spec show: their own text breaks the
// brief's rules, and the face shows it whole.
var capturedCards = []int64{1850, 1947, 2100, 2101, 2102}

// smallItems are the items of the board page whose attr is value.
func smallItems(t *testing.T, page, attr, value string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var out []*html.Node
	for n := range doc.Descendants() {
		if n.Type == html.ElementNode && n.Data == "article" && htmlAttr(n, attr) == value {
			out = append(out, n)
		}
	}
	return out
}

// columnOf is the title of the board column n stands in.
func columnOf(n *html.Node) string {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "section" && slices.Contains(strings.Fields(htmlAttr(p, "class")), "column") {
			for c := range p.ChildNodes() {
				if c.Type != html.ElementNode || c.Data != "h3" {
					continue
				}
				for _, title := range columnTitles {
					if strings.HasPrefix(nodeText(c), title) {
						return title
					}
				}
			}
		}
	}
	return ""
}

// nodeText is the text n shows.
func nodeText(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// classed are the elements under n with class name.
func classed(n *html.Node, name string) []*html.Node {
	var out []*html.Node
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && slices.Contains(strings.Fields(htmlAttr(d, "class")), name) {
			out = append(out, d)
		}
	}
	return out
}

func hasClass(n *html.Node, name string) bool {
	return slices.Contains(strings.Fields(htmlAttr(n, "class")), name)
}

// within says whether n sits inside an element that match holds for,
// short of stop.
func within(n, stop *html.Node, match func(*html.Node) bool) bool {
	for p := n.Parent; p != nil && p != stop; p = p.Parent {
		if p.Type == html.ElementNode && match(p) {
			return true
		}
	}
	return false
}

// faceCutting are the rules of the face's stylesheet that would cut a
// title or a why line: an ellipsis, a line clamp, a line that never wraps.
var faceCutting = regexp.MustCompile(`text-overflow:\s*ellipsis|line-clamp|white-space:\s*nowrap`)

// Every small card of the gallery meets the face contract: its title at
// most 70 runes, an ask's at most 110 (the ask limit), and never cut; a
// why line unless it is finished, a human's bare issue or a routine,
// whose file holds no why line; its place when it belongs to a plan; why
// it waits when it is queued; at most 300 characters of prose; at most
// one button that acts and no field; every #N a link but its own number;
// none of the factory's own machinery.
func TestEverySmallCardMeetsTheContract(t *testing.T) {
	css, err := os.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range strings.Split(string(css), "}") {
		sel, body, ok := strings.Cut(rule, "{")
		if ok && regexp.MustCompile(`\.face\b|\.para\b|\.why\b|\.title\b`).MatchString(sel) && faceCutting.MatchString(body) &&
			!strings.Contains(sel, ".visually-hidden") && !strings.Contains(sel, ".foot") && !strings.Contains(sel, ".band") {
			t.Errorf("the rule %s{%s} cuts the face's text", strings.TrimSpace(sel), body)
		}
	}
	for _, e := range galleryEntries() {
		t.Run(e.Group+"/"+e.Kind, func(t *testing.T) {
			r := newRig(t)
			ck, _ := r.loggedIn(t)
			target := e.Seat(r)
			switch {
			case target == galleryStatus:
				t.Skip("the status strip is no card")
			case strings.HasPrefix(target, galleryInbox+":"):
				t.Skip("an inbox row is no card")
			}
			rec := r.do(httptest.NewRequest(http.MethodGet, "/?project="+labCodeProject.Name, nil), ck)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET / = %d", rec.Code)
			}
			attr, value := "data-card", target
			if name, ok := strings.CutPrefix(target, galleryRoutine); ok {
				attr, value = "data-ghost", name
			}
			var board *factoryd.BoardTask
			var card *factoryd.Task
			if i := slices.IndexFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == target }); i >= 0 {
				board, card = &r.tasks.tasks[i], &r.tasks.tasks[i].Task
			}
			nodes := smallItems(t, rec.Body.String(), attr, value)
			if len(nodes) == 0 {
				t.Fatalf("the board shows no item %s=%q", attr, value)
			}
			known := func(n int64) bool {
				_, ok, err := r.tasks.TaskByIssue(t.Context(), labCodeProject.Repository, n)
				return ok && err == nil
			}
			for _, n := range nodes {
				checkSmallCard(t, smallItem{n, columnOf(n), board, card, known})
			}
		})
	}
}

func checkSmallCard(t *testing.T, it smallItem) {
	t.Helper()
	var b strings.Builder
	if err := html.Render(&b, it.n); err != nil {
		t.Fatal(err)
	}
	shown := b.String()
	titles := classed(it.n, "title")
	if len(titles) != 1 {
		t.Fatalf("the face has %d titles:\n%s", len(titles), shown)
	}
	title := nodeText(titles[0])
	captured := it.card != nil && slices.Contains(capturedCards, it.card.IssueNumber) && len(classed(it.n, "ask-row")) == 0
	// a human's issue keeps its own title, which the door does not bound
	human := it.card != nil && factoryd.BriefJobOf(*it.card) == factoryd.BriefHumanIssue && len(classed(it.n, "ask-row")) == 0
	limit := 70
	if len(classed(it.n, "ask-row")) > 0 {
		limit = 110
	}
	if n := utf8.RuneCountInString(title); n == 0 || n > limit && !captured && !human {
		t.Errorf("the title %q has %d runes, want 1 to %d", title, n, limit)
	}
	if strings.HasSuffix(title, "…") || strings.HasSuffix(title, "...") {
		t.Errorf("the title %q is cut", title)
	}
	final := hasClass(it.n, "final")
	bare := it.card != nil && it.card.Why == "" && (factoryd.BriefJobOf(*it.card) == factoryd.BriefHumanIssue || captured)
	routine := it.card == nil
	if whys := classed(it.n, "why"); (len(whys) == 0 || nodeText(whys[0]) == "") && !final && !bare && !routine {
		t.Errorf("the face has no why line:\n%s", shown)
	}
	if final && len(classed(it.n, "why")) > 0 {
		t.Errorf("a finished face shows its why line:\n%s", shown)
	}
	if c := it.card; c != nil && (c.Source == factoryd.SourcePlan && c.Parent != "" || c.Track == policy.TrackPlan) {
		if places := classed(it.n, "place"); len(places) == 0 || nodeText(places[0]) == "" {
			t.Errorf("the card of a plan shows no place:\n%s", shown)
		}
	}
	if it.column == "Queued" && it.board != nil {
		waits := ""
		if w := classed(it.n, "waits"); len(w) > 0 {
			waits = nodeText(w[0])
		}
		if want := waitWords(*it.board); !strings.Contains(waits, want) {
			t.Errorf("the queued card's wait %q does not say %q:\n%s", waits, want, shown)
		}
	}
	prose := 0
	for _, name := range []string{"para", "part"} {
		for _, p := range classed(it.n, name) {
			prose += utf8.RuneCountInString(nodeText(p))
		}
	}
	if prose > 300 {
		t.Errorf("the face holds %d characters of prose, want at most 300:\n%s", prose, shown)
	}
	acts := 0
	for d := range it.n.Descendants() {
		if d.Type != html.ElementNode {
			continue
		}
		switch d.Data {
		case "button":
			if htmlAttr(d, "type") != "button" {
				acts++
			}
		case "pre", "textarea", "select":
			t.Errorf("the face holds a %s:\n%s", d.Data, shown)
		case "input":
			if htmlAttr(d, "type") != "hidden" {
				t.Errorf("the face holds a field %s:\n%s", htmlAttr(d, "name"), shown)
			}
		}
	}
	if acts > 1 {
		t.Errorf("the face holds %d buttons, want at most one:\n%s", acts, shown)
	}
	own := ""
	if it.card != nil && it.card.IssueNumber > 0 {
		own = "#" + strconv.FormatInt(it.card.IssueNumber, 10)
	}
	for d := range it.n.Descendants() {
		if d.Type != html.TextNode || within(d, it.n, func(p *html.Node) bool { return p.Data == "a" || hasClass(p, "ref-of") }) {
			continue
		}
		for _, ref := range refNumber.FindAllString(d.Data, -1) {
			if n, _ := strconv.ParseInt(ref[1:], 10, 64); ref != own && it.known(n) {
				t.Errorf("%s stands on the face without its link:\n%s", ref, shown)
			}
		}
	}
	var texts []string
	for _, name := range []string{"para", "part", "foot", "band", "ask-row"} {
		for _, p := range classed(it.n, name) {
			texts = append(texts, nodeText(p))
		}
	}
	for d := range it.n.Descendants() {
		if d.Type == html.ElementNode {
			texts = append(texts, htmlAttr(d, "aria-label"), htmlAttr(d, "title"))
		}
	}
	if captured {
		texts = slices.DeleteFunc(texts, func(s string) bool { return strings.Contains(s, title) })
	}
	for _, s := range texts {
		for _, re := range facefixtures.Forbidden {
			if m := re.FindString(s); m != "" {
				t.Errorf("%q names %q", s, m)
			}
		}
	}
}

// smallCard is the board's small card of card id on the code project.
func smallCard(t *testing.T, r *rig, ck *http.Cookie, id string) string {
	t.Helper()
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project="+labCodeProject.Name, nil), ck).Body.String()
	return faceOf(t, body, id)
}

// A face's one button is the suggested answer: on #2004 at its budget
// floor the factory agent's successor, which posts the answer on its case
// at once; beside it who suggests it and the count of the others. A
// suggested answer with a field holds no button: its label opens the card
// with that answer's fields open.
func TestTheSuggestedAnswerIsTheOneButton(t *testing.T) {
	all := facefixtures.All()
	i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool {
		return fx.Group == facefixtures.GroupCase && fx.Kind == string(factoryd.CaseBudgetFloor)
	})
	fx := all[i]
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.seedScene(fixtureScene(fx, all))
	face := smallCard(t, r, ck, fx.Task.ID)
	for _, want := range []string{
		`<form method="post" action="/tasks/t-e758758121d7/resolve" hx-post="/tasks/t-e758758121d7/resolve" class="press" data-to="running">`,
		`<input type="hidden" name="case_id" value="5847"><input type="hidden" name="action" value="successor"><button type="submit" class="pick">Start a fresh card on this work</button></form>`,
		`<span class="by">factory agent&#39;s pick</span><span class="others">or <a href="#card=t-e758758121d7">3 others ›</a></span>`,
	} {
		if !strings.Contains(face, want) {
			t.Fatalf("the face lacks %s:\n%s", want, face)
		}
	}
	if n := strings.Count(face, `type="submit"`); n != 1 {
		t.Fatalf("the face holds %d buttons, want the suggestion's one:\n%s", n, face)
	}
	if rec := r.do(post("/tasks/t-e758758121d7/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"5847"}, "action": {"successor"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the press = %d: %s", rec.Code, rec.Body)
	}
	if got, _ := actedOnce(t, r); got.CaseID != 5847 || got.Rung != 3 || got.Res.Action != factoryd.ActSuccessor {
		t.Fatalf("Act took %+v, want the successor on case 5847 by the human", got)
	}

	scene := fixtureScene(fx, all)
	ask := scene.asks[fx.Case.ID]
	ask.Detail = maps.Clone(ask.Detail)
	ask.Detail["preselected"] = map[string]any{"action": factoryd.ActFund}
	scene.asks[fx.Case.ID] = ask
	r = newRig(t)
	ck, _ = r.loggedIn(t)
	r.seedScene(scene)
	face = smallCard(t, r, ck, fx.Task.ID)
	if strings.Contains(face, `type="submit"`) || !strings.Contains(face, `<a class="pick-link" href="#card=t-e758758121d7:press=fund">Add money</a>`) {
		t.Fatalf("a suggestion with a field shows a button, or no link that opens the card:\n%s", face)
	}
}

// A queued card names the card it waits for with its title and state; a
// human's bare issue shows its title and its state, no why line and no
// empty line; a merged card's state is where its chain stands, linked to
// the run.
func TestTheFootSaysWhereACardStands(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-deps", Repository: "sf/code", IssueNumber: 7, Source: factoryd.SourceWebhook, CreatedAt: now,
			Objective: "Show the export\n\nShow it."}, WaitingOn: "#8"},
		{Task: factoryd.Task{ID: "t-b", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 8, Source: factoryd.SourceWebhook, CreatedAt: now,
			Objective: "Write the export\n\nWrite it."}},
		{Task: factoryd.Task{ID: "t-c", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 9, Source: factoryd.SourceWebhook, CreatedAt: now,
			PRNumber: 40, FinishedAt: &now, Objective: "Ship the export\n\nShip it."}, ChainRun: 812, ChainState: "running"},
	}
	if face := smallCard(t, r, ck, "t-a"); !strings.Contains(face, `<span class="it waits">waits for <a class="ref-link" href="#card=t-b">#8</a> <span class="ref-of">(Write the export, queued)</span></span>`) {
		t.Errorf("the waiting card does not name #8 with its title and state:\n%s", face)
	}
	face := smallCard(t, r, ck, "t-b")
	if !strings.Contains(face, `<p class="para"><strong class="title">Write the export</strong>.</p>`) || strings.Contains(face, `class="why"`) {
		t.Errorf("the bare issue's paragraph is not its title alone:\n%s", face)
	}
	if !strings.Contains(face, `<i class="dot" aria-hidden="true"></i>queued</span>`) {
		t.Errorf("the bare issue shows no state:\n%s", face)
	}
	if face := smallCard(t, r, ck, "t-c"); !strings.Contains(face, `/sf/code/actions/runs/812" target="_blank" rel="noopener">chain run 812</a></span>`) {
		t.Errorf("the merged card's state is not its chain's run:\n%s", face)
	}
}

// A queued card names the refusal admit recorded on it: the month cap, the
// card it waits on, its missing issue, else its turn. A card behind
// another in the merge queue names the card that holds the turn.
func TestTheFootSaysWhyACardWaits(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	queued := func(id string, n int64, reason string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: n, Source: factoryd.SourceWebhook,
			CreatedAt: now, QueueReason: reason, Objective: "Card " + id + "\n\nIt."}}
	}
	review := func(id string, n int64, ahead int, behind int64) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: n, Source: factoryd.SourceWebhook,
			CreatedAt: now, PRNumber: n + 100, Gate: factoryd.GateMergeQueue, GateSince: &now, Objective: "Card " + id + "\n\nIt."}, QueueAhead: ahead, MergeBehind: behind}
	}
	r.tasks.tasks = []factoryd.BoardTask{queued("t-cap", 30, factoryd.QueueMonthCap), queued("t-dep", 31, factoryd.QueueWaitsOn+"32"), queued("t-dep2", 32, ""),
		queued("t-iss", 0, factoryd.QueueNoIssue), queued("t-turn", 33, factoryd.WorkBoundReason+"4 of 4"),
		review("t-head", 40, 0, 0), review("t-two", 41, 1, 40), review("t-three", 42, 2, 40)}
	for id, want := range map[string]string{
		"t-cap":   `<span class="it waits">waits for the project&#39;s month cap to rise</span>`,
		"t-dep":   `<span class="it waits">waits for <a class="ref-link" href="#card=t-dep2">#32</a> <span class="ref-of">(Card t-dep2, queued)</span></span>`,
		"t-iss":   `<span class="it waits">waits for its issue to be filed</span>`,
		"t-turn":  `<span class="it waits">waits for its turn to start</span>`,
		"t-two":   `<span class="it waits">waits to merge behind <a class="ref-link" href="#card=t-head">#40</a>`,
		"t-three": `</a> <span class="ref-of">(Card t-head, `,
	} {
		if face := smallCard(t, r, ck, id); !strings.Contains(face, want) {
			t.Errorf("%s does not say %q:\n%s", id, want, face)
		}
	}
	if face := smallCard(t, r, ck, "t-three"); !strings.Contains(face, ` and 1 more card</span>`) {
		t.Errorf("t-three does not count the card between it and the turn:\n%s", face)
	}
}

// A failed card's state names its failure in one clause: the first
// sentence of its end's reason, none when that is no short clause.
func TestAFailureIsOneClause(t *testing.T) {
	for reason, want := range map[string]string{
		"The checks stayed red after three runs. The log names TestExport.": "the checks stayed red after three runs",
		"PR #40 was closed without a merge.":                                "PR #40 was closed without a merge",
		"session cap of 3 reached: the review asked for changes":            "",
		"":                                     "",
		strings.Repeat("a long reason ", 10):   "",
		"closed into successor t-5f3a12b4c9d0": "",
	} {
		if got := failureClause(reason); got != want {
			t.Errorf("failureClause(%q) = %q, want %q", reason, got, want)
		}
	}
}

// A card closed into its successor names the successor by its issue, a
// link with its title and state, never by its task id, on the board and
// on the card's own swap.
func TestAFailedCardNamesItsSuccessorByItsIssue(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	succ := factoryd.SuccessorID("t-old")
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-old", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 40, Source: factoryd.SourceWebhook, CreatedAt: now,
			FinishedAt: &now, Objective: "Export the board\n\nExport it."}},
		{Task: factoryd.Task{ID: succ, Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 41, Source: factoryd.SourceSuccessor, CreatedAt: now,
			Objective: "Carry the export on\n\nCarry it."}},
	}
	if _, err := r.events.Record(context.Background(), factoryd.Event{TaskID: "t-old", Repository: "sf/code", At: now, Kind: factoryd.EventTaskEnded,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"final": "failed", "reason": "closed into successor " + succ}}); err != nil {
		t.Fatal(err)
	}
	want := `failed: closed into successor <a class="ref-link" href="#card=` + succ + `">#41</a> <span class="ref-of">(Carry the export on, queued)</span>`
	board := smallCard(t, r, ck, "t-old")
	swap := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-old", nil), ck).Body.String()
	for name, face := range map[string]string{"the board": board, "the swap": swap} {
		if !strings.Contains(face, want) || strings.Contains(face, succ+"<") || strings.Contains(face, "successor t-") {
			t.Errorf("%s: the failed card does not name #41 by its link:\n%s", name, face)
		}
	}
}
