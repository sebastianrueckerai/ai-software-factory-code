package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
	"golang.org/x/net/html"
)

// openedOf is the opened card of drawer body: its article.
func openedOf(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out []*html.Node
	for n := range doc.Descendants() {
		if n.Type == html.ElementNode && n.Data == "article" && hasClass(n, "opened") {
			out = append(out, n)
		}
	}
	if len(out) != 1 {
		t.Fatalf("the drawer holds %d opened cards:\n%s", len(out), body)
	}
	return out[0]
}

// textOutside is the text n shows outside the elements skip holds for.
func textOutside(n *html.Node, skip func(*html.Node) bool) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		for c := range m.ChildNodes() {
			switch {
			case c.Type == html.TextNode:
				b.WriteString(c.Data)
			case c.Type == html.ElementNode && !skip(c):
				walk(c)
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// quoted: n holds a writer's own words, quoted whole.
func quoted(n *html.Node) bool { return hasClass(n, "quote") }

// openedProse are the classes of the prose above an opened card's folds.
var openedProse = []string{"o-title", "o-why", "o-question", "o-askwhy", "o-story", "o-by", "o-who", "o-note", "o-sent", "o-summary"}

// Every opened card of the gallery meets the contract: the head, the ask
// block, the summary and the folds in that order; at most 900 characters
// of prose above the folds; 2 to 4 answers in the ask block, each a
// button with its effect line, and beside an answer the viewer may not
// press, or in place of all, the line that names who can; no field shows
// before a press; every fold is
// closed; every answer posts what it names; a plan's card has the plan
// with its own row marked; every card says what each session got; a live
// card can be ended; and the texts pass the guide's checks and name none
// of the factory's own words.
func TestEveryOpenedCardMeetsTheContract(t *testing.T) {
	for _, e := range galleryEntries() {
		t.Run(e.Group+"/"+e.Kind, func(t *testing.T) {
			r := newRig(t)
			ck, _ := r.loggedIn(t)
			target := e.Seat(r)
			if target == galleryStatus || strings.HasPrefix(target, galleryRoutine) || strings.HasPrefix(target, galleryInbox+":") {
				t.Skip("the status strip, a routine and an inbox row have no opened card")
			}
			rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+target+"/drawer?tab=overview", nil), ck)
			if rec.Code != http.StatusOK {
				t.Fatalf("the drawer = %d: %s", rec.Code, rec.Body)
			}
			var card *factoryd.Task
			if i := slices.IndexFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == target }); i >= 0 {
				card = &r.tasks.tasks[i].Task
			}
			known := func(n int64) bool {
				_, ok, err := r.tasks.TaskByIssue(t.Context(), labCodeProject.Repository, n)
				return ok && err == nil
			}
			checkOpenedCard(t, rec.Body.String(), card, known)
		})
	}
}

func checkOpenedCard(t *testing.T, body string, card *factoryd.Task, known func(int64) bool) {
	t.Helper()
	o := openedOf(t, body)
	var b strings.Builder
	if err := html.Render(&b, o); err != nil {
		t.Fatal(err)
	}
	shown := b.String()
	one := func(name string) *html.Node {
		got := classed(o, name)
		if len(got) > 1 {
			t.Fatalf("the opened card holds %d %s:\n%s", len(got), name, shown)
		}
		if len(got) == 0 {
			return nil
		}
		return got[0]
	}
	head, ask, summary, folds := one("o-head"), one("o-ask"), one("o-summary"), one("o-folds")
	if head == nil || folds == nil {
		t.Fatalf("the opened card has no head or no folds:\n%s", shown)
	}
	order := map[*html.Node]int{}
	i := 0
	for n := range o.Descendants() {
		order[n] = i
		i++
	}
	last := -1
	for _, part := range []*html.Node{head, ask, summary, folds} {
		if part == nil {
			continue
		}
		if order[part] < last {
			t.Errorf("the opened card's parts stand out of order (head, ask, summary, folds):\n%s", shown)
		}
		last = order[part]
	}
	if len(classed(o, "o-title")) != 1 || nodeText(classed(o, "o-title")[0]) == "" {
		t.Errorf("the head has no title:\n%s", shown)
	}

	prose := 0
	for _, name := range openedProse {
		for _, p := range classed(o, name) {
			prose += utf8.RuneCountInString(nodeText(p))
		}
	}
	if prose > 900 {
		t.Errorf("the opened card holds %d characters of prose above its folds, want at most 900:\n%s", prose, shown)
	}

	if ask != nil {
		if len(classed(ask, "o-question")) != 1 {
			t.Errorf("the ask block has no question:\n%s", shown)
		}
		answers := classed(ask, "answer")
		disabled := slices.ContainsFunc(answers, func(a *html.Node) bool {
			return slices.ContainsFunc(classed(a, "pick"), func(p *html.Node) bool {
				return slices.ContainsFunc(p.Attr, func(a html.Attribute) bool { return a.Key == "disabled" })
			})
		})
		switch who := classed(ask, "o-who"); {
		case len(who) > 0 && len(answers) > 0 && !disabled:
			t.Errorf("the ask block names who can answer beside answers the viewer may all press:\n%s", shown)
		case len(answers) > 4 || len(who) == 0 && len(answers) < 2:
			t.Errorf("the ask block holds %d answers, want 2 to 4:\n%s", len(answers), shown)
		}
		for _, a := range answers {
			picks, effects := classed(a, "pick"), classed(a, "effect")
			if len(picks) != 1 || nodeText(picks[0]) == "" || len(effects) != 1 || nodeText(effects[0]) == "" {
				t.Errorf("an answer is no button with its effect line:\n%s", shown)
			}
		}
	}

	for n := range o.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		if n.Data == "input" && htmlAttr(n, "type") == "hidden" && htmlAttr(n, "value") == "" && within(n, o, func(p *html.Node) bool { return hasClass(p, "answer") }) {
			t.Errorf("an answer posts %s empty:\n%s", htmlAttr(n, "name"), shown)
		}
		if n.Data == "details" && slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == "open" }) {
			t.Errorf("a fold stands open: %s", nodeText(n))
		}
		field := n.Data == "select" || n.Data == "textarea" || n.Data == "input" && htmlAttr(n, "type") != "hidden"
		if field && !within(n, o, func(p *html.Node) bool {
			return p.Data == "details" || slices.ContainsFunc(p.Attr, func(a html.Attribute) bool { return a.Key == "hidden" })
		}) {
			t.Errorf("the field %s shows before a press:\n%s", htmlAttr(n, "name"), shown)
		}
	}

	fold := func(label string) *html.Node {
		for n := range o.Descendants() {
			if n.Type == html.ElementNode && n.Data == "summary" && strings.HasPrefix(nodeText(n), label) {
				return n.Parent
			}
		}
		return nil
	}
	if fold("What each session got") == nil {
		t.Errorf("the opened card does not say what each session got:\n%s", shown)
	}
	if fold("Other actions") == nil {
		t.Errorf("the opened card has no other actions:\n%s", shown)
	}
	if c := card; c != nil {
		child := c.Source == factoryd.SourcePlan && c.Parent != ""
		if p := fold("The plan"); (child || c.Track == policy.TrackPlan) && p == nil {
			t.Errorf("the card of a plan has no fold of its plan:\n%s", shown)
		} else if child && len(classed(p, "this")) != 1 {
			t.Errorf("the plan's fold does not mark this card:\n%s", shown)
		}
		if !factoryd.Final(c.Status) && !slices.ContainsFunc(classed(o, "pick"), func(n *html.Node) bool { return nodeText(n) == "End the card" }) {
			t.Errorf("the live card cannot be ended from its opened card:\n%s", shown)
		}
	}

	captured := card != nil && slices.Contains(capturedCards, card.IssueNumber)
	own := ""
	if card != nil && card.IssueNumber > 0 {
		own = "#" + strconv.FormatInt(card.IssueNumber, 10)
	}
	scanned := []*html.Node{head}
	if ask != nil {
		scanned = append(scanned, ask)
	}
	scanned = append(scanned, classed(o, "o-sent")...)
	scanned = append(scanned, classed(o, "o-summary")...)
	for _, s := range scanned {
		for d := range s.Descendants() {
			if d.Type != html.TextNode || within(d, s, func(p *html.Node) bool { return p.Data == "a" || quoted(p) }) {
				continue
			}
			for _, ref := range refNumber.FindAllString(d.Data, -1) {
				if n, _ := strconv.ParseInt(ref[1:], 10, 64); ref != own && known(n) {
					t.Errorf("%s stands on the opened card without its link:\n%s", ref, shown)
				}
			}
		}
	}
	var texts []string
	for _, name := range append(slices.Clone(openedProse), "o-line", "pick", "effect", "o-label") {
		for _, p := range classed(o, name) {
			texts = append(texts, textOutside(p, quoted))
		}
	}
	for n := range o.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		switch n.Data {
		case "summary", "label", "legend":
			texts = append(texts, textOutside(n, quoted))
		}
		texts = append(texts, htmlAttr(n, "aria-label"), htmlAttr(n, "title"), htmlAttr(n, "placeholder"))
	}
	texts = slices.DeleteFunc(texts, func(s string) bool { return s == "" })
	if captured {
		title := factoryd.CardTitle(*card)
		texts = slices.DeleteFunc(texts, func(s string) bool {
			return strings.Contains(s, title) || card.Summary != "" && strings.Contains(s, card.Summary)
		})
	}
	for _, s := range texts {
		if faults := writingguide.Lint(s); len(faults) > 0 {
			t.Errorf("%q: %v", s, faults)
		}
		for _, re := range faceForbidden {
			if m := re.FindString(s); m != "" {
				t.Errorf("%q names %q", s, m)
			}
		}
	}
}

// answerPost is an answer's form on the opened card as its send posts
// it: where, the values it opens with (its hidden inputs and its fields'
// defaults), the column it moves the card to, and its button's label.
type answerPost struct {
	Action, To, Label string
	Values            url.Values
}

// answerForm is the form of answer key on drawer body.
func answerForm(t *testing.T, body, key string) answerPost {
	t.Helper()
	o := openedOf(t, body)
	for n := range o.Descendants() {
		if n.Type != html.ElementNode || n.Data != "form" || htmlAttr(n, "data-answer") != key {
			continue
		}
		p := answerPost{Action: htmlAttr(n, "action"), To: htmlAttr(n, "data-to"), Values: url.Values{}}
		for f := range n.Descendants() {
			if f.Type != html.ElementNode {
				continue
			}
			if hasClass(f, "pick") {
				p.Label = nodeText(f)
			}
			name := htmlAttr(f, "name")
			switch {
			case name == "":
			case f.Data == "input" && htmlAttr(f, "type") == "checkbox":
				if slices.ContainsFunc(f.Attr, func(a html.Attribute) bool { return a.Key == "checked" }) {
					p.Values.Add(name, htmlAttr(f, "value"))
				}
			case f.Data == "input":
				p.Values.Add(name, htmlAttr(f, "value"))
			case f.Data == "textarea":
				p.Values.Add(name, nodeText(f))
			case f.Data == "select":
				for op := range f.Descendants() {
					if op.Type == html.ElementNode && op.Data == "option" && slices.ContainsFunc(op.Attr, func(a html.Attribute) bool { return a.Key == "selected" }) {
						p.Values.Add(name, htmlAttr(op, "value"))
					}
				}
			}
		}
		return p
	}
	t.Fatalf("the opened card holds no answer %q:\n%s", key, body)
	return answerPost{}
}

// One send posts the answer, its fields and its note together: the
// opened card's "Clear the audit's findings" opens its boxes and its
// reason, and the one send carries the ticked findings and the reason to
// Act as one answer.
func TestOneSendPostsAnswerFieldsAndNote(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	cappedCard(r, t)
	two := []string{"secrets at rig_test.go:71", "loopback endpoint at rig_test.go:64"}
	seatAsk(r, "t-e", factoryd.CaseGateStop, roomyFacts(func(f *factoryd.CaseFacts) {
		f.Head, f.FindingTexts = "h2", map[string]string{}
		for _, text := range append(two, "test-only credential at rig_test.go:41") {
			id := factoryd.FindingID(text)
			f.Findings, f.AuditFindings, f.FindingTexts[id] = append(f.Findings, id), append(f.AuditFindings, id), text
		}
	}))
	body := drawer(t, r, ck, "t-e", "overview")
	clear := answerForm(t, body, factoryd.ActConfirmAudit)
	action, values := clear.Action, clear.Values
	if action != "/tasks/t-e/resolve" || values.Get("action") != factoryd.ActConfirmAudit || values.Get("case_id") != "1" {
		t.Fatalf("the answer posts to %q with %v, want the case's resolve", action, values)
	}
	if values.Has("confirm_audit.findings") || values.Get("confirm_audit.note") != "" {
		t.Fatalf("the clearing opens with %v, want no box ticked and no reason", values)
	}
	values["confirm_audit.findings"] = two
	values.Set("confirm_audit.note", "The rig's fakes.")
	if rec := r.do(post(action, values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the send = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActConfirmAudit || got.Res.Args["findings"] != strings.Join(two, "\n") || got.Res.Note != "The rig's fakes." {
		t.Fatalf("Act took %+v, want the two ticked findings and the reason in one answer", got.Res)
	}
}

// A press on an answer whose fields open, then a press on a second answer,
// closes the first answer's fields, and the send posts the second answer
// alone: on #2004's opened card "Add money" opens its amount, "End the
// card" closes it, and the send holds no amount (Review Focus 4). It drives
// the page in a browser, which the test needs.
func TestSecondAnswerClosesTheFirst(t *testing.T) {
	modules := cmpEnv("NODE_PATH", "/tmp/claude-1000/pw/node_modules")
	node, err := exec.LookPath("node")
	if _, serr := os.Stat(filepath.Join(modules, "playwright")); err != nil || serr != nil {
		t.Skip("no node or no playwright")
	}
	all := facefixtures.All()
	i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool {
		return fx.Group == facefixtures.GroupCase && fx.Kind == string(factoryd.CaseBudgetFloor)
	})
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	id := r.seedScene(fixtureScene(all[i], all))
	srv := httptest.NewServer(r.c)
	defer srv.Close()
	const driver = `const { chromium } = require("playwright");
(async () => {
  const [base, name, value, id, project] = process.argv.slice(1);
  const browser = await chromium.launch();
  const ctx = await browser.newContext();
  await ctx.addCookies([{name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax"}]);
  const page = await ctx.newPage();
  const posts = [];
  page.on("request", (q) => { if (q.method() === "POST") posts.push(q.postData() || ""); });
  await page.goto(base + "/?project=" + project + "#card=" + id);
  const ask = page.locator("#drawer .o-ask");
  await ask.locator("button.pick", {hasText: "Add money"}).click();
  const amount = ask.locator("form[data-answer=fund] input[name='fund.eur']");
  const shown = await amount.isVisible();
  await amount.fill("1.50");
  await ask.locator("button.pick", {hasText: "End the card"}).click();
  const hidden = !(await amount.isVisible());
  const sends = await ask.locator("button.send:visible").count();
  await ask.locator("form[data-answer=close] button.send").click();
  await page.waitForTimeout(500);
  console.log(JSON.stringify({shown, hidden, sends, posts}));
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, srv.URL, ck.Name, ck.Value, id, labCodeProject.Name)
	cmd.Env = append(os.Environ(), "NODE_PATH="+modules)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("the browser: %v\n%s", err, stderr)
	}
	var got struct {
		Shown, Hidden bool
		Sends         int
		Posts         []string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the browser said %s: %v", out, err)
	}
	if !got.Shown || !got.Hidden || got.Sends != 1 {
		t.Errorf("Add money's amount shown %v, closed by End the card %v, %d sends in view; want shown, closed and one", got.Shown, got.Hidden, got.Sends)
	}
	if len(got.Posts) != 1 {
		t.Fatalf("the page posted %d times, want once: %q", len(got.Posts), got.Posts)
	}
	posted, err := url.ParseQuery(got.Posts[0])
	if err != nil {
		t.Fatal(err)
	}
	if posted.Get("action") != factoryd.ActClose || posted.Has("fund.eur") || posted.Has("eur") {
		t.Errorf("the send posted %v, want the end of the card and no amount", posted)
	}
}

func cmpEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
