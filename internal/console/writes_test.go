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
// block.
func newWritesRig(t *testing.T) (*rig, *chatGateway) {
	r, _, g := newChatRig(t)
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-cap", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14,
			Objective: "Trim the logs.", Usage: factoryd.TaskUsage{Attempts: 3}},
			Escalation: "the session cap is reached: every finding sent to the agent so far goes to a human\nlast: the checks failed"},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 7,
			Objective: "Add the schema."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-f", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 31,
			Objective: "Add the cache."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-a", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "spec",
			Repository: "sf/code", IssueNumber: 21, Objective: "Add the API."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-b", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "spec",
			Repository: "sf/code", IssueNumber: 22, Objective: "Add the web UI."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: policy.LanePlan, Process: "spec",
			Repository: "sf/code", IssueNumber: 40, Objective: "Plan the shortener."}})
	return r, g
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

// new_card takes a size: the proposal names it, and Do it files the issue
// with its body ending in the size line.
func TestChatNewCardSize(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	args := `{"title":"Cache metrics","body":"Count hits.","lane":"backlog","size":"large"}`
	proposed, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"File a card, title Cache metrics, body Count hits., lane backlog, size large","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 in lane backlog."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nSize: large")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"lane": "backlog", "title": "Cache metrics", "size": "large", "thread": int64(1)}})
}

// new_card's lane defaults to backlog: a call without one is proposed
// with it and files the card there.
func TestChatNewCardLaneDefaultsToBacklog(t *testing.T) {
	r, g := newWritesRig(t)
	before := r.events.count()
	stored := `{"lane":"backlog","title":"Cache metrics"}`
	proposed, done, _ := proposeAndDo(t, r, g, "new_card", `{"title":"Cache metrics"}`)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + stored + `,"description":"File a card, title Cache metrics, lane backlog","id":"TOKEN","tool":"new_card"}`, `done {}`})
	equal(t, "Do it", done, ran("new_card", stored, "Filed sf/code#41 in lane backlog."))
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"lane": "backlog", "title": "Cache metrics", "thread": int64(1)}})
}

// set_autonomy without a backlog cap keeps the project's: its proposal
// names the cap that stays, 5 from the project file, and carries none;
// with a cap it names the new one.
func TestChatSetAutonomyKeepsTheCap(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", BacklogCap: 5}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "set_autonomy", `{"autonomy":"assisted"}`, "set_autonomy", `{"autonomy":"assisted","backlog_cap":3}`)}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"go assisted"}})
	equal(t, "proposals", events, []string{
		`proposal {"arguments":{"autonomy":"assisted"},"description":"Set the autonomy level, autonomy assisted, backlog cap 5 stays","id":"TOKEN","tool":"set_autonomy"}`,
		`proposal {"arguments":{"autonomy":"assisted","backlog_cap":3},"description":"Set the autonomy level, autonomy assisted, backlog cap 3","id":"TOKEN","tool":"set_autonomy"}`,
		`done {}`})
}

// A rung on new_card reaches the issue as a Rung line with its reason
// (roadmap 222, item 5); a rung without a reason is refused.
func TestChatNewCardRung(t *testing.T) {
	r, g := newWritesRig(t)
	args := `{"title":"Cache metrics","body":"Count hits.\n\nAcceptance:\n- The metrics work","lane":"ready","size":"small","rung":"frontier","rung_reason":"touches the queue"}`
	_, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 in lane ready."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The metrics work\n\nSize: small\nRung: frontier, touches the queue")
	if _, _, err := r.c.doFileCard(t.Context(), actor{}, "code", "x", "y", "ready", "", "", "cheap", ""); err == nil {
		t.Fatal("a rung without a reason was filed")
	}
}

// The face offers a writer Close on a card in the backlog or waiting on a
// human, behind a confirm, and on no running or final card; the answer
// form of a card at its session cap says what the answer does, and its
// answer is a grant: one session_granted row via button.
func TestCardFaceCloseAndGrant(t *testing.T) {
	r, _ := newWritesRig(t)
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	closeForm := `<form method="post" action="/tasks/t-a/close" hx-post="/tasks/t-a/close" hx-target="#board" hx-swap="outerHTML" ` +
		`hx-confirm="Close #21? Its issue closes as you, and the card ends." class="inline close" data-to="closed">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code">` +
		`<input type="hidden" name="view" value="wide">` +
		`<button type="submit" class="deny" aria-label="Close #21">Close</button></form>`
	if face := faceOf(t, body, "t-a"); !strings.Contains(face, closeForm) {
		t.Errorf("the backlog card's face =\n%s\nwant it to hold\n%s", face, closeForm)
	}
	for id, closes := range map[string]bool{"t-e": true, "t-cap": true, "t-r": false, "t-done": false, "t-f": false} {
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
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-n", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Repository: "sf/code", Objective: "Tidy the docs."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-nf", Kind: "agent", Status: "failed", Repository: "sf/code", Source: factoryd.SourcePresort, Objective: "Write it up."}})
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
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-nw", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Source: factoryd.SourcePresort,
			Objective: "Write it up."}, Escalation: "the model provider refused the first call of 6 sessions in a row (status 429): busy"},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-nf", Kind: "agent", Status: "failed", Repository: "sf/code", Source: factoryd.SourcePresort, Objective: "Write it up."}})
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
