package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// readsTimeline is t-r's timeline as factoryd writes it: one session that
// pushed c0ffee1, two checks runs (the newest 1/2), two model reviews and
// an audit.
const readsTimeline = `{"step":"session_start","time":"2026-10-11T09:00:00Z","session":"s-1","attempt":1,"model":"coding-cheap"}
{"step":"checks","time":"2026-10-11T09:20:00Z","started":"2026-10-11T09:15:00Z","commit":"c0ffee0","state":"failure","jobs":[{"name":"test","status":"failure","run_s":58}]}
{"step":"model_review","time":"2026-10-11T09:21:00Z","started":"2026-10-11T09:20:00Z","commit":"c0ffee0","verdict":"concerns","summary":"the cache leaks","findings":["cache.go: evict","cache.go: bound","cache_test.go: test"],"round":1}
{"step":"pushed","time":"2026-10-11T09:40:00Z","session":"s-1","branch":"factory/t-r","commit":"c0ffee1"}
{"step":"session_end","time":"2026-10-11T09:40:01Z","session":"s-1","attempt":1,"outcome":"pushed","started":"2026-10-11T09:00:00Z","commit":"c0ffee1"}
{"step":"checks","time":"2026-10-11T09:45:00Z","started":"2026-10-11T09:41:00Z","commit":"c0ffee1","state":"failure","jobs":[{"name":"test","status":"success","run_s":61,"queue_s":3},{"name":"vet","status":"failure","run_s":12},{"name":"deploy","status":"skipped","run_s":0}]}
{"step":"model_review","time":"2026-10-11T09:46:00Z","started":"2026-10-11T09:45:00Z","commit":"c0ffee1","verdict":"concerns","summary":"the key ignores the tenant","findings":["cache.go:12: key the entries by tenant","cache_test.go: add a test of two tenants"],"note":"read in one part","round":2}
{"step":"audit","time":"2026-10-11T09:47:00Z","started":"2026-10-11T09:46:00Z","commit":"c0ffee1","verdict":"clear"}
`

// newReadsRig is the chat rig with what the reads read. Plan card t-p #20
// holds revision 1 of three cards, of which api is filed as #21 t-c1
// (running, three sessions spent of its cap of four) and web as #22 t-c2
// (waiting on #21, after #19 t-d, done); t-r #11 has spent €1.23 of
// €5.00 and one session, and its pull request #9 the timeline
// readsTimeline.
func newReadsRig(t *testing.T) (*rig, *chatGateway) {
	r, _, g := newChatRig(t)
	plan, err := json.Marshal(factoryd.Plan{Summary: "An API, then its web UI.", Cards: []factoryd.PlanCard{
		{Key: "api", Role: "foundation", Title: "API layer", Size: "small"},
		{Key: "web", Role: "feature", Title: "Web UI", Size: "medium", DependsOn: []string{"api"}},
		{Key: "accept", Role: "integration", Title: "Acceptance tests", Size: "small"}}})
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	for i := range r.tasks.tasks {
		if k := &r.tasks.tasks[i]; k.ID == "t-r" {
			k.BudgetEUR, k.SpentEUR, k.Usage.Attempts = 5, 1.23, 1
		}
	}
	for _, k := range []factoryd.Task{
		{ID: "t-p", Status: "done", Lane: policy.LanePlan, Process: "spec", IssueNumber: 20, Objective: "Build the link shortener."},
		{ID: "t-d", Status: "done", IssueNumber: 19, Objective: "Add the schema."},
		{ID: "t-c1", Status: "running", Gate: factoryd.GateSession, Lane: policy.LaneReady, Process: "maintenance", IssueNumber: 21,
			Source: factoryd.SourcePlan, Parent: "t-p", PlanKey: "api", BudgetEUR: 2, SpentEUR: 0.5,
			Usage: factoryd.TaskUsage{Attempts: 3, ExtraAttempts: 1}, Objective: "Build the API."},
		{ID: "t-c2", Status: "waiting-deps", Lane: policy.LaneReady, Process: "maintenance", IssueNumber: 22,
			Source: factoryd.SourcePlan, Parent: "t-p", PlanKey: "web", BudgetEUR: 2, Objective: "Build the web UI."},
	} {
		k.Kind, k.Repository = "agent", "sf/code"
		r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: k})
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-p": {{TaskID: "t-p", Revision: 1, Plan: plan, SHA256: "5e1f"}}}
	r.tasks.deps = map[string][]string{"t-c2": {"t-d", "t-c1"}}
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(readsTimeline)
	return r, g
}

// reads runs the calls (name, arguments, ...) in one round of a chat
// turn and answers what the agent reads of each; a read records no event.
func reads(t *testing.T, r *rig, g *chatGateway, nameArgs ...string) []string {
	t.Helper()
	ck, s := r.loggedIn(t)
	before := r.events.count()
	g.replies = [][]string{{call(1, nameArgs...)}, {say("Read.")}}
	if code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"read"}}); code != http.StatusOK || events[len(events)-1] != "done {}" {
		t.Fatalf("turn = %d %v", code, events)
	}
	chatRows(t, r.events, before, "sebastian")
	calls := g.calls()
	msgs := messages(t, calls[len(calls)-1])
	var out []string
	for _, m := range msgs[len(msgs)-len(nameArgs)/2:] {
		equal(t, "the message's role", m["role"], "tool")
		out = append(out, m["content"].(string))
	}
	return out
}

// refused runs tool name on in as the rig's member, outside a chat turn,
// and answers its refusal's status and sentence.
func refused(t *testing.T, r *rig, name string, in toolInput) (int, string) {
	t.Helper()
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, _ := r.c.chatTool(name)
	_, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, in)
	var no *refusal
	if !errors.As(err, &no) {
		t.Fatalf("%s %+v = %v, want a refusal", name, in, err)
	}
	return no.status, no.msg
}

// The plan read is a plan's newest revision: its number and sha256, its
// summary, each card's row with the dependencies factoryd adds and its
// issue once filed, and each filed card's state, gate and spend; a card
// of the plan reads its plan's; any other card is refused, and so is a
// plan with no revision yet.
func TestChatPlanRead(t *testing.T) {
	r, g := newReadsRig(t)
	plan := "revision 1, sha256 5e1f\nsummary: An API, then its web UI.\n" +
		`api foundation "API layer" small depends on none issue #21` + "\n" +
		`web feature "Web UI" medium depends on api issue #22` + "\n" +
		`accept integration "Acceptance tests" small depends on api, web not filed` + "\n" +
		`#21 t-c1 "api" running (runs an agent session), gate session (an agent works in a sandbox), €0.50 of €2.00` + "\n" +
		`#22 t-c2 "web" waiting-deps (waits for the cards it depends on), €0.00 of €2.00`
	equal(t, "the plan reads", reads(t, r, g, "plan", `{"task_id":"t-p"}`, "plan", `{"task_id":"t-c1"}`, "plan", `{"task_id":"t-m"}`),
		[]string{plan, "card of plan #20\n" + plan, "Refused: Card #5 is no plan and belongs to none."})
	status, _ := refused(t, r, "plan", toolInput{TaskID: "t-m"})
	equal(t, "the refusal's status", status, http.StatusConflict)
	r.tasks.mu.Lock()
	r.tasks.plans = nil
	r.tasks.mu.Unlock()
	status, msg := refused(t, r, "plan", toolInput{TaskID: "t-c2"})
	equal(t, "a plan without a revision", []any{status, msg}, []any{http.StatusConflict, "Plan #20 has no revision yet."})
}

// The card read is the card in full: its state, gate and spend, lane,
// issue, sessions of its cap, the cards it depends on (the open ones
// first) and those that depend on it, its pull request with its head and
// state, and pointers to the reads holding the reviews, the audit, the
// checks, the plan and the sessions, before its objective.
func TestChatCardInFull(t *testing.T) {
	r, g := newReadsRig(t)
	equal(t, "the card reads", reads(t, r, g, "card", `{"task_id":"t-r"}`, "card", `{"task_id":"t-c2"}`, "card", `{"task_id":"t-c1"}`), []string{
		`#11 t-r "Speed up the board" running (runs an agent session), gate session (an agent works in a sandbox), €1.23 of €5.00` + "\n" +
			"lane ready of process maintenance\nissue https://git.example/sf/code/issues/11\nsessions: 1 of 3\n" +
			"pull request #9 https://git.example/sf/code/pulls/9, head c0ffee1, open\n" +
			"reviews: concerns (2 findings): see pr\naudit: clear: see pr\nchecks: 1/2: see pr\nsessions: 1: see session\n" +
			"objective: Speed up the board.",
		`#22 t-c2 "web" waiting-deps (waits for the cards it depends on), €0.00 of €2.00` + "\n" +
			"lane ready of process maintenance\nissue https://git.example/sf/code/issues/22\nsessions: 0 of 3\n" +
			"depends on #21 (runs an agent session)\ndepends on #19 (is done)\nplan: see plan\nobjective: Build the web UI.",
		`#21 t-c1 "api" running (runs an agent session), gate session (an agent works in a sandbox), €0.50 of €2.00` + "\n" +
			"lane ready of process maintenance\nissue https://git.example/sf/code/issues/21\nsessions: 3 of 4\n" +
			"needed by #22 (waits for the cards it depends on)\nplan: see plan\nobjective: Build the API.",
	})
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(readsTimeline + `{"step":"signal","time":"2026-10-11T10:00:00Z","signal":"pr_merged","author":"sebastian"}` + "\n")
	if got := reads(t, r, g, "card", `{"task_id":"t-r"}`)[0]; !strings.Contains(got, "\npull request #9 https://git.example/sf/code/pulls/9, head c0ffee1, merged\n") {
		t.Errorf("the card of a merged pull request reads\n%s", got)
	}
}

// The pr read is a card's pull request: the newest checks' jobs, the
// newest model review in full and the earlier ones' verdicts, the newest
// audit with its findings, the files with their totals, and with with_diff
// the diff; a card without one is refused.
func TestChatPRRead(t *testing.T) {
	r, g := newReadsRig(t)
	head := "pull request #9 of card #11\nchecks: failure\n  test success 61 s\n  vet failure 12 s\n  deploy skipped 0 s\n" +
		"review round 2: concerns: the key ignores the tenant\n  1. cache.go:12: key the entries by tenant\n" +
		"  2. cache_test.go: add a test of two tenants\n  note: read in one part\nreview round 1: concerns\n"
	files := "a.go +40 -3\nb.go +5 -5\n2 files, +45 -8"
	equal(t, "the pr reads", reads(t, r, g, "pr", `{"task_id":"t-r"}`, "pr", `{"task_id":"t-r","with_diff":true}`, "pr", `{"task_id":"t-m"}`), []string{
		head + "audit: clear\n" + files,
		head + "audit: clear\n" + files + "\n\ndiff --git a/cache.go b/cache.go\n-\treturn nil\n+\treturn c.entries[k]\n",
		"Refused: Card #5 has no pull request.",
	})
	status, _ := refused(t, r, "pr", toolInput{TaskID: "t-m"})
	equal(t, "the refusal's status", status, http.StatusConflict)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-big", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 30, PRNumber: 32}})
	r.tasks.mu.Unlock()
	big := "pull request #32 of card #30\nchecks: none\nreview: none\naudit: none"
	for page := 1; page <= filesPages; page++ {
		for i := range 50 {
			big += fmt.Sprintf("\ngen/p%d-%d.go +1 -0", page, i)
		}
	}
	equal(t, "the pr read of more files than the drawer reads", reads(t, r, g, "pr", `{"task_id":"t-big"}`),
		[]string{big + "\n500 files, +500 -0 (more files than listed)"})

	r.objects["sessions/t-r/timeline.jsonl"] = []byte(readsTimeline + `{"step":"audit","time":"2026-10-11T09:50:00Z","commit":"c0ffee1","verdict":"findings","findings":[` +
		`{"item":"secret in the diff: private key","file":"x_test.go","line":14,"why":"an added line has the shape of a private key"},` +
		`{"item":"the audit gave no verdict","why":"the answer is not one JSON object"}]}` + "\n")
	equal(t, "the pr read after a second audit", reads(t, r, g, "pr", `{"task_id":"t-r"}`), []string{
		head + "audit: findings\n  x_test.go:14 secret in the diff: private key: an added line has the shape of a private key\n" +
			"  the audit gave no verdict: the answer is not one JSON object\n" + files,
	})
}

// The file read is a file of the project's repository at a ref, read raw
// with each path segment escaped and the path cleaned: cut at 64 KiB with
// its size, a binary file and a path out of the repository refused.
func TestChatFileRead(t *testing.T) {
	r, g := newReadsRig(t)
	equal(t, "the file reads", reads(t, r, g,
		"file", `{"ref":"main","path":"README.md"}`,
		"file", `{"ref":"main","path":"bin/x"}`,
		"file", `{"ref":"main","path":"missing.txt"}`,
		"file", `{"ref":"main","path":"../x"}`,
		"file", `{"ref":"main","path":"big.txt"}`,
		"file", `{"ref":"main","path":"/docs/./a b.md"}`,
		"file", `{"ref":"main","path":"huge.txt"}`,
		"file", `{"path":"README.md"}`,
	), []string{
		strings.Repeat("r", 1200),
		"Refused: bin/x at main is binary (100 bytes).",
		"Refused: There is no missing.txt at main.",
		"Refused: The path ../x leaves the repository.",
		strings.Repeat("g", 64<<10) + "\n(cut at 64 KiB of 71680)",
		"spaced",
		strings.Repeat("h", 64<<10) + "\n(cut at 64 KiB of at least 1048576)",
		"Refused: The file read needs a ref and a path.",
	})
	for _, c := range []struct {
		in     toolInput
		status int
	}{
		{toolInput{Ref: "main", Path: "../x"}, http.StatusBadRequest},
		{toolInput{Ref: "main", Path: "a/../../x"}, http.StatusBadRequest},
		{toolInput{Ref: "main", Path: "."}, http.StatusBadRequest},
		{toolInput{Ref: "main"}, http.StatusBadRequest},
		{toolInput{Ref: "main", Path: "missing.txt"}, http.StatusNotFound},
		{toolInput{Ref: "dev", Path: "README.md"}, http.StatusNotFound},
	} {
		if status, msg := refused(t, r, "file", c.in); status != c.status {
			t.Errorf("file %+v refused %d %q, want %d", c.in, status, msg, c.status)
		}
	}
}

// The reads run at once and act on nothing: none is a write, and none is
// in the default confirm list.
func TestChatReadsAreNeverProposed(t *testing.T) {
	for _, name := range []string{"plan", "card", "pr", "file"} {
		tool, ok := (&Console{}).chatTool(name)
		if !ok || tool.write || slices.Contains(policy.DefaultChat.Confirm, name) {
			t.Errorf("tool %s: offered %v, write %v, confirmed by default %v; want a read run at once", name, ok, tool.write, slices.Contains(policy.DefaultChat.Confirm, name))
		}
	}
}
