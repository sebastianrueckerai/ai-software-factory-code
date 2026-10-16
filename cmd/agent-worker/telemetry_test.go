package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// recordedStream is the adapter's event stream of a session as
// testdata/opencode-run-*.jsonl yield it: a command, an edit, a failed
// write, an MCP call, two model steps and the worker's checkpoint.
func recordedStream(t0 time.Time) []harness.Event {
	return []harness.Event{
		{Time: t0.Add(1 * time.Second), Kind: harness.EventCheckpoint},
		{Time: t0.Add(2 * time.Second), Kind: harness.EventCommand, Detail: map[string]any{
			"command": "echo recorded-fixture > hello.txt", "status": "completed", "output": "(no output)",
			"exit": 0, "duration_ms": int64(37), "bytes": 11}},
		{Time: t0.Add(3 * time.Second), Kind: harness.EventModelCall, Detail: map[string]any{
			"reason": "tool-calls", "cost": 0.00021,
			"tokens": map[string]any{"total": float64(165), "input": float64(120), "output": float64(45), "reasoning": float64(0)}}},
		{Time: t0.Add(4 * time.Second), Kind: harness.EventFileChange, Detail: map[string]any{
			"path": "hello.txt", "tool": "edit", "sha256": "885a", "duration_ms": int64(12), "bytes": 15}},
		{Time: t0.Add(5 * time.Second), Kind: harness.EventToolCall, Detail: map[string]any{
			"tool": "write", "status": "error", "error": "The write tool was called with invalid arguments",
			"args": map[string]any{"path": "notes.md", "content": "SECRET-ARG"}, "args_sha256": "ab", "duration_ms": int64(2)}},
		{Time: t0.Add(6 * time.Second), Kind: harness.EventCommand, Detail: map[string]any{
			"command": "go test ./...", "status": "completed", "output": "FAIL", "exit": 1, "duration_ms": int64(1500), "bytes": 4}},
		{Time: t0.Add(7 * time.Second), Kind: harness.EventToolCall, Detail: map[string]any{
			"tool": "factory-tools_echo", "status": "completed", "args": map[string]any{"text": "hello-from-model"},
			"args_sha256": "cd", "duration_ms": int64(4)}},
		{Time: t0.Add(8 * time.Second), Kind: harness.EventModelCall, Detail: map[string]any{
			"reason": "stop", "cost": 0.0001, "text": "done",
			"tokens": map[string]any{"total": float64(40), "input": float64(30), "output": float64(8), "reasoning": float64(2)}}},
		{Time: t0.Add(9 * time.Second), Kind: harness.EventCheckpoint, Detail: map[string]any{"diff": map[string]any{"files": 1}}},
	}
}

func TestSessionTelemetryFromARecordedStream(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	tel, err := newSessionTelemetry(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)),
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), "t1", "s-1", "factory/coding-cheap")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	now := t0
	tel.now = func() time.Time { return now }
	tel.start()

	endBrief := tel.phase("brief")
	endBrief(nil)
	endRun := tel.phase("run")
	for _, ev := range recordedStream(t0) {
		tel.observe(ev)
	}
	endRun(nil)
	endPush := tel.phase("push")
	endPush(errors.New("push refused"))
	now = t0.Add(95 * time.Second)
	tel.end("failed")

	spans := map[string]sdktrace.ReadOnlySpan{}
	var order []string
	for _, s := range rec.Ended() {
		spans[s.Name()] = s
		order = append(order, s.Name())
	}
	want := []string{"brief", "bash", "edit", "write", "bash", "factory-tools_echo", "run", "push", "session"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("spans %v, want %v", order, want)
	}
	root := spans["session"]
	if root.Parent().IsValid() {
		t.Fatal("the session span has a parent; it must be the trace's root")
	}
	if a := attrs(root); a["task.id"] != "t1" || a["session.id"] != "s-1" || a["model_alias"] != "coding-cheap" || a["outcome"] != "failed" {
		t.Fatalf("session attributes %v", a)
	}
	for _, name := range []string{"brief", "run", "push"} {
		if spans[name].Parent().SpanID() != root.SpanContext().SpanID() || attrs(spans[name])["task.id"] != "t1" {
			t.Fatalf("%s: parent %v, attributes %v", name, spans[name].Parent(), attrs(spans[name]))
		}
	}
	if spans["push"].Status().Code != codes.Error || spans["push"].Status().Description != "push failed" {
		t.Fatalf("push status %v", spans["push"].Status())
	}
	run := spans["run"].SpanContext().SpanID()
	var tools []map[string]string
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "brief", "run", "push", "session":
			continue
		}
		if s.Parent().SpanID() != run {
			t.Fatalf("tool span %s is not under run", s.Name())
		}
		a := attrs(s)
		a["duration"] = s.EndTime().Sub(s.StartTime()).String()
		a["code"] = s.Status().Code.String()
		for k, v := range a {
			for _, secret := range []string{"echo recorded", "(no output)", "SECRET-ARG", "hello-from-model", "go test", "FAIL", "invalid arguments"} {
				if strings.Contains(v, secret) {
					t.Fatalf("span %s attribute %s carries %q", s.Name(), k, secret)
				}
			}
		}
		tools = append(tools, a)
	}
	wantTools := []map[string]string{
		{"task.id": "t1", "tool.name": "bash", "tool.status": "ok", "tool.exit_code": "0", "tool.bytes": "11", "duration": "37ms", "code": "Unset"},
		{"task.id": "t1", "tool.name": "edit", "tool.status": "ok", "tool.bytes": "15", "duration": "12ms", "code": "Unset"},
		{"task.id": "t1", "tool.name": "write", "tool.status": "error", "duration": "2ms", "code": "Error"},
		{"task.id": "t1", "tool.name": "bash", "tool.status": "error", "tool.exit_code": "1", "tool.bytes": "4", "duration": "1.5s", "code": "Error"},
		{"task.id": "t1", "tool.name": "factory-tools_echo", "tool.status": "ok", "duration": "4ms", "code": "Unset"},
	}
	if !reflect.DeepEqual(tools, wantTools) {
		t.Fatalf("tool spans\n%v\nwant\n%v", tools, wantTools)
	}
	if end := spans["edit"].EndTime(); !end.Equal(t0.Add(4 * time.Second)) {
		t.Fatalf("edit span ends %v, want the event's time", end)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]float64{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		got[m.Name] = map[string]float64{}
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, dp := range d.DataPoints {
				got[m.Name][dp.Attributes.Encoded(attribute.DefaultEncoder())] = float64(dp.Value)
			}
		case metricdata.Histogram[float64]:
			for _, dp := range d.DataPoints {
				got[m.Name][dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Sum
				if dp.Count != 1 {
					t.Fatalf("%s: %d samples", m.Name, dp.Count)
				}
			}
		}
	}
	wantMetrics := map[string]map[string]float64{
		"worker.tool.calls": {
			"status=ok,tool=bash": 1, "status=error,tool=bash": 1, "status=ok,tool=edit": 1,
			"status=error,tool=write": 1, "status=ok,tool=factory-tools_echo": 1,
		},
		"worker.session.tokens": {
			"direction=input,model_alias=coding-cheap": 150, "direction=output,model_alias=coding-cheap": 53,
			"direction=reasoning,model_alias=coding-cheap": 2,
		},
		"worker.session.duration": {"model_alias=coding-cheap,outcome=failed": 95},
	}
	if !reflect.DeepEqual(got, wantMetrics) {
		t.Fatalf("metrics\n%v\nwant\n%v", got, wantMetrics)
	}
}

func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestSessionOutcome(t *testing.T) {
	ok, fail := harness.Result{Success: true}, harness.Result{Success: false}
	for _, c := range []struct {
		res     harness.Result
		stopped bool
		commit  string
		pushErr error
		want    string
	}{
		{ok, false, "c1", nil, "pushed"},
		{ok, false, "", nil, "no_changes"},
		{ok, false, "", errors.New("refused"), "failed"},
		{fail, true, "c1", nil, "stopped"},
		{fail, true, "", nil, "stopped"},
		{fail, false, "", nil, "failed"},
	} {
		if got := sessionOutcome(c.res, c.stopped, c.commit, c.pushErr); got != c.want {
			t.Errorf("sessionOutcome(%v, %v, %q, %v) = %s, want %s", c.res.Success, c.stopped, c.commit, c.pushErr, got, c.want)
		}
	}
}

// The session span is exported before the result line: factoryd deletes
// the pod as soon as it reads that line.
func TestResultLineComesAfterTheSessionSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel, err := newSessionTelemetry(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)),
		sdkmetric.NewMeterProvider(), "t1", "s-1", "factory/coding-cheap")
	if err != nil {
		t.Fatal(err)
	}
	tel.start()
	var sessionEndedFirst bool
	w := writerFunc(func(p []byte) (int, error) {
		for _, s := range rec.Ended() {
			sessionEndedFirst = sessionEndedFirst || s.Name() == "session"
		}
		return len(p), nil
	})
	writeResult(w, harness.Result{Success: true}, func() { tel.end("pushed") })
	if !sessionEndedFirst {
		t.Fatal("the result line was written before the session span ended")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
