package factoryd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// Every activity of task-t1's workflow runs inside a span named after the
// activity and carrying task.id t1; a failed one ends in error.
func TestActivitySpansCarryTheTaskID(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	spans := &ActivitySpans{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("factoryd")}
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{spans}})
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "task-t1"})
	env.RegisterActivityWithOptions(func(ctx context.Context, in SessionInput) error {
		return errors.New("worker pod failed")
	}, activity.RegisterOptions{Name: "RunSession"})
	env.RegisterActivityWithOptions(func(ctx context.Context, taskID string) error { return nil },
		activity.RegisterOptions{Name: "Reconcile"})
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute,
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
		if err := workflow.ExecuteActivity(ctx, "Reconcile", "t1").Get(ctx, nil); err != nil {
			return err
		}
		return workflow.ExecuteActivity(ctx, "RunSession", SessionInput{TaskID: "t1"}).Get(ctx, nil)
	}, workflow.RegisterOptions{Name: "Probe"})
	env.ExecuteWorkflow("Probe")

	ended := rec.Ended()
	if len(ended) != 2 {
		t.Fatalf("%d spans, want 2", len(ended))
	}
	for i, want := range []struct {
		name string
		code codes.Code
	}{{"Reconcile", codes.Unset}, {"RunSession", codes.Error}} {
		s := ended[i]
		if s.Name() != want.name || s.Status().Code != want.code {
			t.Fatalf("span %d = %s %v, want %s %v", i, s.Name(), s.Status(), want.name, want.code)
		}
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		if attrs["task.id"] != "t1" || attrs["temporal.workflow_id"] != "task-t1" {
			t.Fatalf("span %s attributes %v", s.Name(), attrs)
		}
	}
	if ended[1].Status().Description != "RunSession failed" {
		t.Fatalf("RunSession status %v", ended[1].Status())
	}
}

// An activity outside a task workflow gets its span without a task id.
func TestActivitySpansWithoutATaskWorkflow(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	spans := &ActivitySpans{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("factoryd")}
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{spans}})
	env.RegisterActivityWithOptions(func(ctx context.Context) error { return nil }, activity.RegisterOptions{Name: "Probe"})
	if _, err := env.ExecuteActivity("Probe"); err != nil {
		t.Fatal(err)
	}
	ended := rec.Ended()
	if len(ended) != 1 || ended[0].Name() != "Probe" {
		t.Fatalf("spans %v", ended)
	}
	for _, kv := range ended[0].Attributes() {
		if kv.Key == "task.id" {
			t.Fatalf("task.id %q on a non-task activity", kv.Value.Emit())
		}
	}
}
