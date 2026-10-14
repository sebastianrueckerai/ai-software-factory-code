package console

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// At assisted the agent's own start runs unconfirmed, and its event row
// records the decider: the level, the merge rule, the chat task behind it
// (roadmap 239's recording).
func TestChatRecordsTheDecider(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	g.replies = [][]string{{call(1, "move_card", `{"task_id":"t-m","lane":"ready"}`)}, {say("Moved it.")}}
	ck, s := r.loggedIn(t)
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"move the cache card to ready"}})
	equal(t, "code", code, 200)
	applied := `applied {"result":"Moved #5 to lane ready; it leaves the backlog once factoryd picks it up.","tool":"move_card","undo":"TOKEN"}`
	if !slices.Contains(events, applied) {
		t.Errorf("the stream lacks the applied line: %v", events)
	}
	var moved *factoryd.Event
	for i, e := range r.events.all() {
		if e.Kind == factoryd.EventCardMoved && e.TaskID == "t-m" {
			moved = &r.events.all()[i]
		}
	}
	if moved == nil {
		t.Fatal("no card_moved row")
	}
	if moved.Detail["autonomy"] != "assisted" || moved.Detail["decider"] != "agent" {
		t.Errorf("detail = %v, want autonomy assisted and decider agent", moved.Detail)
	}
	if moved.Detail["merge_rule"] != "human" {
		t.Errorf("detail merge_rule = %v, want human", moved.Detail["merge_rule"])
	}
	if moved.Detail["chat_task"] != factoryd.ChatTaskID("ai-software-factory-code", time.Now()) {
		t.Errorf("detail chat_task = %v, want this month's chat task", moved.Detail["chat_task"])
	}
}

// The selector's two keys land in the project file, comments kept, the
// cap only away from the default.
func TestEditProjectFile(t *testing.T) {
	raw := []byte("# the code project\nrepository: sf/code\nmerge_rule: human\n")
	out, err := editProjectFile(raw, policy.AutonomyAssisted, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"# the code project", "repository: sf/code", "autonomy: assisted"} {
		if !strings.Contains(got, want) {
			t.Errorf("editProjectFile lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "backlog_cap") {
		t.Errorf("editProjectFile wrote the default cap:\n%s", got)
	}
	out, err = editProjectFile(out, policy.AutonomyAutonomous, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got = string(out); !strings.Contains(got, "autonomy: autonomous") || !strings.Contains(got, "backlog_cap: 3") {
		t.Errorf("editProjectFile = %s, want autonomous with cap 3", got)
	}
	out, err = editProjectFile(out, policy.AutonomyManual, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got = string(out); strings.Contains(got, "backlog_cap") {
		t.Errorf("editProjectFile kept the cap at the default:\n%s", got)
	}
}

// The roadmap structure check: numbers unique, Sources and
// Where-it-stands kept.
func TestRoadmapFindings(t *testing.T) {
	item := func(n int, sources, stands bool) string {
		s := "- **" + strconv.Itoa(n) + ". The item.** The body."
		if sources {
			s += " Sources: the discussion."
		}
		if stands {
			s += " Where it stands: planned."
		}
		return s + "\n"
	}
	if f := roadmapFindings(item(1, true, true) + item(2, true, true)); len(f) != 0 {
		t.Errorf("a sound roadmap: findings %v, want none", f)
	}
	f := roadmapFindings(item(1, true, true) + item(1, true, true))
	if len(f) != 1 || f[0] != "item 1 appears twice" {
		t.Errorf("a doubled item: findings %v, want the twice line", f)
	}
	f = roadmapFindings(item(1, false, true))
	if len(f) != 1 || f[0] != "item 1 lost its Sources line" {
		t.Errorf("a sourceless item: findings %v, want the Sources line", f)
	}
	f = roadmapFindings("no items at all\n")
	if len(f) != 1 || f[0] != "the roadmap holds no numbered items" {
		t.Errorf("an empty roadmap: findings %v, want the no-items line", f)
	}
}
