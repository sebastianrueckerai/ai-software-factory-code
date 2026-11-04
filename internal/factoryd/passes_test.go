package factoryd

import (
	"context"
	"log"
	"os"
	"regexp"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// lateOf reads factory.pass.late per pass. It takes no t, so a pass's own
// run may read it.
func lateOf(reader *sdkmetric.ManualReader) (map[string]float64, error) {
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "factory.pass.late" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
				v, _ := dp.Attributes.Value("pass")
				out[v.AsString()] = dp.Value
			}
		}
	}
	return out, nil
}

func latePasses(t *testing.T) (*Passes, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	p := &Passes{}
	if err := p.RegisterLate(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")); err != nil {
		t.Fatal(err)
	}
	return p, reader
}

// nextRun is the next value of ch, or the test's end after 5 s: a pass
// that never runs fails rather than hangs.
func nextRun[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("no run within 5s")
	}
	var zero T
	return zero
}

// passSeen is the gauge as a run saw it when it started.
type passSeen struct {
	late map[string]float64
	err  error
}

// A run that blocks past its bound is cancelled by its context at the
// bound, logged with its name and the time over, and sets
// factory.pass.late to 1. The next run ends at once, within the bound,
// and sets it back to 0. Each run reads the gauge as it starts: the run
// before it has ended by then.
func TestPassLoopBoundsARunAndFlagsIt(t *testing.T) {
	p, reader := latePasses(t)
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const bound = 100 * time.Millisecond
	cut := make(chan error, 1)
	seen := make(chan passSeen, 2)
	var runs atomic.Int32
	start := time.Now()
	p.Run(ctx, "heal", time.Millisecond, bound, func(ctx context.Context) error {
		switch runs.Add(1) {
		case 1:
			<-ctx.Done()
			cut <- ctx.Err()
			return ctx.Err()
		case 2, 3:
			late, err := lateOf(reader)
			seen <- passSeen{late: late, err: err}
		}
		return nil
	})
	if err := nextRun(t, cut); err != context.DeadlineExceeded {
		t.Fatalf("the first run's context ended with %v, want its deadline", err)
	}
	if took := time.Since(start); took < bound {
		t.Fatalf("the first run was cut after %s, before its bound of %s", took, bound)
	}
	second := nextRun(t, seen)
	if second.err != nil {
		t.Fatal(second.err)
	}
	if second.late["heal"] != 1 {
		t.Fatalf("factory.pass.late after the cut run = %v, want heal 1", second.late)
	}
	if !regexp.MustCompile(`pass heal: ended \S+ over its bound of 100ms`).MatchString(logs.String()) {
		t.Fatalf("logs = %q, want the cut run named with its time over", logs.String())
	}
	third := nextRun(t, seen)
	if third.err != nil {
		t.Fatal(third.err)
	}
	if third.late["heal"] != 0 {
		t.Fatalf("factory.pass.late after a quick run = %v, want heal 0", third.late)
	}
}

// Ages names every pass from its start. heal hangs on a call that ignores
// its context: it reads late at its bound while it hangs, ages from its
// start, and stale keeps its cadence beside it, ageing from its newest
// end. heal is never let go, so no line of its lands in a later test.
func TestPassesAgesNameEveryPass(t *testing.T) {
	p, reader := latePasses(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hung := make(chan struct{})
	before := time.Now()
	p.Run(ctx, "heal", time.Hour, 20*time.Millisecond, func(context.Context) error {
		<-hung
		return nil
	})
	healStarted := time.Now()
	var staleRuns atomic.Int32
	p.Run(ctx, "stale", time.Millisecond, time.Minute, func(context.Context) error {
		staleRuns.Add(1)
		return nil
	})
	var late map[string]float64
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		var err error
		if late, err = lateOf(reader); err != nil {
			t.Fatal(err)
		}
		if late["heal"] == 1 || time.Now().After(deadline) {
			break
		}
	}
	if late["heal"] != 1 || late["stale"] != 0 {
		t.Fatalf("factory.pass.late while heal hangs = %v, want heal 1 and stale 0", late)
	}
	if n := staleRuns.Load(); n < 2 {
		t.Fatalf("stale ran %d times while heal hung, want its cadence kept", n)
	}
	now := time.Now().Add(time.Minute)
	ages := p.Ages(now)
	names := make([]string, 0, len(ages))
	for name := range ages {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"heal", "stale"}; !slices.Equal(names, want) {
		t.Fatalf("Ages names %q, want %q", names, want)
	}
	if heal := ages["heal"]; heal < now.Sub(healStarted) || heal > now.Sub(before) {
		t.Fatalf("heal's age = %s, want its age from its start, within [%s, %s]", heal, now.Sub(healStarted), now.Sub(before))
	}
	if stale := ages["stale"]; stale <= 0 || stale >= ages["heal"] {
		t.Fatalf("stale's age = %s, want younger than heal's %s: it ended since heal started", stale, ages["heal"])
	}
}
