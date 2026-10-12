package console

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Split is offered on a pull request classed large at its approval gate
// only, on the board's card and in the drawer; the member's split, by the
// button or the confirmed split_card, signals the card's workflow, and
// any other card is refused with its state. The card read names the
// split's cards on the card and the card it came from on each of them.
func TestSplitCard(t *testing.T) {
	r, _, g := newChatRig(t)
	var signalled []string
	r.c.cfg.SplitCard = func(_ context.Context, taskID, by string) error {
		signalled = append(signalled, taskID+" by "+by)
		return nil
	}
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-big", Kind: "agent", Status: "in-review", Lane: policy.LaneReady, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 30, PRNumber: 32, Objective: "Links."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-hi", Kind: "agent", Status: "in-review", Lane: policy.LaneReady, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 31, PRNumber: 33, Objective: "Index."}})
	r.tasks.mu.Unlock()
	r.approvals.rows = append(r.approvals.rows,
		factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-big", Project: "sf/code", PRNumber: 32, Status: "pending",
			Spec: json.RawMessage(`{"risk_class":"large"}`)},
		factoryd.ApprovalRequest{ID: 7, Kind: factoryd.ApprovalMerge, TaskID: "t-hi", Project: "sf/code", PRNumber: 33, Status: "pending",
			Spec: json.RawMessage(`{"risk_class":"high"}`)})
	ck, s := r.loggedIn(t)

	const split = `aria-label="Split PR #32 of t-big into 2 or 3 cards">Split</button>`
	get := func(path string) string {
		return r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
	}
	board := get("/")
	if strings.Count(board, ">Split</button>") != 1 || !strings.Contains(board, `action="/tasks/t-big/split"`) || !strings.Contains(board, split) {
		t.Fatalf("the board's Split buttons:\n%s", board)
	}
	if !strings.Contains(get("/cards/t-big/drawer"), split) || strings.Contains(get("/cards/t-hi/drawer"), ">Split</button>") {
		t.Fatal("the drawer: Split on t-big only")
	}

	for id, want := range map[string]string{
		"t-hi": "Card #31 waits at its approval gate, where a human approves the merge, not a large pull request at its approval gate: approve_merge or deny_merge decide its merge request.",
		"t-m":  "Card #5 waits in the backlog until a member moves it into a lane, not a large pull request at its approval gate: move_card starts it.",
	} {
		status, msg := refused(t, r, "split_card", toolInput{TaskID: id})
		equal(t, "split_card on "+id, []any{status, msg}, []any{http.StatusConflict, want})
	}
	rec := r.do(post("/tasks/t-hi/split", url.Values{"csrf": {s.csrf}}), ck)
	equal(t, "the button on t-hi", []any{rec.Code, rec.Body.String()}, []any{http.StatusConflict,
		"Card #31 waits at its approval gate, where a human approves the merge, not a large pull request at its approval gate: approve_merge or deny_merge decide its merge request.\n"})

	const note = "Split #30: a planner splits pull request #32 into 2 or 3 cards, which start once factoryd's check passes; the card then closes."
	before := r.events.count()
	req := post("/tasks/t-big/split", url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	rec = r.do(req, ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), html.EscapeString(note)) {
		t.Fatalf("the button = %d:\n%s", rec.Code, rec.Body)
	}
	row := r.events.all()[before]
	equal(t, "the button's row", []any{len(r.events.all()) - before, row.Kind, row.TaskID, row.Via, row.Detail["pr"]},
		[]any{1, factoryd.EventCardSplit, "t-big", factoryd.ViaButton, int64(32)})

	g.replies = [][]string{{call(1, "split_card", `{"task_id":"t-big"}`)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"split #30"}})
	equal(t, "the proposal", events, []string{
		`proposal {"arguments":{"task_id":"t-big"},"description":"Split a card, #30 t-big \"Links\"","id":"TOKEN","tool":"split_card"}`, `done {}`})
	_, events, _ = chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	equal(t, "Do it", events, []string{
		`tool {"arguments":{"task_id":"t-big"},"duration_ms":0,"name":"split_card","result":"` + note + `"}`,
		`applied {"result":"` + note + `","tool":"split_card"}`, `done {}`})
	equal(t, "signalled", signalled, []string{"t-big by sebastian", "t-big by sebastian"})

	// the split runs: a second split, and an approval of the pending merge
	// request, are refused until it ends; no review reaches Forgejo
	r.tasks.mu.Lock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == "t-big" })].Status = "running"
	r.tasks.mu.Unlock()
	const running = "Card #30 runs the split of pull request #32: its cards follow, or its findings come to the card; approve, deny or split it once the split ends."
	for _, tool := range []string{"split_card", "approve_merge"} {
		status, msg := refused(t, r, tool, toolInput{TaskID: "t-big"})
		equal(t, tool+" while the split runs", []any{status, msg}, []any{http.StatusConflict, running})
	}
	calls := len(r.fg.calls)
	rec = r.do(post("/approvals/6/approve", url.Values{"csrf": {s.csrf}}), ck)
	equal(t, "Approve while the split runs", []any{rec.Code, rec.Body.String(), len(r.fg.calls)}, []any{http.StatusConflict, running + "\n", calls})
	equal(t, "signalled", signalled, []string{"t-big by sebastian", "t-big by sebastian"})

	// "split into" and "split from" stand side by side: a split's card split
	// again has both, a plan's card that was split its "split into" only
	r.tasks.mu.Lock()
	for _, k := range []factoryd.Task{
		{ID: "t-c1", IssueNumber: 41, Parent: "t-big"}, {ID: "t-c2", IssueNumber: 42, Parent: "t-big", PRNumber: 50}, {ID: "t-g", IssueNumber: 43, Parent: "t-c2"},
		{ID: "t-pc", IssueNumber: 21, Parent: "t-plan", PRNumber: 22}, {ID: "t-pg", IssueNumber: 23, Parent: "t-pc"},
	} {
		k.Kind, k.Status, k.Lane, k.Process, k.Repository, k.Source, k.Objective = "agent", "queued", policy.LaneReady, "maintenance", "sf/code", factoryd.SourcePlan, "A part."
		r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: k})
	}
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Lane: policy.LanePlan,
		Process: "maintenance", Repository: "sf/code", IssueNumber: 20, Objective: "A plan."}})
	r.tasks.mu.Unlock()
	got := reads(t, r, g, "card", `{"task_id":"t-big"}`, "card", `{"task_id":"t-c2"}`, "card", `{"task_id":"t-c1"}`, "card", `{"task_id":"t-pc"}`)
	for i, want := range []string{"\nsplit into #41, #42\n", "\nsplit from #30\nsplit into #43\n", "\nsplit from #30\n", "\nsplit into #23\n"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("card read %d lacks %q:\n%s", i, want, got[i])
		}
	}
	if strings.Contains(got[2], "split into") || strings.Contains(got[3], "split from") {
		t.Errorf("the card reads:\n%s\n\n%s", got[2], got[3])
	}
}
