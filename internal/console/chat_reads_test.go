package console

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// readsTimeline is t-r's timeline as factoryd writes it: one session that
// pushed c0ffee1, two checks runs (the newest 1/2), two model reviews and
// an audit.
const readsTimeline = `{"step":"session_start","time":"2026-10-11T09:00:00Z","session":"s-1","attempt":1,"model":"small"}
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
		{ID: "t-p", Status: "done", Track: policy.TrackPlan, Process: "spec", IssueNumber: 20, Objective: "Build the link shortener."},
		{ID: "t-d", Status: "done", IssueNumber: 19, Objective: "Add the schema."},
		{ID: "t-c1", Status: "running", Gate: factoryd.GateSession, Track: policy.TrackDirect, Process: "maintenance", IssueNumber: 21,
			Source: factoryd.SourcePlan, Parent: "t-p", PlanKey: "api", BudgetEUR: 2, SpentEUR: 0.5,
			Usage: factoryd.TaskUsage{Attempts: 3, ExtraAttempts: 1}, Objective: "Build the API."},
		{ID: "t-c2", Status: "waiting-deps", Track: policy.TrackDirect, Process: "maintenance", IssueNumber: 22,
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

// refused runs tool name on in as the rig's member, outside a chat turn
// but with the project's chat block, and answers its refusal's status and
// sentence.
func refused(t *testing.T, r *rig, name string, in toolInput) (int, string) {
	t.Helper()
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, _ := r.c.chatTool(name)
	v, _ := r.c.inForce(req.Context())
	_, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, cfg: v.Document.ChatOf(v.Projects["ai-software-factory-code"])}, in)
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
	plan := "revision 1, sha256 5e1f\nsummary: An API, then its web UI.\n" + readsPlanRows
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

// A revision that takes merged cards off main names them after its
// summary.
func TestChatPlanReadTakesOffMain(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	var p factoryd.Plan
	if err := json.Unmarshal(r.tasks.plans["t-p"][0].Plan, &p); err != nil {
		t.Fatal(err)
	}
	p.TakeOffMain = []string{"old-api"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.plans["t-p"][0].Plan = raw
	r.tasks.mu.Unlock()
	equal(t, "the plan read", reads(t, r, g, "plan", `{"task_id":"t-p"}`),
		[]string{"revision 1, sha256 5e1f\nsummary: An API, then its web UI.\ntakes off main: old-api\n" + readsPlanRows})
}

// A plan in its second round lists its rounds after its summary: a round
// without a card has no line, the current one is marked, then the parts
// still to plan.
func TestChatPlanReadNamesTheRound(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	var p factoryd.Plan
	if err := json.Unmarshal(r.tasks.plans["t-p"][0].Plan, &p); err != nil {
		t.Fatal(err)
	}
	p.Round, p.Outline = 2, []string{"the console"}
	for i := range p.Cards {
		p.Cards[i].Round = 2
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.plans["t-p"][0].Plan = raw
	r.tasks.mu.Unlock()
	equal(t, "the plan read", reads(t, r, g, "plan", `{"task_id":"t-p"}`),
		[]string{"revision 1, sha256 5e1f\nsummary: An API, then its web UI.\nrounds:\n- round 2: 2 cards, 0 done. This round.\n- still to plan: the console\n" + readsPlanRows})
}

// readsPlanRows are plan t-p's rows and its filed cards as the plan read
// lists them.
const readsPlanRows = `api foundation "API layer" small depends on none issue #21` + "\n" +
	`web feature "Web UI" medium depends on api issue #22` + "\n" +
	`accept integration "Acceptance tests" small depends on api, web not filed` + "\n" +
	`#21 t-c1 "api" running (runs an agent session), gate session (an agent works in a sandbox), €0.50 of €2.00` + "\n" +
	`#22 t-c2 "web" waiting-deps (waits for the cards it depends on), €0.00 of €2.00`

// A revision without questions of its own lists the answers of the
// newest earlier revision with answered ones, which the planner built it
// from; a revision with its own questions lists only those.
func TestChatPlanBuiltFromAnswers(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	second, third := r.tasks.plans["t-p"][0], r.tasks.plans["t-p"][0]
	second.Revision, second.SHA256 = 2, "6a2b"
	third.Revision, third.SHA256 = 3, "7c3d"
	r.tasks.plans["t-p"] = append(r.tasks.plans["t-p"], second)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": {
		{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}, Answer: "Postgres", AnsweredBy: "sebastian"},
		{Text: "How long are links kept?", Choices: []string{"30 days", "forever"}, Answer: "30 days", AnsweredBy: "alice"}}}
	r.tasks.mu.Unlock()
	const summary = "summary: An API, then its web UI.\n"
	const built = "built from the answers of revision 1:\n1. Which database? -> Postgres (sebastian)\n2. How long are links kept? -> 30 days (alice)\n"
	equal(t, "revision 2 without questions", reads(t, r, g, "plan", `{"task_id":"t-p"}`),
		[]string{"revision 2, sha256 6a2b\n" + summary + built + readsPlanRows})
	r.tasks.mu.Lock()
	r.tasks.plans["t-p"] = append(r.tasks.plans["t-p"], third)
	r.tasks.mu.Unlock()
	equal(t, "revision 3 after a revision 2 without questions", reads(t, r, g, "plan", `{"task_id":"t-p"}`),
		[]string{"revision 3, sha256 7c3d\n" + summary + built + readsPlanRows})
	r.tasks.mu.Lock()
	r.tasks.questions["t-p 3"] = []factoryd.PlanQuestion{{Text: "Which cache?", Choices: []string{"none", "redis"}}}
	r.tasks.mu.Unlock()
	equal(t, "revision 3 with its own question", reads(t, r, g, "plan", `{"task_id":"t-p"}`),
		[]string{"revision 3, sha256 7c3d\n" + summary + "1. Which cache? (choices: none / redis)\n" + readsPlanRows})
}

// The card read is the card in full: its state, gate and spend, lane,
// issue, sessions of its cap, the cards it depends on (the open ones
// first) and those that depend on it, its pull request with its head and
// state, and pointers to the reads holding the reviews, the audit, the
// checks, the plan and the sessions, before its objective.
func TestChatCardInFull(t *testing.T) {
	r, g := newReadsRig(t)
	// t-r's four small calls (trCalls): prompt tokens 30,000 to
	// 52,000, the lower of the middle two 40,000; 130,000 of 168,651 cached
	// (77.1%); 0.0072 EUR over four
	equal(t, "the card reads", reads(t, r, g, "card", `{"task_id":"t-r"}`, "card", `{"task_id":"t-c2"}`, "card", `{"task_id":"t-c1"}`), []string{
		`#11 t-r "Speed up the board." running (runs an agent session), gate session (an agent works in a sandbox), €1.23 of €5.00` + "\n" +
			"track direct of process maintenance\nissue https://git.example/sf/code/issues/11\nsessions: 1 of 3\n" +
			s2Cost + "cost: small, 4 calls, prompt tokens per call median 40,000 max 52,000, 77% cached, 0.0018 EUR per call\n" +
			"pull request #9 https://git.example/sf/code/pulls/9, head c0ffee1, open\n" +
			"reviews: concerns (2 findings): see pr\naudit: clear: see pr\nchecks: 1/2: see pr\nsessions: 1: see session\n" +
			"objective: Speed up the board.",
		`#22 t-c2 "web" waiting-deps (waits for the cards it depends on), €0.00 of €2.00` + "\n" +
			"track direct of process maintenance\nissue https://git.example/sf/code/issues/22\nsessions: 0 of 3\ncost: no model calls\n" +
			"depends on #21 (runs an agent session)\ndepends on #19 (is done)\nplan: see plan\nobjective: Build the web UI.",
		`#21 t-c1 "api" running (runs an agent session), gate session (an agent works in a sandbox), €0.50 of €2.00` + "\n" +
			"track direct of process maintenance\nissue https://git.example/sf/code/issues/21\nsessions: 3 of 4\ncost: no model calls\n" +
			"needed by #22 (waits for the cards it depends on)\nplan: see plan\nobjective: Build the API.",
	})
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(readsTimeline + `{"step":"signal","time":"2026-10-11T10:00:00Z","signal":"pr_merged","author":"sebastian"}` + "\n")
	if got := reads(t, r, g, "card", `{"task_id":"t-r"}`)[0]; !strings.Contains(got, "\npull request #9 https://git.example/sf/code/pulls/9, head c0ffee1, merged\n") {
		t.Errorf("the card of a merged pull request reads\n%s", got)
	}
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(readsTimeline +
		`{"step":"queue_synced","time":"2026-10-11T10:00:00Z","from":"c0ffee1","to":"c0ffee2","main":"0a1b2c3"}` + "\n" +
		`{"step":"signal","time":"2026-10-11T10:01:00Z","signal":"pr_merged","author":"sebastian","pr":10,"ignored":"not the task's pull request"}` + "\n")
	if got := reads(t, r, g, "card", `{"task_id":"t-r"}`)[0]; !strings.Contains(got, "\npull request #9 https://git.example/sf/code/pulls/9, head c0ffee2, open\n") {
		t.Errorf("the card of a pull request the queue synced, with another pull request merged, reads\n%s", got)
	}
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(readsTimeline +
		`{"step":"checks_rerun","time":"2026-10-11T10:00:00Z","commit":"c0ffee1","head":"c0ffee3"}` + "\n")
	if got := reads(t, r, g, "card", `{"task_id":"t-r"}`)[0]; !strings.Contains(got, "\npull request #9 https://git.example/sf/code/pulls/9, head c0ffee3, open\n") {
		t.Errorf("the card of a pull request whose checks ran again on an empty commit reads\n%s", got)
	}
}

// A group revert's agent card reads its own cap of 2 sessions, not the
// policy's 3, in its sessions and at its cap.
func TestChatCardReadsAGroupRevertsCap(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-gr", Kind: "agent", Repository: "sf/code", Status: "waiting-human",
		Track: policy.TrackDirect, Process: "maintenance", IssueNumber: 23, Source: factoryd.SourceGroupRevert, BudgetEUR: 2,
		Usage: factoryd.TaskUsage{Attempts: 2}, Objective: "Revert plan #20: 2 merges, newest first"},
		Escalation: "the session cap is reached: every finding sent to the agent so far goes to a human\nlast: the checks failed"})
	r.tasks.mu.Unlock()
	if got := reads(t, r, g, "card", `{"task_id":"t-gr"}`)[0]; !strings.Contains(got, "\nsessions: 2 of 2\nat its session cap: 2 of 2 sessions used, 0 granted\n") {
		t.Errorf("the group revert's card reads\n%s", got)
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

// longFile is the rig's long.txt: 5,000 lines of 16 bytes each, so the
// 64 KiB cut falls on the boundary after line 4096 and every range reads
// whole lines.
var longFile = func() string {
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "body %010d\n", i)
	}
	return b.String()
}()

// The file read is a file of the project's repository at a ref, read raw
// with each path segment escaped and the path cleaned: cut at 64 KiB with
// its size and the line to read on from, from and to one line-numbered
// window under the same cap, a range off its lines answered with the line
// count, a binary file and a path out of the repository refused.
// A read with an ask answers one digest on the small alias: the digest's
// input holds the ask and the read's text cut to the read's own limit,
// and only the labelled answer enters the thread.
func TestChatFileAskDigests(t *testing.T) {
	r, g := newReadsRig(t)
	equal(t, "the asked file reads", reads(t, r, g,
		"file", `{"ref":"main","path":"README.md","ask":"what is this?"}`,
		"file", `{"ref":"main","path":"big.txt","ask":"what is big.txt?"}`,
	), []string{
		"README.md at main: digest 1",
		"big.txt at main (the first 64.0 KiB of 70.0 KiB): digest 2",
	})
	if len(g.digests) != 2 {
		t.Fatalf("%d digest calls, want two", len(g.digests))
	}
	one := g.digests[0]["messages"].([]any)[0].(map[string]any)
	if one["role"] != "user" || len(g.digests[0]["messages"].([]any)) != 1 {
		t.Fatalf("the digest's message = %v", one)
	}
	if content := one["content"].(string); !strings.Contains(content, "what is this?") || !strings.Contains(content, strings.Repeat("r", 1200)) {
		t.Fatalf("the digest's ask and text = %q", content)
	}
	content := g.digests[1]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if want := len("what is big.txt?") + 2 + (64 << 10); len(content) != want {
		t.Fatalf("the digest's text = %d bytes, want %d: the ask, a break, the 64 KiB cut", len(content), want)
	}
}

// The ask rides diff, read_doc and session_step like file's: one digest
// on the small alias, the labelled answer the tool's result.
func TestChatAskDigestsTheOtherReads(t *testing.T) {
	r, g := newReadsRig(t)
	withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
	r.objects["sessions/t-r/s-1.jsonl"] = []byte(`{"time":"2026-10-11T10:00:10Z","kind":"tool_call","detail":{"tool":"read","status":"completed","args":{"path":"a.go"},"duration_ms":120}}` + "\n")
	equal(t, "the asked reads", reads(t, r, g,
		"diff", `{"task_id":"t-r","ask":"what changed?"}`,
		"session_step", `{"task_id":"t-r","ask":"what did the session do?"}`,
	), []string{
		"the diff of task t-r: digest 1",
		"session 1 of task t-r: digest 2",
	})
	for i, want := range []string{"what changed?\n\ndiff --git", "what did the session do?"} {
		if content := g.digests[i]["messages"].([]any)[0].(map[string]any)["content"].(string); !strings.Contains(content, want) {
			t.Fatalf("digest %d lacks %q: %q", i, want, content)
		}
	}
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, _ := r.c.chatTool("read_doc")
	v, _ := r.c.inForce(req.Context())
	cfg := v.Document.ChatOf(v.Projects["ai-software-factory-code"])
	cfg.DocRepo = "sf/docs"
	out, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, cfg: cfg,
		task: factoryd.ChatTaskID("ai-software-factory-code", time.Now())}, toolInput{Path: "docs/plans/cache.md", Ask: "what is the store?"})
	if err != nil || out != "docs/plans/cache.md: digest 3" {
		t.Fatalf("the asked read_doc = %q, %v", out, err)
	}
	if content := g.digests[2]["messages"].([]any)[0].(map[string]any)["content"].(string); !strings.Contains(content, "what is the store?\n\n# The cache plan") {
		t.Fatalf("the read_doc digest's ask and text = %q", content)
	}
}

// treeJSON is main's recursive tree: two files, a directory entry and a
// file under docs.
const treeJSON = `{"truncated":false,"tree":[
{"path":"cmd","type":"tree"},
{"path":"cmd/app.go","type":"blob","size":312},
{"path":"docs/a.md","type":"blob","size":20},
{"path":"README.md","type":"blob","size":1200}]}`

// tgz is the tarball the archive endpoint answers: name and body a file.
func tgz(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The tree read lists the repository's files at a ref, one line a file,
// directories without lines of their own; a path prefix narrows it; the
// cut at 500 lines names the whole count, Forgejo's own truncation with
// it; and the refusals name a missing ref, an empty one and a path that
// leaves the repository.
func TestChatTreeRead(t *testing.T) {
	r, g := newReadsRig(t)
	r.fg.trees = map[string]string{"main": treeJSON}
	equal(t, "the tree reads", reads(t, r, g,
		"tree", `{"ref":"main"}`,
		"tree", `{"ref":"main","path":"docs"}`,
	), []string{
		"cmd/app.go 312\ndocs/a.md 20\nREADME.md 1200\n",
		"docs/a.md 20\n",
	})
	// a wide, Forgejo-truncated tree: 502 files, the first 500 shown
	wide := `{"truncated":true,"tree":[` + strings.TrimSuffix(strings.Repeat(`{"path":"g/x.go","type":"blob","size":1},`, 502), ",") + `]}`
	r.fg.trees["wide"] = wide
	if got := reads(t, r, g, "tree", `{"ref":"wide"}`)[0]; !strings.HasPrefix(got, "g/x.go 1\n") ||
		!strings.HasSuffix(got, "(cut at 500 of 502 files; Forgejo's tree answer is itself truncated)\n") {
		t.Fatalf("the cut tree = %q", got)
	}
	r.fg.trees["bare"] = `{"truncated":false,"tree":[{"path":"cmd","type":"tree"}]}`
	equal(t, "the empty subtree", reads(t, r, g, "tree", `{"ref":"bare","path":"cmd"}`),
		[]string{"There are no files under cmd at bare."})
	var treeRefusals []string
	var treeStatuses []int
	for _, in := range []toolInput{{}, {Ref: "nope"}, {Ref: "main", Path: "../x"}} {
		code, sentence := refused(t, r, "tree", in)
		treeStatuses = append(treeStatuses, code)
		treeRefusals = append(treeRefusals, sentence)
	}
	equal(t, "the tree refusal statuses", treeStatuses, []int{400, 404, 400})
	equal(t, "the tree refusals", treeRefusals, []string{
		"The tree read needs a ref.",
		"There is no tree at nope.",
		"The path ../x leaves the repository.",
	})
}

// The grep read walks the ref's tarball in memory: one line a match,
// binaries skipped by the NUL sniff, a file's first 64 KiB read, a line
// cut at 200 characters, the walk stopped at 50 matches with the count
// line naming every bound; and the refusals name a missing pattern or
// ref, one that does not compile, a ref with no archive and an archive
// over the read limit.
func TestChatGrepRead(t *testing.T) {
	r, g := newReadsRig(t)
	long := strings.Repeat("x", 150) + "cache" + strings.Repeat("y", 200)
	r.fg.archives = map[string][]byte{"main": tgz(t,
		[2]string{"a.go", "the cache leaks\nplain\ncache hits\n"},
		[2]string{"b.bin", "\x00cache\n"},
		[2]string{"docs/d.md", "cache in docs\n"},
		[2]string{"long.txt", long + "\n"},
		[2]string{"late.txt", strings.Repeat("n\n", 32<<10) + "cache\n"},
	)}
	equal(t, "the greps", reads(t, r, g,
		"grep", `{"pattern":"cache","ref":"main"}`,
		"grep", `{"pattern":"c.che in","ref":"main","path":"docs"}`,
	), []string{
		"a.go:1:the cache leaks\na.go:3:cache hits\ndocs/d.md:1:cache in docs\nlong.txt:1:" + long[:200] +
			"\n(scanned 5 of 5 files, 4 matches)\n",
		"docs/d.md:1:cache in docs\n(scanned 1 of 1 file, 1 match)\n",
	})
	// 51 matches: the 50 shown, the count line naming the cut
	r.fg.archives["many"] = tgz(t, [2]string{"hits.txt", strings.Repeat("cache\n", 51)})
	equal(t, "the match cut", reads(t, r, g, "grep", `{"pattern":"cache","ref":"many"}`),
		[]string{repeatLines("hits.txt", "cache", 50) + "(scanned 1 of 1 file, 50 matches, cut)\n"})
	r.fg.archives["big"] = bytes.Repeat([]byte{0}, 100<<20+1)
	var grepRefusals []string
	var grepStatuses []int
	for _, in := range []toolInput{
		{Ref: "main"},
		{Pattern: "(", Ref: "main"},
		{Pattern: "cache", Ref: "nope"},
		{Pattern: "cache", Ref: "main", Path: "../x"},
		{Pattern: "cache", Ref: "big"},
	} {
		code, sentence := refused(t, r, "grep", in)
		grepStatuses = append(grepStatuses, code)
		grepRefusals = append(grepRefusals, sentence)
	}
	equal(t, "the grep refusal statuses", grepStatuses, []int{400, 400, 404, 400, 413})
	equal(t, "the grep refusals", grepRefusals, []string{
		"The grep read needs a pattern and a ref.",
		"The grep pattern \"(\" does not compile: error parsing regexp: missing closing ): `(`.",
		"There is no archive at nope.",
		"The path ../x leaves the repository.",
		"The archive of big is over the grep read's limit of 100.0 MiB.",
	})
}

// repeatLines is count match lines path:1..count of text.
func repeatLines(path, text string, count int) string {
	var b strings.Builder
	for i := range count {
		fmt.Fprintf(&b, "%s:%d:%s\n", path, i+1, text)
	}
	return b.String()
}

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
		"file", `{"ref":"main","path":"long.txt"}`,
		"file", `{"path":"README.md"}`,
		"file", `{"ref":"main","path":"//README.md"}`,
	), []string{
		strings.Repeat("r", 1200),
		"Refused: bin/x at main is binary (100 bytes).",
		"Refused: There is no missing.txt at main.",
		"Refused: The path ../x leaves the repository.",
		strings.Repeat("g", 64<<10) + "\n(cut at 64 KiB of 71680 bytes; read on from line 1)",
		"spaced",
		strings.Repeat("h", 64<<10) + "\n(cut at 64 KiB of at least 1048576 bytes; read on from line 1)",
		longFile[:64<<10] + "\n(cut at 64 KiB of 80000 bytes; read on from line 4097)",
		"Refused: The file read needs a ref and a path.",
		strings.Repeat("r", 1200),
	})
	longHead := "long.txt at main: 80000 bytes, 5000 lines."
	var window, tail string
	for i := 1; i <= 4369; i++ {
		window += fmt.Sprintf("line %d: body %010d\n", i, i)
	}
	window += "… a narrower range fits: raise from or lower to.\n"
	for i := 4998; i <= 5000; i++ {
		tail += fmt.Sprintf("line %d: body %010d\n", i, i)
	}
	equal(t, "the file reads its ranges", reads(t, r, g,
		"file", `{"ref":"main","path":"long.txt","from":4097,"to":4099}`,
		"file", `{"ref":"main","path":"long.txt","from":4998}`,
		"file", `{"ref":"main","path":"long.txt","from":1,"to":5000}`,
		"file", `{"ref":"main","path":"long.txt","from":30,"to":10}`,
		"file", `{"ref":"main","path":"long.txt","from":5001}`,
		"file", `{"ref":"main","path":"big.txt","from":1}`,
	), []string{
		longHead + "\n\nline 4097: body 0000004097\nline 4098: body 0000004098\nline 4099: body 0000004099\n",
		longHead + "\n\n" + tail,
		longHead + "\n\n" + window,
		longHead + " from 30 and to 10 name no line range of its 5000 lines: name from at or before to, inside lines 1 to 5000.",
		longHead + " from 5001 and to 0 name no line range of its 5000 lines: name from at or before to, inside lines 1 to 5000.",
		"big.txt at main: 71680 bytes, 1 lines.\n\n… a narrower range fits: raise from or lower to.\n",
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

// longDiff is the rig's long pull request diff: a header line of 31 bytes
// and 5,000 lines of 16, so the 20 KB cut falls inside line 1280 and every
// range reads whole lines.
var longDiff = func() string {
	var b strings.Builder
	b.WriteString("diff --git a/long.go b/long.go\n")
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "body %010d\n", i)
	}
	return b.String()
}()

// The diff read is a card's pull request diff, short read whole, long cut
// at 20 KB with its size and the line to read on from, from and to one
// line-numbered window under the same cap, a range off its lines answered
// with the line count.
func TestChatDiffRead(t *testing.T) {
	r, g := newReadsRig(t)
	equal(t, "the short diff reads", reads(t, r, g, "diff", `{"task_id":"t-r"}`), []string{
		"diff --git a/cache.go b/cache.go\n-\treturn nil\n+\treturn c.entries[k]\n",
	})
	r.fg.mu.Lock()
	r.fg.pullDiff = longDiff
	r.fg.mu.Unlock()
	equal(t, "the long diff reads", reads(t, r, g, "diff", `{"task_id":"t-r"}`), []string{
		longDiff[:20<<10] + "\n(cut at 20 KB of 80031 bytes; read on from line 1280)",
	})
	head := "card #11's diff: 80031 bytes, 5001 lines."
	window := "line 1: diff --git a/long.go b/long.go\n"
	for i := 2; i <= 1364; i++ {
		window += fmt.Sprintf("line %d: body %010d\n", i, i-1)
	}
	window += "… a narrower range fits: raise from or lower to.\n"
	equal(t, "the diff reads its ranges", reads(t, r, g,
		"diff", `{"task_id":"t-r","from":1280,"to":1282}`,
		"diff", `{"task_id":"t-r","from":5000}`,
		"diff", `{"task_id":"t-r","from":1,"to":5001}`,
		"diff", `{"task_id":"t-r","from":30,"to":10}`,
		"diff", `{"task_id":"t-r","from":5002}`,
	), []string{
		head + "\n\nline 1280: body 0000001279\nline 1281: body 0000001280\nline 1282: body 0000001281\n",
		head + "\n\nline 5000: body 0000004999\nline 5001: body 0000005000\n",
		head + "\n\n" + window,
		head + " from 30 and to 10 name no line range of its 5001 lines: name from at or before to, inside lines 1 to 5001.",
		head + " from 5002 and to 0 name no line range of its 5001 lines: name from at or before to, inside lines 1 to 5001.",
	})
}

// ciHead is main's head in the fake Forgejo.
var ciHead = "abc1234" + strings.Repeat("0", 33)

// The ci read is a commit's combined status, main's head unless a sha
// names another: its state, then each status in Forgejo's order with its
// description and target link, read as the member; a sha skips the
// branch read, and a bad ref or sha is refused before any read.
func TestChatCIRead(t *testing.T) {
	r, g := newReadsRig(t)
	r.fg.mu.Lock()
	r.fg.statuses = map[string]string{
		ciHead: `{"state":"failure","statuses":[{"context":"ci / checks","status":"success"},` +
			`{"context":"ci / smoke","status":"failure","description":"Job 'smoke' failed"}]}`,
		"def5678": `{"state":"success","statuses":[{"context":"ci / checks","status":"success","target_url":"https://git.example/sf/code/actions/runs/7/jobs/0"}]}`,
	}
	r.fg.mu.Unlock()
	equal(t, "the ci reads", reads(t, r, g,
		"ci", `{}`,
		"ci", `{"sha":"DEF5678"}`,
		"ci", `{"ref":"dev"}`,
		"ci", `{"ref":"main..x"}`,
		"ci", `{"sha":"xyz1234"}`,
	), []string{
		"main at abc1234: failure\nci / checks: success\nci / smoke: failure (Job 'smoke' failed)",
		"commit def5678: success\nci / checks: success https://git.example/sf/code/actions/runs/7/jobs/0",
		"Refused: There is no branch dev.",
		`Refused: The ref must be a branch name under 200 bytes, without "..".`,
		"Refused: The sha must be 7 to 64 hex digits.",
	})
	r.fg.mu.Lock()
	r.fg.statuses = nil
	r.fg.mu.Unlock()
	equal(t, "no statuses", reads(t, r, g, "ci", `{}`), []string{"main at abc1234: no CI status yet"})
	var asked []string
	r.fg.mu.Lock()
	for _, c := range r.fg.calls {
		if strings.Contains(c, "/branches/") || strings.Contains(c, "/commits/") {
			asked = append(asked, c)
		}
	}
	r.fg.mu.Unlock()
	equal(t, "the forgejo reads", asked, []string{
		"GET /api/v1/repos/sf/code/branches/main Bearer access-1",
		"GET /api/v1/repos/sf/code/commits/" + ciHead + "/status Bearer access-1",
		"GET /api/v1/repos/sf/code/commits/def5678/status Bearer access-1",
		"GET /api/v1/repos/sf/code/branches/dev Bearer access-1",
		"GET /api/v1/repos/sf/code/branches/main Bearer access-2",
		"GET /api/v1/repos/sf/code/commits/" + ciHead + "/status Bearer access-2",
	})
	for _, c := range []struct {
		in     toolInput
		status int
	}{
		{toolInput{Ref: strings.Repeat("a", 200)}, http.StatusBadRequest},
		{toolInput{Ref: strings.Repeat("a", 199)}, http.StatusNotFound},
		{toolInput{Ref: "..", SHA: "abc1234"}, http.StatusBadRequest},
		{toolInput{SHA: "abc123"}, http.StatusBadRequest},
		{toolInput{SHA: strings.Repeat("a", 65)}, http.StatusBadRequest},
		{toolInput{SHA: "abc123g"}, http.StatusBadRequest},
	} {
		if status, msg := refused(t, r, "ci", c.in); status != c.status {
			t.Errorf("ci %+v refused %d %q, want %d", c.in, status, msg, c.status)
		}
	}
}

// The reads run at once and act on nothing: none is a write, and none is
// in the default confirm list.
func TestChatReadsAreNeverProposed(t *testing.T) {
	for _, name := range []string{"plan", "card", "pr", "file", "session", "session_steps", "session_step", "session_search", "ci"} {
		tool, ok := (&Console{}).chatTool(name)
		if !ok || tool.write || slices.Contains(policy.DefaultChat.Confirm, name) {
			t.Errorf("tool %s: offered %v, write %v, confirmed by default %v; want a read run at once", name, ok, tool.write, slices.Contains(policy.DefaultChat.Confirm, name))
		}
	}
}
