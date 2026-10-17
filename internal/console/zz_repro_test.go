package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

	// 3. the merge approval asks first: why a human, the model review,
	// the recommendation — not one undifferentiated block
	wide := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	start := strings.Index(wide, `aria-labelledby="approval-4"`)
	merge := wide[start : start+strings.Index(wide[start:], "</article>")]
	for _, want := range []string{
		`<p class="why">the project&#39;s merge rule is human; risk class high is requires_approval</p>`,
		`<p class="info"><b>Model review</b> approve: adds the column safely</p>`,
		`<p class="recommendation">Recommendation: Adds the column; merge it.</p>`,
		`action="/approvals/4/approve"`, `class="deny"`,
	} {
		if !strings.Contains(merge, want) {
			t.Errorf("FIX 3 MISSING: the merge approval lacks %s:\n%s", want, merge)
		}
	}
	if strings.Contains(merge, `<p class="need-line"><b>Model review</b>`) {
		t.Errorf("FAULT 3 STILL PRESENT: the model review renders as a flat need-line:\n%s", merge)
	}
}

// The oversized needs-you card, held fixed at the construction point:
// every part a merge approval's face carries is bounded where the face
// is assembled, and the stylesheet clamps whatever a board face shows,
// so no field or card kind can grow the column's copy again (ruling
// 295). The drawer keeps the whole text.
func TestMergeFaceWhispers(t *testing.T) {
	r := newRig(t)
	pad := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "pad %d the face past every bound a card keeps. ", i)
		}
		return b.String()
	}
	deep := func(word string) string { return pad(40) + " " + word + " tail" }
	reco := "`bash check` exits 0 and one `tofu -check` block holds.\n```go\n" + deep("RECO_TAIL_MARKER") + "\n```"
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 33, Objective: "add the column"}}}
	r.approvals.rows = []factoryd.ApprovalRequest{{
		ID: 4, Kind: "merge", TaskID: "t-m", Project: "sf/code", PRNumber: 9, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal","verdict_summary":"the change is safe. ` + deep("SUMMARY_TAIL_MARKER") + `",` +
			`"review":{"verdict":"approve","findings":["a finding. ` + deep("FINDING_TAIL_MARKER") + `","second. ` + deep("F2") + `","third. ` + deep("F3") + `","fourth. ` + deep("F4_TAIL_MARKER") + `"],` +
			`"criteria":[{"kind":"human","criterion":"no data loss","evidence":"the migration keeps every row. ` + deep("EVIDENCE_TAIL_MARKER") + `"}]},` +
			`"recommendation":` + strconv.Quote(reco) + `,"why_human":["a human decides this merge. ` + deep("WHY_TAIL_MARKER") + `"]}`),
	}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-4"`)
	if start < 0 {
		t.Fatal("no merge approval card on the board")
	}
	face := body[start : start+strings.Index(body[start:], "</article>")]
	if s := askline(reco); !strings.Contains(face, s[:40]) {
		t.Errorf("FIX MISSING: the face does not whisper the recommendation's first line:\n%s", face)
	}
	for _, tail := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "EVIDENCE_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER", "```"} {
		if strings.Contains(face, tail) {
			t.Errorf("FAULT STILL PRESENT: the face carries %s:\n%s", tail, face)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	for _, whole := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "EVIDENCE_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER"} {
		if !strings.Contains(drawer, whole) {
			t.Errorf("FIX MISSING: the drawer lost the whole text (%s):\n%s", whole, drawer)
		}
	}
	css, err := os.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, clamp := range []string{`.cards .card :is(`, `-webkit-line-clamp`} {
		if !strings.Contains(string(css), clamp) {
			t.Errorf("FIX MISSING: the stylesheet lacks the face clamp %s", clamp)
		}
	}
}
