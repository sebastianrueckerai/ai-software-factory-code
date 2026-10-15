package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Requires a disposable Postgres: GATEWAY_TEST_DATABASE_URL must point at a
// database this test may wipe. Skipped otherwise; CI stays hermetic.
func TestPGStore(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, audit, ledger, holds, policy_activations, policy_versions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Booting twice must be safe: migrations are idempotent.
	s2, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("second OpenStore: %v", err)
	}
	defer s2.db.Close()

	if _, ok, err := s.Task(ctx, "t1"); err != nil || ok {
		t.Fatalf("unknown task = (%v, %v), want (false, nil)", ok, err)
	}

	// factoryd owns task rows; seed one the way it would.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, required_capabilities, repository, model_alias) VALUES ('t1', 10, 2, 'running', '["coder"]', 'sf/app', 'coding-cheap')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("Task = (%v, %v), want (true, nil)", ok, err)
	}
	want := Task{ID: "t1", BudgetEUR: 10, SpentEUR: 2, Status: "running", Classes: []string{"coder"}, Repository: "sf/app", ModelAlias: "coding-cheap"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task = %+v, want %+v", got, want)
	}

	// a hold and its release book the task and the ledger together
	h, err := s.TakeHold(ctx, policy.HoldRequest{TaskID: "t1", Kind: policy.KindModel, Ref: "coding-cheap",
		Bytes: 1000, Price: policy.Price{In: 1e-6, Out: 4e-6, MaxOutput: 32768}, WantOut: 1024, DayEUR: 20, MonthEUR: 50, ProjectEUR: math.Inf(1)})
	if err != nil || h.MaxOut != 1024 {
		t.Fatalf("TakeHold = (%+v, %v), want 1024 tokens", h, err)
	}
	cost := 0.5
	// the hold is 1000 × 1e-6 + 1024 × 4e-6 = 0.005096 EUR: 0.5 is over it
	if b, err := s.ReleaseHold(ctx, h, &cost, policy.Usage{}); err != nil || b.Spent != 2.5 ||
		b.OverHold != "COST OVER HOLD: model coding-cheap cost 0.500000 EUR against a hold of 0.005096 EUR; booked in full" {
		t.Fatalf("ReleaseHold = (%+v, %v), want spent 2.5, flagged", b, err)
	}
	var ledger string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(task_id || ' ' || kind || ' ' || ref || ' ' || eur::text, ';') FROM ledger`).Scan(&ledger); err != nil || ledger != "t1 model coding-cheap 0.5" {
		t.Fatalf("ledger = (%q, %v)", ledger, err)
	}

	if _, ok, err := s.CurrentPolicy(ctx); err != nil || ok {
		t.Fatalf("CurrentPolicy before any load = (%v, %v), want (false, nil)", ok, err)
	}
	doc := testPolicy(0, 20).Document
	v, _, err := policy.Record(ctx, s.db, "abc", policy.Tree{Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	cur, ok, err := s.CurrentPolicy(ctx)
	if err != nil || !ok || cur.ID != v.ID || !reflect.DeepEqual(cur.Document, doc) {
		t.Fatalf("CurrentPolicy = (%+v, %v, %v), want version %d", cur, ok, err, v.ID)
	}

	spend := 2.75
	if err := s.InsertAudit(ctx, AuditRow{
		TaskID: "t1", Decision: "allowed", Reason: "", ModelAlias: "coding-cheap", SpendEUR: &spend, PolicyVersion: &v.ID,
		OutputLimit: &h.MaxOut, HoldEUR: &h.EUR, OverHold: "COST OVER HOLD: x", Deployment: "zai/glm-5.3",
	}); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	if err := s.InsertAudit(ctx, AuditRow{
		TaskID: "t1", Decision: "refused_budget", Reason: "budget_exhausted", Tool: "web_scrape",
	}); err != nil {
		t.Fatalf("InsertAudit (no spend): %v", err)
	}

	if err := s.InsertAudit(ctx, AuditRow{TaskID: "t2", Kind: policy.PurposeReview, Decision: "allowed", ModelAlias: "review-frontier"}); err != nil {
		t.Fatalf("InsertAudit (review): %v", err)
	}
	var kinds string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(task_id || '=' || kind, ';' ORDER BY id) FROM audit`).Scan(&kinds); err != nil ||
		kinds != "t1=agent;t1=agent;t2=review" {
		t.Fatalf("audit kinds = (%q, %v), want an agent's two and the review", kinds, err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE task_id = 't1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
	var flags string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(decision || '=' || over_hold, ';' ORDER BY id) FROM audit WHERE task_id = 't1'`).Scan(&flags); err != nil ||
		flags != "allowed=COST OVER HOLD: x;refused_budget=" {
		t.Fatalf("over_hold = (%q, %v)", flags, err)
	}
	var deployments string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(decision || '=' || deployment, ';' ORDER BY id) FROM audit WHERE task_id = 't1'`).Scan(&deployments); err != nil ||
		deployments != "allowed=zai/glm-5.3;refused_budget=" {
		t.Fatalf("deployment = (%q, %v)", deployments, err)
	}
	var nullSpend any
	if err := s.db.QueryRowContext(ctx,
		`SELECT spend_eur FROM audit WHERE decision = 'refused_budget'`).Scan(&nullSpend); err != nil {
		t.Fatal(err)
	}
	if nullSpend != nil {
		t.Fatalf("spend_eur of refusal = %v, want NULL", nullSpend)
	}
	var tools []string
	rows, err := s.db.QueryContext(ctx, `SELECT tool FROM audit WHERE task_id = 't1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tool string
		rows.Scan(&tool)
		tools = append(tools, tool)
	}
	if !reflect.DeepEqual(tools, []string{"", "web_scrape"}) {
		t.Fatalf("audit tools = %q, want a model call's empty tool, then web_scrape", tools)
	}
	var auditVersion int64
	var limit int
	var held float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT policy_version_id, output_limit, hold_eur::float8 FROM audit WHERE decision = 'allowed'`).Scan(&auditVersion, &limit, &held); err != nil ||
		auditVersion != v.ID || limit != 1024 || held != h.EUR {
		t.Fatalf("audit = version %d, limit %d, hold %v (%v); want %d, 1024, %v", auditVersion, limit, held, err, v.ID, h.EUR)
	}
}

// A settled stream books LiteLLM's cost from its final chunk, which prices
// the cached tokens at the cache-read rate: on an alias of 0.30 in, 0.03
// cache read and 1.50 out EUR per million, 140,000 prompt tokens, 131,600 of
// them cached, and 900 output tokens cost 8,400 × 0.30e-6 + 131,600 ×
// 0.03e-6 + 900 × 1.50e-6 = 0.00252 + 0.003948 + 0.00135 = 0.007818 EUR,
// booked with the usage and the bytes sent upstream.
func TestSettledStreamBooksItsUsage(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, audit, ledger, holds, policy_activations, policy_versions CASCADE;
		INSERT INTO tasks (id, budget_eur, spent_eur, status, required_capabilities, repository, model_alias) VALUES ('t1', 10, 0, 'running', '["coder"]', 'sf/app', 'coding-cheap')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.Record(ctx, s.db, "abc", policy.Tree{Document: testPolicy(0, 20).Document}); err != nil {
		t.Fatal(err)
	}
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, headers: map[string]string{"Content-Type": "text/event-stream"},
		body: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":140000,\"completion_tokens\":900,\"prompt_tokens_details\":{\"cached_tokens\":131600},\"cost\":0.007818}}\n\n" +
			"data: [DONE]\n\n"})
	srv := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "t1", SAUID: "sa"}}, Store: s, Minter: &fakeMinter{}, LiteLLMURL: stub.URL,
		Prices: fakePrices{"coding-cheap": {In: 0.30e-6, CacheRead: 0.03e-6, Out: 1.50e-6, MaxOutput: 32768}}})
	if rr := doChat(t, srv.Routes("test"), `{"model":"coding-cheap","stream":true,"messages":[]}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	_, bodies, _ := stub.seen()
	var row string
	if err := s.db.QueryRowContext(ctx, `SELECT eur::text || ' ' || prompt_tokens || ' ' || cached_tokens || ' ' || output_tokens || ' ' || body_bytes
		FROM ledger WHERE task_id = 't1'`).Scan(&row); err != nil || len(bodies) != 1 || row != fmt.Sprintf("0.007818 140000 131600 900 %d", len(bodies[0])) {
		t.Fatalf("ledger row = %q (%v), want 0.007818 EUR, 140000 prompt, 131600 cached, 900 output tokens and the upstream body's bytes", row, err)
	}
}

// A sandbox's call books its ledger row under the session its token's pod
// names (roadmap 243), and every audit row names its task's issue author,
// the human behind the call (roadmap 41): the allowed call's and a
// refusal's, and nobody for a task no row knows.
func TestRowsCarryTheSessionAndTheAuthor(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, audit, ledger, holds, policy_activations, policy_versions CASCADE;
		INSERT INTO tasks (id, budget_eur, spent_eur, status, required_capabilities, repository, model_alias, author)
		VALUES ('t1', 10, 0, 'running', '["coder"]', 'sf/app', 'coding-cheap', 'alice')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.Record(ctx, s.db, "abc", policy.Tree{Document: testPolicy(0, 20).Document}); err != nil {
		t.Fatal(err)
	}
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, headers: map[string]string{"Content-Type": "text/event-stream"},
		body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":10,\"cost\":0.0004}}\n\ndata: [DONE]\n\n"})
	srv := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "t1", SAUID: "sa", Session: "s-1"}}, Store: s, Minter: &fakeMinter{}, LiteLLMURL: stub.URL,
		Prices: fakePrices{"coding-cheap": {In: 0.30e-6, Out: 1.50e-6, MaxOutput: 32768}}})
	if rr := doChat(t, srv.Routes("test"), `{"model":"coding-cheap","stream":true,"messages":[]}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if rr := doChat(t, srv.Routes("test"), `{"model":"coding-frontier","messages":[]}`); rr.Code != http.StatusForbidden {
		t.Fatalf("another model's call = %d (%s), want the capability refusal", rr.Code, rr.Body.String())
	}
	if err := s.InsertAudit(ctx, AuditRow{TaskID: "t-gone", Decision: "refused_task_unknown"}); err != nil {
		t.Fatal(err)
	}
	var session string
	if err := s.db.QueryRowContext(ctx, `SELECT session FROM ledger WHERE task_id = 't1'`).Scan(&session); err != nil || session != "s-1" {
		t.Fatalf("ledger session = (%q, %v), want s-1", session, err)
	}
	var authors string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(decision || '=' || author, ';' ORDER BY id) FROM audit`).Scan(&authors); err != nil ||
		authors != "allowed=alice;refused_capability=alice;refused_task_unknown=" {
		t.Fatalf("audit authors = (%q, %v)", authors, err)
	}
}
