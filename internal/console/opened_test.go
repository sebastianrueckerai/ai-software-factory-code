package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
var openedProse = []string{"o-title", "o-why", "o-question", "o-askwhy", "o-story", "o-by", "o-who", "o-note", "o-sent", "o-summary", "effect", "problem"}

// Every opened card of the gallery meets the contract: the head, the ask
// block, the summary and the folds in that order; at most 900 characters
// of prose above the folds, effect, problem and quoted lines counted; 2 to 4 answers in the ask block, each a
// button with its effect line, and beside an answer the viewer may not
// press, or in place of all, the line that names who can; no field shows
// before a press; every fold is
// closed; every answer posts what it names on the card it stands on; no
// label stands twice; a
// plan's card has the plan
// with its own row marked; every card says what each session got; a live
// card can be ended; and the texts name none of the factory's own
// machinery.
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

// A card whose title is its ask shows the ask once: the head holds the
// title and the why, and the ask block opens with its story. A card whose
// title names its work keeps the question and its why in the ask block.
func TestOpenedCardSaysItsAskOnce(t *testing.T) {
	texts := func(kind string, classes ...string) []string {
		t.Helper()
		for _, e := range galleryEntries() {
			if e.Group+"/"+e.Kind != kind {
				continue
			}
			r := newRig(t)
			ck, _ := r.loggedIn(t)
			rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+e.Seat(r)+"/drawer?tab=overview", nil), ck)
			o := openedOf(t, rec.Body.String())
			var out []string
			for _, class := range classes {
				line := ""
				for _, n := range classed(o, class) {
					line += textOutside(n, func(*html.Node) bool { return false })
				}
				out = append(out, line)
			}
			return out
		}
		t.Fatalf("the gallery holds no %s", kind)
		return nil
	}
	equal(t, "a card whose title asks", texts("card/roll refused", "o-title", "o-why", "o-label", "o-question", "o-askwhy", "o-story"), []string{
		"Fix the signing of the images factoryd, gateway, then close this card.",
		"The factory deploys only images signed with its key, so these images stay off the cluster.",
		"Your call", "", "",
		"Main's run is green at commit 50f5db9, and 2 images of it carry no signature by the factory's key."})
	equal(t, "a card whose title names its work", texts("action/extend", "o-title", "o-why", "o-question", "o-askwhy"), []string{
		"Board export: import a snapshot back.",
		"Read a nightly snapshot back into an empty database.",
		"The card used all 3 sessions with 2 review findings open. How should it go on?",
		"A card runs only the sessions it was given, and this one has used all 3."})
	_, _, _, _, asks := openedEntry(t, "card", "roll refused", "")
	if want := `<h2 class="o-title" tabindex="-1">Fix the signing of the images <span class="term">factoryd</span>, <span class="term">gateway</span>, then close this card.</h2>`; !strings.Contains(asks, want) {
		t.Errorf("the title that asks marks no factory words, want %s:\n%s", want, asks)
	}
	_, _, _, _, names := openedEntry(t, "action", "extend", "")
	if want := `<h2 class="o-title" tabindex="-1">Board export: import a snapshot back.</h2>`; !strings.Contains(names, want) {
		t.Errorf("the title that names the work is not plain, want %s:\n%s", want, names)
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
	bare := func(name string) string {
		n := one(name)
		if n == nil {
			return ""
		}
		return strings.TrimRight(textOutside(n, func(*html.Node) bool { return false }), ".?!")
	}
	for _, pair := range [][2]string{{"o-title", "o-question"}, {"o-why", "o-askwhy"}} {
		if said := bare(pair[1]); said != "" && said == bare(pair[0]) {
			t.Errorf("the ask repeats the head's line %q", said)
		}
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
			if !within(p, o, func(n *html.Node) bool { return n == folds }) {
				prose += utf8.RuneCountInString(nodeText(p))
			}
		}
	}
	if prose > 900 {
		t.Errorf("the opened card holds %d characters of prose above its folds, want at most 900:\n%s", prose, shown)
	}

	if ask != nil {
		if n := len(classed(ask, "o-question")); n > 1 {
			t.Errorf("the ask block has %d questions:\n%s", n, shown)
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
		if n.Data == "form" && hasClass(n, "answer") && htmlAttr(n, "hx-target") != "" {
			t.Errorf("an answer targets %s, not the card it stands on:\n%s", htmlAttr(n, "hx-target"), shown)
		}
		if n.Data == "input" && htmlAttr(n, "type") == "hidden" && htmlAttr(n, "value") == "" && within(n, o, func(p *html.Node) bool { return hasClass(p, "answer") }) {
			t.Errorf("an answer posts %s empty:\n%s", htmlAttr(n, "name"), shown)
		}
		if n.Data == "details" && slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == "open" }) {
			t.Errorf("a fold stands open: %s", nodeText(n))
		}
		field := n.Data == "select" || n.Data == "textarea" || n.Data == "input" && htmlAttr(n, "type") != "hidden"
		typed := n.Data == "textarea" || n.Data == "input" && slices.Contains([]string{"text", "number"}, htmlAttr(n, "type"))
		if typed && (htmlAttr(n, "id") == "" || !slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == "hx-preserve" })) {
			t.Errorf("the typed field %s loses its words on a swap:\n%s", htmlAttr(n, "name"), shown)
		}
		if field && !within(n, o, func(p *html.Node) bool {
			return slices.ContainsFunc(p.Attr, func(a html.Attribute) bool { return a.Key == "hidden" })
		}) {
			t.Errorf("the field %s shows before a press:\n%s", htmlAttr(n, "name"), shown)
		}
	}

	// each press's fields sit in its own answer's form, so a send posts
	// that answer's fields alone
	for n := range o.Descendants() {
		box := htmlAttr(n, "aria-controls")
		if n.Type != html.ElementNode || !hasClass(n, "pick") || box == "" {
			continue
		}
		form := n.Parent
		for form != nil && form.Data != "form" {
			form = form.Parent
		}
		if form == nil || !slices.ContainsFunc(slices.Collect(form.Descendants()), func(d *html.Node) bool {
			return d.Type == html.ElementNode && d.Data == "fieldset" && htmlAttr(d, "id") == box
		}) {
			t.Errorf("the press %q opens fields outside its own form:\n%s", nodeText(n), shown)
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
	picks := map[string]int{}
	for _, p := range classed(o, "pick") {
		picks[nodeText(p)]++
	}
	for label, n := range picks {
		if n > 1 {
			t.Errorf("the opened card offers %q %d times:\n%s", label, n, shown)
		}
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
	// the texts a human reads: no word of any, a writer's quoted words too,
	// is a forbidden one
	var texts, whole []string
	read := func(n *html.Node) {
		texts, whole = append(texts, textOutside(n, quoted)), append(whole, textOutside(n, func(*html.Node) bool { return false }))
	}
	for _, name := range append(slices.Clone(openedProse), "o-line", "pick", "o-label") {
		for _, p := range classed(o, name) {
			read(p)
		}
	}
	// a writer's words in their own fold are quoted words too
	for _, p := range classed(o, "own-words") {
		whole = append(whole, nodeText(p))
	}
	for n := range o.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		switch n.Data {
		case "summary", "label", "legend":
			read(n)
		}
		for _, a := range []string{htmlAttr(n, "aria-label"), htmlAttr(n, "title"), htmlAttr(n, "placeholder")} {
			texts, whole = append(texts, a), append(whole, a)
		}
	}
	drop := func(s string) bool {
		return s == "" || captured && (strings.Contains(s, factoryd.CardTitle(*card)) || card.Summary != "" && strings.Contains(s, card.Summary))
	}
	for _, s := range slices.DeleteFunc(texts, drop) {
		for _, re := range facefixtures.Forbidden {
			if m := re.FindString(s); m != "" {
				t.Errorf("%q names %q", s, m)
			}
		}
	}
	for _, s := range slices.DeleteFunc(whole, drop) {
		if m := factoryd.FactoryName(s); m != "" {
			t.Errorf("%q names %q", s, m)
		}
	}
	for _, by := range classed(o, "o-by") {
		for _, q := range classed(by, "quote") {
			if faults := factoryd.GroundsFaults(nodeText(q)); len(faults) > 0 {
				t.Errorf("the grounds %q name the machinery: %v", nodeText(q), faults)
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
			f.Findings, f.AuditFindings, f.AuditConfirmable, f.FindingTexts[id] = append(f.Findings, id), append(f.AuditFindings, id), append(f.AuditConfirmable, id), text
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
	ids := []string{factoryd.FindingID(two[0]), factoryd.FindingID(two[1])}
	values["confirm_audit.findings"] = ids
	values.Set("confirm_audit.note", "The rig's fakes.")
	if rec := r.do(post(action, values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the send = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActConfirmAudit || got.Res.Args["findings"] != strings.Join(ids, "\n") || got.Res.Note != "The rig's fakes." {
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

// openedEntry seats the gallery entry group/kind and answers the rig, the
// session's cookie and csrf, the card and the card's opened overview.
func openedEntry(t *testing.T, group, kind, query string) (*rig, *http.Cookie, string, string, string) {
	t.Helper()
	i := slices.IndexFunc(galleryEntries(), func(e galleryEntry) bool { return e.Group == group && e.Kind == kind })
	if i < 0 {
		t.Fatalf("the gallery holds no %s/%s", group, kind)
	}
	r := newRig(t)
	ck, s := r.loggedIn(t)
	target := galleryEntries()[i].Seat(r)
	return r, ck, s.csrf, target, drawer(t, r, ck, target, "overview"+query)
}

// A card that waits on its issue's author asks the agent's question on
// its opened card. "Answer in my words" opens one required box under it
// with its Send, and the send hands Act the human's words as the answer,
// to post on the issue; Not now and End the card stand beside it.
func TestAnInformationRequestAnswersInMyWords(t *testing.T) {
	const own = factoryd.ActContinue + "/own_words"
	r, ck, _, target, body := openedEntry(t, "case", string(factoryd.CaseAgentRequestInfo), "")
	ask := askBlockOf(t, body)
	for _, want := range []string{
		`<p class="o-question">Which Hetzner location does the load test&#39;s server use, fsn1 or nbg1?</p>`,
		`data-press="` + own + `">Answer in my words</button> <span class="effect">Sends your words to the agent as its answer.</span>`,
		`name="` + own + `.note" rows="2" maxlength="4000" placeholder="The answer the agent needs" required hx-preserve></textarea>` + "\n" +
			`<button type="submit" class="send">Send</button></fieldset>`,
		`data-press="note">Not now</button>`, `data-press="close">End the card</button>`,
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the request's ask lacks %s:\n%s", want, ask)
		}
	}
	box := regexp.MustCompile(`<fieldset class="fields" id="[^"]*-continue-own-words-note-set" hidden>`)
	if !box.MatchString(ask) {
		t.Errorf("Answer in my words's box shows before its press:\n%s", ask)
	}
	send := answerForm(t, body, own)
	if send.Action != "/tasks/"+target+"/resolve" || send.Values.Get("action") != own {
		t.Fatalf("Answer in my words posts to %q with %v", send.Action, send.Values)
	}
	send.Values.Set(own+".note", "fsn1, beside the database.")
	if rec := r.do(post(send.Action, send.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the send = %d: %s", rec.Code, rec.Body)
	}
	got, posts := actedOnce(t, r)
	if got.Res.Action != factoryd.ActContinue || got.Res.Note != "fsn1, beside the database." || !posts ||
		fmt.Sprint(got.CaseID) != send.Values.Get("case_id") {
		t.Fatalf("Act took %+v (posts %v), want the words as the answer on case %s, posted", got, posts, send.Values.Get("case_id"))
	}
}

// Add money opens with the ask's amount filled in, the one number the ask
// and its story price: on #2004's floor the ask's 2.68 EUR, on a floor
// without findings 0.38 EUR, and at the spend limit the raise's 0.38 EUR.
// The small card's pick opens the card with that field open, and the
// send funds the amount shown.
func TestAddMoneyOpensWithTheAsksAmount(t *testing.T) {
	for _, tc := range []struct {
		group, kind, key, amount string
	}{
		{"case", string(factoryd.CaseBudgetFloor), factoryd.ActFund, "2.68"},
		{"variant", "budget_floor without findings", factoryd.ActFund, "0.38"},
		{"variant", "budget_floor at the spend limit", factoryd.ActRaiseCeiling, "0.38"},
	} {
		r, ck, _, target, body := openedEntry(t, tc.group, tc.kind, "&press="+tc.key)
		send := answerForm(t, body, tc.key)
		if got := send.Values.Get(tc.key + ".eur"); got != tc.amount {
			t.Errorf("%s: %s opens with %q, want the ask's %s", tc.kind, send.Label, got, tc.amount)
		}
		if !strings.Contains(askBlockOf(t, body), `aria-expanded="true" aria-controls="f-`) {
			t.Errorf("%s: the pick's press does not open the answer's field:\n%s", tc.kind, askBlockOf(t, body))
		}
		if rec := r.do(post(send.Action, send.Values), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: the send = %d: %s", tc.kind, rec.Code, rec.Body)
		}
		got, _ := actedOnce(t, r)
		if got.Res.Action != tc.key || fmt.Sprintf("%.2f", got.Res.Args["eur"]) != tc.amount || got.TaskID != target {
			t.Errorf("%s: Act took %+v, want %s of %s EUR", tc.kind, got.Res, tc.key, tc.amount)
		}
	}
}

// A card that holds answers its run has not taken says so, and its
// answers in the human's own words wait: "Answer in my words" shows
// disabled, while the answers that end or hold the card stay.
func TestPendingAnswersHoldTheOwnWords(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15,
		Objective: "add the feed"}, AnswersPending: 1}}
	seatAsk(r, "t-q", factoryd.CaseAgentEscalate, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts = 1 }))
	r.openCases["t-q"][0].Problem = factoryd.AgentAsks + "Which port does the feed use?"
	ck, _ := r.loggedIn(t)
	body := drawer(t, r, ck, "t-q", "overview")
	ask := askBlockOf(t, body)
	if !strings.Contains(body, `<p class="o-sent liveness" role="status">This card holds answers its run has not taken.`) ||
		!strings.Contains(ask, `<button type="button" class="pick" disabled>Answer in my words</button>`) || strings.Contains(ask, `data-answer="continue/own_words"`) {
		t.Errorf("a card with pending answers offers its own words:\n%s", ask)
	}
	for _, key := range []string{factoryd.ActNote, factoryd.ActClose} {
		if !strings.Contains(ask, `data-answer="`+key+`"`) {
			t.Errorf("a card with pending answers lost %s:\n%s", key, ask)
		}
	}
}

// A waiting card whose cases could not be read says the read failed, not
// that its answer is on its way.
func TestAFailedCaseReadSaysSo(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15,
		Objective: "add the feed"}}}
	r.c.cfg.CaseDesk = func(context.Context, string) (factoryd.CaseDesk, error) {
		return factoryd.CaseDesk{}, errors.New("the store did not answer")
	}
	ck, _ := r.loggedIn(t)
	body := drawer(t, r, ck, "t-q", "overview")
	if strings.Contains(body, "on its way") || !strings.Contains(body, "The cases could not be read: the store did not answer. Reload the board.") {
		t.Errorf("the opened card of a failed case read:\n%s", body)
	}
}

// The folds' hints say what they hold: the full brief of a card with a
// pending request names the request's evidence, and a human card, which no
// agent works, holds no agent's view.
func TestTheFoldsSayWhatTheyHold(t *testing.T) {
	for _, tc := range []struct{ group, kind, hint string }{
		{"approval", factoryd.ApprovalMerge, "the pull request"},
		{"approval", factoryd.ApprovalResource, "the server"},
	} {
		_, _, _, _, body := openedEntry(t, tc.group, tc.kind, "")
		if want := `<summary>The full brief<span class="hint">` + tc.hint; !strings.Contains(body, want) {
			t.Errorf("%s: the full brief's hint is no %q:\n%s", tc.kind, tc.hint, body)
		}
	}
	_, _, _, _, body := openedEntry(t, "case", string(factoryd.CaseHumanCard), "")
	if strings.Contains(body, `data-sec="view"`) {
		t.Errorf("a human card shows an agent's view:\n%s", body)
	}
}

// A writer's words longer than a story line stand whole in a closed fold
// of their own, and the story says so in one line; shorter words stay
// quoted in the story.
func TestALongQuoteStandsInItsOwnFold(t *testing.T) {
	r, ctx, v := refsRig(t)
	long := strings.Repeat("The registries do not resolve in this sandbox. ", 5) + "How should the agent get the new digests?"
	short := "Rotate it now, the old one expires Friday."
	face := factoryd.Face{Ask: "How should it go on?", Story: factoryd.Story{Happened: factoryd.AgentAsks + long, View: "The project agent's view: " + short}}
	a := r.c.askOf(ctx, v, card{Task: factoryd.Task{ID: "t-1", Repository: "sf/code"}}, &faceView{}, face, map[string]int{})
	equal(t, "the story", a.Story, []storyLine{{Lead: "The agent&#39;s own words stand whole in the fold below."}, {Lead: "The project agent&#39;s view: ", Quote: short}})
	equal(t, "the folds", a.Words, []ownWords{{Title: "The agent's own words", Hint: "48 words", Text: long}})

	_, _, _, _, body := openedEntry(t, "case", string(factoryd.CaseAgentEscalate), "")
	for _, want := range []string{
		`<li>The agent&#39;s own words stand whole in the fold below.</li>`,
		`<details class="fold" data-sec="words"><summary>The agent&#39;s own words<span class="hint">41 words</span></summary>` +
			`<div class="fold-body"><p class="own-words">The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, ` +
			`registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), none of which resolve in this sandbox. ` +
			`How should the new digests, sha256 sums and versions be obtained?</p></div></details>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the escalate card lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(askBlockOf(t, body), "upstream registries") {
		t.Errorf("the ask block quotes the long question:\n%s", body)
	}
}

// A field's id moves with its default: a swap keeps what the human typed
// (hx-preserve) while the default stands, and shows the new default once
// it moves, so Send never posts an amount the ask no longer holds.
func TestAFieldsIDMovesWithItsDefault(t *testing.T) {
	r, ctx, v := refsRig(t)
	a := factoryd.Answer{Action: factoryd.ActFund, Label: "Add money", Fields: []factoryd.Field{
		{Name: "eur", Label: "EUR", Kind: factoryd.FieldMoney, Default: func(f factoryd.Facts) string { return f.Card.Why }}}}
	press := func(factoryd.Answer, string) (*pressForm, func(string) string) {
		return &pressForm{Post: "/tasks/t-1/resolve"}, func(field string) string { return "fund." + field }
	}
	id := func(amount string) string {
		f := factoryd.Facts{Card: factoryd.Task{ID: "t-1", Why: amount}}
		return r.c.answerOf(ctx, v, card{Task: f.Card}, "t-1-5", factoryd.ActFund, a, f, press, nil).Fields[0].ID
	}
	if first, again, moved := id("2.68"), id("2.68"), id("3.10"); first != again || first == moved {
		t.Errorf("the field's ids are %q, %q at the same amount and %q at another; want one id per amount", first, again, moved)
	}
}

// A promise's answer on a criteria request settles one promise and leaves
// the plan's card where it stands: its form names no column to move to,
// so the press lands with the board that answers it and the card's
// answers come back at once.
func TestAPromisesAnswerMovesNoCard(t *testing.T) {
	_, _, _, _, page := openedEntry(t, facefixtures.GroupApproval, factoryd.ApprovalCriteria, "")
	for _, answer := range []string{"answer_criteria", "answer_criteria/not_met", "answer_criteria/waive"} {
		i := strings.Index(page, `data-answer="`+answer+`"`)
		if i < 0 {
			continue
		}
		if form, _, _ := strings.Cut(page[i:], ">"); strings.Contains(form, "data-to") {
			t.Errorf("the answer %s moves its card: %s", answer, form)
		}
	}
	if !strings.Contains(page, `data-answer="answer_criteria/waive"`) {
		t.Fatal("the criteria request's card offers no Drop this promise")
	}
}
