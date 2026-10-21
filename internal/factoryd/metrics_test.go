package factoryd

import (
	"bytes"
	"context"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

func testMetrics(t *testing.T) (*telemetry.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	m, err := telemetry.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	return m, reader
}

type histPoint struct {
	Count uint64
	Sum   float64
}

func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Aggregation, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data, true
			}
		}
	}
	return nil, false
}

func attrKey(s attribute.Set) string { return s.Encoded(attribute.DefaultEncoder()) }

// histograms maps each attribute set ("k=v,k=v", sorted) to its count and sum.
func histograms(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]histPoint {
	t.Helper()
	out := map[string]histPoint{}
	data, ok := collect(t, reader, name)
	if !ok {
		return out
	}
	for _, dp := range data.(metricdata.Histogram[float64]).DataPoints {
		out[attrKey(dp.Attributes)] = histPoint{dp.Count, dp.Sum}
	}
	return out
}

func sums(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	data, ok := collect(t, reader, name)
	if !ok {
		return out
	}
	switch d := data.(type) {
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = dp.Value
		}
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = float64(dp.Value)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = dp.Value
		}
	default:
		t.Fatalf("%s: unexpected %T", name, data)
	}
	return out
}

// ci_runs is append-only: the first finished run of a commit carries the
// push time every later figure reads, so a re-run never rewrites it.
func TestPGStoreRecordCIRun(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE ci_runs`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 9, 59, 58, 0, time.UTC)
	if err := s.RecordCIRun(ctx, "sf/ai-software-factory-code", "c62cd2d1", at, 315000); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCIRun(ctx, "sf/ai-software-factory-code", "c62cd2d1", at.Add(time.Hour), 999000); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCIRun(ctx, "sf/ai-software-factory-code", "9f1e0c3a", at.Add(6*time.Minute), 1000); err != nil {
		t.Fatal(err)
	}
	var pushedAt time.Time
	var duration int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT pushed_at, duration_ms FROM ci_runs WHERE repository = 'sf/ai-software-factory-code' AND commit = 'c62cd2d1'`).
		Scan(&pushedAt, &duration); err != nil {
		t.Fatal(err)
	}
	if !pushedAt.Equal(at) || duration != 315000 {
		t.Fatalf("the first run of the commit won: %v %d", pushedAt, duration)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM ci_runs`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d, %v; want 2", n, err)
	}
}

// The merge queue writes the card's merge commit once: the first write
// stands, so a re-merged pull request never rewrites the delivery fact.
func TestPGStoreSetTaskMergeCommit(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, finished_at) VALUES ('t1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', now())`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskMergeCommit(ctx, "t1", "m-abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskMergeCommit(ctx, "t1", "m-later"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("task read: %v %v", ok, err)
	}
	if got.MergeCommit != "m-abc" {
		t.Fatalf("merge commit %q, want the first write m-abc", got.MergeCommit)
	}
}

// An unknown task id drops the merge-commit fact with a log line; the
// re-pass of a card whose commit already stands stays quiet.
func TestPGStoreSetTaskMergeCommitUnknownTaskLogs(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, finished_at) VALUES ('u1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', now())`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	for _, c := range [][2]string{{"u1", "m-abc"}, {"u1", "m-later"}, {"nope", "m-x"}} {
		if err := s.SetTaskMergeCommit(ctx, c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	log.SetOutput(prev)
	if strings.Count(buf.String(), "nope") != 1 {
		t.Fatalf("log = %q, want one line naming the unknown task", buf.String())
	}
	if strings.Contains(buf.String(), "u1") {
		t.Fatalf("log = %q, want the first write and the re-pass quiet", buf.String())
	}
}

// The metrics fixture: repository sf/code over window [Sep 28, Oct 12)
// (prev [Sep 14, Sep 28)), spark buckets b0..b6 = seven weeks from Aug 24,
// each ending where the window does. sf/other holds the scoping rows.
func seedMetricsFigures(t *testing.T, s *PGStore) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"events", "alerts", "ledger", "audit", "project_rolls", "ci_runs", "rolls", "chat_messages", "tasks"} {
		if _, err := s.db.ExecContext(ctx, `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// cards: c1, c2 (reverted+refiled), c3, c6 (the SigNoz fix), c5 (prev),
	// c4 (chat, no card), c7 (running, reverted — no figure may count it),
	// o1 (sf/other), r1 and r0 (the factory's roll cards, which no card
	// figure counts: r1 rolled commit cx as s-2, r0 rolled cs3 as s-3)
	must(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at, finished_at, files, insertions, deletions, merge_commit) VALUES
		('c1', 1, 0, 'done', 'sf/code', 'agent', 'webhook', $1, $2, 3, 120, 40, 'm-c1'),
		('c2', 1, 0, 'done', 'sf/code', 'agent', 'webhook', $3, $4, NULL, NULL, NULL, ''),
		('c3', 1, 0, 'done', 'sf/code', 'agent', 'webhook', $5, $6, 9, 240, 95, 'm-c3'),
		('c6', 1, 0, 'done', 'sf/code', 'agent', 'alert',   $7, $8, NULL, NULL, NULL, ''),
		('c5', 1, 0, 'done', 'sf/code', 'agent', 'webhook', '2026-09-18T08:00Z', '2026-09-20T10:00Z', 5, 80, 20, ''),
		('c4', 1, 0, 'done', 'sf/code', 'chat',  'webhook', '2026-10-06T09:00Z', '2026-10-07T12:00Z', NULL, NULL, NULL, ''),
		('o1', 1, 0, 'done', 'sf/other', 'agent', 'webhook', '2026-10-01T07:00Z', '2026-10-02T17:00Z', 2, 10, 5, ''),
		('r1', 1, 0, 'done', 'sf/code', 'agent', 'roll', '2026-10-03T10:00Z', '2026-10-03T10:05Z', 4, 8, 8, 's-2'),
		('r0', 1, 0, 'merged', 'sf/code', 'agent', 'roll', '2026-09-22T10:00Z', '2026-09-22T11:05Z', NULL, NULL, NULL, 's-3')`,
		"2026-09-29T06:00Z", "2026-10-02T10:00Z", // c1: 76 h lifetime
		"2026-10-01T08:00Z", "2026-10-03T09:00Z", // c2: 49 h
		"2026-10-06T10:00Z", "2026-10-08T16:00Z", // c3: 54 h
		"2026-10-06T09:00Z", "2026-10-06T21:00Z") // c6: 12 h
	must(`UPDATE tasks SET reverted_at = '2026-10-04T12:00Z' WHERE id = 'c2'`)
	must(`UPDATE tasks SET reverted_at = '2026-09-22T09:00Z' WHERE id = 'c5'`)
	must(`UPDATE tasks SET alert_fingerprint = 'fp-x' WHERE id = 'c6'`)
	must(`UPDATE tasks SET alert_fingerprint = 'fp-p' WHERE id = 'c5'`)
	must(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at, finished_at, alert_fingerprint) VALUES
		('c8', 1, 0, 'merged', 'sf/code', 'agent', 'alert', '2026-10-07T08:30Z', '2026-10-07T09:00Z', 'fp-m')`)
	must(`UPDATE tasks SET alert_fingerprint = '' WHERE id = 'c3'`)
	must(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at) VALUES
		('c7', 1, 0, 'running', 'sf/code', 'agent', 'webhook', '2026-10-05T08:00Z')`)
	must(`UPDATE tasks SET reverted_at = '2026-10-06T10:00Z' WHERE id = 'c7'`)
	must(`UPDATE tasks SET alert_fingerprint = 'fp-c2' WHERE id = 'c2'`)
	must(`UPDATE tasks SET sha = 'cx' WHERE id = 'r1'`)
	must(`UPDATE tasks SET sha = 'cs3' WHERE id = 'r0'`)
	// the merges that put the rolled commits on main, a minute before each
	// commit's CI run: merged cards, not done ones, so no card figure
	// counts them; a merged card is finished at its merge
	must(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, merge_commit, finished_at) VALUES
		('m0', 1, 0, 'merged', 'sf/code', 'agent', 'webhook', 'cr-0', '2026-09-25T09:29Z'),
		('m1', 1, 0, 'merged', 'sf/code', 'agent', 'webhook', 'cr-1', '2026-09-20T08:59Z'),
		('m2', 1, 0, 'merged', 'sf/code', 'agent', 'webhook', 'cr-2', '2026-10-11T22:59Z'),
		('m4', 1, 0, 'merged', 'sf/code', 'agent', 'webhook', 'cr-4', '2026-10-05T10:59Z'),
		('m5', 1, 0, 'merged', 'sf/code', 'agent', 'webhook', 'cr-5', '2026-08-23T23:39Z'),
		('mo', 1, 0, 'merged', 'sf/other', 'agent', 'webhook', 'ox', '2026-10-02T13:59Z')`)
	merged := func(task, repo, at string) {
		t.Helper()
		must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES ($1, $2, $3, 'pull_request_merged', 'factoryd', 'factoryd', '{}')`, at, repo, task)
	}
	merged("m0", "sf/code", "2026-09-25T09:29Z") // 31 min to d-0 ready
	merged("m1", "sf/code", "2026-09-20T08:59Z") // 46 min to d-1
	merged("m2", "sf/code", "2026-10-11T22:59Z") // 60 min to d-2
	merged("m4", "sf/code", "2026-10-05T10:59Z") // 61 min to d-4
	merged("m5", "sf/code", "2026-08-23T23:39Z") // 21 min to d-5
	merged("mo", "sf/other", "2026-10-02T13:59Z")
	status := func(task, at, status string) {
		t.Helper()
		must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES ($1, 'sf/code', $2, 'task_status', 'factoryd', 'factoryd', $3)`,
			at, task, `{"status":"`+status+`"}`)
	}
	status("c1", "2026-09-29T07:00Z", "running")
	status("c1", "2026-10-01T09:00Z", "in-review")
	status("c1", "2026-10-01T09:30Z", "in-review")     // the gate sets the status again: the same review
	status("c1", "2026-10-01T10:00Z", "waiting-human") // blocked 5 h to closing
	status("c1", "2026-10-01T15:00Z", "closing")
	status("c1", "2026-10-02T10:00Z", "done")
	status("c2", "2026-10-01T08:30Z", "running")
	status("c2", "2026-10-02T09:00Z", "waiting-poster") // blocked 24 h to done
	status("c2", "2026-10-03T09:00Z", "done")
	status("c3", "2026-10-06T10:30Z", "running")
	status("c3", "2026-10-07T11:00Z", "in-review") // first review: changes (the next new status is running)
	status("c3", "2026-10-07T11:30Z", "in-review")
	status("c3", "2026-10-07T14:00Z", "running")
	status("c3", "2026-10-08T09:00Z", "in-review")
	status("c3", "2026-10-08T10:00Z", "closing")
	status("c3", "2026-10-08T16:00Z", "done")
	status("c6", "2026-10-06T09:30Z", "running")
	status("c6", "2026-10-06T21:00Z", "done")
	status("r1", "2026-10-03T10:01Z", "running")
	status("r1", "2026-10-03T10:02Z", "in-review")
	status("r1", "2026-10-03T10:04Z", "closing")
	status("r1", "2026-10-03T10:05Z", "done")
	status("c4", "2026-10-06T09:30Z", "running")
	status("c4", "2026-10-07T12:00Z", "done")
	status("c5", "2026-09-18T09:00Z", "running")
	status("c5", "2026-09-19T10:00Z", "in-review") // accepted (next is closing)
	status("c5", "2026-09-19T11:00Z", "closing")
	status("c5", "2026-09-19T12:00Z", "waiting-human") // blocked 22 h to done
	status("c5", "2026-09-20T10:00Z", "done")
	// human interventions: four in window (one on no card), one on the
	// roll card r1 that counts for nothing, none in prev, one in sf/other
	must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES
		('2026-10-01T11:00Z', 'sf/code', 'c1', 'comment', 'sebastian', 'button', '{}'),
		('2026-10-02T10:00Z', 'sf/code', 'c2', 'comment', 'sebastian', 'webhook', '{}'),
		('2026-10-07T12:00Z', 'sf/code', 'c3', 'comment', 'sebastian', 'console', '{}'),
		('2026-10-04T10:00Z', 'sf/code', NULL, 'card_filed', 'sebastian', 'button', '{}'),
		('2026-10-03T10:04Z', 'sf/code', 'r1', 'merge_approved', 'sebastian', 'button', '{}'),
		('2026-10-02T18:00Z', 'sf/other', 'o1', 'comment', 'sebastian', 'button', '{}')`)
	must(`INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
		('c1', '2026-10-02', 'model', 'big', 2.00),
		('c2', '2026-10-03', 'model', 'small', 3.00),
		('c2', '2026-10-03', 'model', 'review/big', 0.60),
		('c3', '2026-10-08', 'model', 'big', 2.20),
		('c4', '2026-10-07', 'model', 'compact/small', 1.00),
		('c4', '2026-10-07', 'model', 'vision/vision', 0.05),
		('c4', '2026-10-07', 'tool', 'web_scrape', 0.30),
		('c4', '2026-10-07', 'tool', 'web_search', 0.10),
		('c4', '2026-10-07', 'tool', 'fal_run', 0.05),
		('c4', '2026-10-07', 'tool', 'replicate_predict', 0.01),
		('m1', '2026-09-20', 'tool', 'web_scrape', 0.20),
		('c5', '2026-09-20', 'model', 'big', 1.50),
		('o1', '2026-10-02', 'model', 'big', 99.00)`)
	must(`INSERT INTO audit (ts, task_id, decision, reason, model_alias, deployment, prompt_tokens, output_tokens, call_cost_eur) VALUES
		('2026-10-02T10:05Z', 'c1', 'call', '', 'big', 'zai/glm-5.3', 1000, 500, 0.50),
		('2026-10-03T09:05Z', 'c2', 'call', '', 'big', 'zai/glm-5.3', 800, 400, 0.40),
		('2026-10-03T09:10Z', 'c2', 'call', '', 'big', 'dashscope/qwen3.8-max', 200, 100, 0.10), -- the fallback served the review
		('2026-10-08T16:05Z', 'c3', 'call', '', 'big', 'zai/glm-5.3', 1200, 600, 0.60),
		('2026-10-08T16:10Z', 'c3', 'call', '', 'small', 'zai/glm-5.3-flash', 200, 100, 0.02),
		('2026-10-07T12:05Z', 'c4', 'call', '', 'small', 'zai/glm-5.3-flash', 300, 100, 0.03),
		('2026-10-07T12:06Z', 'c4', 'call', '', 'vision', 'dashscope/qwen3-vl-plus', 100, 100, 0.05),
		('2026-10-03T10:03Z', 'r1', 'call', '', 'small', 'zai/glm-5.3-flash', 100, 50, 0.01),
		('2026-09-20T10:05Z', 'c5', 'call', '', 'big', 'zai/glm-5.3', 500, 250, 0.25),
		('2026-10-02T17:05Z', 'o1', 'call', '', 'big', 'zai/glm-5.3', 9999, 9999, 9.99)`)
	must(`INSERT INTO alerts (kind, source, task_id, raised_at, fingerprint) VALUES
		('alert', 'signoz', 'c6', '2026-10-06T08:00Z', 'fp-x'),   -- fixed 13 h later by c6
		('alert', 'signoz', 'c2', '2026-10-04T12:00Z', 'fp-c2'),  -- c2's refile: its alert back at the revert, no fix task
		('alert', 'signoz', 'c5', '2026-09-15T08:00Z', 'fp-p'),   -- prev: fixed 122 h later by c5
		('alert', 'signoz', 'c3', '2026-10-07T13:30Z', ''),       -- empty fingerprint pairs with nothing
		('alert', 'signoz', 'c6', '2026-10-07T08:00Z', 'fp-m'),   -- its fix card c8 is merged, waiting for the chain: no restore yet
		('escalation', 'gate', 'c3', '2026-10-07T13:00Z', NULL),   -- judged by the controller, an AI member
		('escalation', 'gate', 'c1', '2026-10-01T12:00Z', NULL),   -- judged by sebastian
		('escalation', 'gate', 'c6', '2026-10-06T10:00Z', NULL),   -- judged by the project agent
		('escalation', 'gate', 'c2', '2026-10-02T12:00Z', NULL),   -- closed unjudged with its card
		('escalation', 'gate', 'c5', '2026-09-21T10:00Z', NULL),   -- closed unjudged with its card
		('escalation', 'gate', 'o1', '2026-10-02T18:30Z', NULL)`)
	must(`UPDATE alerts SET resolved_by = 'controller', verdict = 'real' WHERE kind = 'escalation' AND task_id = 'c3'`)
	must(`UPDATE alerts SET resolved_by = 'sebastian', verdict = 'noise' WHERE kind = 'escalation' AND task_id = 'c1'`)
	must(`UPDATE alerts SET resolved_by = 'agent', verdict = 'real' WHERE kind = 'escalation' AND task_id = 'c6'`)
	must(`UPDATE alerts SET resolved_by = 'factoryd', verdict = 'unjudged' WHERE kind = 'escalation' AND task_id IN ('c5', 'c2')`)
	if _, _, err := policy.Record(ctx, s.db, "metrics-test", policy.Tree{Document: policy.Document{AIMembers: []string{"controller"}}}); err != nil {
		t.Fatal(err)
	}
	must(`INSERT INTO project_rolls (repository, namespace, digest, commit, ready_at) VALUES
		('sf/code', 'web', 'd-0', 'cr-0', '2026-09-25T10:00Z'),  -- prev, b4
		('sf/code', 'web', 'd-1', 'cr-1', '2026-09-20T09:45Z'),  -- prev, b3
		('sf/code', 'web', 'd-4', 'cr-4', '2026-10-05T12:00Z'),  -- window, b6
		('sf/code', 'web', 'd-2', 'cr-2', '2026-10-11T23:59Z'),  -- window, b6
		('sf/code', 'web', 'd-3', 'cr-3', '2026-10-12T00:01Z'),  -- outside: no bucket, no window
		('sf/code', 'web', 'd-5', 'cr-5', '2026-08-24T00:00Z'),  -- exactly SparkFrom: b0
		('sf/other', 'web', 'd-o', 'ox', '2026-10-02T15:00Z')`)
	must(`INSERT INTO ci_runs (repository, commit, pushed_at, duration_ms) VALUES
		('sf/code', 'cr-0', '2026-09-25T09:30Z', 200000),
		('sf/code', 'cr-1', '2026-09-20T09:00Z', 210000),
		('sf/code', 'cr-4', '2026-10-05T11:00Z', 300000),
		('sf/code', 'cr-2', '2026-10-11T23:00Z', 330000),
		('sf/code', 'cx',   '2026-10-02T10:00Z', 240000), -- rolled by the factory as s-2 (card r1)
		('sf/code', 'cs3',  '2026-09-22T09:00Z', 205000), -- rolled as s-3 (card r0): prev, b4
		('sf/code', 'cr-5', '2026-08-23T23:40Z', 90000),
		('sf/other', 'ox',  '2026-10-02T14:00Z', 500000)`)
	must(`INSERT INTO rolls (sha, image, deployment, pushed_at, applied_at, live_at) VALUES
		('s-1', 'factoryd', 'factoryd', '2026-10-04T08:00Z', '2026-10-04T08:30Z', '2026-10-04T09:00Z'), -- window, b5
		('s-2', 'factoryd', 'factoryd', '2026-10-07T07:00Z', '2026-10-07T07:30Z', '2026-10-07T08:00Z'), -- window, b6
		('s-2', 'worker', 'factoryd', '2026-10-07T07:00Z', '2026-10-07T07:30Z', '2026-10-07T08:05Z'),   -- the same roll's other images
		('s-2', 'gateway', 'factory-gateway', '2026-10-07T07:00Z', '2026-10-07T07:30Z', '2026-10-07T08:10Z'),
		('s-3', 'factoryd', 'factoryd', '2026-09-22T10:30Z', '2026-09-22T10:45Z', '2026-09-22T11:00Z'), -- prev, b4
		('s-4', 'factoryd', 'factoryd', '2026-10-12T00:10Z', '2026-10-12T00:20Z', '2026-10-12T00:30Z')`) // outside
	must(`INSERT INTO chat_messages (repository, login, role, content, tool_calls, at) VALUES
		('sf/code', 'sebastian', 'assistant', '', '[{"name":"read","ms":120000},{"name":"run","ms":60000}]', '2026-10-03T12:00Z'),
		('sf/code', 'sebastian', 'assistant', '', '[{"name":"read","ms":600000}]', '2026-10-09T15:00Z'),
		('sf/code', 'sebastian', 'assistant', '', NULL, '2026-09-20T08:00Z'),
		('sf/code', 'sebastian', 'user', 'hi', NULL, '2026-10-06T09:00Z'),
		('sf/other', 'sebastian', 'assistant', '', '[{"name":"read","ms":60000}]', '2026-10-06T10:00Z')`)
}

func metricsStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(context.Background(), schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// window: [Sep 28, Oct 12), prev [Sep 14, Sep 28), buckets b0..b6 from Aug 24
func metricsTestWindow() (from, to time.Time) {
	return time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
}

func metricsReport(t *testing.T, repository string) MetricsReport {
	t.Helper()
	from, to := metricsTestWindow()
	s := metricsStore(t)
	seedMetricsFigures(t, s)
	r, err := s.ProjectMetrics(context.Background(), repository, from, to)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMetricsFigures(t *testing.T) {
	r := metricsReport(t, "sf/code")
	fig := func(t *testing.T, name string, got, want Figure) {
		t.Helper()
		if got != want {
			t.Fatalf("%s = %+v, want %+v", name, got, want)
		}
	}
	shares := func(t *testing.T, name string, got, want []MixShare) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %+v, want %+v", name, got, want)
		}
		for i, w := range want {
			if got[i].Alias != w.Alias || math.Abs(got[i].Share-w.Share) > 1e-9 {
				t.Fatalf("%s[%d] = %+v, want %+v", name, i, got[i], w)
			}
		}
	}
	// deploys: the last weekly bucket answers (r4+r2 in b6), the bucket
	// before it moves (b5 empty) — a per-week name never shows a window total
	fig(t, "deploys", r.Deploys, Figure{Value: 2, Prev: 0, Has: true, PrevHas: true,
		Weeks: [7]float64{1, 0, 0, 1, 1, 0, 2}})
	// lead, merge to ready: d-2 60 min, d-4 61 min -> median 60.5 min;
	// prev d-0 31, d-1 46 -> 38.5; d-5 21 in b0
	fig(t, "lead time", r.LeadTime, Figure{Value: 3630000, Prev: 2310000, Has: true, PrevHas: true,
		Weeks: [7]float64{1260000, 0, 0, 2760000, 1860000, 0, 3630000}})
	// cycle: c1 76 h, c2 49 h, c3 54 h, c6 12 h -> median (49+54)/2 = 51.5 h
	fig(t, "card cycle", r.CardCycle, Figure{Value: 185400000, Prev: 180000000, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 180000000, 0, 225000000, 118800000}})
	// throughput: four cards done in window (c1 c2 c3 c6), c5 in prev;
	// the spark spans 49 days, so c5's Sep 20 finish lands in b3
	fig(t, "throughput", r.Throughput, Figure{Value: 4, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1, 0, 2, 2}})
	// change failure: 1 reverted (c2) of 4; prev c5 1 of 1
	fig(t, "change failure", r.ChangeFailure, Figure{Value: 0.25, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1, 0, 0.5, 0}})
	// reverted/refiled: c2 refiled (alert at its reverted_at), c5 a bare
	// revert; c7's revert is no done card, so the spark drops it too
	if r.RevertedRefiled != (RevertedRefiled{Reverted: 0, Refiled: 1, Prev: [2]float64{1, 0}, Has: true,
		Weeks: [7]float64{0, 0, 0, 0, 1, 1, 0}}) {
		t.Fatalf("reverted/refiled = %+v", r.RevertedRefiled)
	}
	// restore: a1 fixed by c6 13 h later; the empty fingerprint pairs with
	// nothing; prev 122 h
	fig(t, "time to restore", r.TimeToRestore, Figure{Value: 46800000, Prev: 439200000, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 439200000, 0, 0, 46800000}})
	// spend: the last week's euros (c3 2.2, chat c4 1, its vision look 0.05 and its tools 0.46), the week before
	// (c1 2 + c2 3.6) — per week, not the window's 8.8
	fig(t, "spend", r.Spend, Figure{Value: 3.71, Prev: 5.60, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1.70, 0, 5.60, 3.71}})
	// tools: c4's four tool calls 0.46 and its vision look 0.05 in the
	// last week, m1's 0.20 in b3
	fig(t, "tool spend", r.ToolSpend, Figure{Value: 0.51, Prev: 0, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0.20, 0, 0, 0.51}})
	// tool mix: of the window's 0.51, scrape 0.30, search 0.10, fal 0.05
	// (before vision's 0.05 by name), then vision and replicate 0.01 -> other
	shares(t, "tool mix", r.ToolMix, []MixShare{{"web_scrape", 3000.0 / 51}, {"web_search", 1000.0 / 51}, {"fal_run", 500.0 / 51}, {"other", 600.0 / 51}})
	// per card: totals 2, 3.6, 2.2, 0 -> median (2+2.2)/2 = 2.1
	fig(t, "per card", r.PerCard, Figure{Value: 2.10, Prev: 1.50, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1.50, 0, 2.80, 1.10}})
	// tokens/card: c1 1500 + c2 1500 + c3 2100 = 5100 over 4 cards; prev c5 750 over 1
	fig(t, "tokens per card", r.TokensPerCard, Figure{Value: 1275, Prev: 750, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 750, 0, 1500, 1050}})
	// spend by model, the window's ledger model rows by the class in
	// their ref: big c1 2 + c2's review 0.6 + c3 2.2 = 4.8, small c2 3 +
	// chat c4's compaction 1 = 4; the vision row is a tool's
	wantSpend := []MixShare{{"big", 480.0 / 8.8}, {"small", 400.0 / 8.8}}
	shares(t, "model spend", r.ModelSpend, wantSpend)
	// served by, the window's audit euros by deployment: glm-5.3 1.50,
	// qwen3.8-max 0.10 (the fallback), glm-5.3-flash 0.06, then the vision
	// model's 0.05 folds into other, of 1.71
	shares(t, "served by", r.ServedBy, []MixShare{{"zai/glm-5.3", 150.0 / 1.71}, {"dashscope/qwen3.8-max", 10.0 / 1.71},
		{"zai/glm-5.3-flash", 6.0 / 1.71}, {"other", 5.0 / 1.71}})
	// interventions: 4 human events over 4 cards (r1's approval is a roll
	// card's: none); prev none over 1; b5 holds 3 over c1 and c2
	fig(t, "interventions", r.Interventions, Figure{Value: 1, Prev: 0, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 0, 1.5, 0.5}})
	// escalations: c1's, c2's, c6's and c3's in the window (b5, b5, b6,
	// b6), c5's in prev (b4); o1's is sf/other's
	fig(t, "escalations", r.Escalations, Figure{Value: 4, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 1, 2, 2}})
	// escalated share: every one of the window's done cards c1 c2 c3 c6
	// escalated -> 4/4; prev c5 1/1; weeks by the cards' finish: b3 c5,
	// b5 c1 and c2, b6 c3 and c6
	fig(t, "escalated share", r.EscalatedShare, Figure{Value: 1, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1, 0, 1, 1}})
	// handled: sebastian judged c1's (human), the controller c3's and the
	// agent c6's (AI), c2's closed unjudged; prev c5's closed unjudged;
	// the human's per bucket
	if r.EscalationsHandled != (EscalationsHandled{Human: 1, AI: 2, Unjudged: 1, Prev: [3]float64{0, 0, 1}, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}) {
		t.Fatalf("escalations handled = %+v", r.EscalationsHandled)
	}
	// human blocked: (5 h + 24 h) / (76 + 49 + 54 + 12) h; prev 22/50 h
	fig(t, "human blocked", r.HumanBlocked, Figure{Value: 104400.0 / 687600, Prev: 0.44, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0.44, 0, 0.232, 0}})
	// review acceptance: c1 accepted, c3 changes, c2 c6 never reviewed -> 1/2
	fig(t, "review acceptance", r.ReviewAcceptance, Figure{Value: 0.5, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1, 0, 1, 0}})
	// self deploys: the last weekly bucket (s2, one roll of three images)
	// and the one before (s1)
	fig(t, "self deploys", r.SelfDeploys, Figure{Value: 1, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 1, 1, 1}})
	// diff size: medians of {3,120,40} and {9,240,95}; prev c5 alone
	if r.DiffSize != (DiffSize{Files: 6, Insertions: 180, Deletions: 67.5, Prev: [3]float64{5, 80, 20}, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 5, 0, 3, 9}}) {
		t.Fatalf("diff size = %+v", r.DiffSize)
	}
	// chat work: 3 calls, 13 minutes; prev none
	if r.ChatWork != (ChatWork{Calls: 3, Minutes: 13, PrevCalls: 0, PrevMinutes: 0,
		Weeks: [7]float64{0, 0, 0, 0, 0, 2, 1}}) {
		t.Fatalf("chat work = %+v", r.ChatWork)
	}
	// ci duration: cx 240, cr-4 300, cr-2 330 -> median 300; prev 200,
	// 205, 210 -> 205; b4 cr-0 200 and cs3 205 -> 202.5
	fig(t, "ci duration", r.CIDuration, Figure{Value: 300000, Prev: 205000, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 210000, 202500, 240000, 315000}})
}

// The factory's own project deploys by its rolls: its deploys are the
// self-deploys, its lead time runs from the CI run of the commit a roll
// card rolled to the roll's last image live.
func TestMetricsSelfProject(t *testing.T) {
	from, to := metricsTestWindow()
	s := metricsStore(t)
	seedMetricsFigures(t, s)
	s.SelfRepository = "sf/code"
	r, err := s.ProjectMetrics(context.Background(), "sf/code", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r.Deploys != r.SelfDeploys || r.Deploys.Value != 1 {
		t.Fatalf("deploys = %+v, want the self-deploys %+v", r.Deploys, r.SelfDeploys)
	}
	// s-2 (card r1: cx pushed Oct 2 10:00, last image live Oct 7 08:10):
	// 118 h 10 min; prev s-3 (card r0: cs3 09:00, live 11:00): 2 h; s-1
	// has no roll card
	want := Figure{Value: 425400000, Prev: 7200000, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 7200000, 0, 425400000}}
	if r.LeadTime != want {
		t.Fatalf("lead time = %+v, want %+v", r.LeadTime, want)
	}
}

// The other project's rows answer its own figures and leak into none of
// sf/code's (every sf/code expectation above already encodes that).
func TestMetricsScoping(t *testing.T) {
	r := metricsReport(t, "sf/other")
	if r.Throughput.Value != 1 || r.Spend.Weeks[5] != 99 || r.Deploys.Weeks[5] != 1 ||
		r.Escalations.Value != 1 || !r.CIDuration.Has || r.CIDuration.Value != 500000 {
		t.Fatalf("sf/other = throughput %v spend %+v deploys %+v escalations %v ci %+v",
			r.Throughput.Value, r.Spend, r.Deploys, r.Escalations.Value, r.CIDuration)
	}
}

// A project with nothing at all: counts read 0, cohorts read absent.
func TestMetricsEmptyProject(t *testing.T) {
	r := metricsReport(t, "sf/empty")
	for name, f := range map[string]Figure{
		"lead time": r.LeadTime, "card cycle": r.CardCycle, "per card": r.PerCard,
		"tokens per card": r.TokensPerCard, "change failure": r.ChangeFailure,
		"time to restore": r.TimeToRestore, "interventions": r.Interventions,
		"human blocked": r.HumanBlocked, "review acceptance": r.ReviewAcceptance, "escalated share": r.EscalatedShare,
		"ci duration": r.CIDuration,
	} {
		if f != (Figure{}) {
			t.Fatalf("%s = %+v, want the zero figure", name, f)
		}
	}
	if r.RevertedRefiled.Has || r.DiffSize.Has || len(r.ModelSpend) != 0 || len(r.ServedBy) != 0 || len(r.ToolMix) != 0 || r.EscalationsHandled != (EscalationsHandled{}) {
		t.Fatalf("refiled %+v diff %+v mix %+v", r.RevertedRefiled, r.DiffSize, r.ModelSpend)
	}
	if r.ChatWork != (ChatWork{}) {
		t.Fatalf("chat work = %+v", r.ChatWork)
	}
	// counts are facts even at zero
	if !r.Deploys.Has || r.Deploys.Value != 0 || !r.Throughput.Has || r.Throughput.Value != 0 ||
		!r.Spend.Has || r.Spend.Value != 0 || !r.Escalations.Has || !r.ToolSpend.Has || r.ToolSpend.Value != 0 {
		t.Fatalf("counts: %+v %+v %+v %+v", r.Deploys, r.Throughput, r.Spend, r.Escalations)
	}
}

// Rolls and CI runs without a done card: the card-denominator figures read
// absent, the card-free ones answer.
func TestMetricsNoDoneCards(t *testing.T) {
	from, to := metricsTestWindow()
	s := metricsStore(t)
	seedMetricsFigures(t, s)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source) VALUES ('n1', 1, 0, 'running', 'sf/nodone', 'agent', 'webhook')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO project_rolls (repository, namespace, digest, commit, ready_at) VALUES ('sf/nodone', 'web', 'd-n', 'cn', '2026-10-06T10:00Z'),
			('sf/nodone', 'web', 'd-n2', 'cn2', '2026-10-07T10:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ci_runs (repository, commit, pushed_at, duration_ms) VALUES ('sf/nodone', 'cn', '2026-10-06T09:00Z', 600000), ('sf/nodone', 'cn2', '2026-10-07T09:00Z', 300000)`); err != nil {
		t.Fatal(err)
	}
	r, err := s.ProjectMetrics(ctx, "sf/nodone", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r.Deploys.Value != 2 || r.CIDuration.Value != 450000 || !r.CIDuration.Has {
		t.Fatalf("deploys %+v ci %+v", r.Deploys, r.CIDuration)
	}
	if r.Throughput.Value != 0 || r.PerCard.Has || r.TokensPerCard.Has || r.Interventions.Has ||
		r.ReviewAcceptance.Has || r.HumanBlocked.Has || r.ChangeFailure.Has || r.EscalatedShare.Has {
		t.Fatalf("card figures: %+v %+v %+v %+v", r.Throughput, r.PerCard, r.TokensPerCard, r.ReviewAcceptance)
	}
}

// Everything inside the window, nothing before it: Prev reads absent for
// medians, zero for counts.
func TestMetricsPreviousWindowEmpty(t *testing.T) {
	from, to := metricsTestWindow()
	s := metricsStore(t)
	seedMetricsFigures(t, s)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at, finished_at, files, insertions, deletions, merge_commit) VALUES
			('l1', 1, 0, 'done', 'sf/late', 'agent', 'webhook', '2026-10-06T09:00Z', '2026-10-07T09:00Z', 4, 40, 10, 'cl')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES ('2026-10-06T09:00Z', 'sf/late', 'l1', 'pull_request_merged', 'factoryd', 'factoryd', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO project_rolls (repository, namespace, digest, commit, ready_at) VALUES ('sf/late', 'web', 'd-l', 'cl', '2026-10-06T10:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ci_runs (repository, commit, pushed_at, duration_ms) VALUES ('sf/late', 'cl', '2026-10-06T09:00Z', 120000)`); err != nil {
		t.Fatal(err)
	}
	r, err := s.ProjectMetrics(ctx, "sf/late", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !r.LeadTime.Has || r.LeadTime.Value != 3600000 || r.LeadTime.PrevHas || r.LeadTime.Prev != 0 {
		t.Fatalf("lead = %+v", r.LeadTime)
	}
	if !r.Deploys.Has || r.Deploys.Value != 1 || r.Deploys.Prev != 0 || !r.Deploys.PrevHas {
		t.Fatalf("deploys = %+v", r.Deploys)
	}
	if !r.CardCycle.Has || r.CardCycle.PrevHas || r.CardCycle.Value != 86400000 {
		t.Fatalf("cycle = %+v", r.CardCycle)
	}
}

// from == to is one day; a window longer than the spark still answers.
func TestMetricsWindowEdges(t *testing.T) {
	s := metricsStore(t)
	seedMetricsFigures(t, s)
	ctx := context.Background()
	// one day: only c3 finished on Oct 8, no roll went live that day
	r, err := s.ProjectMetrics(ctx, "sf/code", time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.Throughput.Value != 1 || r.CardCycle.Value != 194400000 || r.LeadTime.Has {
		t.Fatalf("one day: throughput %+v cycle %+v lead %+v", r.Throughput, r.CardCycle, r.LeadTime)
	}
	// 90 days: every fixture card but the one past Oct 12; the per-week
	// rows still answer the last weekly bucket; the report serves the
	// asked inclusive dates
	r, err = s.ProjectMetrics(ctx, "sf/code", time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !r.From.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)) || !r.To.Equal(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window = %v..%v, want the asked inclusive dates", r.From, r.To)
	}
	if r.Deploys.Value != 2 || r.Deploys.Prev != 0 || r.Throughput.Value != 5 || r.Throughput.Prev != 0 {
		t.Fatalf("90 days: deploys %+v throughput %+v", r.Deploys, r.Throughput)
	}
}

// Weeks are seven aligned buckets that end where the window does: a fact
// at SparkStart opens bucket 0, a fact just before To closes bucket 6, a
// fact at To belongs to none. The SparkStart fact is outside the window,
// so it counts in the spark only (all else pinned by TestMetricsFigures).
func TestMetricsWeekBuckets(t *testing.T) {
	s := metricsStore(t)
	seedMetricsFigures(t, s)
	from, to := metricsTestWindow()
	// a fresh project with one fact per edge: bucket 0, bucket 6, past To
	ctx := context.Background()
	for _, roll := range []struct{ digest, commit, ready string }{
		{"e-0", "ce-0", "2026-08-24T00:00Z"}, // exactly SparkFrom
		{"e-6", "ce-6", "2026-10-11T23:59Z"}, // last instant of bucket 6
		{"e-x", "ce-x", "2026-10-12T00:00Z"}, // exactly To: outside
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO project_rolls (repository, namespace, digest, commit, ready_at) VALUES ('sf/buckets', 'web', $1, $2, $3)`,
			roll.digest, roll.commit, roll.ready); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.ProjectMetrics(ctx, "sf/buckets", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r.Deploys.Value != 1 || r.Deploys.Weeks != [7]float64{1, 0, 0, 0, 0, 0, 1} {
		t.Fatalf("deploys = %+v", r.Deploys)
	}
}
