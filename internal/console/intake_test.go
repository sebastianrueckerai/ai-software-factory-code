package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat header shows the intake switch to an approver alone: its
// button names the state, open first, closed once flipped, and a writer
// who approves nothing gets no switch.
func TestChatHeaderShowsTheIntakeSwitchForAnApprover(t *testing.T) {
	r, _, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	const code = "/?project=ai-software-factory-code"
	body := r.do(httptest.NewRequest(http.MethodGet, code, nil), ck).Body.String()
	if !strings.Contains(body, `<button type="button" id="intake-btn" class="intake" data-intake="open" aria-pressed="false"`) ||
		!strings.Contains(body, `<span class="intake-name">open</span>`) {
		t.Errorf("the approver's chat header holds no open intake switch")
	}
	ck2, s2 := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s2.csrf}, "intake": {"closed"}}), ck2); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	body = r.do(httptest.NewRequest(http.MethodGet, code, nil), ck).Body.String()
	if !strings.Contains(body, `id="intake-btn" class="intake closed" data-intake="closed" aria-pressed="true"`) ||
		!strings.Contains(body, `<span class="intake-name">closed</span>`) {
		t.Errorf("the closed intake does not name itself on the button")
	}
	rows := r.events.all()
	if len(rows) == 0 || rows[len(rows)-1].Kind != factoryd.EventIntakeChanged ||
		rows[len(rows)-1].Detail["from"] != "open" || rows[len(rows)-1].Detail["to"] != "closed" {
		t.Errorf("the switch recorded %+v, want an intake_changed row from open to closed", rows)
	}
	r.ledger.approvers = []string{}
	body = r.do(httptest.NewRequest(http.MethodGet, code, nil), ck).Body.String()
	if strings.Contains(body, `id="intake-btn"`) {
		t.Errorf("a writer who is no approver sees the intake switch")
	}
}

// A closed intake refuses the wake's own filings: the turn the Waker
// starts calls new_card and the tool answers the refusal; no issue is
// filed and no filing is recorded.
func TestClosedIntakeRefusesTheWakesFilings(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s.csrf}, "intake": {"closed"}}), ck); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	before := len(r.events.all())
	g.replies = [][]string{{call(1, "new_card", `{"title":"Bump the dep","lane":"backlog"}`)}, {say("Your intake is closed, so I filed nothing. Tell me what to file.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_heartbeat", "The daily heartbeat: look for what to start."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	refused := "Refused: The intake of ai-software-factory-code is closed: a wake files nothing. Tell the member what needs filing; their own ask still files and starts."
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	refusedRow := false
	for _, row := range rows {
		for _, tc := range row.ToolCalls {
			if tc.Name == "new_card" {
				refusedRow = tc.Result == refused
			}
		}
	}
	if !refusedRow {
		t.Errorf("the wake's new_card was not refused with\n%s\namong %+v", refused, rows)
	}
	if len(r.fg.issues) != 0 {
		t.Errorf("the wake filed %v", r.fg.issues)
	}
	for _, e := range r.events.all()[before:] {
		if e.Kind == factoryd.EventCardFiled {
			t.Errorf("the wake's filing was recorded: %+v", e)
		}
	}
}

// The member's explicit ask still files and starts on a closed intake:
// their own turn's new_card lands in lane ready, ready to start.
func TestMembersAskStillFilesAndStartsOnAClosedIntake(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s.csrf}, "intake": {"closed"}}), ck); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	before := len(r.events.all())
	g.replies = [][]string{{call(1, "new_card", `{"title":"Fix the flux","body":"Count hits.\n\nAcceptance:\n- The metrics work","lane":"ready","size":"small","rung":"frontier","rung_reason":"touches the queue"}`)}, {say("Filed it for you.")}}
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"the flux broke; file a ready card for it"}})
	equal(t, "code", code, 200)
	if !strings.Contains(events[1], `"result":"Filed sf/code#41 in lane ready."`) {
		t.Errorf("the member's new_card did not file in lane ready: %v", events)
	}
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The metrics work\n\nSize: small\nRung: frontier, touches the queue")
	rows := r.events.all()[before:]
	if len(rows) != 1 || rows[0].Kind != factoryd.EventCardFiled || rows[0].IssueNumber != 41 || rows[0].Detail["lane"] != "ready" {
		t.Errorf("the member's filing recorded %+v, want one card_filed row of #41 in lane ready", rows)
	}
}
