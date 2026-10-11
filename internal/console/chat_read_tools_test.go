package console

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func makePlanJSON() json.RawMessage {
	p := factoryd.Plan{
		Summary: "test plan",
		Cards: []factoryd.PlanCard{
			{Key: "api", Role: "foundation", Title: "API layer", Size: "small"},
			{Key: "web", Role: "feature", Title: "Web UI", Size: "medium", DependsOn: []string{"api"}},
			{Key: "accept", Role: "integration", Title: "Acceptance tests", Size: "small"},
		},
	}
	b, _ := json.Marshal(p)
	return b
}

func newReadToolsRig(t *testing.T) (*rig, *fakeChat, *chatGateway) {
	r, chat, g := newChatRig(t)
	planRev := factoryd.PlanRevision{
		TaskID:    "t-p",
		Revision:  1,
		SHA256:    "abc123",
		Plan:      makePlanJSON(),
		CreatedAt: time.Now(),
	}
	r.tasks.mu.Lock()
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-p": {planRev}}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{
			ID: "t-p", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 20, Objective: "Plan the feature", Source: factoryd.SourcePlan,
		}},
		factoryd.BoardTask{Task: factoryd.Task{
			ID: "t-c1", Kind: "agent", Status: "running", Lane: policy.LaneReady, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 21, Parent: "t-p", Source: factoryd.SourcePlan, PlanKey: "api",
			Objective: "Build API",
		}},
		factoryd.BoardTask{Task: factoryd.Task{
			ID: "t-c2", Kind: "agent", Status: "waiting-deps", Lane: policy.LaneReady, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 22, Parent: "t-p", Source: factoryd.SourcePlan, PlanKey: "web",
			Objective: "Build Web UI",
		}},
	)
	r.tasks.deps = map[string][]string{"t-c2": {"t-c1"}}
	r.tasks.mu.Unlock()

	timeline := `{"step":"checks","time":"2026-10-11T10:00:00Z","started":"2026-10-11T09:58:00Z","jobs":[{"name":"test","status":"success","run_seconds":61},{"name":"vet","status":"failure","run_seconds":12}]}
{"step":"model_review","time":"2026-10-11T10:01:00Z","verdict":"concerns","summary":"two issues found","findings":[{"file":"a.go","line":10,"text":"null check missing"},{"file":"b.go","line":20,"text":"error not handled"}],"note":"review note"}
{"step":"audit","time":"2026-10-11T10:02:00Z","verdict":"clear","findings":[]}
`
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(timeline)

	r.fg.pullFiles = map[int64][]prFile{
		9: {
			{Filename: "a.go", Status: "modified", Additions: 40, Deletions: 3},
			{Filename: "b.go", Status: "modified", Additions: 5, Deletions: 5},
		},
	}
	r.fg.contents = map[string]contentAnswer{
		"main:README.md": {body: strings.Repeat("x", 1200), size: 1200},
		"main:bin/x":     {body: "\x00" + strings.Repeat("y", 99), size: 100, binary: true},
	}

	return r, chat, g
}

type contentAnswer struct {
	body   string
	size   int
	binary bool
	notFound bool
}

func TestPlanToolOnPlanCard(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "plan", `{"task_id":"t-p"}`)}, {say("done")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show plan"}})
	calls := g.calls()
	if len(calls) < 2 {
		t.Fatalf("expected 2 model calls, got %d", len(calls))
	}
	msgs := messages(t, calls[1])
	toolMsg := msgs[len(msgs)-1]
	content, _ := toolMsg["content"].(string)
	if !strings.Contains(content, "api foundation") || !strings.Contains(content, "small") || !strings.Contains(content, "issue #21") {
		t.Errorf("plan result missing api row: %s", content)
	}
	if !strings.Contains(content, "web feature") || !strings.Contains(content, "depends on api") || !strings.Contains(content, "issue #22") {
		t.Errorf("plan result missing web row: %s", content)
	}
	if !strings.Contains(content, "accept integration") || !strings.Contains(content, "not filed") {
		t.Errorf("plan result missing accept row: %s", content)
	}
	if !strings.Contains(content, "revision 1") {
		t.Errorf("plan result missing revision: %s", content)
	}
	if !strings.Contains(content, "#21") || !strings.Contains(content, "runs an agent session") {
		t.Errorf("plan result missing child #21 line: %s", content)
	}
	if !strings.Contains(content, "#22") || !strings.Contains(content, "waits for the cards it depends on") {
		t.Errorf("plan result missing child #22 line: %s", content)
	}
}

func TestPlanToolOnChildCard(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "plan", `{"task_id":"t-c1"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show plan of child"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "card of plan #20") {
		t.Errorf("child plan result missing 'card of plan #20': %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "api foundation") {
		t.Errorf("child plan result missing plan rows: %s", toolEvent)
	}
}

func TestPlanToolOnNonPlanCard(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "plan", `{"task_id":"t-m"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show plan of non-plan"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "no plan and belongs to none") {
		t.Errorf("non-plan result missing refusal: %s", toolEvent)
	}
}

func TestCardToolFull(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "card", `{"task_id":"t-r"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show card"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "sessions:") {
		t.Errorf("card result missing sessions: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "€") {
		t.Errorf("card result missing spend/budget: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "pull request #9") {
		t.Errorf("card result missing pull request: %s", toolEvent)
	}
}

func TestCardToolDependencies(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "card", `{"task_id":"t-c2"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show card with deps"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "depends on #21") || !strings.Contains(toolEvent, "runs an agent session") {
		t.Errorf("card result missing dependency info: %s", toolEvent)
	}
}

func TestPRTool(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "pr", `{"task_id":"t-r"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show pr"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "test success 61 s") {
		t.Errorf("pr result missing test job: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "vet failure 12 s") {
		t.Errorf("pr result missing vet job: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "concerns") {
		t.Errorf("pr result missing review verdict: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "1.") || !strings.Contains(toolEvent, "2.") {
		t.Errorf("pr result missing numbered findings: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "audit: clear") {
		t.Errorf("pr result missing audit: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "a.go +40 -3") {
		t.Errorf("pr result missing file stat: %s", toolEvent)
	}
	if !strings.Contains(toolEvent, "2 files") {
		t.Errorf("pr result missing totals: %s", toolEvent)
	}
}

func TestPRToolNoPR(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "pr", `{"task_id":"t-m"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show pr of no-pr card"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "has no pull request") {
		t.Errorf("pr result missing refusal: %s", toolEvent)
	}
}

func TestFileTool(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "file", `{"ref":"main","path":"README.md"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show file"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "xxxx") {
		t.Errorf("file result missing content: %s", toolEvent)
	}
}

func TestFileToolBinary(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "file", `{"ref":"main","path":"bin/x"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show binary"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "binary") {
		t.Errorf("file result missing binary refusal: %s", toolEvent)
	}
}

func TestFileToolMissing(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "file", `{"ref":"main","path":"missing.txt"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show missing"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "There is no missing.txt at main") {
		t.Errorf("file result missing not-found message: %s", toolEvent)
	}
}

func TestFileToolPathTraversal(t *testing.T) {
	r, _, g := newReadToolsRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "file", `{"ref":"main","path":"../x"}`)}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show traversal"}})
	toolEvent := events[0]
	if !strings.Contains(toolEvent, "Refused") {
		t.Errorf("file result missing path traversal refusal: %s", toolEvent)
	}
}
