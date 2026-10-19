package factoryd

import (
	"context"
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

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
	// c4 (chat, no card), o1 (sf/other)
	must(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at, finished_at, files, insertions, deletions, merge_commit) VALUES
		('c1', 1, 0, 'done', 'sf/code', 'agent', 'webhook', $1, $2, 3, 120, 40, 'm-c1'),
		('c2', 1, 0, 'done', 'sf/code', 'agent', 'webhook', $3, $4, NULL, NULL, NULL, ''),
		('c3', 1, 0, 'done', 'sf/code', 'agent', 'webhook', $5, $6, 9, 240, 95, 'm-c3'),
		('c6', 1, 0, 'done', 'sf/code', 'agent', 'alert',   $7, $8, NULL, NULL, NULL, ''),
		('c5', 1, 0, 'done', 'sf/code', 'agent', 'webhook', '2026-09-18T08:00Z', '2026-09-20T10:00Z', 5, 80, 20, ''),
		('c4', 1, 0, 'done', 'sf/code', 'chat',  'webhook', '2026-10-06T09:00Z', '2026-10-07T12:00Z', NULL, NULL, NULL, ''),
		('o1', 1, 0, 'done', 'sf/other', 'agent', 'webhook', '2026-10-01T07:00Z', '2026-10-02T17:00Z', 2, 10, 5, '')`,
		"2026-09-29T06:00Z", "2026-10-02T10:00Z", // c1: 76 h lifetime
		"2026-10-01T08:00Z", "2026-10-03T09:00Z", // c2: 49 h
		"2026-10-06T10:00Z", "2026-10-08T16:00Z", // c3: 54 h
		"2026-10-06T09:00Z", "2026-10-06T21:00Z") // c6: 12 h
	must(`UPDATE tasks SET reverted_at = '2026-10-04T12:00Z' WHERE id = 'c2'`)
	must(`UPDATE tasks SET reverted_at = '2026-09-22T09:00Z' WHERE id = 'c5'`)
	must(`UPDATE tasks SET alert_fingerprint = 'fp-x' WHERE id = 'c6'`)
	must(`UPDATE tasks SET alert_fingerprint = 'fp-p' WHERE id = 'c5'`)
	status := func(task, at, status string) {
		t.Helper()
		must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES ($1, 'sf/code', $2, 'task_status', 'factoryd', 'factoryd', $3)`,
			at, task, `{"status":"`+status+`"}`)
	}
	status("c1", "2026-09-29T07:00Z", "running")
	status("c1", "2026-10-01T09:00Z", "in-review")
	status("c1", "2026-10-01T10:00Z", "waiting-human") // blocked 5 h to closing
	status("c1", "2026-10-01T15:00Z", "closing")
	status("c1", "2026-10-02T10:00Z", "done")
	status("c2", "2026-10-01T08:30Z", "running")
	status("c2", "2026-10-02T09:00Z", "waiting-poster") // blocked 24 h to done
	status("c2", "2026-10-03T09:00Z", "done")
	status("c3", "2026-10-06T10:30Z", "running")
	status("c3", "2026-10-07T11:00Z", "in-review") // first review: changes (next is running)
	status("c3", "2026-10-07T14:00Z", "running")
	status("c3", "2026-10-08T09:00Z", "in-review")
	status("c3", "2026-10-08T10:00Z", "closing")
	status("c3", "2026-10-08T16:00Z", "done")
	status("c6", "2026-10-06T09:30Z", "running")
	status("c6", "2026-10-06T21:00Z", "done")
	status("c4", "2026-10-06T09:30Z", "running")
	status("c4", "2026-10-07T12:00Z", "done")
	status("c5", "2026-09-18T09:00Z", "running")
	status("c5", "2026-09-19T10:00Z", "in-review") // accepted (next is closing)
	status("c5", "2026-09-19T11:00Z", "closing")
	status("c5", "2026-09-19T12:00Z", "waiting-human") // blocked 22 h to done
	status("c5", "2026-09-20T10:00Z", "done")
	// human interventions: three in window, none in prev, one in sf/other
	must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES
		('2026-10-01T11:00Z', 'sf/code', 'c1', 'comment', 'sebastian', 'button', '{}'),
		('2026-10-02T10:00Z', 'sf/code', 'c2', 'comment', 'sebastian', 'webhook', '{}'),
		('2026-10-07T12:00Z', 'sf/code', 'c3', 'comment', 'sebastian', 'console', '{}'),
		('2026-10-02T18:00Z', 'sf/other', 'o1', 'comment', 'sebastian', 'button', '{}')`)
	must(`INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
		('c1', '2026-10-02', 'model', 'x', 2.00),
		('c2', '2026-10-03', 'model', 'x', 3.00),
		('c2', '2026-10-03', 'model', 'x', 0.60),
		('c3', '2026-10-08', 'model', 'x', 2.20),
		('c4', '2026-10-07', 'model', 'x', 1.00),
		('c5', '2026-09-20', 'model', 'x', 1.50),
		('o1', '2026-10-02', 'model', 'x', 99.00)`)
	must(`INSERT INTO audit (ts, task_id, decision, reason, model_alias, prompt_tokens, output_tokens) VALUES
		('2026-10-02T10:05Z', 'c1', 'call', '', 'coding-frontier', 1000, 500),
		('2026-10-03T09:05Z', 'c2', 'call', '', 'coding-frontier', 800, 400),
		('2026-10-03T09:10Z', 'c2', 'call', '', 'review-frontier', 200, 100),
		('2026-10-08T16:05Z', 'c3', 'call', '', 'coding-frontier', 1200, 600),
		('2026-10-08T16:10Z', 'c3', 'call', '', 'glm-flash', 200, 100),
		('2026-10-07T12:05Z', 'c4', 'call', '', 'pulse', 300, 100),
		('2026-09-20T10:05Z', 'c5', 'call', '', 'coding-frontier', 500, 250),
		('2026-10-02T17:05Z', 'o1', 'call', '', 'coding-frontier', 9999, 9999)`)
	must(`INSERT INTO alerts (kind, source, task_id, raised_at, fingerprint) VALUES
		('alert', 'signoz', 'c6', '2026-10-06T08:00Z', 'fp-x'),   -- fixed 13 h later by c6
		('alert', 'signoz', 'c2', '2026-10-04T12:00Z', NULL),     -- c2's refile, no fix task
		('alert', 'signoz', 'c5', '2026-09-15T08:00Z', 'fp-p'),   -- prev: fixed 122 h later by c5
		('escalation', 'gate', 'c3', '2026-10-07T13:00Z', NULL),
		('escalation', 'gate', 'c5', '2026-09-21T10:00Z', NULL),
		('escalation', 'gate', 'o1', '2026-10-02T18:30Z', NULL)`)
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
		('sf/code', 'cx',   '2026-10-02T10:00Z', 240000), -- pushed, never rolled
		('sf/code', 'cr-5', '2026-08-23T23:40Z', 90000),
		('sf/other', 'ox',  '2026-10-02T14:00Z', 500000)`)
	must(`INSERT INTO rolls (sha, image, deployment, pushed_at, applied_at, live_at) VALUES
		('s-1', 'factoryd', 'factoryd', '2026-10-04T08:00Z', '2026-10-04T08:30Z', '2026-10-04T09:00Z'), -- window, b5
		('s-2', 'factoryd', 'factoryd', '2026-10-07T07:00Z', '2026-10-07T07:30Z', '2026-10-07T08:00Z'), -- window, b6
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
	r, err := s.Metrics(context.Background(), repository, from, to)
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
	// deploys: r2+r4 in window, r0+r1 in prev; buckets by ready_at
	fig(t, "deploys", r.Deploys, Figure{Value: 2, Prev: 2, Has: true, PrevHas: true,
		Weeks: [7]float64{1, 0, 0, 1, 1, 0, 2}})
	// lead: r2 59 min, r4 60 min -> median 59.5 min; prev r0 30, r1 45 -> 37.5
	fig(t, "lead time", r.LeadTime, Figure{Value: 3570000, Prev: 2250000, Has: true, PrevHas: true,
		Weeks: [7]float64{1200000, 0, 0, 2700000, 1800000, 0, 3570000}})
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
	// reverted/refiled: c2 refiled (alert at its reverted_at), c5 a bare revert
	if r.RevertedRefiled != (RevertedRefiled{Reverted: 0, Refiled: 1, Prev: [2]float64{1, 0}, Has: true,
		Weeks: [7]float64{0, 0, 0, 0, 1, 1, 0}}) {
		t.Fatalf("reverted/refiled = %+v", r.RevertedRefiled)
	}
	// restore: a1 fixed by c6 13 h later; a2 has no fingerprinted fix; prev 122 h
	fig(t, "time to restore", r.TimeToRestore, Figure{Value: 46800000, Prev: 439200000, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 439200000, 0, 0, 46800000}})
	// spend: 2 + 3.6 + 2.2 + 1 (chat) = 8.8; prev 1.5
	fig(t, "spend", r.Spend, Figure{Value: 8.80, Prev: 1.50, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1.50, 0, 5.60, 3.20}})
	// per card: totals 2, 3.6, 2.2, 0 -> median (2+2.2)/2 = 2.1
	fig(t, "per card", r.PerCard, Figure{Value: 2.10, Prev: 1.50, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1.50, 0, 2.80, 1.10}})
	// tokens/card: c1 1500 + c2 1500 + c3 2100 = 5100 over 4 cards; prev c5 750 over 1
	fig(t, "tokens per card", r.TokensPerCard, Figure{Value: 1275, Prev: 750, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 750, 0, 1500, 1050}})
	// mix: coding 4500, pulse 400 (chat c4 counts), glm 300, review 300 -> other
	wantMix := []MixShare{
		{"coding-frontier", 4500.0 / 55.0}, // = 81.8182 % of 5500
		{"pulse", 400.0 / 55.0},
		{"glm-flash", 300.0 / 55.0},
		{"other", 300.0 / 55.0},
	}
	if len(r.ModelMix) != len(wantMix) {
		t.Fatalf("model mix = %+v", r.ModelMix)
	}
	for i, w := range wantMix {
		if r.ModelMix[i].Alias != w.Alias || math.Abs(r.ModelMix[i].Share-w.Share) > 1e-9 {
			t.Fatalf("model mix[%d] = %+v, want %+v", i, r.ModelMix[i], w)
		}
	}
	// interventions: 3 human events over 4 cards; prev none over 1
	fig(t, "interventions", r.Interventions, Figure{Value: 0.75, Prev: 0, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0.5}})
	// escalations: es1 window, es2 prev (es3 is sf/other's)
	fig(t, "escalations", r.Escalations, Figure{Value: 1, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 1, 0, 1}})
	// human blocked: (5 h + 24 h) / (76 + 49 + 54 + 12) h; prev 22/50 h
	fig(t, "human blocked", r.HumanBlocked, Figure{Value: 104400.0 / 687600, Prev: 0.44, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0.44, 0, 0.232, 0}})
	// review acceptance: c1 accepted, c3 changes, c2 c6 never reviewed -> 1/2
	fig(t, "review acceptance", r.ReviewAcceptance, Figure{Value: 0.5, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1, 0, 1, 0}})
	// self deploys: s1+s2 window, s3 prev (s4 outside)
	fig(t, "self deploys", r.SelfDeploys, Figure{Value: 2, Prev: 1, Has: true, PrevHas: true,
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
	// ci duration: cx 240, cr-4 300, cr-2 330 -> median 300; prev 200, 210 -> 205
	fig(t, "ci duration", r.CIDuration, Figure{Value: 300000, Prev: 205000, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 210000, 200000, 240000, 315000}})
}

// The other project's rows answer its own figures and leak into none of
// sf/code's (every sf/code expectation above already encodes that).
func TestMetricsScoping(t *testing.T) {
	r := metricsReport(t, "sf/other")
	if r.Throughput.Value != 1 || r.Spend.Value != 99 || r.Deploys.Value != 1 ||
		r.Escalations.Value != 1 || !r.CIDuration.Has || r.CIDuration.Value != 500000 {
		t.Fatalf("sf/other = throughput %v spend %v deploys %v escalations %v ci %+v",
			r.Throughput.Value, r.Spend.Value, r.Deploys.Value, r.Escalations.Value, r.CIDuration)
	}
}

// A project with nothing at all: counts read 0, cohorts read absent.
func TestMetricsEmptyProject(t *testing.T) {
	r := metricsReport(t, "sf/empty")
	for name, f := range map[string]Figure{
		"lead time": r.LeadTime, "card cycle": r.CardCycle, "per card": r.PerCard,
		"tokens per card": r.TokensPerCard, "change failure": r.ChangeFailure,
		"time to restore": r.TimeToRestore, "interventions": r.Interventions,
		"human blocked": r.HumanBlocked, "review acceptance": r.ReviewAcceptance,
		"ci duration": r.CIDuration,
	} {
		if f != (Figure{}) {
			t.Fatalf("%s = %+v, want the zero figure", name, f)
		}
	}
	if r.RevertedRefiled.Has || r.DiffSize.Has || len(r.ModelMix) != 0 {
		t.Fatalf("refiled %+v diff %+v mix %+v", r.RevertedRefiled, r.DiffSize, r.ModelMix)
	}
	if r.ChatWork != (ChatWork{}) {
		t.Fatalf("chat work = %+v", r.ChatWork)
	}
	// counts are facts even at zero
	if !r.Deploys.Has || r.Deploys.Value != 0 || !r.Throughput.Has || r.Throughput.Value != 0 ||
		!r.Spend.Has || r.Spend.Value != 0 || !r.Escalations.Has {
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
	r, err := s.Metrics(ctx, "sf/nodone", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r.Deploys.Value != 2 || r.CIDuration.Value != 450000 || !r.CIDuration.Has {
		t.Fatalf("deploys %+v ci %+v", r.Deploys, r.CIDuration)
	}
	if r.Throughput.Value != 0 || r.PerCard.Has || r.TokensPerCard.Has || r.Interventions.Has ||
		r.ReviewAcceptance.Has || r.HumanBlocked.Has || r.ChangeFailure.Has {
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
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at, finished_at, files, insertions, deletions) VALUES
			('l1', 1, 0, 'done', 'sf/late', 'agent', 'webhook', '2026-10-06T09:00Z', '2026-10-07T09:00Z', 4, 40, 10)`); err != nil {
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
	r, err := s.Metrics(ctx, "sf/late", from, to)
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
	r, err := s.Metrics(ctx, "sf/code", time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.Throughput.Value != 1 || r.CardCycle.Value != 194400000 || r.LeadTime.Has {
		t.Fatalf("one day: throughput %+v cycle %+v lead %+v", r.Throughput, r.CardCycle, r.LeadTime)
	}
	// 90 days: every fixture roll but the one past Oct 12; prev window empty
	r, err = s.Metrics(ctx, "sf/code", time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.Deploys.Value != 5 || r.Deploys.Prev != 0 || r.Throughput.Value != 5 || r.Throughput.Prev != 0 {
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
	r, err := s.Metrics(ctx, "sf/buckets", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r.Deploys.Value != 1 || r.Deploys.Weeks != [7]float64{1, 0, 0, 0, 0, 0, 1} {
		t.Fatalf("deploys = %+v", r.Deploys)
	}
}
