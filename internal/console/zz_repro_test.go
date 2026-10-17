package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The issue's three faults, reproduced against the current rendering.
func TestIssueFaultsReproduced(t *testing.T) {
	// 1. the full question text on the board face of a shaped question
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
	face := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-q")
	for _, fault := range []string{`<p class="why">`, `<p class="info">`, `<p class="recommendation">`, `<pre class="reason detail">`} {
		if !strings.Contains(face, fault) {
			t.Errorf("FAULT 1 NOT REPRODUCED: the face does not carry %s", fault)
		}
	}
	t.Logf("FAULT 1 the board face carries the full question text:\n%s", face)

	// 2. the drawer's question text renders as escaped text, markup raw
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-q/drawer?tab=overview", nil), ck).Body.String()
	q.Why = "## Why the card stopped\n- the cap counts `4` calls\n\n```go\nfmt.Println(4)\n```"
	esc := ""
	for _, part := range strings.Split(drawer, `<p class="why">`)[1:] {
		esc, _, _ = strings.Cut(part, "</article>")
	}
	if !strings.Contains(esc, "&lt;h2&gt;") && !strings.Contains(esc, "## Why") {
		t.Errorf("FAULT 2 NOT REPRODUCED: the drawer shows the why as %q", esc)
	}
	t.Logf("FAULT 2 the drawer's why renders as raw text (no headings, no lists, no code blocks):\n%.400s", esc)

	// 3. the merge approval's drawer: the review as one undifferentiated block
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-4"`)
	merge := body[start : start+strings.Index(body[start:], "</article>")]
	if !strings.Contains(merge, `<p class="need-line"><b>Model review</b>`) {
		t.Errorf("FAULT 3 NOT REPRODUCED: no flat model-review line:\n%s", merge)
	}
	t.Logf("FAULT 3 the merge approval's ask-first shape is missing; the review renders as flat need-lines:\n%s", merge)
}
