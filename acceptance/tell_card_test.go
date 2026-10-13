package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
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
// end-to-end through the console's HTTP surface with the console test
// harness' faked Forgejo, store and steer.

const repository = "sf/code"
const login = "sebastian"

// tellRig is the harness with tell_card's cards; it answers the rig, the
// project's chat path and the member's session.
func tellRig(t *testing.T) (*console.AcceptanceRig, string, *http.Cookie, string) {
	t.Helper()
	r, path := console.NewAcceptanceRig(t)
	ck, csrf := r.Login(t)
	return r, path, ck, csrf
}

// postTell runs the chat's turn on message as the member and answers its
// status and stream body.
func postTell(t *testing.T, r *console.AcceptanceRig, path string, ck *http.Cookie, csrf, message string) (int, string) {
	t.Helper()
	form := url.Values{"message": {message}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := r.Post(req, ck)
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, string(body)
}

// proposalOf answers the tell_card proposal the stream carries: its
// token and the description the member reads before they press Do it.
func proposalOf(t *testing.T, body string) (string, string) {
	t.Helper()
	for _, msg := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		line := strings.Split(msg, "\n")
		if len(line) < 2 || line[0] != "event: proposal" {
			continue
		}
		var p struct {
			ID          string         `json:"id"`
			Tool        string         `json:"tool"`
			Description string         `json:"description"`
			Arguments   map[string]any `json:"arguments"`
		}
		data, _ := strings.CutPrefix(line[1], "data: ")
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			t.Fatalf("proposal event %q: %v", data, err)
		}
		if p.Tool != "tell_card" {
			t.Errorf("the proposal's tool = %q, want tell_card", p.Tool)
			continue
		}
		return p.ID, p.Description
	}
	t.Fatalf("the stream holds no tell_card proposal:\n%s", body)
	return "", ""
}

// doIt presses the member's Do it on proposal id and answers the status
// and stream body of the run.
func doIt(t *testing.T, r *console.AcceptanceRig, path string, ck *http.Cookie, csrf, id string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path+"/proposals/"+id, strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := r.Post(req, ck)
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, string(body)
}

// appliedResult answers the result the applied tool call left in the
// conversation, and whether the call was stored as applied.
func appliedResult(t *testing.T, r *console.AcceptanceRig, tool string) (string, bool) {
	t.Helper()
	for _, c := range r.ToolCalls(t, repository, login) {
		if c.Name == tool && c.State == "applied" {
			return c.Args, true
		}
	}
	return "", false
}

// equal answers what got differs from want in the test's words.
func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s =\n%#v\nwant\n%#v", what, got, want)
	}
}

// wantEvent is one event row the tell leaves: kind, task, issue and the
// detail it must carry, via chat by the member.
type wantEvent struct {
	kind   string
	taskID string
	issue  int64
	detail map[string]any
}

// checkEvents answers the rows after the rig's birth to exactly wants.
func checkEvents(t *testing.T, r *console.AcceptanceRig, wants ...wantEvent) {
	t.Helper()
	rows := r.Events()
	if len(rows) != len(wants) {
		t.Fatalf("%d event rows, want %d: %+v", len(rows), len(wants), rows)
	}
	for i, w := range wants {
		got := rows[i]
		equal(t+fmt.Sprintf(" row %d's kind", i), got.Kind, w.kind)
		equal(whatRow(i, "task"), got.TaskID, w.taskID)
		equal(whatRow(i, "issue"), got.IssueNumber, w.issue)
		equal(whatRow(i, "repository"), got.Repository, repository)
		equal(whatRow(i, "actor"), got.Actor, login)
		equal(whatRow(i, "via"), got.Via, factoryd.ViaChat)
		for k, v := range w.detail {
			if !reflect.DeepEqual(got.Detail[k], v) {
				t.Errorf("row %d's detail[%q] = %#v, want %#v (all: %#v)", i, k, got.Detail[k], v, got.Detail)
			}
		}
	}
}

func whatRow(i int, field string) string { return fmt.Sprintf("row %d's %s", i, field) }

// TestTellCardAtRunningSession is the live check of the routing's first
// branch: the member tells a card running its session, the chat proposes
// tell_card, the Do it delivers the text on the steer path and records
// one card_steered row.
func TestTellCardAtRunningSession(t *testing.T) {
	r, path, ck, csrf := tellRig(t)
	var steered []string
	r.SetSteer(func(_ context.Context, taskID, user, text string) error {
		steered = append(steered, taskID+" "+user+": "+text)
		return nil
	})

	code, body := postTell(t, r, path, ck, csrf, "tell card t-r to rebase on main first")
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	token, desc := proposalOf(t, body)
	for _, want := range []string{"Rebase on main first.", "t-r", "#11"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the proposal's description lacks %q:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "event card_steered") == false {
		t.Errorf("the proposal names no route:\n%s", desc)
	}
	if len(r.Steered()) != 0 || len(r.Events()) != 0 {
		t.Fatalf("a proposal ran without the member: steered %+v, events %+v", r.Steered(), r.Events())
	}

	code, body = doIt(t, r, path, ck, csrf, token)
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	note := "Told #11 (its session reads the message): The session of #11 has your message."
	if !strings.Contains(body, note) {
		t.Errorf("the Do it's stream lacks %q:\n%s", note, body)
	}
	if _, applied := appliedResult(t, r, "tell_card"); !applied {
		t.Error("the conversation holds no applied tell_card call")
	}
	equal(t, "the text the session was handed", steered, []string{"t-r sebastian: Rebase on main first."})
	checkEvents(t, r, wantEvent{kind: factoryd.EventCardSteered, taskID: "t-r", issue: 11,
		detail: map[string]any{"text": "Rebase on main first."}})
}

// TestTellCardAtApprovalGate is the live check of the routing's second
// branch: the member tells a card waiting at its approval gate, the Do
// it denies its merge request with changes requested — the member's
// REQUEST_CHANGES review on its pull request, the request denied — and
// records one merge_denied row.
func TestTellCardAtApprovalGate(t *testing.T) {
	r, path, ck, csrf := tellRig(t)

	code, body := postTell(t, r, path, ck, csrf, "tell card t-g to drop the committed binary")
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	token, _ := proposalOf(t, body)
	if q := r.Approval(6); q.Status != "pending" {
		t.Fatalf("the Do it ran unasked: approval request 6 = %q", q.Status)
	}

	code, body = doIt(t, r, path, ck, csrf, token)
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	note := "Told #15 (the text denies its merge request, changes requested): Denied pull request #12 (request changes)."
	if !strings.Contains(body, note) {
		t.Errorf("the Do it's stream lacks %q:\n%s", note, body)
	}

	reviews := r.Reviews()
	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want one: %+v", len(reviews), reviews)
	}
	rv := reviews[0]
	equal(t, "the review's event", rv.Event, "REQUEST_CHANGES")
	equal(t, "the review's body", rv.Body, "Drop the committed binary.")
	equal(t, "the review's commit", rv.CommitID, "c0ffee1")
	equal(t, "the review's call", rv.Path, "POST /api/v1/repos/sf/code/pulls/12/reviews Bearer access-1")

	q := r.Approval(6)
	if q.Status != "denied" || !strings.HasPrefix(q.DecisionNote, "changes requested: ") || q.DecidedBy != login {
		t.Fatalf("approval request 6 = %+v, want it denied with the change request by the member", q)
	}
	checkEvents(t, r, wantEvent{kind: factoryd.EventMergeDenied, taskID: "t-g", issue: 15,
		detail: map[string]any{"pr": int64(12), "action": "request_changes"}})
}
