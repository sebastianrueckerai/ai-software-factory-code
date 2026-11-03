package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// newWritesRig is the chat rig with the cards the writes need beside #5,
// #11 and #13: #14 t-cap waiting at its session cap (three sessions of
// the policy's three), #7 t-done done, #31 t-f failed, and #21 t-a and
// #22 t-b in the backlog of process spec, which has the dependencies
// block. #21, #22 and #5 t-m are cards of plan t-pl; withPlan adds it.
func newWritesRig(t *testing.T) (*rig, *chatGateway) {
	r, _, g := newChatRig(t)
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	for i, bt := range r.tasks.tasks {
		if bt.ID == "t-m" {
			r.tasks.tasks[i].Source, r.tasks.tasks[i].Parent = factoryd.SourcePlan, "t-pl"
		}
	}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-cap", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14,
			Objective: "Trim the logs.", Usage: factoryd.TaskUsage{Attempts: 3}},
			Escalation: "the session cap is reached: every finding sent to the agent so far goes to a human\nlast: the checks failed"},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 7,
			Objective: "Add the schema."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-f", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 31,
			Objective: "Add the cache."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-a", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec",
			Repository: "sf/code", IssueNumber: 21, Objective: "Add the API.", Source: factoryd.SourcePlan, Parent: "t-pl"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-b", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec",
			Repository: "sf/code", IssueNumber: 22, Objective: "Add the web UI.", Source: factoryd.SourcePlan, Parent: "t-pl"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Track: policy.TrackPlan, Process: "spec",
			Repository: "sf/code", IssueNumber: 40, Objective: "Plan the shortener."}})
	return r, g
}

// withPlan adds plan #30 t-pl, whose cards are also #31 t-k1 and #32
// t-k2, and #33 t-d, a direct card of no plan. They sit apart from the
// rig so the queue holds no more cards than the cards it already counts.
func withPlan(r *rig) {
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-pl", Kind: "agent", Status: "waiting-human", Track: policy.TrackPlan, Process: "spec",
			Repository: "sf/code", IssueNumber: 30, Objective: "Plan the links."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-k1", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec",
			Repository: "sf/code", IssueNumber: 31, Objective: "Add the first link.", Source: factoryd.SourcePlan, Parent: "t-pl"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-k2", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec",
			Repository: "sf/code", IssueNumber: 32, Objective: "Add the second link.", Source: factoryd.SourcePlan, Parent: "t-pl"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-d", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec",
			Repository: "sf/code", IssueNumber: 33, Objective: "Add the lone card."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-d2", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec",
			Repository: "sf/code", IssueNumber: 34, Objective: "Add the other lone card."}})
}

// proposeAndDo has the agent call tool with args, which proposes, and the
// member press Do it: it answers the proposal's stream, the Do it's and
// the Do it's undo tokens.
func proposeAndDo(t *testing.T, r *rig, g *chatGateway, tool, args string, corrective ...string) (proposed, done, undo []string) {
	t.Helper()
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, tool, args)}}
	for _, c := range corrective {
		g.replies = append(g.replies, []string{say(c)})
	}
	_, proposed, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"do it"}})
	if len(tokens) != 1 {
		t.Fatalf("%s proposed %v", tool, proposed)
	}
	_, done, undo = chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	return proposed, done, undo
}

// ran is the stream of a Do it of tool with args that answered result
// and offers no undo.
func ran(tool, args, result string) []string {
	return []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"` + tool + `","result":"` + result + `"}`,
		`applied {"result":"` + result + `","tool":"` + tool + `"}`, `done {}`}
}

// writesOf is what Forgejo was asked to change, "METHOD path" in order.
func writesOf(f *fakeForgejo) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if parts := strings.Fields(c); parts[0] != http.MethodGet {
			out = append(out, parts[0]+" "+parts[1])
		}
	}
	return out
}

// close proposes; Do it comments the note on the card's issue as the
// member and then closes the issue as them, which intake turns into the
// card's end, with one card_closed row via chat. A final card refuses with
// its state. The face's Close button runs the same action: one row via
// button and a 303 to the filtered board.
func TestChatClose(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"task_id":"t-m","note":"Superseded by #7."}`
	proposed, done, _ := proposeAndDo(t, r, g, "close", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Close a card, #5 t-m \"Fix the cache. It drops entries.\", note Superseded by #7.","expires":"EXPIRES","id":"TOKEN","tool":"close"}`,
		`done {}`})
	equal(t, "Do it", done, ran("close", args, "Closed #5; factoryd ends the card."))
	equal(t, "Forgejo", writesOf(r.fg), []string{"POST /api/v1/repos/sf/code/issues/5/comments", "PATCH /api/v1/repos/sf/code/issues/5"})
	equal(t, "the note", r.fg.comments["/api/v1/repos/sf/code/issues/5/comments"], "Superseded by #7.")
	equal(t, "the patch", r.fg.patched, []string{`/api/v1/repos/sf/code/issues/5 {"state":"closed"}`})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-m", IssueNumber: 5, Kind: factoryd.EventCardClosed, Detail: map[string]any{"note": "Superseded by #7."}})

	code, msg := refused(t, r, "close", toolInput{TaskID: "t-done"})
	equal(t, "close of a done card", []any{code, msg},
		[]any{http.StatusConflict, "Card #7 is done, not a card that is not final: the card is final: a new card continues the work."})

	ck, s := r.loggedIn(t)
	before = r.events.count()
	rec := r.do(post("/tasks/t-m/close", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	equal(t, "the button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-m", detail: map[string]any{"note": ""}})
	equal(t, "the button's patch", r.fg.patched[1:], []string{`/api/v1/repos/sf/code/issues/5 {"state":"closed"}`})
}

// A merged card's issue closed with its merge, so closing it again sends
// no webhook: the console's and the chat's Close end a merged card through
// CloseCard as the member, its note commented on the issue first, the
// issue left as it is.
func TestClosingAMergedCard(t *testing.T) {
	r, g := newWritesRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-mg", Kind: "agent", Status: "merged", Repository: "sf/code",
		IssueNumber: 9, PRNumber: 19, Objective: "Add the feed."}})
	r.tasks.mu.Unlock()
	before := r.events.count()
	args := `{"task_id":"t-mg","note":"The chain cannot carry it."}`
	_, done, _ := proposeAndDo(t, r, g, "close", args)
	equal(t, "Do it", done, ran("close", args, "Closed #9."))
	equal(t, "closed", r.closed, []string{"t-mg by sebastian"})
	equal(t, "Forgejo", writesOf(r.fg), []string{"POST /api/v1/repos/sf/code/issues/9/comments"})
	equal(t, "the note", r.fg.comments["/api/v1/repos/sf/code/issues/9/comments"], "The chain cannot carry it.")
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-mg", IssueNumber: 9, Kind: factoryd.EventCardClosed, Detail: map[string]any{"note": "The chain cannot carry it."}})

	ck, s := r.loggedIn(t)
	before = r.events.count()
	rec := r.do(post("/tasks/t-mg/close", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	equal(t, "the button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	equal(t, "closed by the button", r.closed, []string{"t-mg by sebastian", "t-mg by sebastian"})
	equal(t, "the patches", r.fg.patched, []string(nil))
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-mg", detail: map[string]any{"note": ""}})
}

// edit proposes; Do it edits the issue's title alone as the member, and
// the card's objective becomes the issue's title and body as Forgejo
// answers them, which the next session's brief reads; one card_edited row
// via chat. A running card refuses, and an edit of neither title nor
// body is refused.
func TestChatEdit(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"task_id":"t-m","title":"Fix the cache eviction"}`
	proposed, done, _ := proposeAndDo(t, r, g, "edit", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Edit a card, #5 t-m \"Fix the cache. It drops entries.\", title Fix the cache eviction","expires":"EXPIRES","id":"TOKEN","tool":"edit"}`,
		`done {}`})
	equal(t, "Do it", done, ran("edit", args, "Edited #5; the card's next session reads the edit."))
	equal(t, "the patch", r.fg.patched, []string{`/api/v1/repos/sf/code/issues/5 {"title":"Fix the cache eviction"}`})
	k, _, _ := r.tasks.Task(t.Context(), "t-m")
	equal(t, "the objective", k.Objective, "Fix the cache eviction\n\nIt drops entries.")
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-m", IssueNumber: 5, Kind: factoryd.EventCardEdited, Detail: map[string]any{"title": "Fix the cache eviction"}})

	code, msg := refused(t, r, "edit", toolInput{TaskID: "t-r", Title: "Faster"})
	equal(t, "edit of a running card", []any{code, msg}, []any{http.StatusConflict,
		"Card #11 runs an agent session, not a card not in a session: steer sends its session a message; its pull request comes when the session ends."})
	code, msg = refused(t, r, "edit", toolInput{TaskID: "t-m", Body: " \n"})
	equal(t, "edit of nothing", []any{code, msg}, []any{http.StatusBadRequest, "a title or a body is required"})
}

// A body alone replaces the issue's body: the objective keeps the title
// Forgejo answers, and the row counts the body's bytes.
func TestEditBody(t *testing.T) {
	r, _ := newWritesRig(t)
	_, s := r.loggedIn(t)
	before := r.events.count()
	note, err := r.c.doEdit(t.Context(), actor{s: s, via: factoryd.ViaButton}, "t-m", "", "Entries vanish under load.")
	equal(t, "doEdit", []any{note, err}, []any{"Edited #5; the card's next session reads the edit.", nil})
	equal(t, "the patch", r.fg.patched, []string{`/api/v1/repos/sf/code/issues/5 {"body":"Entries vanish under load."}`})
	k, _, _ := r.tasks.Task(t.Context(), "t-m")
	equal(t, "the objective", k.Objective, "Fix the cache\n\nEntries vanish under load.")
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardEdited, repo: "sf/code", taskID: "t-m", detail: map[string]any{"body_bytes": 26}})
}

// grant_session proposes on the card at its session cap; Do it extends
// its live case by one session through Act on the human's rung, the
// human's answer the note posted on its issue, and Act's grant row names
// the comment.
func TestChatGrantSession(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"task_id":"t-cap","text":"Trim to 1 MB first."}`
	proposed, done, _ := proposeAndDo(t, r, g, "grant_session", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Grant a session, #14 t-cap \"Trim the logs.\", text Trim to 1 MB first.","expires":"EXPIRES","id":"TOKEN","tool":"grant_session"}`,
		`done {}`})
	equal(t, "Do it", done, ran("grant_session", args, "Case 1 took extend."))
	got, posts := actedOnce(t, r)
	equal(t, "the grant", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-cap", CaseID: 1, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "Trim to 1 MB first."}}, true})
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/code/issues/14/comments"], "Trim to 1 MB first.")
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-cap", IssueNumber: 14, Kind: factoryd.EventSessionGranted,
			Detail: map[string]any{"action": factoryd.ActExtend, "case_id": int64(1), "answer": "Trim to 1 MB first.", "comment": int64(1)}})
}

// Roadmap 237: grant_session, answer and a comment on a card waiting for
// an answer carry at most factoryd.MaxAnswer bytes, as the answer box
// does, and post nothing longer. A comment on a card in the backlog, or
// on a plan waiting on its approvers (the planner gets it whole), keeps
// the 64 KiB bound.
func TestChatAnswerTextsBoundedAtMaxAnswer(t *testing.T) {
	r, _ := newWritesRig(t)
	long := strings.Repeat("x", 4001)
	for _, tc := range []struct{ tool, id string }{{"grant_session", "t-cap"}, {"answer", "t-e"}, {"comment", "t-e"}} {
		code, msg := refused(t, r, tc.tool, toolInput{TaskID: tc.id, Text: long})
		equal(t, tc.tool+" on "+tc.id, []any{code, msg}, []any{http.StatusBadRequest, "an answer of 1 to 4000 bytes is required; this one has 4001"})
	}
	equal(t, "the comments", len(r.fg.comments), 0)
	_, s := r.loggedIn(t)
	a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat}
	for _, id := range []string{"t-m", "t-plan"} {
		if _, _, err := r.c.doComment(t.Context(), a, id, long); err != nil {
			t.Errorf("comment on %s: %v", id, err)
		}
	}
	equal(t, "the comments", len(r.fg.comments), 2)
}

// set_dependency proposes; Do it stores the edge and makes the issue
// depend on the other's as the bot, with one dependency_set row via chat,
// and offers its inverse as the undo. A card that started refuses, a
// process without the dependencies block refuses, a cycle refuses naming
// the dependency that closes it; remove takes the edge off both, and its
// undo puts it back.
func TestChatSetDependency(t *testing.T) {
	r, g := newWritesRig(t)
	withPlan(r)
	ck, s := r.loggedIn(t)
	before := r.events.count()
	args := `{"task_id":"t-b","depends_on":"t-a"}`
	proposed, done, _ := proposeAndDo(t, r, g, "set_dependency", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Set a dependency, #22 t-b \"Add the web UI.\", depends on #21 t-a \"Add the API.\"","expires":"EXPIRES","id":"TOKEN","tool":"set_dependency"}`,
		`done {}`})
	set := "#22 depends on #21 now."
	equal(t, "Do it", done, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_dependency","result":"` + set + `"}`,
		`applied {"result":"` + set + `","tool":"set_dependency","undo":"TOKEN"}`, `done {}`})
	equal(t, "the stored edges", r.tasks.deps, map[string][]string{"t-b": {"t-a"}})
	equal(t, "the issue dependencies", r.issueDeps, []string{"sf/code#22 on #21"})
	row := func(removed bool) factoryd.Event {
		return factoryd.Event{Repository: "sf/code", TaskID: "t-b", IssueNumber: 22, Kind: factoryd.EventDependencySet,
			Detail: map[string]any{"depends_on": "t-a", "removed": removed}}
	}
	chatRows(t, r.events, before, "sebastian", row(false))

	for _, tc := range []struct {
		in   toolInput
		code int
		want string
	}{
		{toolInput{TaskID: "t-r", DependsOn: "t-a"}, http.StatusConflict,
			"Card #11 runs an agent session, not a card that has not started: steer sends its session a message; its pull request comes when the session ends."},
		{toolInput{TaskID: "t-m", DependsOn: "t-a"}, http.StatusConflict, "Process maintenance has no dependencies block: its cards do not wait on others."},
		{toolInput{TaskID: "t-a", DependsOn: "t-b"}, http.StatusConflict, "#22 already depends on #21, so #21 cannot depend on #22."},
		{toolInput{TaskID: "t-b", DependsOn: "t-a"}, http.StatusConflict, "#22 already depends on #21."},
		{toolInput{TaskID: "t-a", DependsOn: "t-done"}, http.StatusConflict,
			"Card #7 is done, not a card that is not final: the card is final: a new card continues the work."},
		{toolInput{TaskID: "t-a", DependsOn: "t-m", Remove: true}, http.StatusConflict, "#21 does not depend on #5."},
		{toolInput{TaskID: "t-a", DependsOn: "t-a"}, http.StatusBadRequest, "a card cannot depend on itself"},
		{toolInput{TaskID: "t-k1", DependsOn: "t-d"}, http.StatusConflict,
			"#31 is in plan #30 and #33 is in no plan. A dependency joins cards of one plan only. Ask for the change in plan #30."},
		{toolInput{TaskID: "t-d", DependsOn: "t-k1"}, http.StatusConflict,
			"#33 is in no plan and #31 is in plan #30. A dependency joins cards of one plan only. Ask for #33 in plan #30."},
		{toolInput{TaskID: "t-d2", DependsOn: "t-d"}, http.StatusConflict,
			"#34 is in no plan and #33 is in no plan. A dependency joins cards of one plan only. Wait for #33 to finish and file #34 again after it, or file the work of both as one plan."},
	} {
		code, msg := refused(t, r, "set_dependency", tc.in)
		equal(t, "set_dependency "+tc.in.TaskID+" on "+tc.in.DependsOn, []any{code, msg}, []any{tc.code, tc.want})
	}

	args = `{"task_id":"t-b","depends_on":"t-a","remove":true}`
	proposed, done, undo := proposeAndDo(t, r, g, "set_dependency", args)
	equal(t, "removal's proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Set a dependency, #22 t-b \"Add the web UI.\", no longer depends on #21 t-a \"Add the API.\"","expires":"EXPIRES","id":"TOKEN","tool":"set_dependency"}`,
		`done {}`})
	gone := "#22 no longer depends on #21."
	equal(t, "removal's Do it", done, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_dependency","result":"` + gone + `"}`,
		`applied {"result":"` + gone + `","tool":"set_dependency","undo":"TOKEN"}`, `done {}`})
	equal(t, "the stored edges", r.tasks.deps, map[string][]string{"t-b": {}})
	equal(t, "the issue dependencies", r.issueDeps, []string{})
	chatRows(t, r.events, before, "sebastian", row(false), row(true))

	_, events, _ := chatPost(t, r, ck, s, chatPath+"/undo/"+undo[0], nil)
	equal(t, "the removal's undo", events, []string{`tool {"arguments":{},"duration_ms":0,"name":"undo set_dependency","result":"` + set + `"}`,
		`applied {"result":"` + set + `","tool":"set_dependency"}`, `done {}`})
	equal(t, "the stored edges", r.tasks.deps, map[string][]string{"t-b": {"t-a"}})
	equal(t, "the issue dependencies", r.issueDeps, []string{"sf/code#22 on #21"})
	chatRows(t, r.events, before, "sebastian", row(false), row(true), row(false))
}

// set_dependency between two cards of one plan succeeds, and an edge
// across groups already in the store can still be removed.
func TestChatSetDependencyInsideAPlan(t *testing.T) {
	r, g := newWritesRig(t)
	withPlan(r)
	args := `{"task_id":"t-k2","depends_on":"t-k1"}`
	_, done, _ := proposeAndDo(t, r, g, "set_dependency", args)
	set := "#32 depends on #31 now."
	equal(t, "Do it", done, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_dependency","result":"` + set + `"}`,
		`applied {"result":"` + set + `","tool":"set_dependency","undo":"TOKEN"}`, `done {}`})

	r.tasks.deps = map[string][]string{"t-k1": {"t-d"}}
	args = `{"task_id":"t-k1","depends_on":"t-d","remove":true}`
	_, done, _ = proposeAndDo(t, r, g, "set_dependency", args)
	gone := "#31 no longer depends on #33."
	equal(t, "removal", done, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_dependency","result":"` + gone + `"}`,
		`applied {"result":"` + gone + `","tool":"set_dependency","undo":"TOKEN"}`, `done {}`})
}

// new_card takes a track with its one-line reason, and a size: the
// proposal names them, and Do it files the issue with its track's label
// and its body ending in the size line; the card_filed row records the
// track and the reason.
func TestChatNewCardSize(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"title":"Cache metrics","body":"Count hits.","acceptance":["The hits count"],"track":"direct","track_reason":"one small change","size":"large"}`
	proposed, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"File a card, title Cache metrics, body Count hits., track direct, track_reason one small change, size large, acceptance The hits count","expires":"EXPIRES","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The hits count\n\nSize: large")
	equal(t, "the issue's labels", fmt.Sprint(r.fg.issues[0]["labels"]), "[1]")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "reason": "one small change", "title": "Cache metrics", "size": "large", "thread": int64(1)}})
}

// A new_card call without a size files: the sizing step sizes the card
// before its first session.
func TestChatNewCardFilesWithoutASize(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"title":"Cache metrics","body":"Count hits.","acceptance":["The hits count"],"track":"direct","track_reason":"one small change"}`
	_, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The hits count")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "reason": "one small change", "title": "Cache metrics", "thread": int64(1)}})
}

// new_card files nothing without a track or the one line why: the
// proposal refuses a call that names no track, one of the old lanes, or
// a blank track_reason, so the agent corrects it before Do it, and the
// run refuses them too; plan on a process without a plan track is the
// project's refusal.
func TestChatNewCardNeedsATrackAndItsReason(t *testing.T) {
	r, g := newWritesRig(t)
	noTrack := "track: direct builds at once, plan plans first, human is a person's card"
	noReason := "track_reason: why the card takes track direct, one line"
	ck, s := r.loggedIn(t)
	for args, refusal := range map[string]string{
		`{"title":"Cache metrics"}`: noTrack,
		`{"title":"Cache metrics","track":"backlog","track_reason":"later"}`: noTrack,
		`{"title":"Cache metrics","track":"ready","track_reason":"now"}`:     noTrack,
		`{"title":"Cache metrics","track":"direct"}`:                         noReason,
		`{"title":"Cache metrics","track":"direct","track_reason":"  "}`:     noReason,
	} {
		g.replies = [][]string{{call(1, "new_card", args)}, {say("Refused.")}}
		_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"file it"}})
		equal(t, "the proposal of "+args, events, []string{
			`tool {"arguments":` + args + `,"duration_ms":0,"name":"new_card","result":"Refused: ` + refusal + `"}`, `text {"text":"Refused."}`, `done {}`})
	}
	for _, tc := range []struct {
		in      toolInput
		refusal string
	}{
		{toolInput{Title: "Cache metrics"}, noTrack},
		{toolInput{Title: "Cache metrics", Track: "direct", TrackReason: " \t "}, noReason},
		{toolInput{Title: "Cache metrics", Track: "plan", TrackReason: "a spec"},
			"Project ai-software-factory-code follows process maintenance, which has no plan track: file the card on track direct."},
	} {
		code, msg := refused(t, r, "new_card", tc.in)
		equal(t, "the run of "+tc.in.Track, []any{code, msg}, []any{http.StatusBadRequest, tc.refusal})
	}
	equal(t, "the issues filed", len(r.fg.issues), 0)
}

// new_card carries a pull request: carry_pr files the issue with its body
// ending in the Carries line that puts the head in the agent's clone. A
// brief that asks the sandbox to obtain a pull request's commits without
// carry_pr is refused at the proposal, so the agent corrects it before
// Do it, and the run refuses it too.
func TestChatNewCardCarriesAPullRequest(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"title":"Re-land the feed fix","body":"Carry pull request #9 to green checks.","acceptance":["The checks are green"],"track":"direct","track_reason":"the fix is known","size":"small","carry_pr":9}`
	proposed, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"File a card, title Re-land the feed fix, body Carry pull request #9 to green checks., track direct, track_reason the fix is known, size small, carries #9, acceptance The checks are green","expires":"EXPIRES","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Carry pull request #9 to green checks.\n\nAcceptance:\n- The checks are green\n\nSize: small\nCarries: #9")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "reason": "the fix is known", "title": "Re-land the feed fix", "size": "small", "carry": int64(9), "thread": int64(1)}})

	for _, brief := range []string{
		"Carry pull request #9 to green checks.",
		"Get PR #1257's original diff and reapply it.",
		"Cherry-pick the commits of pull request #9.",
		"Rebase the PR onto main.",
		"Fetch refs/pull/1257/head.",
	} {
		if !carryAsk.MatchString(brief) {
			t.Errorf("%q asks the sandbox to obtain a pull request's commits", brief)
		}
	}
	for _, brief := range []string{
		"Ship the API. Depends on #21.",
		"Cherry-pick the last commit on main.",
		"Read the PR's description and fix the cache.",
		"Carries: #9",
	} {
		if carryAsk.MatchString(brief) {
			t.Errorf("%q does not ask the sandbox to obtain a pull request's commits", brief)
		}
	}

	refusal := "The sandbox holds no git credential, so its agent cannot fetch a pull request's commits mid-session: name carry_pr as the pull request's number, which supplies its head in the clone."
	code, msg := refused(t, r, "new_card", toolInput{Title: "Re-land the feed fix", Body: "Carry pull request #9 to green checks.", Track: policy.TrackDirect, TrackReason: "the fix is known"})
	equal(t, "a carry brief without carry_pr", []any{code, msg}, []any{http.StatusBadRequest, refusal})
	// the proposal judges the brief, so the agent corrects it before Do it
	ck, s := r.loggedIn(t)
	args = `{"body":"Carry pull request #9 to green checks.","title":"Re-land the feed fix","track":"direct","track_reason":"the fix is known"}`
	g.replies = [][]string{{call(1, "new_card", args)}, {say("Refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"re-land the feed fix"}})
	equal(t, "the proposal of a carry brief without carry_pr", events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"new_card","result":"Refused: ` + refusal + `"}`, `text {"text":"Refused."}`, `done {}`})
}

// A card's relative link, in its title or its body, resolves against the
// document repository's whole tree at HEAD, read page by page at the sha
// the first page answers: a link to a plan, or to the root's README.md
// beside docs/README.md, files; a link to no file, or to several, is
// refused at the run and at the proposal, with the way out for a path in
// the code repository. Without a document repository or the docs token
// nothing is checked, and a docs outage files the card.
func TestChatNewCardRefusesALinkToNoDocument(t *testing.T) {
	// eight entries, four pages of two: README.md, docs, docs/README.md,
	// docs/notes.md, docs/plans, the plan, guide, then guide/notes.md
	docs := map[string]string{"docs/plans/2026-10-14-lane-collapse.md": "# Lane collapse\n", "README.md": "# Read me\n", "docs/README.md": "# Docs\n",
		"docs/notes.md": "# Notes\n", "guide/notes.md": "# Guide notes\n"}
	r, g := newWritesRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
	f := withDocs(t, r, docs)
	args := `{"title":"Collapse the lanes","body":"Builds [the plan](../plans/2026-10-14-lane-collapse.md) as [the readme](README.md) says.","acceptance":["The board shows no lanes"],"track":"direct","track_reason":"the plan is written","size":"small"}`
	_, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "a link to the plan and to the root's README.md", done, ran("new_card", args, "Filed sf/code#41 on track direct."))

	f.treeRefs = nil
	const asText = " A path in the code repository goes in as text, not as a link."
	none := "The link ../plans/2026-10-15-none.md resolves to no file in the document repository sf/docs: name the path from the repository's root." + asText
	code, msg := refused(t, r, "new_card", toolInput{Title: "Collapse the lanes", Body: "Builds [the plan](../plans/2026-10-15-none.md).", Track: policy.TrackDirect, TrackReason: "the plan is written"})
	equal(t, "a link to no file", []any{code, msg}, []any{http.StatusBadRequest, none})
	equal(t, "the tree's pages, after the first at its sha", f.treeRefs, []string{"HEAD page 1", "c0ffee page 2", "c0ffee page 3", "c0ffee page 4"})
	code, msg = refused(t, r, "new_card", toolInput{Title: "Collapse the lanes by [the plan](../plans/2026-10-15-none.md)", Body: "Collapse.", Track: policy.TrackDirect, TrackReason: "the plan is written"})
	equal(t, "a link to no file in the title", []any{code, msg}, []any{http.StatusBadRequest, none})
	code, msg = refused(t, r, "new_card", toolInput{Title: "Collapse the lanes", Body: "Read [the notes](notes.md).", Track: policy.TrackDirect, TrackReason: "the plan is written"})
	equal(t, "a link to two files", []any{code, msg}, []any{http.StatusBadRequest,
		"The link notes.md matches 2 files in the document repository sf/docs (docs/notes.md, guide/notes.md): name one path from the repository's root." + asText})

	ck, s := r.loggedIn(t)
	args = `{"body":"Builds [the plan](../plans/2026-10-15-none.md).","title":"Collapse the lanes","track":"direct","track_reason":"the plan is written"}`
	inAcceptance := `{"acceptance":["The lanes follow [the plan](../plans/2026-10-15-none.md)"],"body":"Collapse the lanes.","title":"Collapse the lanes","track":"direct","track_reason":"the plan is written"}`
	g.replies = [][]string{{call(1, "new_card", args, "new_card", inAcceptance)}, {say("Refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"collapse the lanes"}})
	equal(t, "the proposals of a link to no file, in the body and in an acceptance item", events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"new_card","result":"Refused: ` + none + `"}`,
		`tool {"arguments":` + inAcceptance + `,"duration_ms":0,"name":"new_card","result":"Refused: ` + none + `"}`, `text {"text":"Refused."}`, `done {}`})

	args = `{"title":"Collapse the lanes","body":"Builds [the plan](../plans/2026-10-15-none.md).","acceptance":["The board shows no lanes"],"track":"direct","track_reason":"the plan is written","size":"small"}`
	for _, tt := range []struct {
		name  string
		seat  func(r *rig) *docsFake
		reads []string
	}{
		{"a project without a document repository", func(r *rig) *docsFake { return withDocs(t, r, docs) }, nil},
		{"a console without the docs token", func(r *rig) *docsFake {
			r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
			f := withDocs(t, r, docs)
			r.c.cfg.DocsToken = ""
			return f
		}, nil},
		// the proposal's read and the run's, each refused
		{"a docs outage", func(r *rig) *docsFake {
			r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
			f := withDocs(t, r, docs)
			f.failTree = true
			return f
		}, []string{"HEAD page 1", "HEAD page 1"}},
	} {
		r, g := newWritesRig(t)
		f := tt.seat(r)
		_, done, _ := proposeAndDo(t, r, g, "new_card", args)
		equal(t, tt.name, []any{done, f.treeRefs}, []any{ran("new_card", args, "Filed sf/code#41 on track direct."), tt.reads})
	}
}

// set_autonomy without a queue cap keeps the project's: its proposal
// names the cap that stays, 5 from the project file, and carries none;
// with a cap it names the new one.
func TestChatSetAutonomyKeepsTheCap(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", QueueCap: 5}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "set_autonomy", `{"autonomy":"assisted"}`, "set_autonomy", `{"autonomy":"assisted","queue_cap":3}`)}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"go assisted"}})
	equal(t, "proposals", events, []string{
		`proposal {"arguments":{"autonomy":"assisted"},"description":"Set the autonomy level, autonomy assisted, queue cap 5 stays","expires":"EXPIRES","id":"TOKEN","tool":"set_autonomy"}`,
		`proposal {"arguments":{"autonomy":"assisted","queue_cap":3},"description":"Set the autonomy level, autonomy assisted, queue cap 3","expires":"EXPIRES","id":"TOKEN","tool":"set_autonomy"}`,
		`done {}`})
}

// set_pool, always confirmed, changes the project file's escalation pool
// money as the approver's merged pull request and records one
// pool_changed row naming the human; a top-up grows the money the file
// sets, and a top-up of money the file does not set refuses at the
// proposal, before a write.
func TestChatSetPool(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human"}}
	infra := withInfra(t, r)
	head := "# the code project\nrepository: sf/code\nmerge_rule: human\nautonomy: assisted\nbudget:\n  month_eur: 10\n  task_max_eur: 4\nescalation:\n  pool:\n    eur: 20\n"
	infra.files = map[string]string{"factory/projects/ai-software-factory-code.yaml": head}
	next := 25.0
	r.c.cfg.PolicyReload = func(context.Context) error {
		r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human",
			Budget: &policy.ProjectBudget{MonthEUR: 10}, Escalation: &policy.Escalation{Pool: policy.Draw{EUR: next}}}
		return nil
	}
	args := `{"action":"set","eur":25,"reason":"the findings cost money"}`
	_, done, _ := proposeAndDo(t, r, g, "set_pool", args)
	equal(t, "set", done, ran("set_pool", args,
		"The pool of ai-software-factory-code is €25.00 now, was €20.00: sf/infra#12 carries the change and the policy in force holds it."))
	if got, _ := infra.contents[len(infra.contents)-1].body["message"].(string); !strings.HasPrefix(got, "Project ai-software-factory-code: escalation pool 25.00 EUR\n") || !strings.Contains(got, "Factory-Role: chat\n") {
		t.Errorf("the commit's message = %q", got)
	}
	rows := r.events.all()
	e := rows[len(rows)-1]
	equal(t, "the event", []any{e.Kind, e.Actor}, []any{factoryd.EventPoolChanged, "sebastian"})
	equal(t, "the event's detail", e.Detail, map[string]any{"action": "set", "from_eur": 20.0, "confirmed_eur": 25.0, "to_eur": 25.0,
		"reason": "the findings cost money", "pr": "sf/infra#12"})

	// a top-up grows the standing pool and keeps the file's other caps
	infra.files["factory/projects/ai-software-factory-code.yaml"] = strings.Replace(head, "eur: 20", "eur: 25", 1)
	next = 30.0
	args = `{"action":"top_up","eur":5,"reason":"a second finding"}`
	_, done, _ = proposeAndDo(t, r, g, "set_pool", args)
	equal(t, "top up", done, ran("set_pool", args,
		"The pool of ai-software-factory-code is €30.00 now, was €25.00: sf/infra#12 carries the change and the policy in force holds it."))
	if got, _ := infra.contents[len(infra.contents)-1].body["message"].(string); !strings.HasPrefix(got, "Project ai-software-factory-code: escalation pool +5.00 EUR\n") {
		t.Errorf("the top-up's message = %q", got)
	}
	rows = r.events.all()
	equal(t, "the top-up's detail", rows[len(rows)-1].Detail, map[string]any{"action": "top_up", "from_eur": 25.0, "confirmed_eur": 5.0, "to_eur": 30.0,
		"reason": "a second finding", "pr": "sf/infra#12"})

	// a top-up of money the file does not set refuses at the proposal,
	// before anything writes: the pool's money is the policy's
	infra.files["factory/projects/ai-software-factory-code.yaml"] = "# the code project\nrepository: sf/code\nmerge_rule: human\n"
	args = `{"action":"top_up","eur":5,"reason":"no money of its own"}`
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "set_pool", args)}, {say("Refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"top the pool up"}})
	equal(t, "the refusal", events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_pool","result":"Refused: projects/ai-software-factory-code.yaml sets no escalation.pool.eur, so the pool's money is the policy's: set the pool instead of a top-up"}`,
		`text {"text":"Refused."}`, `done {}`})
	args = `{"action":"set","eur":25,"reason":"no bounds"}`

	// a project file of no YAML — an empty one, or one of whitespace
	// alone — refuses at the proposal too, not a panic
	for _, empty := range []string{"", " \n\t "} {
		infra.files["factory/projects/ai-software-factory-code.yaml"] = empty
		g.replies = [][]string{{call(1, "set_pool", args)}, {say("Refused.")}}
		_, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"set the pool"}})
		equal(t, "the empty file's refusal", events, []string{
			`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_pool","result":"Refused: projects/ai-software-factory-code.yaml holds no YAML the pool can read: a policy PR corrects it"}`,
			`text {"text":"Refused."}`, `done {}`})
	}
	if len(infra.contents) != 2 || len(infra.pulls) != 2 || len(infra.merges) != 2 {
		t.Fatalf("the refusals aside, %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
}

// set_pool is an approver's: a writer of the project's code who approves
// nothing has the call refused with the rule's words.
func TestChatSetPoolNeedsAnApprover(t *testing.T) {
	r, _, g := newChatRig(t)
	members(r)
	ck, s := memberLogin(t, r, "controller")
	args := `{"action":"set","eur":25,"reason":"the findings cost money"}`
	g.replies = [][]string{{call(1, "set_pool", args)}, {say("Refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"set a pool"}})
	equal(t, "the refusal", events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_pool","result":"Refused: Only the approvers of project ai-software-factory-code may change the escalation pool."}`,
		`text {"text":"Refused."}`, `done {}`})
}

// poolChatRig is the writes rig with the project's file on infra's main
// holding an escalation pool of standing EUR, the loaded policy's pool at
// standing and the reload that lands it, and a month's draws of used EUR.
func poolChatRig(t *testing.T, standing, used float64) (*rig, *chatGateway, *infraFake) {
	t.Helper()
	r, g := newWritesRig(t)
	head := "# the code project\nrepository: sf/code\nmerge_rule: human\nautonomy: assisted\nbudget:\n  month_eur: 10\n  task_max_eur: 4\nescalation:\n  pool:\n    eur: " +
		strings.Replace(fmt.Sprintf("%.2f", standing), ".00", "", 1) + "\n"
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human",
		Budget: &policy.ProjectBudget{MonthEUR: 10}, Escalation: &policy.Escalation{Pool: policy.Draw{EUR: standing}}}}
	r.drawn = policy.Draw{EUR: used}
	infra := withInfra(t, r)
	infra.files = map[string]string{"factory/projects/ai-software-factory-code.yaml": head}
	return r, g, infra
}

// mustPolicy is the rig's policy in force.
func mustPolicy(t *testing.T, r *rig) policy.Version {
	t.Helper()
	v, ok, err := r.ledger.CurrentPolicy(t.Context())
	if err != nil || !ok {
		t.Fatalf("CurrentPolicy = %v, %v", ok, err)
	}
	return v
}

// poolReload lands EUR e as the loaded policy's pool.
func poolChatReload(r *rig, e float64) {
	r.c.cfg.PolicyReload = func(context.Context) error {
		r.ledger.files["ai-software-factory-code"].Escalation.Pool.EUR = e
		return nil
	}
}

// A pool proposal is the member's to confirm: the turn captures the
// change — the project file's blob, its standing pool, the call's
// operation, amount and reason — and writes nothing until they press Do
// it; their corrected amount, the one thing a press may change, reaches
// the guarded writer as their own action, and the row names them.
func TestPoolProposal(t *testing.T) {
	r, g, infra := poolChatRig(t, 8, 3)
	poolChatReload(r, 12)
	ck, s := r.loggedIn(t)
	args := `{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"}`
	g.replies = [][]string{{call(1, "set_pool", args)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool for the findings"}})
	equal(t, "the proposal", events, []string{
		`proposal {"arguments":{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"},` +
			`"description":"Set the pool, the pool stands at 8.00 EUR, set it at 10.00 EUR, reason Allow two small verified follow-ups, it would stand at 10.00 EUR",` +
			`"expires":"EXPIRES","id":"TOKEN","pool":{"action":"set","eur":10,"from_eur":8,"to_eur":10,"reason":"Allow two small verified follow-ups"},` +
			`"tool":"set_pool"}`,
		`done {}`})
	if len(tokens) != 1 {
		t.Fatalf("the turn proposed %v", tokens)
	}
	if len(infra.contents) != 0 || len(infra.pulls) != 0 || len(infra.merges) != 0 {
		t.Fatalf("nothing writes before the confirmation: %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
	_, done, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":12}}`)
	equal(t, "the corrected Do it", done, ran("set_pool", `{"action":"set","eur":12,"reason":"Allow two small verified follow-ups"}`,
		"The pool of ai-software-factory-code is €12.00 now, was €8.00: sf/infra#12 carries the change and the policy in force holds it."))
	if got, _ := infra.contents[0].body["message"].(string); !strings.HasPrefix(got, "Project ai-software-factory-code: escalation pool 12.00 EUR\n") {
		t.Errorf("the commit's message = %q", got)
	}
	var content string
	if files, ok := infra.contents[0].body["files"].([]any); ok {
		if f, ok := files[0].(map[string]any); ok {
			content, _ = f["content"].(string)
		}
	}
	if raw, err := base64.StdEncoding.DecodeString(content); err != nil || !strings.Contains(string(raw), "\n    eur: 12\n") {
		t.Errorf("the project file the guarded writer committed holds %q (err %v)", content, err)
	}
	rows := r.events.all()
	e := rows[len(rows)-1]
	equal(t, "the event", []any{e.Kind, e.Actor}, []any{factoryd.EventPoolChanged, "sebastian"})
	equal(t, "the event's detail", e.Detail, map[string]any{"action": "set", "from_eur": 8.0, "confirmed_eur": 12.0, "to_eur": 12.0,
		"reason": "Allow two small verified follow-ups", "pr": "sf/infra#12"})

	// the typed pool press's outcomes: each refusal is a real HTTP status
	// with its exact wording and no refused write; a valid stale press
	// consumes the token and its replay is 404; pending 503 and repair 500
	// identify the already-merged PR, consume once and replay 404 with no
	// second PR
	head := "# the code project\nrepository: sf/code\nmerge_rule: human\nautonomy: assisted\nbudget:\n  month_eur: 10\n  task_max_eur: 4\nescalation:\n  pool:\n    eur: 12\n"
	tableHead := "# the code project\nrepository: sf/code\nmerge_rule: human\nautonomy: assisted\nbudget:\n  month_eur: 10\n  task_max_eur: 4\nescalation:\n  pool:\n    eur: 8\n"
	infra.mu.Lock()
	infra.files["factory/projects/ai-software-factory-code.yaml"] = head
	infra.shas = map[string]string{"factory/projects/ai-software-factory-code.yaml": "blob1"}
	infra.mu.Unlock()
	r.ledger.files["ai-software-factory-code"].Escalation.Pool.EUR = 12
	poolChatReload(r, 12)
	for _, row := range []struct {
		name   string
		seat   func(*testing.T)
		code   int
		body   string
		writes int // commits added by this press alone
	}{
		{"stale source 409", func(t *testing.T) {
			infra.mu.Lock()
			infra.shas["factory/projects/ai-software-factory-code.yaml"] = "blob2"
			infra.mu.Unlock()
		}, http.StatusConflict,
			"projects/ai-software-factory-code.yaml changed since the change was proposed (its blob is now blob2): read it again and propose the change anew",
			0},
		{"candidate refusal 422", func(t *testing.T) {
			infra.mu.Lock()
			infra.shas["factory/projects/ai-software-factory-code.yaml"] = "blob1"
			infra.mu.Unlock()
			r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) {
				return []string{"escalation: card_draw.eur 4 is above pool.eur 3"}, nil
			}
		}, http.StatusUnprocessableEntity,
			"The project file would not load: escalation: card_draw.eur 4 is above pool.eur 3",
			0},
		{"unreadable block 409", func(t *testing.T) {
			r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) { return nil, nil }
			infra.mu.Lock()
			infra.files["factory/projects/ai-software-factory-code.yaml"] = "# a broken block\nescalation: always\n"
			infra.mu.Unlock()
		}, http.StatusConflict,
			"projects/ai-software-factory-code.yaml holds an escalation block the pool cannot read: a policy PR corrects it",
			0},
		{"pending 503", func(t *testing.T) {
			r.c.cfg.PolicyReload = func(context.Context) error { return nil }
		}, http.StatusServiceUnavailable,
			"The pool change is merged as sf/infra#12, but the policy in force still holds the pool of ai-software-factory-code at EUR 8.00: the loader picks the merge up on its next poll. The pool_changed row is written; the new ceiling counts only once the policy holds it.",
			1},
		{"repair 500", func(t *testing.T) {
			r.c.cfg.PolicyReload = func(context.Context) error {
				r.ledger.files["ai-software-factory-code"].Escalation.Pool.EUR = 12
				return nil
			}
			r.events.fail = errors.New("the events store is down")
		}, http.StatusInternalServerError,
			"The pool change is merged as sf/infra#12, but its pool_changed row could not be written: the events store is down. The change stands: write the missing row (action set, from_eur 8.00, confirmed_eur 12.00, to_eur 12.00, reason \"Allow two small verified follow-ups\") and do not repeat the change.",
			1},
	} {
		t.Run(row.name, func(t *testing.T) {
			infra.mu.Lock()
			infra.files["factory/projects/ai-software-factory-code.yaml"] = tableHead
			infra.shas = map[string]string{"factory/projects/ai-software-factory-code.yaml": "blob1"}
			infra.mu.Unlock()
			r.ledger.files["ai-software-factory-code"].Escalation.Pool.EUR = 8
			r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) { return nil, nil }
			g.replies = [][]string{{call(1, "set_pool", `{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"}`)}}
			_, _, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool for the findings"}})
			row.seat(t)
			// a landed press commits, opens and merges one pull request;
			// a refused press touches nothing
			before, pullsBefore, mergesBefore := len(infra.contents), len(infra.pulls), len(infra.merges)
			if code, body, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":12}}`); code != row.code || body[0] != row.body {
				t.Fatalf("%s = %d %q, want %d %q", row.name, code, body, row.code, row.body)
			}
			if got, prs, mrg := len(infra.contents)-before, len(infra.pulls)-pullsBefore, len(infra.merges)-mergesBefore; got != row.writes || prs != row.writes || mrg != row.writes {
				t.Fatalf("%s wrote %d commits, %d pull requests, %d merges, want %d each", row.name, got, prs, mrg, row.writes)
			}
			pressed, pulls, merges := len(infra.contents), len(infra.pulls), len(infra.merges)
			if code, _, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil); code != http.StatusNotFound {
				t.Fatalf("%s replay = %d, want 404", row.name, code)
			}
			if len(infra.contents) != pressed || len(infra.pulls) != pulls || len(infra.merges) != merges {
				t.Fatalf("%s replay wrote again: %d commits, %d pull requests, %d merges, want %d, %d, %d",
					row.name, len(infra.contents), len(infra.pulls), len(infra.merges), pressed, pulls, merges)
			}
		})
	}
	r.events.fail = nil
}

// bindFailChat fails the current-conversation read of the Chat it wraps,
// so a press whose proposal holds no thread answers the read's refusal.
type bindFailChat struct{ Chat }

func (bindFailChat) CurrentConversation(context.Context, string, string) (int64, error) {
	return 0, errors.New("the store is down")
}

// The pool's confirmation is the member's press and only theirs: full
// auto and a custom confirm list that leaves set_pool out still propose
// the call instead of running it, a human who writes the project's code
// nothing gets the route's 403, another member's press is 403 and leaves
// the proposal for its owner, an expired or used one is 404, and a wrong
// correction — malformed, a forbidden field, an amount the pool cannot
// take, a press against another tool's proposal — is 400 with the
// proposal left usable. A revoked approver's and an unbindable thread's
// press write nothing. The row's actor is the pressing member's, from
// the session alone.
func TestPoolProposalAccess(t *testing.T) {
	r, g, infra := poolChatRig(t, 10, 3)
	members(r)
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: policy.MergeAuto,
		Autonomy: policy.AutonomyAutonomous, Budget: &policy.ProjectBudget{MonthEUR: 10}, Escalation: &policy.Escalation{Pool: policy.Draw{EUR: 10}}}
	r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], "anna")
	r.forgetWriters()
	poolChatReload(r, 11)
	setArgs := `{"action":"set","eur":10,"reason":"the findings cost money"}`

	// at full auto the call still proposes; nothing runs
	ck, s := memberLogin(t, r, "alice")
	g.replies = [][]string{{call(1, "set_pool", setArgs)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool for the findings"}})
	equal(t, "the proposal at full auto", events, []string{
		`proposal {"arguments":` + setArgs + `,"description":"Set the pool, the pool stands at 10.00 EUR, set it at 10.00 EUR, reason the findings cost money, it would stand at 10.00 EUR",` +
			`"expires":"EXPIRES","id":"TOKEN","pool":{"action":"set","eur":10,"from_eur":10,"to_eur":10,"reason":"the findings cost money"},` +
			`"tool":"set_pool"}`,
		`done {}`})

	// a custom confirm list that leaves set_pool out proposes it too: the
	// member alone decides the pool's money
	req := httptest.NewRequest(http.MethodPost, chatPath, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	custom := &turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, owner: "alice",
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, cfg: policy.ChatConfig{Confirm: []string{}}}
	custom.stream(func(string) error { return nil }, nil)
	result, customToken, customCode := custom.call(factoryd.OperatorCall{ID: "call_1", Name: "set_pool", Arguments: setArgs})
	if customToken == "" || customCode != 0 || result != "Proposed to alice: Set the pool, the pool stands at 10.00 EUR, set it at 10.00 EUR, reason the findings cost money, it would stand at 10.00 EUR. Nothing runs until they press Do it." {
		t.Fatalf("a custom confirm list that omits set_pool: %q %q %d", result, customToken, customCode)
	}

	// a member who writes the project's code nothing does not even reach
	// the proposal
	bk, bs := memberLogin(t, r, "bob")
	if code, body, _ := chatPostRaw(t, r, bk, bs, chatPath+"/proposals/"+customToken, `{"correction":{"eur":11}}`); code != http.StatusForbidden ||
		body[0] != "you write no project of ai-software-factory-code" {
		t.Fatalf("bob's corrected Do it = %d %q, want 403", code, body)
	}

	// another member's press is 403 and leaves the proposal for its owner
	ak, as := memberLogin(t, r, "anna")
	if code, body, _ := chatPost(t, r, ak, as, chatPath+"/proposals/"+customToken, nil); code != http.StatusForbidden ||
		body[0] != "It is another human's." {
		t.Fatalf("anna's Do it of alice's proposal = %d %q, want 403", code, body)
	}
	if p, ok := r.chat.proposal(customToken); !ok || !p.used.IsZero() {
		t.Fatalf("anna's press spent alice's proposal: %+v", p)
	}
	if code, _, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+customToken, `{"correction":{"eur":11}}`); code != http.StatusOK {
		t.Fatalf("alice's own corrected Do it after anna's = %d, want 200", code)
	}
	if code, _, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+customToken, nil); code != http.StatusNotFound {
		t.Fatalf("the used proposal's replay = %d, want 404", code)
	}

	// an expired proposal is gone the same way
	g.replies = [][]string{{call(1, "set_pool", setArgs)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool again"}})
	expired := tokens[0]
	r.chat.mu.Lock()
	for _, p := range r.chat.proposals {
		if p.Token == expired {
			p.ExpiresAt = time.Now().Add(-time.Second)
		}
	}
	r.chat.mu.Unlock()
	if code, _, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+expired, nil); code != http.StatusNotFound {
		t.Fatalf("the expired proposal's press = %d, want 404", code)
	}

	// a wrong correction is 400 with its exact words and leaves the
	// proposal usable
	g.replies = [][]string{{call(1, "set_pool", setArgs)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool once more"}})
	wrong := []struct {
		body string
		want string
	}{
		{`{"correction":{"eur":"12"}}`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"correction":null}`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"correction":{"eur":12,"reason":"x"}}`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"correction":{"eur":12,"actor":"eve"}}`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"correction":{"eur":12,"eur":13}}`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"correction":{"eur":12}} trailing`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"eur":12}`, `The correction is one JSON object {"correction":{"eur":12}} and nothing else.`},
		{`{"correction":{"eur":1e999}}`, "a pool change's amount is a finite number in EUR"},
		{`{"correction":{"eur":12.005}}`, "a pool change's amount is whole cents"},
		{`{"correction":{"eur":-3}}`, "a set takes an amount above 0 EUR: a pool of 0 takes the policy's figure"},
		{`{"correction":{"eur":900000000001}}`, "a pool change's amount is too large"},
		{strings.Repeat(" ", 8<<10) + `{"correction":{"eur":12}}`, "The correction's body is over 8192 bytes; press the proposal again."},
	}
	for _, w := range wrong {
		if code, body, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], w.body); code != http.StatusBadRequest || body[0] != w.want {
			t.Errorf("the correction %s = %d %q, want 400 %q", w.body, code, body, w.want)
		}
	}
	if p, ok := r.chat.proposal(tokens[0]); !ok || !p.used.IsZero() {
		t.Fatalf("the wrong corrections spent the proposal: %+v", p)
	}
	if code, _, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":11}}`); code != http.StatusOK {
		t.Fatalf("the proposal's own corrected Do it after the wrong ones = %d, want 200", code)
	}
	rows := r.events.all()
	e := rows[len(rows)-1]
	equal(t, "the event", []any{e.Kind, e.Actor}, []any{factoryd.EventPoolChanged, "alice"})

	// a press against another tool's proposal corrects nothing
	g.replies = [][]string{{call(1, "set_autonomy", `{"autonomy":"assisted"}`)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"go assisted"}})
	if code, body, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":11}}`); code != http.StatusBadRequest ||
		body[0] != "A set_autonomy proposal takes no correction: only the pool's amount may be corrected." {
		t.Fatalf("the correction of a set_autonomy proposal = %d %q, want 400", code, body)
	}

	// a revoked approver's press answers a real pre-stream 403 with zero
	// writes: the authority the proposal-time admission granted was
	// revoked since, and the typed press refuses it before anything runs
	g.replies = [][]string{{call(1, "set_pool", setArgs)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool once more"}})
	r.ledger.approvers = nil
	if code, body, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":11}}`); code != http.StatusForbidden ||
		body[0] != "Only the approvers of project ai-software-factory-code may change the escalation pool." {
		t.Fatalf("the revoked approver's press = %d %q, want 403", code, body)
	}

	// a press whose thread cannot bind writes nothing: the bind runs
	// before the press's write, so the store's refusal answers the press
	// and the pool stands until a new proposal
	r.ledger.approvers = []string{"alice"}
	g.replies = [][]string{{call(1, "set_pool", setArgs)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool once more"}})
	r.chat.mu.Lock()
	for _, p := range r.chat.proposals {
		if p.Token == tokens[0] {
			p.Conversation = 0
		}
	}
	r.chat.mu.Unlock()
	plain, commits, pulls, merges := r.c.cfg.Chat, len(infra.contents), len(infra.pulls), len(infra.merges)
	r.c.cfg.Chat = bindFailChat{plain}
	code, body, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":11}}`)
	r.c.cfg.Chat = plain
	if code != http.StatusInternalServerError || body[0] != "cannot read the conversation" {
		t.Fatalf("the press of a thread that cannot bind = %d %q, want 500", code, body)
	}
	if len(infra.contents) != commits || len(infra.pulls) != pulls || len(infra.merges) != merges {
		t.Fatalf("the press of a thread that cannot bind wrote: %d commits, %d pull requests, %d merges, want %d, %d, %d",
			len(infra.contents), len(infra.pulls), len(infra.merges), commits, pulls, merges)
	}
	if len(infra.contents) != 2 || len(infra.pulls) != 2 || len(infra.merges) != 2 {
		t.Fatalf("the refusals aside, %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
}

// A corrected top-up grows the standing pool by the corrected amount
// alone and the month's draws stand; a project file that moved since the
// proposal answers 409 without a write — the attempt was valid, its token
// is spent — and the replay is 404. An ask's bound pool choice runs the
// same captured change on one press: the binding stores the offer's
// capture — operation, reason, source and pool — and the press confirms
// it in the original conversation, no second proposal, no model turn, no
// fresh read; a file that moved since the offer answers 409 with no
// rebase, its attempt spent, and the replay is 404.
func TestPoolProposalTopUp(t *testing.T) {
	r, g, infra := poolChatRig(t, 10, 3)
	poolChatReload(r, 14)
	ck, s := r.loggedIn(t)
	args := `{"action":"top_up","eur":5,"reason":"a second finding"}`
	g.replies = [][]string{{call(1, "set_pool", args)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"top the pool up"}})
	equal(t, "the proposal", events, []string{
		`proposal {"arguments":` + args + `,` +
			`"description":"Set the pool, the pool stands at 10.00 EUR, top it up by 5.00 EUR, reason a second finding, it would stand at 15.00 EUR",` +
			`"expires":"EXPIRES","id":"TOKEN","pool":{"action":"top_up","eur":5,"from_eur":10,"to_eur":15,"reason":"a second finding"},` +
			`"tool":"set_pool"}`,
		`done {}`})
	_, done, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":4}}`)
	equal(t, "the corrected top-up", done, ran("set_pool", `{"action":"top_up","eur":4,"reason":"a second finding"}`,
		"The pool of ai-software-factory-code is €14.00 now, was €10.00: sf/infra#12 carries the change and the policy in force holds it."))
	rows := r.events.all()
	e := rows[len(rows)-1]
	equal(t, "the event's detail", e.Detail, map[string]any{"action": "top_up", "from_eur": 10.0, "confirmed_eur": 4.0, "to_eur": 14.0,
		"reason": "a second finding", "pr": "sf/infra#12"})
	if pl := r.c.ProjectPool(t.Context(), mustPolicy(t, r), factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, time.Now()); pl.Drawn != (policy.Draw{EUR: 3}) || pl.Pool.EUR != 14 {
		t.Errorf("the pool after the top-up = %+v, want 3 EUR drawn of 14", pl)
	}

	// a project file that moved since the proposal refuses without a
	// write and consumes the token
	g.replies = [][]string{{call(1, "set_pool", args)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"top the pool up again"}})
	infra.shas = map[string]string{"factory/projects/ai-software-factory-code.yaml": "blob2"}
	if code, body, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":4}}`); code != http.StatusConflict ||
		body[0] != "projects/ai-software-factory-code.yaml changed since the change was proposed (its blob is now blob2): read it again and propose the change anew" {
		t.Fatalf("the changed file's Do it = %d %q, want 409", code, body)
	}
	if len(infra.contents) != 1 || len(infra.pulls) != 1 || len(infra.merges) != 1 {
		t.Fatalf("the stale proposal wrote: %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
	if code, _, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil); code != http.StatusNotFound {
		t.Fatalf("the stale attempt's replay = %d, want 404", code)
	}

	// the ask-bound pool choice carries the offer's capture: one press
	// of it confirms the change in the original conversation, as the
	// member's own session, and the row names them
	bound := `{"ask":"Top the escalation pool up by 4 EUR?","choices":["top it up by 4 EUR","hold"],"tool":"set_pool","args":{"action":"top_up","eur":4,"reason":"a second finding"},"choice":"top it up by 4 EUR"}`
	askRow := func() (id int64, token string) {
		t.Helper()
		rows, err := r.chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rows {
			if m.Kind == factoryd.MessageAsk {
				id, token = m.ID, m.Proposal
			}
		}
		if id == 0 || token == "" {
			t.Fatalf("no bound ask row among %d rows", len(rows))
		}
		return
	}
	g.replies = [][]string{{call(1, "ask", bound)}}
	_, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"offer the top-up for my press"}})
	equal(t, "the bound ask", events, []string{
		`tool {"arguments":` + bound + `,"duration_ms":0,"name":"ask","result":"Asked the human; this turn ends. Pressing “top it up by 4 EUR” runs Set the pool, the pool stands at 10.00 EUR, top it up by 4.00 EUR, reason a second finding, it would stand at 14.00 EUR; another press answers it."}`,
		`done {}`})
	ask, token := askRow()
	if p, ok := r.chat.proposal(token); !ok || p.Choice != "top it up by 4 EUR" || p.Login != "sebastian" || p.Conversation != 1 ||
		p.Pool == nil || p.Pool.Action != poolTopUp || p.Pool.Reason != "a second finding" ||
		p.Pool.Source != "blob2" || p.Pool.From != 1000 || p.Pool.EUR != 4 {
		t.Fatalf("the ask binding's capture = %+v", p)
	}
	g.replies = nil
	code, events, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(ask, 10)}, "choice": {"top it up by 4 EUR"}})
	if code != http.StatusOK {
		t.Fatalf("the bound press = %d: %v", code, events)
	}
	const landed = "The pool of ai-software-factory-code is €14.00 now, was €10.00: sf/infra#12 carries the change and the policy in force holds it."
	equal(t, "the one-click confirmation", events, []string{
		`tool {"arguments":{"action":"top_up","eur":4,"reason":"a second finding"},"duration_ms":0,"name":"set_pool","result":"` + landed + `"}`,
		`applied {"result":"` + landed + `","tool":"set_pool"}`, `done {}`})
	if n := len(g.calls()); n != 3 {
		t.Fatalf("%d model calls, want the offers' alone: the press confirmed the change in code", n)
	}
	if got, _ := infra.contents[1].body["message"].(string); !strings.HasPrefix(got, "Project ai-software-factory-code: escalation pool +4.00 EUR\n") {
		t.Errorf("the press's commit message = %q", got)
	}
	rows = r.events.all()
	e = rows[len(rows)-1]
	equal(t, "the press's row", []any{e.Kind, e.Actor}, []any{factoryd.EventPoolChanged, "sebastian"})
	equal(t, "the press's detail", e.Detail, map[string]any{"action": "top_up", "from_eur": 10.0, "confirmed_eur": 4.0, "to_eur": 14.0,
		"reason": "a second finding", "pr": "sf/infra#12"})

	// a bound offer whose file moved since the offer answers 409 before
	// the stream, nothing written and nothing rebased onto the moved
	// file, the attempt valid and its token spent; the replay is 404
	g.replies = [][]string{{call(1, "ask", bound)}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"offer the top-up once more"}})
	ask, token = askRow()
	infra.shas = map[string]string{"factory/projects/ai-software-factory-code.yaml": "blob3"}
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(ask, 10)}, "choice": {"top it up by 4 EUR"}})
	equal(t, "the stale bound press", []any{code, body[0]}, []any{http.StatusConflict,
		"projects/ai-software-factory-code.yaml changed since the change was proposed (its blob is now blob3): read it again and propose the change anew"})
	if len(infra.contents) != 2 || len(infra.pulls) != 2 || len(infra.merges) != 2 {
		t.Fatalf("the stale bound press wrote: %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
	code, body, _ = chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(ask, 10)}, "choice": {"top it up by 4 EUR"}})
	equal(t, "the stale attempt's replay", []any{code, body[0]}, []any{http.StatusNotFound, "There is no such proposal or undo; it may have expired."})

	// a file that moves in the press's own gap — after the pre-stream
	// guard read it, before the writer's locked recheck — answers the
	// same 409 before the stream, never an SSE refusal: the attempt is
	// valid, its token spent, nothing is rebased onto the moved file
	// and nothing written; the replay is 404
	g.replies = [][]string{{call(1, "ask", bound)}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"offer the top-up once more, between the guard and the write"}})
	ask, token = askRow()
	g.replies = nil
	infra.mu.Lock()
	guard := len(infra.reads)
	infra.shaOnRead = func(n int) string {
		if n > guard+1 {
			return "blob4"
		}
		return ""
	}
	infra.mu.Unlock()
	code, body, _ = chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(ask, 10)}, "choice": {"top it up by 4 EUR"}})
	equal(t, "the press in the guard's gap", []any{code, body[0]}, []any{http.StatusConflict,
		"projects/ai-software-factory-code.yaml changed since the change was proposed (its blob is now blob4): read it again and propose the change anew"})
	if len(infra.contents) != 2 || len(infra.pulls) != 2 || len(infra.merges) != 2 {
		t.Fatalf("the press in the gap wrote: %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
	if n := len(g.calls()); n != 5 {
		t.Fatalf("%d model calls after the gap's refusal, want the offers' alone", n)
	}
	code, body, _ = chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(ask, 10)}, "choice": {"top it up by 4 EUR"}})
	equal(t, "the gap attempt's replay", []any{code, body[0]}, []any{http.StatusNotFound, "There is no such proposal or undo; it may have expired."})
}

// A rung on new_card reaches the issue as a Rung line with its reason
// (roadmap 222, item 5); a rung without a reason is refused.
func TestChatNewCardRung(t *testing.T) {
	r, g := newWritesRig(t)
	args := `{"title":"Cache metrics","body":"Count hits.\n\nAcceptance:\n- The metrics work","track":"direct","track_reason":"small and clear","size":"small","rung":"frontier","rung_reason":"touches the queue"}`
	_, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The metrics work\n\nSize: small\nRung: frontier, touches the queue")
	if _, _, err := r.c.doFileCard(t.Context(), actor{}, "code", "x", "y", "direct", "", "", "", "cheap", "", "", 0, 0); err == nil {
		t.Fatal("a rung without a reason was filed")
	}
}

// Close of a failed card with an issue closes the issue alone: intake ends
// the card, so CloseCard is not called.
func TestCloseAFailedCardWithAnIssue(t *testing.T) {
	r, g := newWritesRig(t)
	args := `{"task_id":"t-f"}`
	_, done, _ := proposeAndDo(t, r, g, "close", args)
	equal(t, "Do it", done, ran("close", args, "Closed #31; factoryd ends the card."))
	equal(t, "the patch", r.fg.patched, []string{`/api/v1/repos/sf/code/issues/31 {"state":"closed"}`})
	equal(t, "CloseCard", r.closed, []string(nil))
}

// A done plan whose newest revision leaves parts to later rounds is
// between rounds: the face offers a writer Close behind a failed card's
// confirm, and Close closes the plan's issue alone, which intake turns
// into the card's end. A done plan in its last round offers none, and its
// close is refused as any final card's.
func TestCloseAPlanBetweenRounds(t *testing.T) {
	r, g := newWritesRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-br", Kind: "agent", Status: "done", Track: policy.TrackPlan, Repository: "sf/code", IssueNumber: 50,
			Objective: "Build the stats."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-lr", Kind: "agent", Status: "done", Track: policy.TrackPlan, Repository: "sf/code", IssueNumber: 51,
			Objective: "Build the links."}})
	if r.tasks.plans == nil {
		r.tasks.plans = map[string][]factoryd.PlanRevision{}
	}
	r.tasks.plans["t-br"] = []factoryd.PlanRevision{{TaskID: "t-br", Revision: 1, Plan: json.RawMessage(`{"round": 1, "outline": ["the console"]}`)}}
	r.tasks.plans["t-lr"] = []factoryd.PlanRevision{{TaskID: "t-lr", Revision: 1, Plan: json.RawMessage(`{"round": 2}`)}}
	r.tasks.mu.Unlock()
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	closeForm := `<form method="post" action="/tasks/t-br/close" hx-post="/tasks/t-br/close" ` +
		`hx-confirm="Close #50? Its issue closes as you, and the card ends." class="inline close" data-to="closed">`
	if face := faceOf(t, body, "t-br"); !strings.Contains(face, closeForm) {
		t.Errorf("the plan between rounds' face =\n%s\nwant it to hold\n%s", face, closeForm)
	}
	if face := faceOf(t, body, "t-lr"); strings.Contains(face, `action="/tasks/t-lr/close"`) {
		t.Errorf("the plan in its last round's face =\n%s\nwant no Close", face)
	}
	rec := r.do(post("/tasks/t-br/close", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	equal(t, "the button", rec.Code, http.StatusSeeOther)
	equal(t, "the patch", r.fg.patched, []string{`/api/v1/repos/sf/code/issues/50 {"state":"closed"}`})
	equal(t, "CloseCard", r.closed, []string(nil))
	_, done, _ := proposeAndDo(t, r, g, "close", `{"task_id":"t-br"}`)
	equal(t, "Do it", done, ran("close", `{"task_id":"t-br"}`, "Closed #50; factoryd ends the card."))
	code, msg := refused(t, r, "close", toolInput{TaskID: "t-lr"})
	equal(t, "close of the plan in its last round", []any{code, msg},
		[]any{http.StatusConflict, "Card #51 is done, not a card that is not final: the card is final: a new card continues the work."})
}

// The face offers a writer Close on a card waiting on a human, behind a
// confirm, and on a failed one, and on no queued, running, done or closed card; the answer
// form of a card at its session cap says what the answer does, and its
// answer is a grant: one session_granted row via button.
func TestCardFaceCloseAndGrant(t *testing.T) {
	r, _ := newWritesRig(t)
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	closeForm := `<form method="post" action="/tasks/t-plan/close" hx-post="/tasks/t-plan/close" ` +
		`hx-confirm="Close #40? Its issue closes as you, and the card ends." class="inline close" data-to="closed">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code">` +
		`<input type="hidden" name="view" value="wide">` +
		`<button type="submit" class="deny" aria-label="Close #40">Close</button></form>`
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, closeForm) {
		t.Errorf("the waiting card's face =\n%s\nwant it to hold\n%s", face, closeForm)
	}
	for id, closes := range map[string]bool{"t-e": true, "t-cap": true, "t-a": false, "t-r": false, "t-done": false, "t-f": true} {
		if got := strings.Contains(faceOf(t, body, id), `action="/tasks/`+id+`/close"`); got != closes {
			t.Errorf("card %s offers Close: %t, want %t", id, got, closes)
		}
	}
	if esc := escalationOf(t, body, "t-cap"); !strings.Contains(esc, `<button type="submit">Grant one session with this answer</button>`) {
		t.Errorf("the capped card's answer form =\n%s\nwant it to grant one session", esc)
	}
	if esc := escalationOf(t, body, "t-e"); !strings.Contains(esc, `<button type="submit">Answer</button>`) {
		t.Errorf("the question's answer form =\n%s\nwant Answer", esc)
	}
	before := r.events.count()
	rec := r.do(post("/tasks/t-cap/answer", url.Values{"csrf": {s.csrf}, "answer": {"Trim to 1 MB first."}}), ck)
	equal(t, "the grant", rec.Code, http.StatusSeeOther)
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/code/issues/14/comments"], "Trim to 1 MB first.")
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventSessionGranted, repo: "sf/code", taskID: "t-cap",
		detail: map[string]any{"action": factoryd.ActExtend, "case_id": int64(1), "answer": "Trim to 1 MB first.", "comment": int64(1)}})
}

// A card without an issue closes through factoryd's CloseCard alone
// (roadmap 170), by the chat and the face's POST: one card_closed row each,
// no Forgejo call. The face offers Close on a failed one.
func TestCloseACardWithoutAnIssue(t *testing.T) {
	r, g := newWritesRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-n", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Repository: "sf/code", Objective: "Tidy the docs."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-nf", Kind: "agent", Status: "failed", Repository: "sf/code", Objective: "Write it up."}})
	r.tasks.mu.Unlock()
	before := r.events.count()
	args := `{"task_id":"t-n"}`
	_, done, _ := proposeAndDo(t, r, g, "close", args)
	equal(t, "Do it", done, ran("close", args, "Closed card t-n."))
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-n", Kind: factoryd.EventCardClosed, Detail: map[string]any{"note": ""}})

	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	if face := faceOf(t, body, "t-nf"); !strings.Contains(face, `hx-confirm="Close t-nf? The card ends."`) || !strings.Contains(face, `aria-label="Close t-nf"`) {
		t.Errorf("the failed card without an issue's face =\n%s\nwant Close", face)
	}
	before = r.events.count()
	rec := r.do(post("/tasks/t-nf/close", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	equal(t, "the button", rec.Code, http.StatusSeeOther)
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-nf", detail: map[string]any{"note": ""}})
	equal(t, "CloseCard", r.closed, []string{"t-n by sebastian", "t-nf by sebastian"})
	equal(t, "Forgejo", writesOf(r.fg), []string(nil))
}

// A card without an issue waiting on a human (a report task after six
// provider refusals) offers no answer, which has no issue to go on: Close
// ends it, and an answer's refusal says so, as for a failed one.
func TestAWaitingCardWithoutAnIssueOffersClose(t *testing.T) {
	r, _ := newWritesRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-nw", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
			Objective: "Write it up."}, Escalation: "the model provider refused the first call of 6 sessions in a row (status 429): busy"},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-nf", Kind: "agent", Status: "failed", Repository: "sf/code", Objective: "Write it up."}})
	r.tasks.mu.Unlock()
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	if esc := escalationOf(t, body, "t-nw"); strings.Contains(esc, `action="/tasks/t-nw/answer"`) || !strings.Contains(esc, `action="/tasks/t-nw/close"`) {
		t.Errorf("the waiting card without an issue =\n%s\nwant Close and no answer form", esc)
	}
	for id, state := range map[string]string{"t-nw": "waits on a human: an answer, an approval or a decision", "t-nf": "has failed"} {
		code, msg := refused(t, r, "answer", toolInput{TaskID: id, Text: "go on"})
		equal(t, "answer "+id, []any{code, msg}, []any{http.StatusConflict, "Card #0 " + state + ", not a question or a session cap to answer: close ends it (close)."})
	}
}

// A write on a card of a project without agents, such as an answer on
// the infra project's human card, is judged by Forgejo's writers of its
// repository, as on any project: the console's writer lists name only the
// agents' projects and the factory's own.
func TestWritesOnAProjectWithoutAgents(t *testing.T) {
	r, _ := newWritesRig(t)
	r.c.cfg.Writers = func(context.Context) (string, map[string][]string, error) {
		return "sf/code", map[string][]string{"sf/code": {"sebastian"}}, nil
	}
	r.repoWriters["sf/infra"] = []string{"sebastian"}
	r.forgetWriters()
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-human",
		Repository: "sf/infra", IssueNumber: 5, Objective: "The configuration was rejected."}})
	r.tasks.mu.Unlock()
	ck, s := r.loggedIn(t)
	answer := func() int {
		return r.do(post("/tasks/t-h/answer", url.Values{"csrf": {s.csrf}, "answer": {"fixed in infra"}, "project": {"ai-software-factory-infra"}}), ck).Code
	}
	equal(t, "answer on sf/infra", answer(), http.StatusSeeOther)
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/infra/issues/5/comments"], "fixed in infra")
	r.repoWriters["sf/infra"] = nil
	r.forgetWriters()
	equal(t, "answer by a non-writer of sf/infra", answer(), http.StatusForbidden)
}
