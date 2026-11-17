package console

import (
	"context"
	"fmt"
	"html/template"
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

// Each #N names its target: a card on the board opens its drawer, a card
// the board does not hold too, a pull request and an issue open in
// Forgejo, and each is followed by its title and its state in words.
func TestLinkRefsNamesTheTarget(t *testing.T) {
	r, ctx, v := refsRig(t)
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-13", Repository: "sf/code", IssueNumber: 13, Status: "closed", Objective: "Fix the <script>alert(1)</script> & cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-14", Repository: "sf/code", IssueNumber: 14, Status: "waiting-human", Objective: "Change two lines"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-15", Repository: "sf/code", IssueNumber: 15, Status: "running", Track: policy.TrackPlan, Objective: "Plan the cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-16", Repository: "sf/code", IssueNumber: 16, Status: "in-review", Objective: "Build the cache"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-17", Repository: "sf/code", IssueNumber: 17, Status: "waiting-deps", Objective: "Wire the cache"}})
	r.fg.issueReads = map[string]map[string]any{
		"/api/v1/repos/sf/code/issues/40": {"title": "Try the cache", "state": "closed", "pull_request": map[string]any{"merged": false}},
		"/api/v1/repos/sf/code/issues/41": {"title": "Land the cache", "state": "closed", "pull_request": map[string]any{"merged": true}},
		"/api/v1/repos/sf/code/issues/42": {"title": "Grow the cache", "state": "open", "pull_request": map[string]any{"merged": false}},
	}
	for _, tt := range []struct{ text, want string }{
		{"Depends on #12", `Depends on <a class="ref-link" href="#card=t-12">#12</a> (Add the login page, done)`},
		{"Continues #13.", `Continues <a class="ref-link" href="#card=t-13">#13</a> (Fix the &lt;script&gt;alert(1)&lt;/script&gt; &amp; cache, closed).`},
		{"#14, #15, #16 and #17", `<a class="ref-link" href="#card=t-14">#14</a> (Change two lines, needs you), ` +
			`<a class="ref-link" href="#card=t-15">#15</a> (Plan the cache, planning), ` +
			`<a class="ref-link" href="#card=t-16">#16</a> (Build the cache, verifying) and ` +
			`<a class="ref-link" href="#card=t-17">#17</a> (Wire the cache, queued)`},
		{"Carries #40.", `Carries <a class="ref-link" href="https://git.example/sf/code/pulls/40" target="_blank" rel="noopener">#40</a> (Try the cache, closed without a merge).`},
		{"#41 and #42", `<a class="ref-link" href="https://git.example/sf/code/pulls/41" target="_blank" rel="noopener">#41</a> (Land the cache, merged) and ` +
			`<a class="ref-link" href="https://git.example/sf/code/pulls/42" target="_blank" rel="noopener">#42</a> (Grow the cache, open)`},
		{"See #7", `See <a class="ref-link" href="https://git.example/sf/code/issues/7" target="_blank" rel="noopener">#7</a> (Fix the cache, open)`},
		{"#950 is gone & so is <b>", `#950 is gone &amp; so is &lt;b&gt;`},
		{"Run `go test -run #12` and see:\n```\nfailed #12 & more\n```", "Run <code>go test -run #12</code> and see:\n<pre><code>\nfailed #12 &amp; more\n</code></pre>"},
		{"The x&#12 entity and a/#12 path", `The x&amp;#12 entity and a/#12 path`},
	} {
		equal(t, tt.text, r.c.linkRefs(ctx, v, "sf/code", tt.text), template.HTML(tt.want))
	}
}

// A number the board holds costs no read, and one it does not is read
// once per render, however often a face names it: 80 cards that each name
// the card before them and the same pull request read Forgejo once.
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
		if n > 100 && !strings.Contains(got, fmt.Sprintf(`#%d</a> (Card %d, queued)`, n-1, n-1)) {
			t.Fatalf("card %d reads %q", n, got)
		}
	}
	equal(t, "the reads of a render of 80 cards", issueReads(r), []string{"/api/v1/repos/sf/code/issues/99", "/api/v1/repos/sf/code/issues/40"})
}

// A live card answers its number before a final one of the same issue,
// as the store's TaskByIssue does.
func TestLinkRefsTakesTheLiveCard(t *testing.T) {
	r, ctx, v := refsRig(t)
	v.byID["t-12b"] = factoryd.Task{ID: "t-12b", Repository: "sf/code", IssueNumber: 12, Status: "queued", Objective: "Add the login page again"}
	v.byID["t-other"] = factoryd.Task{ID: "t-other", Repository: "sf/other", IssueNumber: 12, Status: "running", Objective: "Another project's card"}
	equal(t, "#12", r.c.linkRefs(ctx, v, "sf/code", "#12"), template.HTML(`<a class="ref-link" href="#card=t-12b">#12</a> (Add the login page again, queued)`))
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

// A project with no document repository has no document to open: the
// link keeps its words.
func TestLinkRefsWithoutDocuments(t *testing.T) {
	r, ctx, v := refsRig(t)
	equal(t, "the link", r.c.linkRefs(ctx, v, "sf/code", "See [the plan](docs/plan.md)."), template.HTML("See the plan."))
}

func gloss(term string) string {
	return ` <small class="gloss">` + template.HTMLEscapeString(factoryd.Terms[term]) + `</small>`
}

// The first use of each term on a face is marked and explained after the
// text, whole words only, in any case and in the plural; a later use, a
// use another text of the face explained, and a use inside a link or code
// stay plain.
func TestExplainTermsMarksFirstUseOnly(t *testing.T) {
	seen := map[string]bool{}
	equal(t, "the why line", explainTerms("The Session stopped &amp; two sessions ran past the session cap.", seen),
		template.HTML(`The <dfn class="term">Session</dfn> stopped &amp; two sessions ran past the <dfn class="term">session cap</dfn>.`+gloss("session")+gloss("session cap")))
	equal(t, "the summary of the same face", explainTerms("A session starts. The criteria hold, two check-ins ran and the gateway passed the gate.", seen),
		template.HTML(`A session starts. The <dfn class="term">criteria</dfn> hold, two <dfn class="term">check-ins</dfn> ran and the <dfn class="term">gateway</dfn> passed the <dfn class="term">gate</dfn>.`+
			gloss("criterion")+gloss("check-in")+gloss("gateway")+gloss("gate")))
	equal(t, "words that only hold a term", explainTerms("An obsession gated the sessional replanning.", nil),
		template.HTML("An obsession gated the sessional replanning."))
	equal(t, "links and code", explainTerms(`<a href="#card=t-1">session</a> and <code>session</code>, then a session.`, nil),
		template.HTML(`<a href="#card=t-1">session</a> and <code>session</code>, then a <dfn class="term">session</dfn>.`+gloss("session")))
}

// explainTerms runs over linkRefs's output without breaking its tags, and
// over its own output without a change.
func TestExplainTermsComposesWithLinkRefs(t *testing.T) {
	r, ctx, v := refsRig(t)
	once := explainTerms(r.c.linkRefs(ctx, v, "sf/code", "The session waits for #12 & `the proof`. Then the session ends."), nil)
	want := template.HTML(`The <dfn class="term">session</dfn> waits for <a class="ref-link" href="#card=t-12">#12</a> (Add the login page, done) &amp; <code>the proof</code>. Then the session ends.` + gloss("session"))
	equal(t, "linked, then explained", once, want)
	equal(t, "explained twice", explainTerms(once, nil), want)
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
