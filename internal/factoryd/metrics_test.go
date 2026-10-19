package factoryd

import (
	"context"
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
