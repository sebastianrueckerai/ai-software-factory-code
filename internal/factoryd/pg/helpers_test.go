package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The small helpers the moved files share with factoryd's own test files
// and cannot reach across the package line: copies, kept verbatim.

const caseTask = "t1410"

// cardCaseStore is the cases_events helper, held for the moved files: a
// store whose one task, caseTask, waits on a human with a budget of 5 EUR,
// its cases, events, intake rows, approvals and plans empty.
func cardCaseStore(t *testing.T, name string) *factoryd.PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := factoryd.OpenStore(ctx, schematest.DSN(t, name))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	if _, err := s.DB().ExecContext(ctx, `TRUNCATE cases, events, intake_events, tasks, approval_requests, plan_revisions, plan_questions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, factoryd.Task{ID: caseTask, Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "sf/r",
		IssueNumber: 1410, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return s
}

func recordCaseEvent(t *testing.T, s *factoryd.PGStore, kind, actor, via string, detail map[string]any) {
	t.Helper()
	if _, err := s.Record(context.Background(), Event{Repository: "sf/r", TaskID: caseTask, IssueNumber: 1410, Kind: kind,
		Actor: actor, Via: via, Detail: detail}); err != nil {
		t.Fatalf("Record %s: %v", kind, err)
	}
}

const capAsk = "Card 1410 asks: grant one more session?"

// askCap records the session cap's ask as Escalate writes it.
func askCap(t *testing.T, s *factoryd.PGStore, comment int64) {
	t.Helper()
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, capAskDetail(comment))
}

// capAskDetail is the session cap's ask row on comment.
func capAskDetail(comment int64) map[string]any {
	return map[string]any{
		"question": capAsk, "ask": capAsk, "comment": comment, "source": "cap",
		"reason": capReached + "\nlast: the review asked for changes",
		"why":    "the card used its 3 sessions", "info": "a grant buys one session",
		"choices": []string{"Grant one session", "Leave it waiting"}}
}

func onlyOpenCase(t *testing.T, s *factoryd.PGStore) Case {
	t.Helper()
	open, err := s.OpenCasesOf(context.Background(), caseTask)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenCasesOf = %+v, %v; want one case", open, err)
	}
	return open[0]
}

func caseByID(t *testing.T, s *factoryd.PGStore, id int64) Case {
	t.Helper()
	c, err := s.CaseByID(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseByID %d: %v", id, err)
	}
	return c
}

func caseStepsOf(t *testing.T, s *factoryd.PGStore, id int64) []CaseStep {
	t.Helper()
	steps, err := s.CaseSteps(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseSteps %d: %v", id, err)
	}
	return steps
}

func openMerge(t *testing.T, s *factoryd.PGStore, head string) int64 {
	t.Helper()
	spec, err := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: head, RedChecks: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalMerge, TaskID: caseTask, Project: "sf/r", PRNumber: 1479, Spec: spec})
	if err != nil {
		t.Fatalf("OpenApproval: %v", err)
	}
	recordCaseEvent(t, s, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{
		"approval": id, "kind": ApprovalMerge, "pr": int64(1479), "class": "standard", "factory": false, "red_checks": true})
	return id
}

type aliasPrices map[string]policy.Price

func (p aliasPrices) Price(_ context.Context, alias string) (policy.Price, error) {
	if price, ok := p[alias]; ok {
		return price, nil
	}
	return policy.Price{}, fmt.Errorf("LiteLLM has no model %q", alias)
}

const testPolicyYAML = `version: 1
budget:
  task_default_eur: 2
  task_max_eur: 5
  day_eur: 20
  month_eur: 50
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder:
    session: code
    models: [small, big]
    tools: {}
    review_model: big
    supervisor_model: supervisor
prices:
  max_output_tokens: {small: 32768, big: 32768, supervisor: 32768, operator: 32768, pulse: 32768}
risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: requires_approval}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: migration, paths: ["internal/*/migrations/**", "internal/policy/schema.sql"]}
    - {class: high, paths: ["k8s/**", "**/Dockerfile*", "go.mod", "go.sum"]}
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`

const testProcessYAML = `tracks:
  direct: {class: coder}
merge: {queue: none}
stages:
  - {name: checks, on_findings: agent, enabled: true}
  - {name: policy, on_findings: fail, enabled: true}
  - {name: review, on_findings: agent, enabled: true}
  - {name: approval, on_findings: human, enabled: true}
  - {name: merge, on_findings: human, enabled: true}
`

// testFiles is a configuration tree with policyYAML as its policy.yaml.
func testFiles(policyYAML string) map[string][]byte {
	return map[string][]byte{
		"policy.yaml":                []byte(policy.Header("policy.yaml") + "\n" + policyYAML),
		"processes/maintenance.yaml": []byte(policy.Header("processes/maintenance.yaml") + "\n" + testProcessYAML),
		"projects/ai-software-factory-infra.yaml": []byte(policy.Header("projects/ai-software-factory-infra.yaml") +
			"\nrepository: sf/ai-software-factory-infra\nmerge_rule: human\nobjects: seed\nagents: none\ntokens: {read: forgejo-config}\n"),
	}
}

// testPolicyVersion is version 7 of testPolicyYAML.
func testPolicyVersion() policy.Version {
	files := testFiles(testPolicyYAML)
	files["projects/repo.yaml"] = []byte(policy.Header("projects/repo.yaml") +
		"\nrepository: sf/repo\nmerge_rule: human\nobjects: seed\nagents: none\ntokens: {read: forgejo-config}\nchain: {mode: off}\n")
	tree, err := policy.ParseTree(files)
	if err != nil {
		panic(err)
	}
	return policy.Version{ID: 7, Commit: "c7", Tree: tree}
}

// openAtHuman opens case c and seats it at the human as the door leaves a
// case, owned by rung 3 with no wake: the seed of a test whose case reached
// the human by a path the test does not drive.
func openAtHuman(ctx context.Context, s factoryd.Store, c factoryd.Case) (factoryd.Case, bool, error) {
	pg, ok := s.(*factoryd.PGStore)
	if !ok {
		return factoryd.Case{}, false, fmt.Errorf("openAtHuman seats a case in Postgres, not in a %T", s)
	}
	got, opened, err := pg.OpenCase(ctx, c)
	if err == nil {
		_, err = pg.DB().ExecContext(ctx, `UPDATE cases SET status = 'at_human', owner_rung = 3, next_wake_at = NULL WHERE id = $1`, got.ID)
	}
	if err == nil {
		got, err = pg.CaseByID(ctx, got.ID)
	}
	return got, opened, err
}

// facedCap is the session cap's case at the human with its face on comment:
// the case its ask opens, seated at the human, then its face's ask row,
// which names it and gives it the face's step of rung 3. The seed of a test
// about a case already faced.
func facedCap(t *testing.T, s *factoryd.PGStore, comment int64) Case {
	t.Helper()
	ctx := context.Background()
	card, _, err := s.Task(ctx, caseTask)
	if err != nil {
		t.Fatal(err)
	}
	ask := capAskDetail(comment)
	c, err := CaseFromAsk(card, Event{Kind: EventQuestionAsked, Detail: ask}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c, _, err = openAtHuman(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	ask["case_id"] = c.ID
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, ask)
	return caseByID(t, s, c.ID)
}
