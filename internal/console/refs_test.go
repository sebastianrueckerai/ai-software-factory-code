package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// refsRig is the rig with sebastian's session in a context and a view
// whose board holds card #12, done.
func refsRig(t *testing.T) (*rig, context.Context, *boardView) {
	t.Helper()
	r := newRig(t)
	_, s := r.loggedIn(t)
	v := &boardView{byID: map[string]factoryd.Task{
		"t-12": {ID: "t-12", Repository: "sf/code", IssueNumber: 12, Status: "done", Objective: "Add the login page"},
	}}
	return r, context.WithValue(t.Context(), ctxKey{}, s), v
}

// issueReads are the rig's Forgejo reads of issues and pull requests.
func issueReads(r *rig) []string {
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	var out []string
	for _, c := range r.fg.calls {
		if strings.HasPrefix(c, "GET /api/v1/repos/sf/code/issues/") {
			out = append(out, strings.Fields(c)[1])
		}
	}
	return out
}

// cardReads are the rig's TaskByIssue reads.
func cardReads(r *rig) []string {
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	return slices.Clone(r.tasks.byIssue)
}

// ref is the HTML of a reference to href, a card's drawer when tab is
// false, named by of.
func ref(n int, href string, tab bool, of string) string {
	t := ""
	if tab {
		t = ` target="_blank" rel="noopener"`
	}
	return fmt.Sprintf(`<a class="ref-link" href="%s"%s>#%d</a> <span class="ref-of">(%s)</span>`, href, t, n, of)
}

// Each #N names its target: a card on the board opens its drawer, a card
// the board does not hold too, a pull request and an issue open in
// Forgejo, and each is followed by its title and its state in words, the
// state alone for a card without a title.
func TestLinkRefsNamesTheTarget(t *testing.T) {
	r, ctx, v := refsRig(t)
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-13", Repository: "sf/code", IssueNumber: 13, Status: "closed", Objective: "Fix the <script>alert(1)</script> & cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-14", Repository: "sf/code", IssueNumber: 14, Status: "waiting-human", Objective: "Change two lines"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-15", Repository: "sf/code", IssueNumber: 15, Status: "running", Track: policy.TrackPlan, Objective: "Plan the cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-16", Repository: "sf/code", IssueNumber: 16, Status: "in-review", Objective: "Build the cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-17", Repository: "sf/code", IssueNumber: 17, Status: "waiting-deps", Objective: "Wire the cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-18", Repository: "sf/code", IssueNumber: 18, Status: "running"}})
	r.fg.issueReads = map[string]map[string]any{
		"/api/v1/repos/sf/code/issues/40": {"title": "Try the cache", "state": "closed", "pull_request": map[string]any{"merged": false}},
		"/api/v1/repos/sf/code/issues/41": {"title": "Land the cache", "state": "closed", "pull_request": map[string]any{"merged": true}},
		"/api/v1/repos/sf/code/issues/42": {"title": "Grow the cache", "state": "open", "pull_request": map[string]any{"merged": false}},
	}
	pulls := "https://git.example/sf/code/pulls/"
	for _, tt := range []struct{ text, want string }{
		{"Depends on #12", "Depends on " + ref(12, "#card=t-12", false, "Add the login page, done")},
		{"Continues #13.", "Continues " + ref(13, "#card=t-13", false, "Fix the &lt;script&gt;alert(1)&lt;/script&gt; &amp; cache, closed") + "."},
		{"#14, #15, #16 and #17", ref(14, "#card=t-14", false, "Change two lines, needs you") + ", " + ref(15, "#card=t-15", false, "Plan the cache, planning") + ", " +
			ref(16, "#card=t-16", false, "Build the cache, verifying") + " and " + ref(17, "#card=t-17", false, "Wire the cache, queued")},
		{"Carries #40.", "Carries " + ref(40, pulls+"40", true, "Try the cache, closed without a merge") + "."},
		{"#41 and #42", ref(41, pulls+"41", true, "Land the cache, merged") + " and " + ref(42, pulls+"42", true, "Grow the cache, open")},
		{"See #7", "See " + ref(7, "https://git.example/sf/code/issues/7", true, "Fix the cache, open")},
		{"After #18", "After " + ref(18, "#card=t-18", false, "building")},
		{"#950 is gone & so is <b>", `#950 is gone &amp; so is &lt;b&gt;`},
		{"Run `go test -run #12` and see:\n```\nfailed #12 & more\n```", "Run <code>go test -run #12</code> and see:\n<pre><code>\nfailed #12 &amp; more\n</code></pre>"},
		{"The x&#12 entity and a/#12 path", `The x&amp;#12 entity and a/#12 path`},
	} {
		equal(t, tt.text, r.c.linkRefs(ctx, v, "sf/code", tt.text), template.HTML(tt.want))
	}
}

// A number the board holds costs no read, and one it does not is read
// once per render, however often a face names it: 80 cards that each name
// the card before them and the same pull request read the store twice and
// Forgejo twice.
func TestLinkRefsReadsOnlyWhatTheBoardLacks(t *testing.T) {
	r, ctx, v := refsRig(t)
	r.fg.issueReads = map[string]map[string]any{
		"/api/v1/repos/sf/code/issues/40": {"title": "Try the cache", "state": "open", "pull_request": map[string]any{"merged": false}},
	}
	for n := 100; n < 180; n++ {
		v.byID[fmt.Sprintf("t-%d", n)] = factoryd.Task{ID: fmt.Sprintf("t-%d", n), Repository: "sf/code", IssueNumber: int64(n), Status: "queued", Objective: fmt.Sprintf("Card %d", n)}
	}
	for n := 100; n < 180; n++ {
		got := string(r.c.linkRefs(ctx, v, "sf/code", fmt.Sprintf("Waits for #%d and #40.", n-1)))
		if n > 100 && !strings.Contains(got, fmt.Sprintf(`#%d</a> <span class="ref-of">(Card %d, queued)</span>`, n-1, n-1)) {
			t.Fatalf("card %d reads %q", n, got)
		}
	}
	equal(t, "the store's reads of a render of 80 cards", cardReads(r), []string{"sf/code#99", "sf/code#40"})
	equal(t, "Forgejo's reads of a render of 80 cards", issueReads(r), []string{"/api/v1/repos/sf/code/issues/99", "/api/v1/repos/sf/code/issues/40"})
}

// A read that fails leaves the rest of the render's numbers the board
// lacks unread and plain, and a lasting fault logs once, not once per
// render.
func TestLinkRefsAfterAReadFails(t *testing.T) {
	r, ctx, v := refsRig(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	equal(t, "before the fault", r.c.linkRefs(ctx, v, "sf/code", "See #7."), template.HTML("See "+ref(7, "https://git.example/sf/code/issues/7", true, "Fix the cache, open")+"."))
	r.fg.mu.Lock()
	r.fg.failAPI = true
	r.fg.mu.Unlock()
	equal(t, "at the fault", r.c.linkRefs(ctx, v, "sf/code", "See #8, #9 and #12."), template.HTML("See #8, #9 and "+ref(12, "#card=t-12", false, "Add the login page, done")+"."))
	equal(t, "after the fault", r.c.linkRefs(ctx, v, "sf/code", "See #10."), template.HTML("See #10."))
	equal(t, "the store's reads", cardReads(r), []string{"sf/code#7", "sf/code#8"})
	equal(t, "Forgejo's reads", issueReads(r), []string{"/api/v1/repos/sf/code/issues/7", "/api/v1/repos/sf/code/issues/8"})
	again := &boardView{byID: v.byID}
	equal(t, "the next render", r.c.linkRefs(ctx, again, "sf/code", "See #8."), template.HTML("See #8."))
	if n := strings.Count(logs.String(), "console: the references of sf/code:"); n != 1 {
		t.Fatalf("the fault logged %d times over two renders, want once:\n%s", n, logs.String())
	}
}

// A live card answers its number before a final one of the same issue,
// as the store's TaskByIssue does.
func TestLinkRefsTakesTheLiveCard(t *testing.T) {
	r, ctx, v := refsRig(t)
	v.byID["t-12b"] = factoryd.Task{ID: "t-12b", Repository: "sf/code", IssueNumber: 12, Status: "queued", Objective: "Add the login page again"}
	v.byID["t-other"] = factoryd.Task{ID: "t-other", Repository: "sf/other", IssueNumber: 12, Status: "running", Objective: "Another project's card"}
	equal(t, "#12", r.c.linkRefs(ctx, v, "sf/code", "#12"), template.HTML(ref(12, "#card=t-12b", false, "Add the login page again, queued")))
}

// A link to a document of the project opens it in the drawer at its
// heading, a link to the web opens a tab, and any other target keeps its
// words.
func TestLinkRefsOpensDocumentsInTheDrawer(t *testing.T) {
	r, ctx, v := refsRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
	withDocs(t, r, map[string]string{"docs/plans/cache.md": "# The cache plan\n\n## Phase B\n", "docs/roadmap.md": "# Roadmap\n"})
	text := "See [phase B](plans/cache.md#phase-b), [the roadmap](/docs/roadmap.md), [the site](https://example.com/a?b=1&c=2), " +
		"[a gone plan](plans/gone.md), [the code](../cmd/main.go) and [a script](javascript:steal)."
	want := `See <a class="doc-link" data-doc="docs/plans/cache.md" href="?doc=docs/plans/cache.md#phase-b">phase B</a>, ` +
		`<a class="doc-link" data-doc="docs/roadmap.md" href="?doc=docs/roadmap.md">the roadmap</a>, ` +
		`<a href="https://example.com/a?b=1&amp;c=2" target="_blank" rel="noopener">the site</a>, ` +
		`a gone plan, the code and a script.`
	equal(t, "the links", r.c.linkRefs(ctx, v, "sf/code", text), template.HTML(want))
}

// A project with no document repository has no document to open, and
// documents the console cannot reach read as none: the link keeps its
// words, and the numbers of the render still resolve.
func TestLinkRefsWithoutDocuments(t *testing.T) {
	r, ctx, v := refsRig(t)
	equal(t, "no documents", r.c.linkRefs(ctx, v, "sf/code", "See [the plan](docs/plan.md)."), template.HTML("See the plan."))
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
	unreachable := &boardView{byID: v.byID}
	equal(t, "documents without a token", r.c.linkRefs(ctx, unreachable, "sf/code", "See [the plan](docs/plan.md) and #7."),
		template.HTML("See the plan and "+ref(7, "https://git.example/sf/code/issues/7", true, "Fix the cache, open")+"."))
}

// A CI run and a commit named in words link to their pages on the forge,
// in the card's repository; a run or a hash without its word stays text.
func TestLinkRefsLinksRunsAndCommits(t *testing.T) {
	r, ctx, v := refsRig(t)
	equal(t, "a run and a commit", r.c.linkRefs(ctx, v, "sf/code", "CI run 3384 on main failed at commit 3c9a1e0 after #12 merged."),
		template.HTML(`<a class="ref-link" href="https://git.example/sf/code/actions/runs/3384" target="_blank" rel="noopener">CI run 3384</a> on main failed at `+
			`<a class="ref-link" href="https://git.example/sf/code/commit/3c9a1e0" target="_blank" rel="noopener">commit 3c9a1e0</a> after `+
			ref(12, "#card=t-12", false, "Add the login page, done")+" merged."))
	equal(t, "no word, no link", r.c.linkRefs(ctx, v, "sf/code", "run 3384 failed at 3c9a1e0, and the commit message is short."),
		template.HTML("run 3384 failed at 3c9a1e0, and the commit message is short."))
	equal(t, "a word of the letters a to f beside a hash", r.c.linkRefs(ctx, v, "sf/code", "The commit defaced the page, and commit deface1 mends it."),
		template.HTML(`The commit defaced the page, and `+
			`<a class="ref-link" href="https://git.example/sf/code/commit/deface1" target="_blank" rel="noopener">commit deface1</a> mends it.`))
}

// A commit the view holds whole is linked by its whole hash, and the text
// keeps the short one: a card's merge commit, a case's fact and the head of
// a pending merge request.
func TestLinkRefsLinksACommitByItsWholeHash(t *testing.T) {
	r, ctx, v := refsRig(t)
	const merged, head, fact = "3c9a1e0f5b6d7c8e9f0a1b2c3d4e5f6a7b8c9d0e", "7eaf7c7000000000000000000000000000000001", "50f5db9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"
	done := v.byID["t-12"]
	done.MergeCommit = merged
	v.byID["t-12"] = done
	v.cases.byCard = map[string][]factoryd.Case{"t-12": {{ID: 1, TaskID: "t-12", Facts: map[string]any{"commit": fact, "run": 3384}}}}
	v.pending = []factoryd.ApprovalRequest{{ID: 4, Spec: json.RawMessage(`{"head_sha":"` + head + `"}`)}}
	link := func(sha string) string {
		return `<a class="ref-link" href="https://git.example/sf/code/commit/` + sha + `" target="_blank" rel="noopener">commit ` + sha[:7] + `</a>`
	}
	equal(t, "three commits the view holds and one it does not",
		r.c.linkRefs(ctx, v, "sf/code", "Main is red at commit 3c9a1e0. Merges commit 7eaf7c7 as you. The roll of commit 50f5db9 failed, not commit 1234567."),
		template.HTML("Main is red at "+link(merged)+". Merges "+link(head)+" as you. The roll of "+link(fact)+" failed, not "+link("1234567")+"."))
}

// The first use of each term on a face is marked, whole words only, in
// any case and in the plural, and recorded in the order of first use; a
// later use, a use another text of the face marked, and a use inside a
// link, code or a reference's title stay plain.
func TestExplainTermsMarksFirstUseOnly(t *testing.T) {
	seen := map[string]int{}
	equal(t, "the ask", explainTerms("The Session stopped &amp; two sessions ran past the session cap.", seen),
		template.HTML(`The <span class="term">Session</span> stopped &amp; two sessions ran past the <span class="term">session cap</span>.`))
	equal(t, "a story line of the same face", explainTerms("A session starts. The criteria hold, two check-ins ran and the gateway passed the gate.", seen),
		template.HTML(`A session starts. The <span class="term">criteria</span> hold, two <span class="term">check-ins</span> ran and the <span class="term">gateway</span> passed the <span class="term">gate</span>.`))
	equal(t, "the order of first use", glosses(seen), []gloss{{"session", factoryd.Terms["session"]}, {"session cap", factoryd.Terms["session cap"]},
		{"criterion", factoryd.Terms["criterion"]}, {"check-in", factoryd.Terms["check-in"]}, {"gateway", factoryd.Terms["gateway"]}, {"gate", factoryd.Terms["gate"]}})
	equal(t, "words that only hold a term", explainTerms("An obsession gated the sessional replanning.", nil),
		template.HTML("An obsession gated the sessional replanning."))
	equal(t, "links, code and a reference's title", explainTerms(`<a href="#card=t-1">session</a>, <code>session</code>, <span class="ref-of">(Store the session, done)</span>, then a session.`, nil),
		template.HTML(`<a href="#card=t-1">session</a>, <code>session</code>, <span class="ref-of">(Store the session, done)</span>, then a <span class="term">session</span>.`))
}

// The words the faces use for a plan's criteria and for the factory's own
// parts are marked: "promise", not the verb, and "planner".
func TestExplainTermsMarksTheWordsTheFacesUse(t *testing.T) {
	seen := map[string]int{}
	equal(t, "an effect line", explainTerms("Send it back to the planner. 4 promises are not met, as the plan promised. SigNoz fired on factoryd.", seen),
		template.HTML(`Send it back to the <span class="term">planner</span>. 4 <span class="term">promises</span> are not met, as the plan promised. `+
			`<span class="term">SigNoz</span> fired on <span class="term">factoryd</span>.`))
	equal(t, "their sentences", glosses(seen), []gloss{{"planner", factoryd.Terms["planner"]}, {"promise", factoryd.Terms["promise"]},
		{"signoz", factoryd.Terms["signoz"]}, {"factoryd", factoryd.Terms["factoryd"]}})
}

// explainTerms runs over linkRefs's output without breaking its tags and
// leaves the target's title alone, and over its own output without a
// change.
func TestExplainTermsComposesWithLinkRefs(t *testing.T) {
	r, ctx, v := refsRig(t)
	v.byID["t-12"] = factoryd.Task{ID: "t-12", Repository: "sf/code", IssueNumber: 12, Status: "done", Objective: "Keep the session in a cookie"}
	seen := map[string]int{}
	once := r.c.factoryText(ctx, v, "sf/code", "The check-in waits for #12 & `the proof`. Then the check-in ends.", seen)
	want := template.HTML(`The <span class="term">check-in</span> waits for ` + ref(12, "#card=t-12", false, "Keep the session in a cookie, done") +
		` &amp; <code>the proof</code>. Then the check-in ends.`)
	equal(t, "linked, then marked", once, want)
	equal(t, "the face's terms", glosses(seen), []gloss{{"check-in", factoryd.Terms["check-in"]}})
	again := map[string]int{}
	equal(t, "marked twice", explainTerms(once, again), want)
	equal(t, "the terms a second pass reads", glosses(again), []gloss{{"check-in", factoryd.Terms["check-in"]}})
}

// A card's own text speaks of the product, where a factory word may name
// the product's own thing: it takes linkRefs alone, and only the text the
// factory composes takes factoryText. factoryText cannot tell the two
// apart itself, so the door a caller picks is the rule.
func TestFactoryWordsOnlyInFactoryText(t *testing.T) {
	r, ctx, v := refsRig(t)
	product := "Store the session in a cookie and add a payment gateway."
	equal(t, "a card's summary", r.c.linkRefs(ctx, v, "sf/code", product), template.HTML(product))
	seen := map[string]int{}
	equal(t, "the factory's story line", r.c.factoryText(ctx, v, "sf/code", "The last session ran out of money.", seen),
		template.HTML(`The last <span class="term">session</span> ran out of money.`))
	equal(t, "the same sentence as the factory's", r.c.factoryText(ctx, v, "sf/code", product, nil),
		template.HTML(`Store the <span class="term">session</span> in a cookie and add a payment <span class="term">gateway</span>.`))
}

// The opened card's fold "What these words mean" lists the face's terms
// in the order of first use, closed, and a face that used none has no
// fold.
func TestGlossesFold(t *testing.T) {
	r := newRig(t)
	seen := map[string]int{}
	explainTerms("The hold-out waits for the merge queue.", seen)
	var b bytes.Buffer
	if err := r.c.tmpl.ExecuteTemplate(&b, "glosses", glosses(seen)); err != nil {
		t.Fatal(err)
	}
	equal(t, "the fold", b.String(), `<details class="fold glosses"><summary>What these words mean</summary><dl>`+
		`<dt>hold-out</dt><dd>`+template.HTMLEscapeString(factoryd.Terms["hold-out"])+`</dd>`+
		`<dt>merge queue</dt><dd>`+template.HTMLEscapeString(factoryd.Terms["merge queue"])+`</dd></dl></details>`)
	b.Reset()
	if err := r.c.tmpl.ExecuteTemplate(&b, "glosses", glosses(map[string]int{})); err != nil {
		t.Fatal(err)
	}
	equal(t, "no terms", b.String(), "")
}

// Every term's sentence stands alone: it uses no other term a reader
// would need explained in turn.
func TestEveryGlossStandsAlone(t *testing.T) {
	for term, s := range factoryd.Terms {
		for _, m := range termPattern.FindAllStringSubmatchIndex(s, -1) {
			if other := termAt(m); other != term {
				t.Errorf("%q: its sentence uses the term %q", term, other)
			}
		}
	}
}
