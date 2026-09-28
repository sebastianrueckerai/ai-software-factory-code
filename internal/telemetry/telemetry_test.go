package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNoEndpointGivesANoopMeter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	provider, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(noop.MeterProvider); !ok {
		t.Fatalf("provider = %T, want noop.MeterProvider", provider)
	}
	m, err := New(provider.Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	m.CIJobDuration.Record(context.Background(), 12)
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointGivesTheSDKMeter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_SERVICE_NAME", "factoryd")
	provider, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("provider = %T, want the SDK provider", provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdown(ctx) // #nosec G104 -- nothing listens; only that it returns
}

func TestHistogramsUseTheFixedBoundaries(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	m.RollDuration.Record(context.Background(), 250)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	h := rm.ScopeMetrics[0].Metrics[0]
	if h.Name != "factory.roll.duration" || h.Unit != "s" {
		t.Fatalf("metric %s unit %s", h.Name, h.Unit)
	}
	dp := h.Data.(metricdata.Histogram[float64]).DataPoints[0]
	want := []float64{5, 10, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600}
	if len(dp.Bounds) != len(want) {
		t.Fatalf("bounds %v", dp.Bounds)
	}
	for i := range want {
		if dp.Bounds[i] != want[i] {
			t.Fatalf("bounds %v, want %v", dp.Bounds, want)
		}
	}
	// 250 falls in (120, 300], the sixth bucket
	if dp.BucketCounts[5] != 1 {
		t.Fatalf("bucket counts %v", dp.BucketCounts)
	}
}

func TestExportErrorsLogAtMostOncePerMinute(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	h := &throttledHandler{log: log.New(&buf, "", 0), now: func() time.Time { return now }}
	h.Handle(errors.New("export: connection refused"))
	now = now.Add(20 * time.Second)
	h.Handle(errors.New("export: connection refused"))
	now = now.Add(20 * time.Second)
	h.Handle(errors.New("export: connection refused"))
	now = now.Add(21 * time.Second)
	h.Handle(errors.New("export: timeout"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d lines: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[1], "export: timeout") || !strings.Contains(lines[1], "2 more suppressed") {
		t.Fatalf("second line %q", lines[1])
	}
}
