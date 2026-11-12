package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

var fixtureStart = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func fixtureAt(m int) time.Time { return fixtureStart.Add(time.Duration(m) * time.Minute) }

type fixtureSeed struct {
	store *PGStore
	tl    *Timeline
	ex    *FixtureExporter
}

func newFixtureSeed(t *testing.T, id string, issue int64) fixtureSeed {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "fixture"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, events, audit, approval_requests, cases CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", BudgetEUR: 5, Status: "running", Repository: "sf/r", IssueNumber: issue,
		Track: "direct", Source: "webhook", Size: "medium", CreatedAt: fixtureStart, Objective: "Filter the divide panels by repository."}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	objects := &fakeObjectStore{}
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{
		"app":   {Repository: "sf/r", Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeHuman},
		"other": {Repository: "sf/other", Approvers: []string{"carol"}}}}
	return fixtureSeed{store: s, tl: &Timeline{Objects: objects},
		ex: &FixtureExporter{Store: s, Objects: objects, Policy: tree, Scrub: redact.New(), Now: func() time.Time { return fixtureAt(60) }}}
}

func (g fixtureSeed) line(t *testing.T, id string, m int, step string, detail map[string]any) {
	t.Helper()
	g.tl.now = func() time.Time { return fixtureAt(m) }
	if err := g.tl.Append(context.Background(), id, step, detail); err != nil {
		t.Fatalf("append %s: %v", step, err)
	}
}

// session writes a session of attempt from minute from to minute to.
func (g fixtureSeed) session(t *testing.T, id string, attempt, from, to int, outcome, commit, summary string) {
	t.Helper()
	sid := "s" + string(rune('0'+attempt))
	g.line(t, id, from, "session_start", map[string]any{"session": sid, "model": "factory/small", "attempt": attempt})
	if commit != "" {
		g.line(t, id, to, "pushed", map[string]any{"session": sid, "commit": commit})
	}
	g.line(t, id, to, "session_end", map[string]any{"session": sid, "model": "factory/small", "attempt": attempt, "outcome": outcome,
		"summary": summary, "started": rfc3339(fixtureAt(from))})
}

// The seeded card: three sessions (pushed, no push, pushed) with 2 + 1 + 3
// audit rows, a review of two findings the first session's check-in
// judged, checks red then green, and a session-cap case a person's extend
// with a fund of 1.50 EUR resolved. The golden is written from this seed:
// costs 0.40 + 0.35, 0.20 and 0.50 + 0.25 + 0.25; agent times 10, 5 and 12
// minutes; the answer at minute 50 is 3000 s; the budget 5 + 1.5.
func TestExportFixtureOfASeededCardEqualsItsGolden(t *testing.T) {
	ctx := context.Background()
	g := newFixtureSeed(t, "t-fx", 1410)
	if _, err := g.store.db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, kind, tool, call_cost_eur, ts) VALUES
		('t-fx', 'allowed', '', 'agent', '', 0.40, '2026-11-01 00:02:00Z'),
		('t-fx', 'allowed', '', 'agent', '', 0.35, '2026-11-01 00:05:00Z'),
		('t-fx', 'allowed', '', 'agent', '', 0.20, '2026-11-01 00:22:00Z'),
		('t-fx', 'allowed', '', 'agent', '', 0.50, '2026-11-01 00:31:00Z'),
		('t-fx', 'allowed', '', 'agent', '', 0.25, '2026-11-01 00:35:00Z'),
		('t-fx', 'allowed', '', 'agent', '', 0.25, '2026-11-01 00:40:00Z')`); err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	g.session(t, "t-fx", 1, 1, 11, "pushed", "1111111", "first push")
	g.line(t, "t-fx", 12, "model_review", map[string]any{"pr": 3, "commit": "1111111", "model": "factory/big", "verdict": "concerns",
		"summary": "two findings", "findings": []string{findingA, findingB}, "round": 1})
	g.line(t, "t-fx", 13, "checks", map[string]any{"commit": "1111111", "state": "failure", "findings": []string{"go test failed"}})
	g.line(t, "t-fx", 14, "checkin", map[string]any{"round": 1, "decision": "handover", "reason_kind": "findings", "reason": "two findings stand",
		"verdicts": map[string]any{findingA: map[string]any{"state": "confirmed"}, findingB: map[string]any{"state": "dropped", "evidence": "x.go:1 at head: guarded"}},
		"record":   map[string]any{"session": "ck1", "parent_session": "s1", "moment": "review", "decision": "handover"}})
	g.session(t, "t-fx", 2, 20, 25, "no_push", "", "no change made")
	g.session(t, "t-fx", 3, 30, 42, "pushed", "3333333", "second push")
	g.line(t, "t-fx", 43, "checks", map[string]any{"commit": "3333333", "state": "success", "findings": []string{}})
	spec, err := json.Marshal(MergeSpec{HeadSHA: "1111111", FailedTests: []FailedTest{{Test: "TestA", Package: "pkg/a", Kind: "failed"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t-fx", Project: "sf/r", PRNumber: 3, Spec: spec}); err != nil {
		t.Fatalf("OpenApproval: %v", err)
	}
	c, _, err := g.store.OpenCase(ctx, Case{TaskID: "t-fx", Repository: "sf/r", Kind: CaseSessionCap, StateKey: "attempts 3 of 3",
		Problem: "The card used its 3 sessions.", Facts: map[string]any{"attempts": 3, "cap": 3}})
	if err != nil {
		t.Fatalf("OpenCase: %v", err)
	}
	detail := map[string]any{"pool": true, "rung": 3, "case_id": c.ID}
	if err := g.store.ResolveCase(ctx, c.ID, CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend, Args: map[string]any{"sessions": 1, "answer": "extend it by one"}, Reason: "the third attempt was close"},
		&CaseDraw{Rung: 3, Extend: policy.Draw{Sessions: 1}, FundEUR: 1.5, PolicyCeiling: 10,
			Event: Event{Repository: "sf/r", TaskID: "t-fx", IssueNumber: 1410, Actor: "sebastian", Via: ViaConsole, Detail: detail}},
		&CaseStep{Rung: 3, Actor: "sebastian", Decision: ActExtend, Grounds: "one more session", Outcome: StepResolved}, nil); err != nil {
		t.Fatalf("ResolveCase: %v", err)
	}
	recordAll(t, g.store, Event{Repository: "sf/r", TaskID: "t-fx", IssueNumber: 1410, Kind: EventSessionGranted, Actor: "sebastian", Via: ViaConsole,
		At: fixtureAt(50), Detail: map[string]any{"action": ActExtend, "case_id": c.ID, "answer": "one more session, 1.50 EUR"}})

	got, err := g.ex.Export(ctx, "sf/r", 1410)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	body, err := got.JSON()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "fixtures", "export-three-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("the export differs from its golden:\n%s", body)
	}
	path := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFixture(path)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if !reflect.DeepEqual(loaded, got) {
		t.Fatalf("the loaded fixture differs from the export:\nloaded %+v\nexport %+v", loaded, got)
	}
}

// extra creates another card of the seed's database.
func (g fixtureSeed) extra(t *testing.T, task Task) {
	t.Helper()
	task.Kind, task.Status, task.Repository, task.CreatedAt = "agent", cmp.Or(task.Status, "running"), "sf/r", fixtureStart
	task.Track, task.Source = cmp.Or(task.Track, "direct"), cmp.Or(task.Source, "webhook")
	if err := g.store.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask %s: %v", task.ID, err)
	}
}

func (g fixtureSeed) event(t *testing.T, id string, issue int64, m int, kind, actor, via string, detail map[string]any) {
	t.Helper()
	recordAll(t, g.store, Event{Repository: "sf/r", TaskID: id, IssueNumber: issue, Kind: kind, Actor: actor, Via: via, At: fixtureAt(m), Detail: detail})
}

func (g fixtureSeed) export(t *testing.T, issue int64) (Fixture, string) {
	t.Helper()
	got, err := g.ex.Export(context.Background(), "sf/r", issue)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	body, err := got.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return got, string(body)
}

// Every text path the export carries is scrubbed and sanitized: a distinct
// userinfo URL and a bearer value sit in each source, and each leaves
// masked and defanged at its own place. A path that
// copies its text raw leaves its own marker unmasked.
func TestExportFixtureMasksEveryTextPath(t *testing.T) {
	ctx := context.Background()
	g := newFixtureSeed(t, "t-lk", 1420)
	n := 0
	canary := func() string {
		n++
		return fmt.Sprintf("https://user:pass@h%d.example.org/x Authorization: Bearer abc123def456 Ｂｅａｒｅｒ wide789ghi012 Ａｕｔｈｏｒｉｚａｔｉｏｎ: ｔｏｋｅｎ ｗｉｄｅ０１２３４５６７８９ａｂｃｄｅｆ", n)
	}
	g.extra(t, Task{ID: "t-plan", IssueNumber: 1400, Track: "plan", Source: "webhook"})
	if _, err := g.store.db.ExecContext(ctx, `UPDATE tasks SET plan_name = $1 WHERE id = 't-plan'`, canary()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.db.ExecContext(ctx, `UPDATE tasks SET source = 'plan', parent_task = 't-plan' WHERE id = 't-lk'`); err != nil {
		t.Fatal(err)
	}
	g.extra(t, Task{ID: "t-dep", IssueNumber: 1401, Status: "closed"})
	if err := g.store.AddTaskDeps(ctx, "t-lk", []string{"t-dep"}); err != nil {
		t.Fatal(err)
	}
	g.event(t, "t-dep", 1401, 5, EventTaskEnded, FactoryDecider, ViaFactoryd, map[string]any{"reason": canary()})

	g.session(t, "t-lk", 1, 1, 11, "no_push", "", canary())
	g.line(t, "t-lk", 12, StepProviderRefused, map[string]any{"status": "429", "reason": canary(), "backoff_s": 30})
	g.line(t, "t-lk", 13, "model_review", map[string]any{"commit": "c1", "verdict": "concerns", "findings": []string{canary()}, "round": 1})
	g.line(t, "t-lk", 14, "audit", map[string]any{"commit": "c1", "verdict": "findings", "findings": []AuditFinding{{Item: canary()}}})
	g.line(t, "t-lk", 15, "gate_criteria", map[string]any{"commit": "c1", "verdict": "", "findings": []string{canary()}, "start_failed": canary()})
	g.line(t, "t-lk", 16, "checks", map[string]any{"commit": "c1", "state": "failure", "findings": []string{canary()}})
	g.line(t, "t-lk", 17, "checkin", map[string]any{"decision": "handover", "reason_kind": "findings", "reason": canary(),
		"record": map[string]any{"parent_session": "s1", "moment": "review"}})
	spec, err := json.Marshal(MergeSpec{HeadSHA: "c1", FailedTests: []FailedTest{{Job: canary(), Test: canary(), Package: canary(), Kind: canary()}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t-lk", Project: "sf/r", PRNumber: 3, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	g.event(t, "t-lk", 1420, 20, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c2", "reviewed": false, "reason": canary()})
	g.event(t, "t-lk", 1420, 21, EventGateAudit, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c2", "ran": false, "reason": canary()})
	g.event(t, "t-lk", 1420, 22, EventQuestionAsked, FactoryDecider, ViaFactoryd,
		map[string]any{"ask": canary(), "why": canary(), "recommendation": canary(), "reason": canary(), "case_kind": "other"})
	c, _, err := g.store.OpenCase(ctx, Case{TaskID: "t-lk", Repository: "sf/r", Kind: CaseOther, StateKey: canary(), Problem: canary(),
		Facts: map[string]any{"nested": map[string]any{"list": []any{canary()}}, canary(): "key"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.store.ResolveCase(ctx, c.ID, CaseResolution{By: "sebastian", Rung: 2, Action: "answer", Args: map[string]any{"note": canary()}, Reason: canary()}, nil,
		&CaseStep{Rung: 2, Actor: "sebastian", Found: canary(), Decision: canary(), Grounds: canary(), Outcome: StepResolved}, nil); err != nil {
		t.Fatal(err)
	}
	g.event(t, "t-lk", 1420, 30, EventQuestionAnswered, "sebastian", ViaChat, map[string]any{"case_id": c.ID, "answer": canary()})
	g.event(t, "t-lk", 1420, 31, EventCardClosed, FactoryDecider, ViaFactoryd, map[string]any{"reason": canary()})

	_, body := g.export(t, 1420)
	for _, leak := range []string{"user:pass", "abc123def456", "wide789ghi012", "wide0123456789abcdef", "https://"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the export holds %q:\n%s", leak, body)
		}
	}
	for i := 1; i <= n; i++ {
		if want := fmt.Sprintf("hxxps://[redacted]@h%d.example.org/[path]", i); !strings.Contains(body, want) {
			t.Errorf("no text of the export holds %q: the path of source %d does not mask its text", want, i)
		}
	}
	if got := strings.Count(body, "Bearer [redacted]"); got < 2*n {
		t.Errorf("%d masked bearer values, want at least %d", got, 2*n)
	}
	if got := strings.Count(body, "Authorization: token [redacted]"); got < n {
		t.Errorf("%d masked token headers, want at least %d", got, n)
	}
}

// The mask names the configured terms: a configured host, the card's
// repository and another project's, a login of the card's records and an
// approver of the policy become stand-ins in any text and key. A login
// never leaves as an actor.
func TestExportFixtureMasksConfiguredTerms(t *testing.T) {
	ctx := context.Background()
	g := newFixtureSeed(t, "t-mk", 1430)
	g.ex.Hosts = []string{"forgejo.internal.example"}
	g.event(t, "t-mk", 1430, 5, EventQuestionAnswered, "sebastian", ViaConsole, map[string]any{"answer": "ok"})
	c, _, err := g.store.OpenCase(ctx, Case{TaskID: "t-mk", Repository: "sf/r", Kind: CaseOther, StateKey: "k",
		Problem: "Sebastian approved sf/r and sf/other; carol saw https://forgejo.internal.example/sf/r/pulls/3 and forgejo.internal.example:3000",
		Facts:   map[string]any{"requested_by": "sebastian"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.store.ResolveCase(ctx, c.ID, CaseResolution{By: "sebastian", Rung: 2, Action: "answer"}, nil,
		&CaseStep{Rung: 2, Actor: "sebastian", Outcome: StepResolved}, nil); err != nil {
		t.Fatal(err)
	}
	got, body := g.export(t, 1430)
	for _, leak := range []string{"ebastian", "carol", "forgejo", "sf/", "internal"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the export holds %q:\n%s", leak, body)
		}
	}
	if p := got.Cases[0].Problem; p != "human approved owner/repo and owner/repo-1; human saw host.example and host.example" {
		t.Errorf("problem = %q", p)
	}
	if r := got.Cases[0].Resolution; r.By != RoleProjectAgent || got.Cases[0].Steps[len(got.Cases[0].Steps)-1].Actor != RoleProjectAgent {
		t.Errorf("a rung 2 resolution by a login = %+v, want the project agent", r)
	}
	if got.Cases[0].Facts["requested_by"] != "human" || got.Card.Repository != "owner/repo" {
		t.Errorf("facts %v, repository %q", got.Cases[0].Facts, got.Card.Repository)
	}
	if got.Answers[0].By != RoleHuman {
		t.Errorf("answer by %q, want human", got.Answers[0].By)
	}
}

// A shape of a person or of an inside host that the mask does not know
// fails the export; the error names the JSON path and the class, not the
// text.
func TestExportFixtureRefusesWhatLooksPersonal(t *testing.T) {
	for _, tc := range []struct {
		name, problem, class, path, value string
		facts                             map[string]any
	}{
		{"email", "write to mia.keller@corp.example", "an email address", "$.cases[0].problem", "mia.keller", nil},
		{"mention", "thanks @dave77", "an @mention", "$.cases[0].problem", "dave77", nil},
		{"key", "x", "an email address", "$.cases[0].facts{key}", "zed", map[string]any{"zed@corp.example": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFixtureSeed(t, "t-sh", 1440)
			if _, _, err := g.store.OpenCase(context.Background(), Case{TaskID: "t-sh", Repository: "sf/r", Kind: CaseOther, StateKey: "k", Problem: tc.problem, Facts: tc.facts}); err != nil {
				t.Fatal(err)
			}
			_, err := g.ex.Export(context.Background(), "sf/r", 1440)
			if err == nil || !strings.Contains(err.Error(), tc.class) || !strings.Contains(err.Error(), tc.path) || strings.Contains(err.Error(), tc.value) {
				t.Fatalf("Export = %v, want a refusal naming %s at %s and no part of the value %q", err, tc.class, tc.path, tc.value)
			}
		})
	}
}

// The review and the audit that never ran leave only their fact rows:
// the gates hold them with the reason, the finished ones with the verdict
// and the findings of their timeline lines, and an audit that gave no
// verdict is no finding. Sorted by time: the review of c1 (minute 10),
// the audit of c1 (11), the refused review of c2 (20), the refused audit
// of c2 (21), the old audit line of c3 (30).
func TestExportFixtureReadsGatesFromFactRows(t *testing.T) {
	g := newFixtureSeed(t, "t-gt", 1450)
	g.line(t, "t-gt", 10, "model_review", map[string]any{"commit": "c1", "verdict": "concerns", "findings": []string{findingA}, "round": 1})
	g.line(t, "t-gt", 11, "audit", map[string]any{"commit": "c1", "verdict": "findings", "findings": []AuditFinding{{Item: "x lacks y"}}})
	g.line(t, "t-gt", 30, "audit", map[string]any{"commit": "c3", "verdict": "findings", "findings": []AuditFinding{{Item: auditNoVerdict}}})
	g.event(t, "t-gt", 1450, 10, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c1", "round": 1, "reviewed": true, "verdict": "concerns"})
	g.event(t, "t-gt", 1450, 11, EventGateAudit, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c1", "ran": true, "verdict": "findings"})
	g.event(t, "t-gt", 1450, 20, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c2", "round": 1, "reviewed": false, "reason": "the card's budget refused the call"})
	g.event(t, "t-gt", 1450, 21, EventGateAudit, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c2", "ran": false, "reason": "the provider refused: 429"})
	got, _ := g.export(t, 1450)
	want := []FixtureGate{
		{Head: "c1", Gate: "review", Verdict: "concerns", Findings: []FixtureFinding{{Text: findingA, ID: idA}}},
		{Head: "c1", Gate: "audit", Verdict: "findings", Findings: []FixtureFinding{{Text: "x lacks y", ID: "ba0337192d0a"}}},
		{Head: "c2", Gate: "review", Findings: []FixtureFinding{}, NotRun: "the card's budget refused the call"},
		{Head: "c2", Gate: "audit", Findings: []FixtureFinding{}, NotRun: "the provider refused: 429"},
		{Head: "c3", Gate: "audit", Findings: []FixtureFinding{}, NotRun: auditNoVerdict},
	}
	if !reflect.DeepEqual(got.Gates, want) {
		t.Fatalf("gates = %+v\nwant %+v", got.Gates, want)
	}
}

// A provider's refusal belongs to the session it held, not to the gates.
func TestExportFixtureHoldsRefusalsInTheirSession(t *testing.T) {
	g := newFixtureSeed(t, "t-rf", 1451)
	g.session(t, "t-rf", 1, 1, 11, "no_push", "", "held")
	g.line(t, "t-rf", 5, StepProviderRefused, map[string]any{"status": "429", "reason": "rate limited", "backoff_s": 30})
	got, _ := g.export(t, 1451)
	if len(got.Gates) != 0 || !reflect.DeepEqual(got.Sessions[0].Refusals, []FixtureRefusal{{Status: "429", Reason: "rate limited", BackoffS: 30}}) {
		t.Fatalf("gates %+v, refusals %+v; want none and the one refusal on the session", got.Gates, got.Sessions[0].Refusals)
	}
}

// Every kind of the codebase's answerKinds is an answer, with its own
// action when the row names none. A merge or plan decision, or a close,
// names the kind of the approval request it settles (settle.go), never a
// question's: the merge approvals are merge_approval, the plan's changes
// plan_confirmation, a close after a decided request none, a close of an
// open merge request merge_approval. A question's answer with no ask has no
// kind. The roles: factoryd, a login by the console a human, by the chat the
// project agent. A board close, as the console writes it (the human's login,
// a note), answers no case: it has no kind whatever request is open, so the
// closes at 38 min (the plan request decided) and 50 min (a merge request
// open) both have none; their notes are the human's, "superseded" being 10
// bytes and "closed" 6.
func TestExportFixtureNamesAnApprovalDecisionsRequestKind(t *testing.T) {
	ctx := context.Background()
	g := newFixtureSeed(t, "t-an", 1460)
	g.event(t, "t-an", 1460, 10, EventQuestionAnswered, "sebastian", ViaChat, map[string]any{"answer": "yes"})
	g.event(t, "t-an", 1460, 14, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"kind": ApprovalMerge, "pr": 3})
	g.event(t, "t-an", 1460, 20, EventMergeApproved, "sebastian", ViaConsole, map[string]any{"pr": 3})
	g.event(t, "t-an", 1460, 24, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"kind": ApprovalMerge, "pr": 4})
	g.event(t, "t-an", 1460, 30, EventMergeDenied, "sebastian", ViaConsole, map[string]any{"pr": 4})
	g.event(t, "t-an", 1460, 34, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"kind": ApprovalPlan})
	g.event(t, "t-an", 1460, 36, EventPlanChangesRequested, "sebastian", ViaConsole, map[string]any{"reason": "split it"})
	g.event(t, "t-an", 1460, 38, EventCardClosed, "sebastian", ViaConsole, map[string]any{"note": "superseded"})
	g.event(t, "t-an", 1460, 44, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"kind": ApprovalMerge, "pr": 5})
	g.event(t, "t-an", 1460, 50, EventCardClosed, "sebastian", ViaConsole, map[string]any{"note": "closed"})
	if _, err := g.store.db.ExecContext(ctx, `DELETE FROM cases WHERE task_id = 't-an'`); err != nil {
		t.Fatal(err)
	}
	got, _ := g.export(t, 1460)
	want := []FixtureAnswer{
		{AtS: 600, By: RoleProjectAgent, Action: EventQuestionAnswered, Args: map[string]any{}, Note: "yes"},
		{AtS: 1200, By: RoleHuman, CaseKind: "merge_approval", Action: EventMergeApproved, Args: map[string]any{}},
		{AtS: 1800, By: RoleHuman, CaseKind: "merge_approval", Action: EventMergeDenied, Args: map[string]any{}},
		{AtS: 2160, By: RoleHuman, CaseKind: "plan_confirmation", Action: EventPlanChangesRequested, Args: map[string]any{}, Note: typed("split it")},
		{AtS: 2280, By: RoleHuman, Action: EventCardClosed, Args: map[string]any{}, Note: typed("superseded")},
		{AtS: 3000, By: RoleHuman, Action: EventCardClosed, Args: map[string]any{}, Note: typed("closed")},
	}
	if !reflect.DeepEqual(got.Answers, want) {
		t.Fatalf("answers = %+v\nwant %+v", got.Answers, want)
	}
}

// The asks the writers write: factoryd's ladder ask names its case by
// case_id, with a source and a reason and no case_kind, and the chat's ask
// names no case. The ask's kind is its case's through case_id, else the
// kind the ladder opens a case of for such a row, which a plain answer
// takes from the newest ask.
func TestExportFixtureTakesAnAsksKindFromItsCase(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rungsOff(g.r)
	out := g.trip(t, AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt")
	tl := &Timeline{Objects: g.r.objects, Events: g.s, Store: g.s}
	task, _, err := g.s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := recordAsk(ctx, tl, task, HumanQuestion{Ask: "Raise the budget?", Why: "short", Info: "x"}, "agent", "chat", ViaChat, "", nil); err != nil {
		t.Fatal(err)
	}
	recordAll(t, g.s, Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaConsole,
		Detail: map[string]any{"case_id": out.CaseID, "answer": "a.txt"}})
	recordAll(t, g.s, Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat,
		Detail: map[string]any{"answer": "yes"}})
	ex := &FixtureExporter{Store: g.s, Objects: g.r.objects, Scrub: redact.New(), Now: func() time.Time { return fixtureAt(60) }}
	got, err := ex.Export(ctx, "demo-app/demo-app", 12)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(got.Asks) != 2 {
		t.Fatalf("asks = %+v, want the ladder's and the chat's", got.Asks)
	}
	if a := got.Asks[0]; a.CaseKind != "agent_escalate" || a.Source != "agent" || a.Reason == "" || a.Ask != "Which file?" {
		t.Errorf("the ladder's ask = %+v, want its case's kind agent_escalate, source agent and a reason", a)
	}
	// the chat's ask names no case; the ladder opens a case for it by its
	// source, and caseSourceKinds maps the chat's to agent_escalate
	if a := got.Asks[1]; a.CaseKind != "agent_escalate" || a.Source != "chat" || a.Ask != "Raise the budget?" {
		t.Errorf("the chat's ask = %+v, want kind agent_escalate and source chat", a)
	}
	if len(got.Answers) != 2 || got.Answers[0].CaseKind != "agent_escalate" || got.Answers[1].CaseKind != "agent_escalate" {
		t.Errorf("answers = %+v, want both on agent_escalate: the first by its case, the second by the newest ask", got.Answers)
	}
}

func TestFixtureRoleIsOneMapForEveryActor(t *testing.T) {
	for _, tc := range []struct {
		rung       int
		actor, via string
		want       string
	}{
		{0, FactoryDecider, "", RoleFactoryd},
		{1, "gpt", "", RoleFactoryd},
		{2, "sebastian", "", RoleProjectAgent},
		{3, "sebastian", "", RoleHuman},
		{-1, FactoryDecider, ViaFactoryd, RoleFactoryd},
		{-1, "sebastian", ViaChat, RoleProjectAgent},
		{-1, ChatUser, "", RoleProjectAgent},
		{-1, "sebastian", ViaConsole, RoleHuman},
		{-1, "sebastian", ViaFactoryd, RoleHuman},
	} {
		if got := fixtureRole(tc.rung, tc.actor, tc.via); got != tc.want {
			t.Errorf("fixtureRole(%d, %q, %q) = %q, want %q", tc.rung, tc.actor, tc.via, got, tc.want)
		}
	}
}

func TestExportFixtureCutsTheSummaryAt2KiB(t *testing.T) {
	g := newFixtureSeed(t, "t-cut", 1412)
	g.session(t, "t-cut", 1, 1, 2, "no_push", "", strings.Repeat("a b ", 1000))
	got, _ := g.export(t, 1412)
	if n := len(got.Sessions[0].Summary); n != 2048+len("…") {
		t.Fatalf("summary of %d bytes, want 2048 and the cut mark", n)
	}
}

func TestExportFixtureOfNoCardIsNotFound(t *testing.T) {
	g := newFixtureSeed(t, "t-x", 1413)
	if _, err := g.ex.Export(context.Background(), "sf/r", 999); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "sf/r#999") {
		t.Fatalf("Export of no card = %v, want ErrNotFound naming sf/r#999", err)
	}
}

// The export reads through a connection that cannot write: it opens no
// migration (an empty database stays empty) and gives the same fixture as a
// read-write store; a write or a schema change through it fails.
func TestExportFixtureRunsReadOnly(t *testing.T) {
	ctx := context.Background()
	g := newFixtureSeed(t, "t-ro", 1470)
	g.session(t, "t-ro", 1, 1, 11, "pushed", "1111111", "done")
	ro, err := OpenStoreReadOnly(ctx, schematest.DSN(t, "fixture"))
	if err != nil {
		t.Fatalf("OpenStoreReadOnly: %v", err)
	}
	defer ro.Close()
	ex := *g.ex
	ex.Store = ro
	viaRO, err := ex.Export(ctx, "sf/r", 1470)
	if err != nil {
		t.Fatalf("Export through the read-only store: %v", err)
	}
	if viaRW, _ := g.export(t, 1470); !reflect.DeepEqual(viaRO, viaRW) {
		t.Fatalf("the read-only export differs:\n%+v\n%+v", viaRO, viaRW)
	}
	for _, stmt := range []string{`UPDATE tasks SET status = 'done'`, `CREATE TABLE fixture_ddl (x int)`, `INSERT INTO events (repository, kind) VALUES ('r', 'k')`} {
		if _, err := ro.db.ExecContext(ctx, stmt); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s through the read-only store = %v, want a read-only refusal", stmt, err)
		}
	}
	empty, err := OpenStoreReadOnly(ctx, schematest.DSN(t, "fixture_empty"))
	if err != nil {
		t.Fatalf("OpenStoreReadOnly of an empty database: %v", err)
	}
	defer empty.Close()
	var tables int
	if err := empty.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("an empty database holds %d tables (%v) after the read-only open, want 0: no migration ran", tables, err)
	}
}

func TestLoadFixtureRefusesWhatTheFormatDoesNotHave(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct{ name, body, want string }{
		{"typo", `{"scritp": {}}`, "scritp"},
		{"nested", `{"script": {"rung1": [{"case_kind": "session_cap", "anwser": {}}]}}`, "anwser"},
		{"tool", `{"script": {"rung2": [{"case_kind": "session_cap", "answer": {"tool": "shout", "args": {}}}]}}`, "shout"},
		{"two", `{} {}`, "more than one"},
		{"stray", `{} }`, "more than one"},
		{"rung", `{"expect": {"resolved_by_rung": [4]}}`, "resolved_by_rung 4"},
		{"count", `{"expect": {"sessions": -1}}`, "negative"},
		{"humans", `{"expect": {"human_cases": -1}}`, "negative"},
	} {
		if _, err := LoadFixture(write(tc.name+".json", tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoadFixture of %s = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
	ok := `{"script": {"rung1": [{"case_kind": "session_cap", "answer": {"decision": "extend"}}], "rung2": [{"case_kind": "session_cap", "answer": {"tool": "attach_analysis", "args": {}}}]}, "expect": {"resolved_by_rung": [1, -1], "sessions": 1, "human_cases": 0, "face": []}}`
	f, err := LoadFixture(write("ok.json", ok))
	if err != nil || f.Expect == nil || !reflect.DeepEqual(f.Expect.ResolvedByRung, []int{1, ResolvedOpen}) {
		t.Fatalf("LoadFixture of a hand-written script = %+v, %v", f.Expect, err)
	}
	if f, err := LoadFixture(write("none.json", `{}`)); err != nil || f.Expect != nil {
		t.Fatalf("a fixture with no expect = %+v, %v; want expect nil, an authored zero is another value", f.Expect, err)
	}
}

// A review with no verdict and no fact row (a line from before the rows)
// is a gate that did not run, its note scrubbed; a check-in's record
// reason is scrubbed when the line carries none.
func TestExportFixtureScrubsLineOnlyNotesAndRecordReasons(t *testing.T) {
	g := newFixtureSeed(t, "t-ln", 1480)
	g.session(t, "t-ln", 1, 1, 11, "no_push", "", "held")
	g.line(t, "t-ln", 12, "model_review", map[string]any{"commit": "c1", "note": "refused: https://user:pass@h1.example.org/x"})
	g.line(t, "t-ln", 13, "checkin", map[string]any{"decision": "handover",
		"record": map[string]any{"parent_session": "s1", "moment": "review", "reason": "see https://user:pass@h2.example.org/x"}})
	got, body := g.export(t, 1480)
	if strings.Contains(body, "user:pass") {
		t.Fatalf("the export holds a userinfo:\n%s", body)
	}
	if got.Gates[0].NotRun != "refused: hxxps://[redacted]@h1.example.org/[path]" || got.Sessions[0].CheckIn.Reason != "see hxxps://[redacted]@h2.example.org/[path]" {
		t.Fatalf("not_run %q, check-in reason %q", got.Gates[0].NotRun, got.Sessions[0].CheckIn.Reason)
	}
}

// Each login of the card's records is masked wherever a text names it,
// whichever record names the login: the policy's approvers, the card's
// author, the actor of an event, and the rung 2 actor of a case's
// resolution and of its step.
func TestExportFixtureMasksLoginsFromEverySource(t *testing.T) {
	ctx := context.Background()
	g := newFixtureSeed(t, "t-lg", 1490)
	g.ex.Policy.Projects = nil
	g.ex.Policy.Document.Approvers = []string{"dora"}
	if _, err := g.store.db.ExecContext(ctx, `UPDATE tasks SET author = 'frank' WHERE id = 't-lg'`); err != nil {
		t.Fatal(err)
	}
	g.event(t, "t-lg", 1490, 5, EventCardCommented, "erin", ViaConsole, map[string]any{})
	c, _, err := g.store.OpenCase(ctx, Case{TaskID: "t-lg", Repository: "sf/r", Kind: CaseOther, StateKey: "k", Problem: "dora erin frank gina hank"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.store.ResolveCase(ctx, c.ID, CaseResolution{By: "gina", Rung: 2, Action: "answer"}, nil,
		&CaseStep{Rung: 2, Actor: "hank", Outcome: StepResolved}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := g.export(t, 1490)
	if p := got.Cases[0].Problem; p != "human human human human human" {
		t.Fatalf("problem = %q, want every login masked", p)
	}
}

func fixtureError(t *testing.T, g fixtureSeed, issue int64) string {
	t.Helper()
	_, err := g.ex.Export(context.Background(), "sf/r", issue)
	if err == nil {
		t.Fatal("Export = nil, want a refusal")
	}
	return err.Error()
}

// An IPv4 address becomes a placeholder, a URL on a configured host or an
// inside host goes whole with its path, an scp-style remote of a configured
// host and the factory's own identities have fixed stand-ins. A full-width
// at sign reads as the sign: the email and the mention it makes are refused.
func TestExportFixtureMasksAddressesURLsAndIdentities(t *testing.T) {
	g := newFixtureSeed(t, "t-ad", 1500)
	g.ex.Hosts = []string{"git.corp.example"}
	g.session(t, "t-ad", 1, 1, 2, "no_push", "",
		"dial 10.244.3.17:5432 and 49.12.34.56; https://git.corp.example/hans-mueller/payroll/issues/2 and http://vault.platform.svc:8200/v1/x/y; "+
			"push git@git.corp.example:hans-mueller/payroll.git by factory-agent@factory.local and factory-wip@factory.local, Co-Authored-By noreply@anthropic.com")
	got, _ := g.export(t, 1500)
	want := "dial 192.0.2.1:5432 and 192.0.2.1; host.example and host.example; push [git remote] by [factory agent] and [factory wip], Co-Authored-By [noreply]"
	if s := got.Sessions[0].Summary; s != want {
		t.Fatalf("summary = %q\nwant %q", s, want)
	}
}

func TestExportFixtureRefusalNamesTheClassPositionAndAction(t *testing.T) {
	// the position is the byte where the shape starts in "pre " + problem:
	// the email at "mia" (13), the mention at its sign (11)
	for _, tc := range []struct{ name, problem, class, action, at string }{
		{"fullwidth email", "write to mia＠corp.example", "an email address", "FIXTURE_MASK", "byte 13"},
		{"fullwidth mention", "thanks ＠dave77", "an @mention", "FIXTURE_KEEP", "byte 11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFixtureSeed(t, "t-rf2", 1510)
			if _, _, err := g.store.OpenCase(context.Background(), Case{TaskID: "t-rf2", Repository: "sf/r", Kind: CaseOther, StateKey: "k", Problem: "pre " + tc.problem}); err != nil {
				t.Fatal(err)
			}
			msg := fixtureError(t, g, 1510)
			for _, want := range []string{tc.class, "$.cases[0].problem", tc.at, tc.action} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q lacks %q", msg, want)
				}
			}
			for _, leak := range []string{"mia", "dave77", "vault"} {
				if strings.Contains(msg, leak) {
					t.Errorf("refusal %q names the value %q", msg, leak)
				}
			}
		})
	}
}

// The brief's label names who wrote it.
func TestExportFixtureLabelsABriefByItsAuthor(t *testing.T) {
	for source, want := range map[string]string{
		"webhook": "[written by a person, 5 bytes]", SourcePlan: "[written by a model, 5 bytes]", SourceChat: "[written by a model, 5 bytes]",
		SourceAlert: "[written by factoryd, 5 bytes]", SourceRoll: "[written by factoryd, 5 bytes]", SourceScreening: "[written by factoryd, 5 bytes]",
		"chain-repair": "[written by factoryd, 5 bytes]", "chain-stall": "[written by factoryd, 5 bytes]", "config": "[written by factoryd, 5 bytes]",
		"roll-refused": "[written by factoryd, 5 bytes]", "roll-failed": "[written by factoryd, 5 bytes]", "image_removal": "[written by factoryd, 5 bytes]",
		"production-held": "[written by factoryd, 5 bytes]", "routine": "[written by factoryd, 5 bytes]",
		"": "[written by a person, 5 bytes]", "api": "[written by a person, 5 bytes]", "successor": "[written by a person, 5 bytes]",
		"delegate": "[written by a model, 5 bytes]",
	} {
		if got := written(briefAuthor(source), "brief"); got != want {
			t.Errorf("brief of source %s = %q, want %q", source, got, want)
		}
	}
}

// Two review rounds of one head take their own findings, though the rows
// come in the opposite order to the lines; a line from before the rows
// stands in time order beside the rows.
func TestExportFixtureMatchesRoundsAndSortsByTime(t *testing.T) {
	g := newFixtureSeed(t, "t-rd", 1520)
	g.line(t, "t-rd", 5, "audit", map[string]any{"commit": "c0", "verdict": "findings", "findings": []AuditFinding{{Item: "old audit"}}})
	g.line(t, "t-rd", 10, "model_review", map[string]any{"commit": "c1", "verdict": "concerns", "findings": []string{"first round"}, "round": 1})
	g.line(t, "t-rd", 12, "model_review", map[string]any{"commit": "c1", "verdict": "concerns", "findings": []string{"second round"}, "round": 2})
	g.event(t, "t-rd", 1520, 12, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c1", "round": 2, "reviewed": true, "verdict": "concerns"})
	g.event(t, "t-rd", 1520, 10, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "c1", "round": 1, "reviewed": true, "verdict": "concerns"})
	got, _ := g.export(t, 1520)
	var heads, texts []string
	for _, gate := range got.Gates {
		heads = append(heads, gate.Head)
		for _, f := range gate.Findings {
			texts = append(texts, f.Text)
		}
	}
	// the rows sort by time: round 1 (minute 10) before round 2 (12), and
	// the old audit line (5) before both
	if !reflect.DeepEqual(heads, []string{"c0", "c1", "c1"}) || !reflect.DeepEqual(texts, []string{"old audit", "first round", "second round"}) {
		t.Fatalf("heads %v, findings %v", heads, texts)
	}
}

// A fact key that holds a login is masked, and keys that mask alike stay
// distinct keys in the byte order of their originals: DORA, Dora, dOra, dora.
func TestExportFixtureMasksKeysAndKeepsCollidingKeys(t *testing.T) {
	g := newFixtureSeed(t, "t-ky", 1530)
	g.ex.Policy.Document.Approvers = []string{"dora"}
	if _, _, err := g.store.OpenCase(context.Background(), Case{TaskID: "t-ky", Repository: "sf/r", Kind: CaseOther, StateKey: "k", Problem: "x",
		Facts: map[string]any{"dora": 4, "Dora": 2, "DORA": 1, "dOra": 3, "plain": 5}}); err != nil {
		t.Fatal(err)
	}
	got, _ := g.export(t, 1530)
	if want := map[string]any{"human": float64(1), "human_2": float64(2), "human_3": float64(3), "human_4": float64(4), "plain": float64(5)}; !reflect.DeepEqual(got.Cases[0].Facts, want) {
		t.Fatalf("facts = %v, want %v", got.Cases[0].Facts, want)
	}
}

// A decision with no request before it names the kind the decision itself
// names: a merge's approval or denial the merge approval, a plan's changes
// the plan confirmation.
func TestExportFixtureNamesADecisionsKindWithNoRequestBefore(t *testing.T) {
	g := newFixtureSeed(t, "t-nr", 1540)
	g.event(t, "t-nr", 1540, 10, EventMergeApproved, "sebastian", ViaConsole, map[string]any{})
	g.event(t, "t-nr", 1540, 20, EventMergeDenied, "sebastian", ViaConsole, map[string]any{})
	g.event(t, "t-nr", 1540, 30, EventPlanChangesRequested, "sebastian", ViaConsole, map[string]any{})
	got, _ := g.export(t, 1540)
	var kinds []string
	for _, a := range got.Answers {
		kinds = append(kinds, a.CaseKind)
	}
	if !reflect.DeepEqual(kinds, []string{"merge_approval", "merge_approval", "plan_confirmation"}) {
		t.Fatalf("kinds = %v", kinds)
	}
}

// A resource decision matches no case kind and has none, like a close; a
// merge decision keeps its family's whatever request is newer, a plan
// request included.
func TestExportFixtureKeepsADecisionsOwnKind(t *testing.T) {
	g := newFixtureSeed(t, "t-rs", 1550)
	g.event(t, "t-rs", 1550, 10, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"kind": ApprovalMerge, "pr": 3})
	g.event(t, "t-rs", 1550, 12, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"kind": ApprovalPlan})
	g.event(t, "t-rs", 1550, 20, EventMergeApproved, "sebastian", ViaConsole, map[string]any{"pr": 3})
	g.event(t, "t-rs", 1550, 30, EventMergeApproved, "sebastian", ViaConsole, map[string]any{"kind": ApprovalResource})
	g.event(t, "t-rs", 1550, 40, EventMergeDenied, "sebastian", ViaConsole, map[string]any{"kind": ApprovalResource})
	got, _ := g.export(t, 1550)
	var kinds []string
	for _, a := range got.Answers {
		kinds = append(kinds, a.CaseKind)
	}
	if !reflect.DeepEqual(kinds, []string{"merge_approval", "", ""}) {
		t.Fatalf("kinds = %v", kinds)
	}
}

// Inside hosts are masked, not refused: a service or cluster name, an
// internal, local, lan or home.arpa name, localhost, and a URL on one with
// its path.
func TestExportFixtureMasksInsideHosts(t *testing.T) {
	g := newFixtureSeed(t, "t-ih", 1560)
	g.session(t, "t-ih", 1, 1, 2, "no_push", "",
		"litellm.platform.svc:4000, temporal.platform.cluster.local, vault.internal, nas.local, box.lan, router.home.arpa, localhost:5432, "+
			"postgres://localhost:5432/factory and http://nas.local/share/hans-mueller/x")
	got, _ := g.export(t, 1560)
	want := "host.example, host.example, host.example, host.example, host.example, host.example, host.example, host.example and host.example"
	if s := got.Sessions[0].Summary; s != want {
		t.Fatalf("summary = %q\nwant %q", s, want)
	}
}

// A host named in a regular expression, with its separators escaped, goes
// as the plain host does, the escaped port with it.
func TestExportFixtureMasksEscapedHosts(t *testing.T) {
	g := newFixtureSeed(t, "t-eh", 1561)
	g.ex.Hosts = []string{"forgejo-http.forgejo.svc.cluster.local", "git.corp.example"}
	g.session(t, "t-eh", 1, 1, 2, "no_push", "",
		`want ^forgejo\-http\.forgejo\.svc\.cluster\.local\:3000$ and forgejo\-http\.forgejo\.svc\.cluster\.local, `+
			`git\.corp\.example:443 and Git\.Corp\.Example`)
	got, body := g.export(t, 1561)
	want := "want ^host.example$ and host.example, host.example and host.example"
	if s := got.Sessions[0].Summary; s != want {
		t.Fatalf("summary = %q\nwant %q", s, want)
	}
	for _, leak := range []string{"forgejo", "corp"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the export holds %q:\n%s", leak, body)
		}
	}
}

// IPv6 addresses, a user at an IP, a URL on an IP host and a configured
// host with no scheme go whole; a time stays; a URL on a public host keeps
// its host and loses its path, query and fragment, but the code
// repository's own path on GitHub stays, and a repository whose name only
// starts like it does not; a full-width login is the login.
func TestExportFixtureMasksTheRestOfTheLeakProbe(t *testing.T) {
	g := newFixtureSeed(t, "t-lp", 1570)
	g.ex.Hosts = []string{"git.corp.example"}
	g.ex.Repositories = []string{"sf/factory-config"}
	g.ex.Policy.Document.Approvers = []string{"sebastian"}
	pub := "github.com/" + publicCodeRepository
	g.session(t, "t-lp", 1, 1, 2, "no_push", "",
		"fd00::1 and [2a01:4f8:c0c:1234::1]:443 at 12:30:45; ssh hans@10.0.0.5; http://10.0.0.5:8080/hans-mueller/payroll; "+
			"git.corp.example/hans-mueller/payroll/issues/2; https://grafana.acme-corp.de/d/abc; https://"+pub+"/pulls/1 and "+pub+"; "+
			"ｓｅｂａｓｔｉａｎ approved sf/factory-config; "+
			"sebastian@example.com, 99+nobody@users.noreply.github.com, git@github.com:sebastian/x.git; "+
			"ssh hans@2a01:4f8::1 and hans@[2a01:4f8::1]; https://grafana.acme-corp.de?orgId=1&var-user=hans-mueller#panel-2 and https://grafana.acme-corp.de#hans-mueller; "+
			"https://git.corp.example?user=hans-mueller; git@git.corp.example:hans/x.git; https://"+pub+"-private/issues/1 and https://"+pub+"x/y; clone https://"+pub+".git.")
	got, _ := g.export(t, 1570)
	want := "2001:db8::1 and [2001:db8::1]:443 at 12:30:45; ssh user@192.0.2.1; host.example; host.example; " +
		"hxxps://grafana[.]acme-corp[.]de/[path]; hxxps://github[.]com/" + publicCodeRepository + "/pulls/1 and " + pub + "; human approved owner/repo-1; " +
		"human@example.invalid, human@example.invalid, [git remote]; " +
		"ssh user@2001:db8::1 and user@[2001:db8::1]; hxxps://grafana[.]acme-corp[.]de/[path] and hxxps://grafana[.]acme-corp[.]de/[path]; " +
		"host.example; [git remote]; hxxps://github[.]com/[path] and hxxps://github[.]com/[path]; clone hxxps://github[.]com/" + publicCodeRepository + ".git."
	if s := got.Sessions[0].Summary; s != want {
		t.Fatalf("summary = %q\nwant %q", s, want)
	}
}

// A run of hex digits and colons is an address only standing alone, with
// a digit and two groups, and as net.ParseIP reads it: scope operators, a
// word with no digit and slices stay; an address with a zone or an IPv4
// tail goes.
func TestExportFixtureKeepsCodeThatLooksLikeAnAddress(t *testing.T) {
	g := newFixtureSeed(t, "t-cd", 1575)
	g.session(t, "t-cd", 1, 1, 2, "no_push", "",
		"use std::vector and Cafe::Bad and a::b; call std::fs::read_to_string and i32::from; slice x[::2] and a[1::2] and a[:, ::2]; a1::b2_c; fe80::1%eth0 and ::ffff:10.0.0.1")
	got, _ := g.export(t, 1575)
	want := "use std::vector and Cafe::Bad and a::b; call std::fs::read_to_string and i32::from; slice x[::2] and a[1::2] and a[:, ::2]; a1::b2_c; 2001:db8::1 and 2001:db8::1"
	if s := got.Sessions[0].Summary; s != want {
		t.Fatalf("summary = %q\nwant %q", s, want)
	}
}

// Each refusal names the export's own list that clears it, never the
// policy's approvers, and the list clears it: an unknown contributor's
// login or mail domain goes into FIXTURE_MASK and is masked, a code
// annotation goes into FIXTURE_KEEP and stays.
func TestExportFixtureRefusalsClearOnTheirNamedList(t *testing.T) {
	for _, tc := range []struct {
		name, text, list, want string
		act                    func(*FixtureExporter)
	}{
		{"email by login", "mail mia@corp.example", "FIXTURE_MASK", "mail human@example.invalid",
			func(e *FixtureExporter) { e.Mask = []string{"mia"} }},
		{"email by dotted login", "mail mia.keller@corp.example", "FIXTURE_MASK", "mail human@example.invalid",
			func(e *FixtureExporter) { e.Mask = []string{"mia.keller"} }},
		{"email by mail domain", "mail mia@corp.example, see corp.example/x", "FIXTURE_MASK", "mail human@example.invalid, see host.example",
			func(e *FixtureExporter) { e.Mask = []string{"corp.example"} }},
		{"mention by login", "thanks @dave77", "FIXTURE_MASK", "thanks @human",
			func(e *FixtureExporter) { e.Mask = []string{"dave77"} }},
		{"annotation kept", "added @Override to run, and @override", "FIXTURE_KEEP", "added @Override to run, and @override",
			func(e *FixtureExporter) { e.Keep = []string{"Override"} }},
		{"decorator kept", "added a @pytest.fixture", "FIXTURE_KEEP", "added a @pytest.fixture",
			func(e *FixtureExporter) { e.Keep = []string{"pytest"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFixtureSeed(t, "t-ca", 1580)
			g.session(t, "t-ca", 1, 1, 2, "no_push", "", tc.text)
			g.ex.Policy.Projects = nil
			msg := fixtureError(t, g, 1580)
			if !strings.Contains(msg, tc.list) || strings.Contains(msg, "approver") {
				t.Fatalf("refusal %q, want it to name %s and no approver", msg, tc.list)
			}
			tc.act(g.ex)
			got, err := g.ex.Export(context.Background(), "sf/r", 1580)
			if err != nil {
				t.Fatalf("Export with %s set = %v, want it to pass", tc.list, err)
			}
			if s := got.Sessions[0].Summary; s != tc.want {
				t.Fatalf("summary = %q, want %q", s, tc.want)
			}
		})
	}
}

// A board close after an ask has no kind: the question's fallback does not
// reach it.
func TestExportFixtureGivesACloseAfterAnAskNoKind(t *testing.T) {
	g := newFixtureSeed(t, "t-ck", 1590)
	g.event(t, "t-ck", 1590, 5, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"ask": "Extend?", "source": "cap", "reason": "capped"})
	g.event(t, "t-ck", 1590, 10, EventCardClosed, "sebastian", ViaConsole, map[string]any{"note": "no"})
	got, _ := g.export(t, 1590)
	if got.Asks[0].CaseKind != "session_cap" || got.Answers[0].Action != EventCardClosed || got.Answers[0].CaseKind != "" {
		t.Fatalf("asks %+v, answers %+v; want the ask a session_cap and the close no kind", got.Asks, got.Answers)
	}
}

// An npm scope is a package, no login: "@types/node" passes, a bare
// "@hans" is still refused.
func TestExportFixtureLetsAnNpmScopePass(t *testing.T) {
	g := newFixtureSeed(t, "t-np", 1600)
	g.session(t, "t-np", 1, 1, 2, "no_push", "", "installed @types/node and @babel/core.js")
	got, _ := g.export(t, 1600)
	if s := got.Sessions[0].Summary; s != "installed @types/node and @babel/core.js" {
		t.Fatalf("summary = %q", s)
	}
	h := newFixtureSeed(t, "t-np2", 1601)
	h.session(t, "t-np2", 1, 1, 2, "no_push", "", "thanks @hans, and @types/node")
	if msg := fixtureError(t, h, 1601); !strings.Contains(msg, "an @mention") || !strings.Contains(msg, "byte 7") {
		t.Fatalf("refusal %q, want the mention at byte 7", msg)
	}
}

// A finding that cites a ledger ruling by its number goes out masked, its
// id is the id of the masked text (the id the replay computes from the
// text it serves), and the check-in's verdict on it is keyed by that id.
func TestExportFixtureMasksRulingCitationsAndKeepsIdsOfTheMaskedText(t *testing.T) {
	g := newFixtureSeed(t, "t-rc", 1562)
	cited := "docs/plan.md: the dated ruling " + "326 and Rulings R" + "12 are not delivered, the over" + "ruling " + "2 holds"
	masked := "docs/plan.md: the dated ruling entry and ruling entry are not delivered, the overruling entry holds"
	g.session(t, "t-rc", 1, 1, 11, "pushed", "1111111", "first push")
	g.line(t, "t-rc", 12, "model_review", map[string]any{"commit": "1111111", "verdict": "concerns", "findings": []string{cited}, "round": 1})
	g.line(t, "t-rc", 14, "checkin", map[string]any{"round": 1, "decision": "handover", "reason_kind": "findings", "reason": "stands",
		"verdicts": map[string]any{cited: map[string]any{"state": "confirmed"}},
		"record":   map[string]any{"session": "ck1", "parent_session": "s1", "moment": "review", "decision": "handover"}})
	g.event(t, "t-rc", 1562, 12, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "1111111", "round": 1, "reviewed": true, "verdict": "concerns"})
	got, body := g.export(t, 1562)
	if strings.Contains(strings.ToLower(body), "ruling "+"3") || strings.Contains(body, "R"+"12") {
		t.Errorf("the export cites a ruling by number:\n%s", body)
	}
	if len(got.Gates) != 1 || len(got.Gates[0].Findings) != 1 {
		t.Fatalf("gates = %+v, want one review with one finding", got.Gates)
	}
	if f := got.Gates[0].Findings[0]; f.Text != masked || f.ID != findingID(masked) {
		t.Errorf("finding = %+v, want text %q with the id of that text, %s", f, masked, findingID(masked))
	}
	if v := got.Sessions[0].CheckIn.Verdicts; len(v) != 1 || v[findingID(masked)] != "confirmed" {
		t.Errorf("verdicts = %v, want one on %s", v, findingID(masked))
	}
}

// Two findings that differ only in the ruling they cite mask to one text, and
// the export refuses them, naming both ids, where it would keep one verdict.
func TestExportFixtureRefusesFindingsThatMaskToOneText(t *testing.T) {
	g := newFixtureSeed(t, "t-cl", 1563)
	a, b := "docs/plan.md: ruling "+"326 is not delivered", "docs/plan.md: ruling "+"327 is not delivered"
	g.session(t, "t-cl", 1, 1, 11, "pushed", "1111111", "first push")
	g.line(t, "t-cl", 12, "model_review", map[string]any{"commit": "1111111", "verdict": "concerns", "findings": []string{a, b}, "round": 1})
	g.event(t, "t-cl", 1563, 12, EventModelReview, FactoryDecider, ViaFactoryd, map[string]any{"commit": "1111111", "round": 1, "reviewed": true, "verdict": "concerns"})
	_, err := g.ex.Export(context.Background(), "sf/r", 1563)
	if err == nil || !strings.Contains(err.Error(), findingID(a)) || !strings.Contains(err.Error(), findingID(b)) {
		t.Fatalf("Export = %v, want a refusal naming %s and %s", err, findingID(a), findingID(b))
	}
}

// The policy gate's class on a head comes out as a gate row, from the
// timeline's approval request: the replay serves it per head.
func TestExportFixtureCarriesTheRiskClassOfAnApprovedHead(t *testing.T) {
	g := newFixtureSeed(t, "t-pc", 1564)
	g.session(t, "t-pc", 1, 1, 11, "pushed", "1111111", "first push")
	g.line(t, "t-pc", 12, "approval_requested", map[string]any{"approval": 4, "pr": 3, "commit": "1111111", "class": "migration"})
	got, _ := g.export(t, 1564)
	want := []FixtureGate{{Head: "1111111", Gate: "policy", Verdict: "migration", Findings: []FixtureFinding{}}}
	if !reflect.DeepEqual(got.Gates, want) {
		t.Fatalf("gates = %+v, want %+v", got.Gates, want)
	}
}
