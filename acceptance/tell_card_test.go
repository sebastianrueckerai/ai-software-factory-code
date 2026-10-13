package acceptance

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The acceptance tests of "tell the card" (roadmap 183, Stage 18 item
// 3.6): one chat write the server routes by the card's state, exercised
// end-to-end through the console's HTTP surface on the console test
// harness with faked Forgejo, store and steer.

const repository = "sf/code"
const login = "sebastian"

// tellRig is the harness for a tell_card run: the member logged in, the
// steer answering, and — when gate is non-empty — card t-g of issue 15
// with pull request 12 in review at its approval gate, holding pending
// merge request 6 at head c0ffee1. It answers the rig, the project's
// chat path, the session cookie and the form token.
func tellRig(t *testing.T, objective string) (*console.AcceptanceRig, string, *http.Cookie, string) {
	t.Helper()
	r, path := console.NewAcceptanceRig(t)
	ck, csrf := r.Login(t)
	r.SetSteer()
	if objective != "" {
		r.TellCard("t-g", 15, 12, 6, objective, "c0ffee1")
	}
	return r, path, ck, csrf
}

// postChat runs one POST on path as the member through a real test server
// and answers its status and stream body. The chat's SSE stream uses
// http.ResponseController.SetWriteDeadline, which httptest.ResponseRecorder
// does not implement; a direct call drops every write, so we go through
// httptest.NewServer like internal/console/chat_test.go's chatPost does.
func postChat(t *testing.T, r *console.AcceptanceRig, path string, ck *http.Cookie, form url.Values) (int, string) {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// eventsOf answers the stream's events as "event data" lines, in order.
func eventsOf(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, msg := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		line := strings.Split(msg, "\n")
		if len(line) < 2 || !strings.HasPrefix(line[0], "event: ") {
			t.Errorf("a stream block of no event: %q", msg)
			continue
		}
		out = append(out, strings.TrimPrefix(line[0], "event: ")+" "+strings.TrimPrefix(line[1], "data: "))
	}
	return out
}

// proposalOf answers the tell_card proposal the stream carries: its
// token and the description the member reads before they press Do it.
func proposalOf(t *testing.T, events []string) (string, string) {
	t.Helper()
	for _, e := range events {
		if !strings.HasPrefix(e, "proposal ") {
			continue
		}
		var p struct {
			ID          string `json:"id"`
			Tool        string `json:"tool"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "proposal ")), &p); err != nil {
			t.Fatalf("the proposal event %q: %v", e, err)
		}
		if p.Tool != "tell_card" {
			t.Errorf("the proposal's tool = %q, want tell_card", p.Tool)
			continue
		}
		if len(p.ID) != 43 {
			t.Errorf("the proposal's token = %q, want a fresh one", p.ID)
		}
		return p.ID, p.Description
	}
	t.Fatalf("the stream holds no tell_card proposal:\n%v", events)
	return "", ""
}

// appliedResult answers the result the applied tell_card call left in the
// conversation.
func appliedResult(t *testing.T, r *console.AcceptanceRig) string {
	t.Helper()
	for _, c := range r.ToolCalls(repository, login) {
		if c.Name == "tell_card" && !strings.HasPrefix(c.Result, "Proposed to ") {
			return c.Result
		}
	}
	t.Error("the conversation holds no applied tell_card call")
	return ""
}

// equal answers what got differs from want in the test's words.
func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s =\n%#v\nwant\n%#v", what, got, want)
	}
}

// checkEvents answers the event spine's rows to exactly wants: kind,
// task, issue and detail, each by chat as the member on the code project.
func checkEvents(t *testing.T, r *console.AcceptanceRig, wants ...factoryd.Event) {
	t.Helper()
	rows := r.Events()
	if len(rows) != len(wants) {
		t.Fatalf("%d event rows, want %d: %+v", len(rows), len(wants), rows)
	}
	for i, w := range wants {
		got := rows[i]
		w.ID, w.At, w.Actor, w.Via = got.ID, got.At, login, factoryd.ViaChat
		w.Repository = repository
		if !reflect.DeepEqual(got, w) {
			t.Errorf("row %d = %+v\nwant      %+v", i, got, w)
		}
	}
}

// jsonQuote is s as it stands inside a JSON string: the stream escapes
// every '<' there.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `"`), `"`)
}

// proposalToken is the one tell_card proposal's token; it fails when the
// stream holds none or a second.
func proposalToken(t *testing.T, events []string) string {
	t.Helper()
	token, _ := proposalOf(t, events)
	return token
}

// TestTellCardAtRunningSession is the live check of the routing's first
// branch: the member tells a card running its session, the chat proposes
// tell_card naming the route and its event, and the Do it delivers the
// text on the steer path and records one card_steered row.
func TestTellCardAtRunningSession(t *testing.T) {
	r, path, ck, csrf := tellRig(t, "")
	r.Reply([]string{console.Call(1, "tell_card", `{"task_id":"t-r","text":"Rebase on main first."}`)})

	code, body := postChat(t, r, path, ck, url.Values{"message": {"tell card t-r to rebase on main first"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	events := eventsOf(t, body)
	token, desc := proposalOf(t, events)
	for _, want := range []string{"→ its session reads the message (#11 running, event card_steered)", "#11 t-r", "text Rebase on main first"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the proposal's description lacks %q:\n%s", want, desc)
		}
	}
	equal(t, "the turn's stream", events, []string{`proposal {"arguments":{"task_id":"t-r","text":"Rebase on main first."},` +
		`"description":"` + jsonQuote(tellRunDescription) + `","id":"` + token + `","tool":"tell_card"}`, `done {}`})
	if len(r.Steered()) != 0 {
		t.Fatalf("a proposed tell ran without the member: %+v", r.Steered())
	}
	checkEvents(t, r)

	code, body = postChat(t, r, path+"/proposals/"+token, ck, url.Values{"csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	equal(t, "the Do it's stream", eventsOf(t, body), []string{
		`tool {"arguments":{"task_id":"t-r","text":"Rebase on main first."},"duration_ms":0,"name":"tell_card",` +
			`"result":"Told #11 (its session reads the message): The session of #11 has your message."}`,
		`applied {"result":"Told #11 (its session reads the message): The session of #11 has your message.","tool":"tell_card"}`,
		`done {}`})
	equal(t, "the text the session was handed", r.Steered(), []string{"t-r sebastian: Rebase on main first."})
	equal(t, "the tell_card call's stored result", appliedResult(t, r),
		"Told #11 (its session reads the message): The session of #11 has your message. It cannot be undone.")
	checkEvents(t, r, factoryd.Event{Repository: repository, TaskID: "t-r", IssueNumber: 11, Kind: factoryd.EventCardSteered,
		Detail: map[string]any{"text": "Rebase on main first."}})
}

// TestTellCardAtApprovalGate is the live check of the routing's second
// branch: the member tells a card waiting at its approval gate, and the
// Do it denies its merge request with changes requested — the member's
// REQUEST_CHANGES review on its pull request, the request denied — and
// records one merge_denied row.
func TestTellCardAtApprovalGate(t *testing.T) {
	r, path, ck, csrf := tellRig(t, "Cache the board.")
	r.Reply([]string{console.Call(1, "tell_card", `{"task_id":"t-g","text":"Drop the committed binary."}`)})

	code, body := postChat(t, r, path, ck, url.Values{"message": {"tell card t-g to drop the committed binary"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	_, desc := proposalOf(t, eventsOf(t, body))
	for _, want := range []string{"→ the text denies its merge request, changes requested (#15 approval-gate, event merge_denied)", "#15 t-g", "text Drop the committed binary"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the proposal's description lacks %q:\n%s", want, desc)
		}
	}
	equal(t, "the proposal's line", desc, gateDescription)
	if q := r.Approval(6); q.Status != "pending" {
		t.Fatalf("the Do it ran unasked: approval request 6 = %q", q.Status)
	}
	checkEvents(t, r)

	token := proposalToken(t, eventsOf(t, body))
	code, body = postChat(t, r, path+"/proposals/"+token, ck, url.Values{"csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	equal(t, "the Do it's stream", eventsOf(t, body), []string{
		`tool {"arguments":{"task_id":"t-g","text":"Drop the committed binary."},"duration_ms":0,"name":"tell_card",` +
			`"result":"Told #15 (the text denies its merge request, changes requested): Denied pull request #12 (request changes)."}`,
		`applied {"result":"Told #15 (the text denies its merge request, changes requested): Denied pull request #12 (request changes).","tool":"tell_card"}`,
		`done {}`})

	reviews := r.Reviews()
	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want one: %+v", len(reviews), reviews)
	}
	rv := reviews[0]
	equal(t, "the review's event", rv["event"], "REQUEST_CHANGES")
	equal(t, "the review's body", rv["body"], "Drop the committed binary.")
	equal(t, "the review's commit", rv["commit_id"], "c0ffee1")
	equal(t, "the review's call", rv["call"], "POST /api/v1/repos/sf/code/pulls/12/reviews Bearer access-1")

	q := r.Approval(6)
	if q.Status != "denied" || !strings.HasPrefix(q.DecisionNote, "changes requested: ") || q.DecidedBy != login {
		t.Fatalf("approval request 6 = %+v, want it denied with the change request by the member", q)
	}
	equal(t, "the tell_card call's stored result", appliedResult(t, r),
		"Told #15 (the text denies its merge request, changes requested): Denied pull request #12 (request changes). It cannot be undone.")
	checkEvents(t, r, factoryd.Event{Repository: repository, TaskID: "t-g", IssueNumber: 15, Kind: factoryd.EventMergeDenied,
		Detail: map[string]any{"pr": int64(12), "action": "request_changes"}})
}

// tellRunDescription is the line the member reads on the running card's
// proposal: the tool, the card, the text, and where the state sends it.
const tellRunDescription = `Tell a card, #11 t-r "Speed up the board", text Rebase on main first.` +
	` → its session reads the message (#11 running, event card_steered)`

// gateDescription is the running card's line at the approval gate: the
// state sends the text to its pending merge request.
const gateDescription = `Tell a card, #15 t-g "Cache the board", text Drop the committed binary.` +
	` → the text denies its merge request, changes requested (#15 approval-gate, event merge_denied)`
