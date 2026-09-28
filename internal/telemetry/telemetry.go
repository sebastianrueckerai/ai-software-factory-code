// Package telemetry is the binaries' OpenTelemetry: metrics, traces and
// logs over OTLP HTTP to OTEL_EXPORTER_OTLP_ENDPOINT, each stamped with the
// binary's service.name, and no-op providers when the endpoint is unset.
// Every export is fire and forget: a failed export drops its data and
// blocks nothing.
package telemetry

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Buckets are every duration histogram's boundaries, in seconds.
var Buckets = []float64{5, 10, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600}

const exportInterval = 30 * time.Second

// TaskID is the attribute key that joins a task's spans and log records
// across the binaries.
const TaskID = attribute.Key("task.id")

// Providers are a binary's three signal providers and their joint shutdown.
type Providers struct {
	Meter    metric.MeterProvider
	Tracer   trace.TracerProvider
	Logger   otellog.LoggerProvider
	Shutdown func(context.Context) error
}

// Setup returns the providers for the binary named service.
func Setup(ctx context.Context, service string) (Providers, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return Providers{Meter: noop.NewMeterProvider(), Tracer: tracenoop.NewTracerProvider(), Logger: lognoop.NewLoggerProvider(),
			Shutdown: func(context.Context) error { return nil }}, nil
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", service)))
	if err != nil {
		return Providers{}, err
	}
	metrics, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
	if err != nil {
		return Providers{}, err
	}
	traces, err := otlptracehttp.New(ctx, otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	if err != nil {
		return Providers{}, err
	}
	logs, err := otlploghttp.New(ctx, otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}))
	if err != nil {
		return Providers{}, err
	}
	otel.SetErrorHandler(&throttledHandler{log: log.Default(), now: time.Now})
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics, sdkmetric.WithInterval(exportInterval))))
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(traces))
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(logs)))
	return Providers{Meter: mp, Tracer: tp, Logger: lp, Shutdown: func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), lp.Shutdown(ctx), mp.Shutdown(ctx))
	}}, nil
}

// throttledHandler logs an export error at most once a minute.
type throttledHandler struct {
	log        *log.Logger
	now        func() time.Time
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func (h *throttledHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if !h.last.IsZero() && now.Sub(h.last) < time.Minute {
		h.suppressed++
		return
	}
	h.log.Printf("telemetry: %v (%d more suppressed)", err, h.suppressed)
	h.last, h.suppressed = now, 0
}

// Metrics are factoryd's synchronous instruments; Meter registers the
// observable ones next to the state they read.
type Metrics struct {
	Meter metric.Meter

	CIJobDuration metric.Float64Histogram
	CIRunDuration metric.Float64Histogram
	CIJobQueue    metric.Float64Histogram
	// CIJobQueueUnmeasured counts jobs first seen already taken: the
	// runner-jobs list holds no creation time, so their queue is unknown.
	CIJobQueueUnmeasured metric.Int64Counter
	BuildVMLifetime      metric.Float64Histogram
	BuildVMReady         metric.Float64Histogram
	BuildVMCost          metric.Float64Counter
	RollDuration         metric.Float64Histogram
	RollApply            metric.Float64Histogram
	RollReady            metric.Float64Histogram
	GateDuration         metric.Float64Histogram
}

// Noop is Metrics on the no-op meter, for tests and callers without one.
func Noop() *Metrics {
	m, _ := New(noop.NewMeterProvider().Meter("")) // #nosec G104 -- the no-op meter never fails
	return m
}

func New(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{Meter: meter}
	var err error
	hist := func(name, desc string) metric.Float64Histogram {
		if err != nil {
			return nil
		}
		var h metric.Float64Histogram
		h, err = meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(Buckets...))
		return h
	}
	m.CIJobDuration = hist("factory.ci.job.duration", "run time of one finished CI job")
	m.CIRunDuration = hist("factory.ci.run.duration", "first job created to last job finished of one CI run")
	m.CIJobQueue = hist("factory.ci.job.queue", "first seen waiting to picked up, 10 s resolution")
	m.BuildVMLifetime = hist("factory.ci.buildvm.lifetime", "build VM created to deleted")
	m.BuildVMReady = hist("factory.ci.buildvm.ready", "build VM created to buildkitd's first answer")
	m.RollDuration = hist("factory.roll.duration", "roll push to both Deployments live")
	m.RollApply = hist("factory.roll.apply", "roll push to Flux applying it")
	m.RollReady = hist("factory.roll.ready", "Flux applying a roll to both Deployments live")
	m.GateDuration = hist("factory.task.gate.duration", "one task gate from start to end")
	if err != nil {
		return nil, err
	}
	m.CIJobQueueUnmeasured, err = meter.Int64Counter("factory.ci.job.queue.unmeasured",
		metric.WithDescription("CI jobs a runner took before a poll saw them waiting"))
	if err != nil {
		return nil, err
	}
	m.BuildVMCost, err = meter.Float64Counter("factory.ci.buildvm.cost", metric.WithUnit("EUR"),
		metric.WithDescription("started hours times the hourly price of a deleted build VM"))
	if err != nil {
		return nil, err
	}
	return m, nil
}
