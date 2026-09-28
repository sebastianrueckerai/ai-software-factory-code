package factoryd

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
)

func TestOpenActivitiesReportsTheOldestPerActivity(t *testing.T) {
	m, reader := testMetrics(t)
	o, err := NewOpenActivities(m.Meter)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	now := t0
	o.now = func() time.Time { return now }
	first := o.start("RunSession")
	now = t0.Add(60 * time.Second)
	second := o.start("RunSession")
	now = t0.Add(90 * time.Second)
	checks := o.start("WaitChecks")
	now = t0.Add(120 * time.Second)
	if got := sums(t, reader, "factory.activity.open_age"); !reflect.DeepEqual(got, map[string]float64{"activity=RunSession": 120, "activity=WaitChecks": 30}) {
		t.Fatalf("open ages %v", got)
	}
	o.finish(first)
	if got := sums(t, reader, "factory.activity.open_age"); !reflect.DeepEqual(got, map[string]float64{"activity=RunSession": 60, "activity=WaitChecks": 30}) {
		t.Fatalf("open ages after the oldest ended %v", got)
	}
	o.finish(second)
	o.finish(checks)
	if got := sums(t, reader, "factory.activity.open_age"); len(got) != 0 {
		t.Fatalf("open ages with nothing running %v", got)
	}
}

func TestOpenActivitiesTracksARunningActivity(t *testing.T) {
	m, reader := testMetrics(t)
	o, err := NewOpenActivities(m.Meter)
	if err != nil {
		t.Fatal(err)
	}
	var during map[string]float64
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{o}})
	env.RegisterActivityWithOptions(func(ctx context.Context) error {
		during = sums(t, reader, "factory.activity.open_age")
		return nil
	}, activity.RegisterOptions{Name: "Probe"})
	if _, err := env.ExecuteActivity("Probe"); err != nil {
		t.Fatal(err)
	}
	if _, ok := during["activity=Probe"]; !ok || len(during) != 1 {
		t.Fatalf("open ages while running %v", during)
	}
	if got := sums(t, reader, "factory.activity.open_age"); len(got) != 0 {
		t.Fatalf("open ages after it ended %v", got)
	}
}

func TestAlertCountReadsTheTableAtExport(t *testing.T) {
	m, reader := testMetrics(t)
	rows := []AlertCount{{"gate_bound", "open", 2}, {"supervisor_trip", "noise", 1}}
	if err := RegisterAlertCount(m.Meter, func(context.Context) ([]AlertCount, error) { return rows, nil }); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"kind=gate_bound,verdict=open": 2, "kind=supervisor_trip,verdict=noise": 1}
	if got := sums(t, reader, "factory.alert.count"); !reflect.DeepEqual(got, want) {
		t.Fatalf("alert counts %v, want %v", got, want)
	}
}

func TestAlertCountWithoutTheTableLogsOnceAndSkips(t *testing.T) {
	logs := captureLog(t)
	m, reader := testMetrics(t)
	if err := RegisterAlertCount(m.Meter, func(context.Context) ([]AlertCount, error) { return nil, ErrNoAlertsTable }); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := sums(t, reader, "factory.alert.count"); len(got) != 0 {
			t.Fatalf("alert counts %v without a table", got)
		}
	}
	if n := strings.Count(logs.String(), "no alerts table"); n != 1 {
		t.Fatalf("logged %d times: %q", n, logs.String())
	}
}
