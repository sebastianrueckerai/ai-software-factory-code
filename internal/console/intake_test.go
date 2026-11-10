package console

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat header shows the intake switch to an approver alone: its
// button names the state, open first, closed once flipped, its tip the
// switch's one meaning (the roadmap pull) and its line the last change,
// who set it and when; a writer who approves nothing gets no switch.
func TestChatHeaderShowsTheIntakeSwitchForAnApprover(t *testing.T) {
	r, _, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	const code = "/?project=ai-software-factory-code"
	body := r.do(httptest.NewRequest(http.MethodGet, code, nil), ck).Body.String()
	if !strings.Contains(body, `<button type="button" id="intake-btn" class="intake" data-intake="open" aria-pressed="false"`) ||
		!strings.Contains(body, `<span class="intake-name">open</span>`) {
		t.Errorf("the approver's chat header holds no open intake switch")
	}
	if !strings.Contains(body, `closed, the roadmap is not pulled`) || strings.Contains(body, "intake-by") {
		t.Errorf("the switch's tip misses the pull's meaning, or an untouched switch names a setter")
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
	if !strings.Contains(body, `set by sebastian, `) || !strings.Contains(body, ` UTC</span>`) {
		t.Errorf("the flipped switch names no setter with a time: %s", body)
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

// A closed intake stops exactly the roadmap pull (2026-11-01): a wake
// turn's new_card that carries a roadmap item answers the refusal, no
// issue is filed and no filing is recorded.
func TestClosedIntakeRefusesTheRoadmapPullsFilings(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s.csrf}, "intake": {"closed"}}), ck); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	before := len(r.events.all())
	g.replies = [][]string{{call(1, "new_card", `{"title":"Build roadmap 276","track":"direct","track_reason":"the pull","roadmap_item":276}`)}, {say("Your intake is closed, so the roadmap was not pulled. Tell me what to file.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "queue_refill",
		"The queue of ai-software-factory-code is below 6: read docs/roadmap.md of the project's doc repository, then end."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	refused := "Refused: The intake of ai-software-factory-code is closed: the roadmap is not pulled. Tell the human what needs filing; an ask or a need still files."
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
		t.Errorf("the pull's new_card was not refused with\n%s\namong %+v", refused, rows)
	}
	if len(r.fg.issues) != 0 {
		t.Errorf("the pull filed %v", r.fg.issues)
	}
	for _, e := range r.events.all()[before:] {
		if e.Kind == factoryd.EventCardFiled {
			t.Errorf("the pull's filing was recorded: %+v", e)
		}
	}
}

// A closed intake takes every filing but the pull (2026-11-01): a wake
// turn's reactive new_card, carrying no roadmap item, files and records
// while the switch is closed.
func TestAClosedIntakeStillTakesAReactiveWakesFiling(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s.csrf}, "intake": {"closed"}}), ck); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	before := len(r.events.all())
	g.replies = [][]string{{call(1, "new_card", `{"title":"Fix the flux","body":"Count hits.\n\nAcceptance:\n- The metrics work","track":"direct","track_reason":"the timer's ask"}`)}, {say("Filed it.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_timer", "Timer: the build broke."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	filed := false
	for _, row := range rows {
		for _, tc := range row.ToolCalls {
			if tc.Name == "new_card" {
				filed = strings.Contains(tc.Result, "Filed sf/code#41 on track direct.")
			}
		}
	}
	if !filed {
		t.Errorf("the reactive wake's new_card did not file: %+v", rows)
	}
	if len(r.fg.issues) != 1 || strings.Contains(fmt.Sprint(r.fg.issues[0]["body"]), "Roadmap:") {
		t.Errorf("the reactive wake's issue = %+v, want one without a Roadmap line", r.fg.issues)
	}
	filedRow := false
	for _, e := range r.events.all()[before:] {
		if e.Kind == factoryd.EventCardFiled {
			filedRow = true
		}
	}
	if !filedRow {
		t.Errorf("the reactive wake's filing was not recorded")
	}
}

// The member's explicit ask still files and starts on a closed intake:
// their own turn's new_card lands on track direct, ready to start.
func TestMembersAskStillFilesAndStartsOnAClosedIntake(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s.csrf}, "intake": {"closed"}}), ck); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	before := len(r.events.all())
	g.replies = [][]string{{call(1, "new_card", `{"title":"Fix the flux","body":"Count hits.\n\nAcceptance:\n- The metrics work","track":"direct","track_reason":"one fix","size":"small","rung":"frontier","rung_reason":"touches the queue"}`)}, {say("Filed it for you.")}}
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"the flux broke; file a direct card for it"}})
	equal(t, "code", code, 200)
	if !strings.Contains(events[1], `"result":"Filed sf/code#41 on track direct."`) {
		t.Errorf("the member's new_card did not file on track direct: %v", events)
	}
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Count hits.\n\nAcceptance:\n- The metrics work\n\nSize: small\nRung: frontier, touches the queue")
	rows := r.events.all()[before:]
	if len(rows) != 1 || rows[0].Kind != factoryd.EventCardFiled || rows[0].IssueNumber != 41 || rows[0].Detail["track"] != "direct" {
		t.Errorf("the member's filing recorded %+v, want one card_filed row of #41 on track direct", rows)
	}
}

// A roadmap item one card already builds is refused naming the card
// (2026-11-01), before any issue is posted; an item no card cites files.
func TestNewCardRefusesATakenRoadmapItem(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	r.tasks.roadmap = map[string]factoryd.Task{"sf/code 276": {ID: "t-holder", Status: "running", IssueNumber: 12}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "new_card", `{"title":"Build roadmap 276","body":"The plan's item.\n\nAcceptance:\n- The pull files it","track":"direct","track_reason":"the pull","roadmap_item":276}`)}, {say("It is taken.")}}
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"file roadmap item 276"}})
	equal(t, "code", code, 200)
	if !strings.Contains(events[0], `"result":"Refused: Card t-holder (#12, running) already builds roadmap item 276. Wait for it to end, or close it and file again with refile_of 12."`) {
		t.Errorf("the taken item's new_card = %v", events)
	}
	if len(r.fg.issues) != 0 {
		t.Fatalf("the taken item filed %v", r.fg.issues)
	}
	// an item no card cites files, its issue carrying the Roadmap line
	g.replies = [][]string{{call(1, "new_card", `{"title":"Build roadmap 312","body":"The plan's item.\n\nAcceptance:\n- The pull files it","track":"direct","track_reason":"the pull","roadmap_item":312}`)}, {say("Filed it.")}}
	code, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"file roadmap item 312"}})
	equal(t, "code", code, 200)
	if !strings.Contains(events[1], `"result":"Filed sf/code#41 on track direct."`) {
		t.Errorf("the free item's new_card = %v", events)
	}
	equal(t, "the issue's body", r.fg.issues[0]["body"], "The plan's item.\n\nAcceptance:\n- The pull files it\n\nRoadmap: #312")
	rows := r.events.all()
	last := rows[len(rows)-1]
	if last.Kind != factoryd.EventCardFiled || last.IssueNumber != 41 || last.Detail["roadmap_item"] != 312 {
		t.Errorf("the pull's filing recorded %+v, want a card_filed row of #41 with roadmap_item 312", last)
	}
}

// The refusal names the holder's state and the field that takes the item
// over, whole, by state.
func TestRoadmapRefusalByHolderState(t *testing.T) {
	for _, c := range []struct{ status, want string }{
		{"running", "Card t-h (#1547, running) already builds roadmap item 66. Wait for it to end, or close it and file again with refile_of 1547."},
		{"closed", "Card t-h (#1547, closed) already builds roadmap item 66. File again with refile_of 1547."},
		{"failed", "Card t-h (#1547, failed) already builds roadmap item 66. File again with refile_of 1547."},
		{"done", "Card t-h (#1547, done) already built roadmap item 66. Cite a new roadmap item, or file without roadmap_item."},
	} {
		got := roadmapRefusal(factoryd.Task{ID: "t-h", Status: c.status, IssueNumber: 1547}, 66)
		equal(t, c.status, got, c.want)
	}
}

// new_card's refile_of writes the "Refile of #N" line beside the Roadmap
// line, and needs the item it frees.
func TestNewCardRefileOf(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "new_card", `{"title":"Refile 66","body":"The plan again.\n\nAcceptance:\n- The pull files it","track":"direct","track_reason":"the pull","refile_of":1547}`)}, {say("Refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"refile"}})
	if !strings.Contains(events[0], "refile_of needs roadmap_item") || len(r.fg.issues) != 0 {
		t.Errorf("refile_of alone = %v, issues %v", events, r.fg.issues)
	}
	g.replies = [][]string{{call(1, "new_card", `{"title":"Refile 66","body":"The plan again.\n\nAcceptance:\n- The pull files it","track":"direct","track_reason":"the pull","roadmap_item":66,"refile_of":1547}`)}, {say("Filed.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"refile 66"}})
	equal(t, "the issue's body", r.fg.issues[0]["body"], "The plan again.\n\nAcceptance:\n- The pull files it\n\nRoadmap: #66\nRefile of #1547")
}
