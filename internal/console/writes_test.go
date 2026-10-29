package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
		`proposal {"arguments":` + args + `,"description":"Close a card, #5 t-m \"Fix the cache. It drops entries.\", note Superseded by #7.","id":"TOKEN","tool":"close"}`,
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
		`proposal {"arguments":` + args + `,"description":"Edit a card, #5 t-m \"Fix the cache. It drops entries.\", title Fix the cache eviction","id":"TOKEN","tool":"edit"}`,
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

// grant_session proposes on the card at its session cap; Do it posts the
// member's answer on its issue, the comment that grants the session, with
// one session_granted row via chat naming it. A card with a question is
// no card at its cap: the refusal names the answer tool.
func TestChatGrantSession(t *testing.T) {
	r, g := newWritesRig(t)
	var whole []string
	r.c.cfg.AnswerWhole = func(ctx context.Context, taskID string, comment int64, by string) error {
		whole = append(whole, fmt.Sprintf("%s %d %s", taskID, comment, by))
		return nil
	}
	before := r.events.count()
	args := `{"task_id":"t-cap","text":"Trim to 1 MB first."}`
	proposed, done, _ := proposeAndDo(t, r, g, "grant_session", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Grant a session, #14 t-cap \"Trim the logs.\", text Trim to 1 MB first.","id":"TOKEN","tool":"grant_session"}`,
		`done {}`})
	equal(t, "Do it", done, ran("grant_session", args, "Your answer is on #14; it grants the card one session."))
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/code/issues/14/comments"], "Trim to 1 MB first.")
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-cap", IssueNumber: 14, Kind: factoryd.EventSessionGranted, Detail: postedDetail(1)})
	equal(t, "the whole signal", whole, []string{"t-cap 1 sebastian"})

	code, msg := refused(t, r, "grant_session", toolInput{TaskID: "t-e", Text: "yes"})
	equal(t, "grant to a question", []any{code, msg}, []any{http.StatusConflict,
		"Card #13 waits on a human: an answer, an approval or a decision, not a card at its session cap: answer it (answer)."})
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
		`proposal {"arguments":` + args + `,"description":"Set a dependency, #22 t-b \"Add the web UI.\", depends on #21 t-a \"Add the API.\"","id":"TOKEN","tool":"set_dependency"}`,
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
		`proposal {"arguments":` + args + `,"description":"Set a dependency, #22 t-b \"Add the web UI.\", no longer depends on #21 t-a \"Add the API.\"","id":"TOKEN","tool":"set_dependency"}`,
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
		`proposal {"arguments":` + args + `,"description":"File a card, title Cache metrics, body Count hits., track direct, track_reason one small change, size large, acceptance The hits count","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The hits count\n\nSize: large")
	equal(t, "the issue's labels", fmt.Sprint(r.fg.issues[0]["labels"]), "[1]")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "reason": "one small change", "title": "Cache metrics", "size": "large", "thread": int64(1)}})
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
		`proposal {"arguments":` + args + `,"description":"File a card, title Re-land the feed fix, body Carry pull request #9 to green checks., track direct, track_reason the fix is known, size small, carries #9, acceptance The checks are green","id":"TOKEN","tool":"new_card"}`,
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
		`proposal {"arguments":{"autonomy":"assisted"},"description":"Set the autonomy level, autonomy assisted, queue cap 5 stays","id":"TOKEN","tool":"set_autonomy"}`,
		`proposal {"arguments":{"autonomy":"assisted","queue_cap":3},"description":"Set the autonomy level, autonomy assisted, queue cap 3","id":"TOKEN","tool":"set_autonomy"}`,
		`done {}`})
}

// A rung on new_card reaches the issue as a Rung line with its reason
// (roadmap 222, item 5); a rung without a reason is refused.
func TestChatNewCardRung(t *testing.T) {
	r, g := newWritesRig(t)
	args := `{"title":"Cache metrics","body":"Count hits.\n\nAcceptance:\n- The metrics work","track":"direct","track_reason":"small and clear","size":"small","rung":"frontier","rung_reason":"touches the queue"}`
	_, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The metrics work\n\nSize: small\nRung: frontier, touches the queue")
	if _, _, err := r.c.doFileCard(t.Context(), actor{}, "code", "x", "y", "direct", "", "", "", "cheap", "", "", 0); err == nil {
		t.Fatal("a rung without a reason was filed")
	}
}

// The face offers a writer Close on a card waiting on a human, behind a
// confirm, and on no queued, running or final card; the answer
// form of a card at its session cap says what the answer does, and its
// answer is a grant: one session_granted row via button.
func TestCardFaceCloseAndGrant(t *testing.T) {
	r, _ := newWritesRig(t)
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	closeForm := `<form method="post" action="/tasks/t-plan/close" hx-post="/tasks/t-plan/close" hx-target="#board" hx-swap="outerHTML" ` +
		`hx-confirm="Close #40? Its issue closes as you, and the card ends." class="inline close" data-to="closed">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code">` +
		`<input type="hidden" name="view" value="wide">` +
		`<button type="submit" class="deny" aria-label="Close #40">Close</button></form>`
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, closeForm) {
		t.Errorf("the waiting card's face =\n%s\nwant it to hold\n%s", face, closeForm)
	}
	for id, closes := range map[string]bool{"t-e": true, "t-cap": true, "t-a": false, "t-r": false, "t-done": false, "t-f": false} {
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
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventSessionGranted, repo: "sf/code", taskID: "t-cap", detail: postedDetail(1)})
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
