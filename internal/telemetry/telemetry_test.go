package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	logcol "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccol "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecol "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func TestNoEndpointGivesNoopProviders(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	p, err := Setup(context.Background(), "factoryd")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Meter.(noop.MeterProvider); !ok {
		t.Fatalf("meter provider = %T, want the no-op one", p.Meter)
	}
	if _, ok := p.Tracer.(tracenoop.TracerProvider); !ok {
		t.Fatalf("tracer provider = %T, want the no-op one", p.Tracer)
	}
	if _, ok := p.Logger.(lognoop.LoggerProvider); !ok {
		t.Fatalf("logger provider = %T, want the no-op one", p.Logger)
	}
	m, err := New(p.Meter.Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	m.CIJobDuration.Record(context.Background(), 12)
	_, span := p.Tracer.Tracer("factoryd").Start(context.Background(), "x")
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// One endpoint carries all three signals, each stamped with the binary's
// service.name; the environment's OTEL_SERVICE_NAME does not override it.
func TestEndpointExportsEverySignalWithTheServiceName(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var res *resourcepb.Resource
		switch r.URL.Path {
		case "/v1/traces":
			var req tracecol.ExportTraceServiceRequest
			if proto.Unmarshal(body, &req) == nil && len(req.ResourceSpans) > 0 {
				res = req.ResourceSpans[0].Resource
			}
		case "/v1/logs":
			var req logcol.ExportLogsServiceRequest
			if proto.Unmarshal(body, &req) == nil && len(req.ResourceLogs) > 0 {
				res = req.ResourceLogs[0].Resource
			}
		case "/v1/metrics":
			var req metriccol.ExportMetricsServiceRequest
			if proto.Unmarshal(body, &req) == nil && len(req.ResourceMetrics) > 0 {
				res = req.ResourceMetrics[0].Resource
			}
		}
		for _, kv := range res.GetAttributes() {
			if kv.Key == "service.name" || kv.Key == "k8s.namespace.name" {
				mu.Lock()
				got[r.URL.Path+" "+kv.Key] = kv.Value.GetStringValue()
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_SERVICE_NAME", "someone-else")
	// the Deployments' env: the namespace SigNoz's alert rules are bound to
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.namespace.name=factory")
	p, err := Setup(context.Background(), "factory-gateway")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, span := p.Tracer.Tracer("t").Start(ctx, "call")
	span.End()
	var rec otellog.Record
	rec.SetBody(otellog.StringValue("audit"))
	p.Logger.Logger("t").Emit(ctx, rec)
	c, err := p.Meter.Meter("t").Int64Counter("c")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(ctx, 1)
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]string{}
	for _, path := range []string{"/v1/traces", "/v1/logs", "/v1/metrics"} {
		want[path+" service.name"], want[path+" k8s.namespace.name"] = "factory-gateway", "factory"
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("service.name per path = %v, want %v", got, want)
	}
}

// Nothing listens: the shutdown still returns, the export error is only
// logged.
func TestAnUnreachableEndpointBlocksNothing(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	p, err := Setup(context.Background(), "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer.Tracer("t").Start(context.Background(), "x")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.Shutdown(ctx) // #nosec G104 -- nothing listens; only that it returns
	if ctx.Err() != nil {
		t.Fatal("shutdown ran into its deadline")
	}
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

// Every request but the probes is a server span named by its route
// pattern (no task id, no scanner's path) with its status code: what a service's 5xx-rate alert rule counts.
func TestHandlerTracesEveryRequestButTheProbes(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "store down", http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) {})
	h := Handler(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)), "factoryd", mux)
	for _, path := range []string{"/tasks/t-1", "/healthz", "/readyz", "/a/scanner/path"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	var got []string
	for _, s := range spans.Ended() {
		code := ""
		for _, a := range s.Attributes() {
			if a.Key == attribute.Key("http.response.status_code") {
				code = a.Value.Emit()
			}
		}
		got = append(got, s.Name()+" "+s.SpanKind().String()+" "+code)
	}
	if want := []string{"GET /tasks/{id} " + trace.SpanKindServer.String() + " 500", "GET / server 200"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans %q, want %q", got, want)
	}
}
