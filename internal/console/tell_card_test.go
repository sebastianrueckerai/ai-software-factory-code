package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tellRig is the chat's rig with a fourth card of the code project: #15
// t-g in review at its approval gate, holding pending merge request 6 of
// pull request #12. It answers the rig, its gateway and the number of
// event rows it starts with.
func tellRig(t *testing.T) (*rig, *chatGateway, int) {
	t.Helper()
	r, _, g := newChatRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-g", Kind: "agent", Status: "in-review",
		Gate: factoryd.GateApproval, Lane: policy.LaneReady, Process: "maintenance", Repository: "sf/code", IssueNumber: 15,
		PRNumber: 12, Objective: "Cache the board."}})
	r.tasks.mu.Unlock()
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-x", Kind: "agent", Status: "done",
		Repository: "sf/code", IssueNumber: 9, Objective: "Done work."}})
	r.tasks.mu.Unlock()
	r.approvals.mu.Lock()
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-g",
		Project: "sf/code", PRNumber: 12, Status: "pending", Spec: json.RawMessage(`{"head_sha":"c0ffee1","risk_class":"low"}`)})
	r.approvals.mu.Unlock()
	return r, g, r.events.count()
}

// toldRun runs tool tell_card on in as the rig's member, outside a chat
// turn, and answers what the agent would read: the result, or the refusal
// in the words the chat gives it.
func toldRun(t *testing.T, r *rig, in toolInput) string {
	t.Helper()
	out, status, msg := toldRefused(t, r, in)
	if status != 0 {
		return "Refused: " + msg
	}
	return out
}

// toldRefused runs tool tell_card on in as the rig's member and answers
// its result with status 0, or its refusal's status and sentence.
func toldRefused(t *testing.T, r *rig, in toolInput) (string, int, string) {
	t.Helper()
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, ok := r.c.chatTool("tell_card")
	if !ok {
		t.Fatal("the chat offers no tool tell_card")
	}
	out, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, in)
	var no *refusal
	if errors.As(err, &no) {
		return "", no.status, no.msg
	}
	if err != nil {
		t.Fatalf("tell_card %+v: %v", in, err)
	}
	return out, 0, ""
}

// A tell at a running card goes to its session as a steer does: the
// member's message on factoryd's path, one card_steered row via chat.
func TestTellCardRunning(t *testing.T) {
	r, _, before := tellRig(t)
	var steered []string
	r.c.cfg.Steer = func(_ context.Context, taskID, user, text string) error {
		steered = append(steered, taskID+" "+user+": "+text)
		return nil
	}
	got := toldRun(t, r, toolInput{TaskID: "t-r", Text: "Rebase on main first."})
	equal(t, "the answer", got, `Told #11 (its session reads the message): The session of #11 has your message.`)
	equal(t, "steered", steered, []string{"t-r sebastian: Rebase on main first."})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-r", IssueNumber: 11, Kind: factoryd.EventCardSteered,
			Detail: map[string]any{"text": "Rebase on main first."}})
}

// A tell at a card in review with a pending merge request denies that
// request with request_changes: the member's REQUEST_CHANGES review on
// its pull request, the request denied, one merge_denied row via chat.
func TestTellCardApprovalGate(t *testing.T) {
	r, _, before := tellRig(t)
	got := toldRun(t, r, toolInput{TaskID: "t-g", Text: "Drop the committed binary."})
	equal(t, "the answer", got, `Told #15 (the text denies its merge request, changes requested): Denied pull request #12 (request changes).`)
	if len(r.fg.reviews) != 1 || r.fg.reviews[0]["event"] != "REQUEST_CHANGES" || r.fg.reviews[0]["body"] != "Drop the committed binary." ||
		r.fg.reviews[0]["commit_id"] != "c0ffee1" {
		t.Fatalf("reviews = %+v, want one REQUEST_CHANGES at c0ffee1 with the text", r.fg.reviews)
	}
	if q := r.approvals.get(6); q.Status != "denied" || !strings.HasPrefix(q.DecisionNote, "changes requested: ") {
		t.Fatalf("approval request 6 = %+v, want it denied with the change request", q)
	}
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-g", IssueNumber: 15, Kind: factoryd.EventMergeDenied,
			Detail: map[string]any{"pr": int64(12), "action": "request_changes"}})
}

// A tell at a factory pull request card's approval gate refuses: no agent
// reads it, and a change request there closes the card (roadmap 264), so
// the refusal names the actions that fit and nothing is decided.
func TestTellCardFactoryApprovalGate(t *testing.T) {
	r, _, _ := tellRig(t)
	r.approvals.rows[len(r.approvals.rows)-1].Spec = json.RawMessage(`{"head_sha":"c0ffee1","risk_class":"low","factory":true,"red_checks":true}`)
	got := toldRun(t, r, toolInput{TaskID: "t-g", Text: "Drop the committed binary."})
	equal(t, "the answer", got, "Refused: Card #15 t-g waits at its approval gate on a pull request factoryd opened, where no agent reads a message: "+
		"approve_merge merges it on green checks, rerun_checks runs red ones again, deny_merge closes it with your words.")
	if q := r.approvals.get(6); q.Status != "pending" || len(r.fg.reviews) != 0 {
		t.Fatalf("approval request 6 = %+v, reviews %+v: want it pending and no review", q, r.fg.reviews)
	}
}

// deny_merge's action defaults to request_changes: a call without one
// proposes and asks for changes as the tell at the approval gate does.
func TestDenyMergeDefaultsToRequestChanges(t *testing.T) {
	r, g, before := tellRig(t)
	stored := `{"action":"request_changes","note":"Drop the committed binary.","task_id":"t-g"}`
	proposed, done, _ := proposeAndDo(t, r, g, "deny_merge", `{"task_id":"t-g","note":"Drop the committed binary."}`)
	equal(t, "proposal", proposed, []string{`proposal {"arguments":` + stored +
		`,"description":"Deny a merge, #15 t-g \"Cache the board.\", action request_changes, note Drop the committed binary.","id":"TOKEN","tool":"deny_merge"}`,
		`done {}`})
	equal(t, "Do it", done, ran("deny_merge", stored, "Denied pull request #12 (request changes)."))
	if len(r.fg.reviews) != 1 || r.fg.reviews[0]["event"] != "REQUEST_CHANGES" {
		t.Fatalf("reviews = %+v, want one REQUEST_CHANGES", r.fg.reviews)
	}
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-g", IssueNumber: 15, Kind: factoryd.EventMergeDenied,
			Detail: map[string]any{"pr": int64(12), "action": "request_changes"}})
}

// A tell at a card waiting on a human with no pending request is the
// answer: the member's comment on its issue, which resumes it, one
// question_answered row via chat.
func TestTellCardWaitingHuman(t *testing.T) {
	r, _, before := tellRig(t)
	got := toldRun(t, r, toolInput{TaskID: "t-e", Text: "a shard"})
	equal(t, "the answer", got, `Told #13 (the text answers the question it waits on): Answered #13.`)
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "a shard" {
		t.Fatalf("the comment = %q, want the text", got)
	}
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-e", IssueNumber: 13, Kind: factoryd.EventQuestionAnswered,
			Detail: postedDetail(1)})
}

// A tell at a card in no telling state is refused 409 naming the states
// the routing knows and the action that fits the card's state; nothing
// is written.
func TestTellCardWrongState(t *testing.T) {
	for _, tc := range []struct{ name, task, want string }{
		{"backlog", "t-m", "Card #5 t-m is backlog (waits in the backlog until a member moves it into a lane), not " +
			"a card running a session, at its approval gate or waiting on a human: move_card starts it."},
		{"done", "t-x", "Card #9 t-x is done (is done), not " +
			"a card running a session, at its approval gate or waiting on a human: the card is final: a new card continues the work."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, before := tellRig(t)
			out, status, msg := toldRefused(t, r, toolInput{TaskID: tc.task, Text: "Start now."})
			if out != "" || status != http.StatusConflict {
				t.Fatalf("a tell at a %s card = %q, %d %q, want a 409 refusal", tc.name, out, status, msg)
			}
			equal(t, "the refusal", msg, tc.want)
			chatRows(t, r.events, before, "sebastian")
		})
	}
}

// A card in review whose gates still run holds no pending merge request,
// so no branch fits it either: 409 names the approval gate as the state
// it would take with one.
func TestTellCardGateWithoutARequest(t *testing.T) {
	r, _, _ := tellRig(t)
	r.approvals.mu.Lock()
	for i := range r.approvals.rows {
		if r.approvals.rows[i].ID == 6 {
			r.approvals.rows[i].Status = "approved"
		}
	}
	r.approvals.mu.Unlock()
	out, status, msg := toldRefused(t, r, toolInput{TaskID: "t-g", Text: "Merge it now."})
	if out != "" || status != http.StatusConflict {
		t.Fatalf("a tell at an in-review card without a request = %q, %d %q, want a 409 refusal", out, status, msg)
	}
	want := "Card #15 t-g is in-review (passes the gates after its session), not " +
		"a card running a session, at its approval gate or waiting on a human: its gates run; nothing to do until they end."
	equal(t, "the refusal", msg, want)
}

// tell_card needs the member's OK by default, which its sentence
// carries, and the system read names its routes.
func TestTellCardIsAChatTool(t *testing.T) {
	if !slices.Contains(policy.DefaultChat.Confirm, "tell_card") {
		t.Fatalf("DefaultChat.Confirm = %v, want tell_card", policy.DefaultChat.Confirm)
	}
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "system", `{}`)}, {say("read it.")}}
	if code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"how does the factory work?"}}); code != http.StatusOK ||
		events[len(events)-1] != "done {}" {
		t.Fatalf("turn = %d %v", code, events)
	}
	var about any
	for _, d := range g.calls()[0]["tools"].([]any) {
		if f := d.(map[string]any)["function"].(map[string]any); f["name"] == "tell_card" {
			about = f["description"]
		}
	}
	equal(t, "tell_card's sentence", about, "Tell a card one text, routed by its state to its session, its merge request or its question (needs OK).")
	msgs := messages(t, g.calls()[1])
	sys, _ := msgs[len(msgs)-1]["content"].(string)
	if want := "\n  tell_card routes by the card's state: to its running session as a message (steer), at its pending merge request as changes requested (deny_merge), " +
		"to the question it waits on as the answer (answer).\n"; !strings.Contains(sys, want) {
		t.Errorf("the manual lacks %q:\n%s", want, sys)
	}
}
