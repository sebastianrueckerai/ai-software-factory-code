package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The oversized needs-you card, held fixed at the construction point:
// every part a merge approval's face carries is bounded where the face
// is assembled, and the stylesheet clamps whatever a board face shows,
// so no field or card kind can grow the column's copy again. The face
// decides through the one partial (one-face), so it carries every part
// of the ask — each at its first line, held under the bound — while the
// drawer keeps the whole text (the 2026-10-17 card, reconciled to the
// one-face plan).
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
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-4"`)
	if start < 0 {
		t.Fatal("no merge approval card on the board")
	}
	face := body[start : start+strings.Index(body[start:], "</article>")]
	// the one-face contract: the face carries every part of the ask, each
	// at its clamped first line — the ask, the why, what the call needs,
	// the recommendation, and each offer's one line of effect
	for _, want := range []string{
		`<p class="ask">Merge PR #9 of t-m? normal risk.</p>`,
		`<p class="why">a human decides this merge. pad 0`,
		`<p>Model review: the change is safe. pad 0`,
		"<p class=\"recommendation\">Recommendation (the model review): `bash check` exits 0 and one `tofu -check` block holds.</p>",
		`<span class="effect">merges PR #9: factoryd merges it</span>`,
		`<span class="effect">sends the pull request back to its agent with your note</span>`,
	} {
		if !strings.Contains(face, want) {
			t.Errorf("the face decides without the part's first line %q:\n%s", want, face)
		}
	}
	for _, tail := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER", "```"} {
		if strings.Contains(face, tail) {
			t.Errorf("the face carries %s beyond its bound:\n%s", tail, face)
		}
	}
	// the one decide: one form, one send, one fund control, one note box
	for what, want := range map[string]int{`class="decide resolve"`: 1, `action="/approvals/4/approve"`: 1,
		`name="fund_eur"`: 1, `<textarea`: 1} {
		if got := strings.Count(face, what); got != want {
			t.Errorf("the face carries %s %d times, want %d:\n%s", what, got, want, face)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	for _, whole := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER"} {
		if !strings.Contains(drawer, whole) {
			t.Errorf("the drawer lost the whole text (%s):\n%s", whole, drawer)
		}
	}
	css, err := os.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, clamp := range []string{`.cards .card :is(`, `-webkit-line-clamp`} {
		if !strings.Contains(string(css), clamp) {
			t.Errorf("the stylesheet lacks the face clamp %s", clamp)
		}
	}
	// the one send: the answer, the fund and the note ride one POST, and
	// land together on the card as one row
	before := r.events.count()
	note := "one send carries all three"
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"action": {"approve"}, "fund_eur": {"1.50"}, "note": {note}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the one send = %d: %s", rec.Code, rec.Body)
	}
	// the merge approve's note rides its row and the pull request's
	// review, not the request's own DecisionNote (the deny keeps its own)
	if a := r.approvals.get(4); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "" {
		t.Errorf("the request = %q by %q, note %q, want approved by sebastian, note %q", a.Status, a.DecidedBy, a.DecisionNote, "")
	}
	if k, ok, _ := r.tasks.Task(t.Context(), "t-m"); !ok || k.BudgetEUR != 1.5 {
		t.Errorf("t-m's budget = %v (ok %v), want the 1.50 the one send carried", k.BudgetEUR, ok)
	}
	checkRows(t, r.events, before,
		wantRow{kind: factoryd.EventMergeApproved, repo: "sf/code", taskID: "t-m",
			detail: map[string]any{"pr": int64(9), "approval": int64(4), "fund_eur": 1.5, "reason": note}})
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
