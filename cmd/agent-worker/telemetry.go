package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// sessionTelemetry makes one session one trace: a root span "session",
// children brief, run and push, and under run a span per tool call from the
// adapter's event stream, with the tool's name, run time, status, exit code
// and bytes written, never its arguments or output.
type sessionTelemetry struct {
	tracer   trace.Tracer
	now      func() time.Time
	alias    string
	task     attribute.KeyValue
	session  attribute.KeyValue
	duration metric.Float64Histogram
	tools    metric.Int64Counter
	tokens   metric.Int64Counter

	mu      sync.Mutex
	began   time.Time
	root    trace.Span
	rootCtx context.Context
	runCtx  context.Context
}

func newSessionTelemetry(tp trace.TracerProvider, mp metric.MeterProvider, taskID, sessionID, model string) (*sessionTelemetry, error) {
	meter := mp.Meter("agent-worker")
	_, alias, _ := strings.Cut(model, "/")
	if alias == "" {
		alias = model
	}
	s := &sessionTelemetry{tracer: tp.Tracer("agent-worker"), now: time.Now, alias: alias,
		task: telemetry.TaskID.String(taskID), session: attribute.String("session.id", sessionID),
		rootCtx: context.Background(), runCtx: context.Background()}
	var err error
	if s.duration, err = meter.Float64Histogram("worker.session.duration", metric.WithUnit("s"),
		metric.WithDescription("one agent session, worker start to result"), metric.WithExplicitBucketBoundaries(telemetry.Buckets...)); err != nil {
		return nil, err
	}
	if s.tools, err = meter.Int64Counter("worker.tool.calls", metric.WithDescription("the agent's tool calls by tool and status")); err != nil {
		return nil, err
	}
	if s.tokens, err = meter.Int64Counter("worker.session.tokens", metric.WithDescription("model tokens the harness reported, by direction")); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *sessionTelemetry) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.began = s.now()
	s.rootCtx, s.root = s.tracer.Start(context.Background(), "session", trace.WithNewRoot(), trace.WithTimestamp(s.began),
		trace.WithAttributes(s.task, s.session, attribute.String("model_alias", s.alias)))
}

// phase starts a child span of the session; the returned func ends it,
// in error when err is not nil. Tool spans go under the run phase.
func (s *sessionTelemetry) phase(name string) func(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, span := s.tracer.Start(s.rootCtx, name, trace.WithTimestamp(s.now()), trace.WithAttributes(s.task))
	if name == "run" {
		s.runCtx = ctx
	}
	return func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End(trace.WithTimestamp(s.now()))
	}
}

func (s *sessionTelemetry) observe(ev harness.Event) {
	if ev.Kind == harness.EventModelCall {
		tokens, _ := ev.Detail["tokens"].(map[string]any)
		for _, dir := range []string{"input", "output", "reasoning"} {
			if n, ok := number(tokens[dir]); ok && n > 0 {
				s.tokens.Add(context.Background(), n, metric.WithAttributes(
					attribute.String("model_alias", s.alias), attribute.String("direction", dir)))
			}
		}
		return
	}
	var name, status string
	attrs := []attribute.KeyValue{s.task}
	switch ev.Kind {
	case harness.EventCommand:
		name, status = "bash", "error"
		if exit, ok := number(ev.Detail["exit"]); ok {
			attrs = append(attrs, attribute.Int64("tool.exit_code", exit))
			if exit == 0 {
				status = "ok"
			}
		}
	case harness.EventFileChange:
		name, _ = ev.Detail["tool"].(string)
		status = "ok"
	case harness.EventToolCall:
		name, _ = ev.Detail["tool"].(string)
		switch ev.Detail["status"] {
		case "completed":
			status = "ok"
		case "error":
			status = "error"
		default:
			return
		}
	default:
		return
	}
	if name == "" {
		name = string(ev.Kind)
	}
	attrs = append(attrs, attribute.String("tool.name", name), attribute.String("tool.status", status))
	if n, ok := number(ev.Detail["bytes"]); ok {
		attrs = append(attrs, attribute.Int64("tool.bytes_written", n))
	}
	end := ev.Time
	if end.IsZero() {
		end = s.now()
	}
	ms, _ := number(ev.Detail["duration_ms"])
	s.mu.Lock()
	parent := s.runCtx
	s.mu.Unlock()
	_, span := s.tracer.Start(parent, name, trace.WithTimestamp(end.Add(-time.Duration(ms)*time.Millisecond)), trace.WithAttributes(attrs...))
	if status == "error" {
		span.SetStatus(codes.Error, "")
	}
	span.End(trace.WithTimestamp(end))
	s.tools.Add(context.Background(), 1, metric.WithAttributes(attribute.String("tool", name), attribute.String("status", status)))
}

// end closes the session span and records the session's duration under
// its outcome: pushed, no_changes, stopped or failed.
func (s *sessionTelemetry) end(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.duration.Record(context.Background(), now.Sub(s.began).Seconds(), metric.WithAttributes(
		attribute.String("outcome", outcome), attribute.String("model_alias", s.alias)))
	s.root.SetAttributes(attribute.String("outcome", outcome))
	s.root.End(trace.WithTimestamp(now))
}

func number(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
