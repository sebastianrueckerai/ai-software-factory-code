package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The oversized needs-you card, held fixed at the construction point:
// the small card and the opened card's ask hold the register's ask and
// none of the parts' tails, so no field or card kind can grow the copy
// again; the opened card's full brief holds the whole text (the
// 2026-10-17 card).
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
		Spec: json.RawMessage(`{"risk_class":"normal","head_sha":"abc123","verdict_summary":"the change is safe. ` + deep("SUMMARY_TAIL_MARKER") + `",` +
			`"review":{"verdict":"approve","findings":["a finding. ` + deep("FINDING_TAIL_MARKER") + `","second. ` + deep("F2") + `","third. ` + deep("F3") + `","fourth. ` + deep("F4_TAIL_MARKER") + `"]},` +
			`"recommendation":` + strconv.Quote(reco) + `,"why_human":["a human decides this merge. ` + deep("WHY_TAIL_MARKER") + `"]}`),
	}}
	ck, s := r.loggedIn(t)
	// the small card asks the register's merge, its model review's pick its
	// one press, and holds no part's tail
	small := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-m")
	if strings.Count(small, "<form") != 1 || !strings.Contains(small, `action="/approvals/4/approve"`) {
		t.Errorf("the small card holds a form other than its pick's press:\n%s", small)
	}
	for _, tail := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER", "```", "pad 0"} {
		if strings.Contains(small, tail) {
			t.Errorf("the small card carries %s:\n%s", tail, small)
		}
	}
	// the opened card's ask holds the register's question, why, the model
	// review's pick with its verdict in one line and each answer's one line
	// of effect, and none of the parts' tails; the full brief holds them whole
	body := drawer(t, r, ck, "t-m", "overview")
	ask := askBlockOf(t, body)
	for _, want := range []string{
		`<p class="o-question">Merge “add the column”?</p>`,
		`<p class="o-askwhy">The change is of normal risk, and the factory&#39;s rules let only a human merge it.</p>`,
		`<p class="o-by">suggested by the model review: <span class="quote">It approved the change with 4 findings.</span></p>`,
		`<span class="effect">Merges the pull request.</span>`,
		`<span class="effect">Returns the pull request to its agent with your note.</span>`,
		`<span class="effect">Closes the pull request. The card ends.</span>`,
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the ask lacks %q:\n%s", want, ask)
		}
	}
	for _, tail := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER", "pad 0"} {
		if strings.Contains(ask, tail) {
			t.Errorf("the ask carries %s:\n%s", tail, ask)
		}
	}
	// one form an answer, each its own send: no fund control beside them,
	// and a note box for each answer that takes words
	for what, want := range map[string]int{`<form `: 3, `action="/approvals/4/approve"`: 1, `action="/approvals/4/deny"`: 2,
		`name="fund_eur"`: 0, `<textarea`: 2} {
		if got := strings.Count(ask, what); got != want {
			t.Errorf("the ask carries %s %d times, want %d:\n%s", what, got, want, ask)
		}
	}
	for _, whole := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER"} {
		if !strings.Contains(body, whole) {
			t.Errorf("the drawer lost the whole text (%s):\n%s", whole, body)
		}
	}
	// Merge it's send merges and lands one row on the card
	before := r.events.count()
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"action": {"approve"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the send = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(4); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "" {
		t.Errorf("the request = %q by %q, note %q, want approved by sebastian, no note", a.Status, a.DecidedBy, a.DecisionNote)
	}
	checkRows(t, r.events, before,
		wantRow{kind: factoryd.EventMergeApproved, repo: "sf/code", taskID: "t-m", detail: map[string]any{"pr": int64(9)}})
}

// The agent's board read runs as no member (a session-less turn): a
// pending red merge request on the board once made it read the head's
// checks as that missing member — a panic that killed factoryd
// and, the card still pending, crash-looped it on every pulse, the
// roll-not-live alert of 2026-10-18. The pulse's face keeps the red
// instead; a member's view still heals it (#1013).
func TestPulseRedMergeRequestKeepsTheRedFace(t *testing.T) {
	r := newRig(t)
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{
		ID: 9, Kind: "merge", TaskID: "t-m", Project: "sf/code", PRNumber: 11, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal","red_checks":true,"head_sha":"deadbeef",` +
			`"verdict_summary":"the change is safe","recommendation":"Merge it.","why_human":["a human decides this merge"]}`),
	})
	req, rerr := http.NewRequestWithContext(context.WithValue(t.Context(), ctxKey{}, &session{}), http.MethodGet, "/", nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	text, err := r.c.boardText(req, projectRows()[0], actor{})
	if err != nil {
		t.Fatalf("FIX MISSING: the pulse's board read dies: %v", err)
	}
	if !strings.Contains(text, "pull request #11") {
		t.Errorf("FIX MISSING: the pulse's board does not show the red merge request:\n%s", text)
	}
	if len(r.fg.calls) != 0 {
		t.Errorf("FAULT STILL PRESENT: the pulse read Forgejo as no member: %v", r.fg.calls)
	}
}
