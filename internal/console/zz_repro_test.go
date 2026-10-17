package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The issue's three faults, held fixed: the board card of a shaped
// question stays small, the drawer renders the question's markup with
// its raw text behind a fold, and a merge approval carries the five-part
// shape every ask-first card carries.
func TestIssueFaultsHeldFixed(t *testing.T) {
	// 1. the board card of a shaped question: the ask line and the
	// buttons; the question's text never on the face
	r := newRig(t)
	q := &factoryd.HumanQuestion{
		Ask:  "The card's session cap is reached. Grant one more session?",
		Why:  "the session cap is reached: every finding sent to a person returns unacted",
		Info: "A grant starts one more session on this card. Park holds it for a person to work with.",
		Choices: []factoryd.QuestionChoice{
			{Value: "grant", Label: "Grant one session"},
			{Value: "park", Label: "Leave it waiting"},
		},
	}
	r.tasks.tasks = []factoryd.BoardTask{{
		Task:       factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
		Escalation: "the session cap is reached: every finding sent to a person returns unacted",
		Question:   q,
	}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	esc, face := escalationOf(t, body, "t-q"), faceOf(t, body, "t-q")
	for _, want := range []string{
		`<p class="ask">The card&#39;s session cap is reached. Grant one more session?</p>`,
		`name="value" value="grant"`, `name="value" value="park"`,
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("FIX 1 MISSING: the board card lacks %s:\n%s", want, esc)
		}
	}
	for _, bulky := range []string{`<p class="why">`, `<div class="why">`, `class="fold"`, `<p class="info">`, `<pre class="reason detail">`} {
		if strings.Contains(esc, bulky) {
			t.Errorf("FAULT 1 STILL PRESENT: the board card carries %s:\n%s", bulky, esc)
		}
	}
	if strings.Contains(face, "the session cap is reached: every finding sent to a person returns unacted") {
		t.Errorf("FAULT 1 STILL PRESENT: the face carries the why as its activity:\n%s", face)
	}
	if !strings.Contains(face, `<p class="activity">The card&#39;s session cap is reached. Grant one more session?`) {
		t.Errorf("FIX 1 MISSING: the face's activity line is not the ask:\n%s", face)
	}

	// 2. the drawer renders the why as markup — a heading, a list, a
	// code block — with the raw words behind a fold
	q.Why = "## Why the card stopped\n- the cap counts `4` calls\n\n```go\nfmt.Println(4)\n```"
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-q/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{
		`<div class="why"><h2>Why the card stopped</h2>`,
		`<li>the cap counts <code>4</code> calls</li>`,
		`<pre><code class="language-go">fmt.Println(4)`,
		`<details class="fold"><summary>The raw text, for copying</summary><pre class="reason detail">## Why the card stopped`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("FIX 2 MISSING: the drawer lacks %s", want)
		}
	}
	if strings.Contains(drawer, "&lt;h2&gt;") || strings.Contains(drawer, "&lt;code&gt;") {
		t.Errorf("FAULT 2 STILL PRESENT: the drawer renders the markup as text")
	}

	// 3. the merge approval's lane face stays small like a question's:
	// the brief on one line, the Approve and Deny buttons and the foot
	// links — the five parts render only in the drawer
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 9}})
	wide := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	start := strings.Index(wide, `aria-labelledby="approval-4"`)
	merge := wide[start : start+strings.Index(wide[start:], "</article>")]
	for _, want := range []string{
		`action="/approvals/4/approve"`, `class="deny"`,
		`<a href="#card=t-m:reviews" class="details">Details</a>`,
	} {
		if !strings.Contains(merge, want) {
			t.Errorf("FIX 3 MISSING: the merge approval lacks %s:\n%s", want, merge)
		}
	}
	for _, bulky := range []string{
		`<p class="why">`, `<p class="info">`, `<p class="finding">`, `<p class="recommendation">`,
		"the project&#39;s merge rule is human", "adds the column safely", "Adds the column; merge it.",
		`<p class="need-line"><b>Model review</b>`,
	} {
		if strings.Contains(merge, bulky) {
			t.Errorf("FAULT 3 STILL PRESENT: the lane face carries the drawer's five parts (%s):\n%s", bulky, merge)
		}
	}
	mergeDrawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{
		`<p class="why">the project&#39;s merge rule is human; risk class high is requires_approval</p>`,
		`<p class="info"><b>Model review</b> approve: adds the column safely</p>`,
		`<p class="recommendation">Recommendation: Adds the column; merge it.</p>`,
	} {
		if !strings.Contains(mergeDrawer, want) {
			t.Errorf("FIX 3 MISSING: the drawer's merge approval lacks %s", want)
		}
	}
}
