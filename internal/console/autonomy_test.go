package console

import (
	"context"
	"encoding/base64"
	"net/http"
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
// cap only away from the default; a cap of 0 keeps the file's, and the
// default takes the key off.
func TestEditProjectFile(t *testing.T) {
	raw := []byte("# the code project\nrepository: sf/code\nmerge_rule: human\n")
	out, err := editProjectFile(raw, autonomyEdit(policy.AutonomyAssisted, 0))
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
	out, err = editProjectFile(out, autonomyEdit(policy.AutonomyAutonomous, 3))
	if err != nil {
		t.Fatal(err)
	}
	if got = string(out); !strings.Contains(got, "autonomy: autonomous") || !strings.Contains(got, "backlog_cap: 3") {
		t.Errorf("editProjectFile = %s, want autonomous with cap 3", got)
	}
	// an existing cap changes as an integer: a quoted "5" fails the schema
	if out, err = editProjectFile(out, autonomyEdit(policy.AutonomyAutonomous, 5)); err != nil {
		t.Fatal(err)
	}
	if got = string(out); !strings.Contains(got, "backlog_cap: 5\n") {
		t.Errorf("editProjectFile = %s, want backlog_cap: 5 unquoted", got)
	}
	if out, err = editProjectFile(out, autonomyEdit(policy.AutonomyManual, 0)); err != nil {
		t.Fatal(err)
	}
	if got = string(out); !strings.Contains(got, "autonomy: manual") || !strings.Contains(got, "backlog_cap: 5\n") {
		t.Errorf("editProjectFile = %s, want manual with cap 5 kept", got)
	}
	if out, err = editProjectFile(out, autonomyEdit(policy.AutonomyManual, policy.DefaultBacklogCap)); err != nil {
		t.Fatal(err)
	}
	if got = string(out); strings.Contains(got, "backlog_cap") {
		t.Errorf("editProjectFile kept the cap at the default:\n%s", got)
	}
}

// The four states in the project file (C14): full auto is autonomous with
// merge_rule auto, autonomous takes merge_rule human, assisted and manual
// keep the rule.
func TestEditProjectFileStates(t *testing.T) {
	raw := []byte("repository: sf/code\nmerge_rule: auto\n")
	for _, tt := range []struct{ state, want string }{
		{policy.AutonomyAssisted, "repository: sf/code\nmerge_rule: auto\nautonomy: assisted\n"},
		{policy.AutonomyAutonomous, "repository: sf/code\nmerge_rule: human\nautonomy: autonomous\n"},
		{policy.AutonomyFullAuto, "repository: sf/code\nmerge_rule: auto\nautonomy: autonomous\n"},
		{policy.AutonomyManual, "repository: sf/code\nmerge_rule: auto\nautonomy: manual\n"},
	} {
		out, err := editProjectFile(raw, autonomyEdit(tt.state, 0))
		equal(t, tt.state, []any{string(out), err}, []any{tt.want, error(nil)})
	}
}

// The level control's change (roadmap 239, C14): the project file read
// with the config token and judged the loader's way, then committed by
// the approver on a branch whose pull request they open and merge; one
// event names the change. A file the loader refuses commits nothing.
func TestSetAutonomyFilesAndMergesAsTheApprover(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	infra.files = map[string]string{"factory/projects/demo-app.yaml": "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\n"}
	var checked []string
	r.c.cfg.CheckChange = func(_ context.Context, changes map[string][]byte) ([]string, error) {
		for path, file := range changes {
			checked = append(checked, path+": "+string(file))
		}
		return nil, nil
	}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	rec := r.do(post("/projects/demo-app/autonomy", url.Values{"csrf": {s.csrf}, "autonomy": {"full auto"}}), ck)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST autonomy = %d %s", rec.Code, rec.Body.String())
	}
	file := "# demo\nrepository: demo-app/demo-app\nmerge_rule: auto\nautonomy: autonomous\n"
	equal(t, "the read and the check", []any{infra.reads, checked}, []any{[]string{"Bearer config-token"}, []string{"projects/demo-app.yaml: " + file}})
	if len(infra.contents) != 1 || infra.contents[0].auth != "Bearer access-1" {
		t.Fatalf("contents = %+v", infra.contents)
	}
	body := infra.contents[0].body
	change := body["files"].([]any)[0].(map[string]any)
	content, _ := base64.StdEncoding.DecodeString(change["content"].(string))
	branch, _ := body["new_branch"].(string)
	if body["branch"] != "main" || !strings.HasPrefix(branch, "config-demo-app-") || body["message"] != "Project demo-app: autonomy full auto\n\nFactory-Role: console\nRequested-by: sebastian\n" ||
		change["operation"] != "update" || change["path"] != "factory/projects/demo-app.yaml" || change["sha"] != "blob1" || string(content) != file {
		t.Fatalf("the commit = %+v (%q)", body, content)
	}
	if len(infra.pulls) != 1 || infra.pulls[0].auth != "Bearer access-1" || infra.pulls[0].body["head"] != branch || infra.pulls[0].body["title"] != "Project demo-app: autonomy full auto" {
		t.Fatalf("pulls = %+v", infra.pulls)
	}
	if len(infra.merges) != 1 || infra.merges[0].auth != "Bearer access-1" {
		t.Fatalf("merges = %+v", infra.merges)
	}
	rows := r.events.all()[before:]
	if len(rows) != 1 {
		t.Fatalf("%d events, want 1: %+v", len(rows), rows)
	}
	equal(t, "the event", rows[0], factoryd.Event{ID: rows[0].ID, At: rows[0].At, Repository: "demo-app/demo-app", Kind: factoryd.EventAutonomyChanged,
		Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"from": "manual", "to": "full auto", "pr": int64(12)}})

	r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) {
		return []string{"projects/demo-app.yaml: autonomy: no"}, nil
	}
	rec = r.do(post("/projects/demo-app/autonomy", url.Values{"csrf": {s.csrf}, "autonomy": {"assisted"}}), ck)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "The project file would not load: projects/demo-app.yaml: autonomy: no") {
		t.Fatalf("a refused file = %d %s", rec.Code, rec.Body.String())
	}
	if len(infra.contents) != 1 {
		t.Fatalf("a refused file committed: %+v", infra.contents)
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
