// Package telemetry is factoryd's OpenTelemetry metrics: an OTLP HTTP
// export every 30 s configured from OTEL_EXPORTER_OTLP_ENDPOINT and
// OTEL_SERVICE_NAME, a no-op meter when the endpoint is unset. The export is
// fire and forget: a failed export drops its samples and blocks nothing.
package telemetry

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Buckets are every duration histogram's boundaries, in seconds.
var Buckets = []float64{5, 10, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600}

const exportInterval = 30 * time.Second

// Setup returns the meter provider and its shutdown.
func Setup(ctx context.Context) (metric.MeterProvider, func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return noop.NewMeterProvider(), func(context.Context) error { return nil }, nil
	}
	exporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
	if err != nil {
		return nil, nil, err
	}
	otel.SetErrorHandler(&throttledHandler{log: log.Default(), now: time.Now})
	p := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(exportInterval))))
	return p, p.Shutdown, nil
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

	CIJobDuration   metric.Float64Histogram
	CIRunDuration   metric.Float64Histogram
	CIJobQueue      metric.Float64Histogram
	BuildVMLifetime metric.Float64Histogram
	BuildVMReady    metric.Float64Histogram
	BuildVMCost     metric.Float64Counter
	RollDuration    metric.Float64Histogram
	RollApply       metric.Float64Histogram
	RollReady       metric.Float64Histogram
	GateDuration    metric.Float64Histogram
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
	m.BuildVMCost, err = meter.Float64Counter("factory.ci.buildvm.cost", metric.WithUnit("EUR"),
		metric.WithDescription("started hours times the hourly price of a deleted build VM"))
	if err != nil {
		return nil, err
	}
	return m, nil
}
