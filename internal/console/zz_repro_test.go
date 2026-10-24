package console

import (
	"context"
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

// The oversized needs-you card, held fixed at the construction point:
// every part a merge approval's face carries is bounded where the face
// is assembled, and the stylesheet clamps whatever a board face shows,
// so no field or card kind can grow the column's copy again (ruling
// 295). The lane face holds no part of the ask — only the brief, at
// one line, when the ask carries one — and the drawer keeps the whole
// text (the 2026-10-17 card).
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
	if s := askline(reco); strings.Contains(face, s[:40]) {
		t.Errorf("FAULT STILL PRESENT: the face carries the recommendation's first line:\n%s", face)
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
