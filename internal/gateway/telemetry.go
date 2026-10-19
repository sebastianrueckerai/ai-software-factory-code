package gateway

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// callBuckets are gateway.call.duration's boundaries, in seconds: model
// and tool calls take from well under a second to the connectors' 120 s.
var callBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Telemetry is the gateway's spans, metrics and audit log records.
type Telemetry struct {
	tp       trace.TracerProvider
	tracer   trace.Tracer
	logger   otellog.Logger
	redact   *redact.Scrubber
	calls    metric.Int64Counter
	duration metric.Float64Histogram
	spend    metric.Float64Counter
	refusals metric.Int64Counter
}

// NewTelemetry builds the gateway's instruments. secrets are the gateway's
// own keys, removed from every audit log record wherever they appear.
func NewTelemetry(tp trace.TracerProvider, mp metric.MeterProvider, lp otellog.LoggerProvider, secrets ...string) (*Telemetry, error) {
	meter := mp.Meter("factory-gateway")
	t := &Telemetry{tp: tp, tracer: tp.Tracer("factory-gateway"), logger: lp.Logger("factory-gateway"), redact: redact.New(secrets...)}
	var err error
	if t.calls, err = meter.Int64Counter("gateway.calls", metric.WithDescription("upstream model and tool calls by kind, name and upstream status")); err != nil {
		return nil, err
	}
	if t.duration, err = meter.Float64Histogram("gateway.call.duration", metric.WithUnit("s"),
		metric.WithDescription("one upstream model or tool call"), metric.WithExplicitBucketBoundaries(callBuckets...)); err != nil {
		return nil, err
	}
	if t.spend, err = meter.Float64Counter("gateway.spend.eur", metric.WithUnit("EUR"),
		metric.WithDescription("what the gateway booked onto its tasks' spend, by project")); err != nil {
		return nil, err
	}
	if t.refusals, err = meter.Int64Counter("gateway.refusals", metric.WithDescription("refused calls by the refusal's decision")); err != nil {
		return nil, err
	}
	return t, nil
}

func noTelemetry() *Telemetry {
	t, _ := NewTelemetry(tracenoop.NewTracerProvider(), noop.NewMeterProvider(), lognoop.NewLoggerProvider()) // #nosec G104 -- the no-op instruments never fail
	return t
}

// caller puts the verified task on the request's server span.
func caller(ctx context.Context, taskID string) {
	if taskID != "" {
		trace.SpanFromContext(ctx).SetAttributes(telemetry.TaskID.String(taskID))
	}
}

// upstreamCall is one model or tool call past admission, from its span's
// start to its booking.
type upstreamCall struct {
	t      *Telemetry
	span   trace.Span
	start  time.Time
	task   Task
	kind   string // model or tool
	name   string // the alias or the tool
	status string
	hold   float64
	cost   float64
	over   bool
	// deployment is the LiteLLM deployment that answered a model call
	deployment string
}

func (t *Telemetry) startCall(ctx context.Context, task Task, kind, name string, hold float64) (context.Context, *upstreamCall) {
	ctx, span := t.tracer.Start(ctx, kind+" "+name, trace.WithSpanKind(trace.SpanKindClient))
	return ctx, &upstreamCall{t: t, span: span, start: time.Now(), task: task, kind: kind, name: name, status: "error", hold: hold}
}

// booked records what the call's booking added to the task's spend.
func (c *upstreamCall) booked(eur float64, overHold string) {
	c.cost, c.over = eur, overHold != ""
	c.t.spend.Add(context.Background(), eur, metric.WithAttributes(attribute.String("project", c.task.Repository)))
}

func (c *upstreamCall) end() {
	c.span.SetAttributes(telemetry.TaskID.String(c.task.ID), attribute.String("project", c.task.Repository),
		attribute.String("kind", c.kind), attribute.String("name", c.name), attribute.String("upstream.status", c.status),
		attribute.Float64("hold_eur", c.hold), attribute.Float64("cost_eur", c.cost), attribute.Bool("over_hold", c.over))
	if c.deployment != "" {
		c.span.SetAttributes(attribute.String("deployment", c.deployment))
	}
	if c.status != "ok" && !strings.HasPrefix(c.status, "2") {
		c.span.SetStatus(codes.Error, "upstream "+c.status)
	}
	c.span.End()
	ctx := context.Background()
	c.t.calls.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", c.kind), attribute.String("name", c.name),
		attribute.String("project", c.task.Repository), attribute.String("status", c.status)))
	c.t.duration.Record(ctx, time.Since(c.start).Seconds(), metric.WithAttributes(attribute.String("kind", c.kind), attribute.String("name", c.name)))
}

// toolStatus is a tool call's upstream status: the provider's HTTP status
// when it refused or failed, ok, or error for a failure without one.
func toolStatus(err error) string {
	var pe *providerError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &pe):
		return strconv.Itoa(pe.status)
	}
	return "error"
}

// Audited is store with every audit row it stores also emitted as one log
// record, and every refusal counted. Postgres stays the record; the log
// record is the searchable copy.
func (t *Telemetry) Audited(store Store) Store { return auditedStore{Store: store, t: t} }

type auditedStore struct {
	Store
	t *Telemetry
}

func (a auditedStore) InsertAudit(ctx context.Context, row AuditRow) error {
	if strings.HasPrefix(row.Decision, "refused_") {
		a.t.refusals.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", row.Decision)))
	}
	if err := a.Store.InsertAudit(ctx, row); err != nil {
		return err
	}
	a.t.logger.Emit(context.Background(), a.t.auditRecord(row))
	return nil
}

// auditRecord is row as a log record: its columns as attributes, strings
// through the scrubber, nil and empty columns left out.
func (t *Telemetry) auditRecord(row AuditRow) otellog.Record {
	var r otellog.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(otellog.SeverityInfo)
	r.SetSeverityText("INFO")
	// the task id (from the verified token) and the decision (a constant)
	// are the gateway's own, and task.id is the cross-binary join key
	r.SetBody(otellog.StringValue("audit " + row.Decision))
	str := func(key, v string) {
		if v != "" {
			r.AddAttributes(otellog.String(key, t.scrub(v)))
		}
	}
	num := func(key string, v *float64) {
		if v != nil {
			r.AddAttributes(otellog.Float64(key, *v))
		}
	}
	if row.TaskID != "" {
		r.AddAttributes(otellog.String(string(telemetry.TaskID), row.TaskID))
	}
	r.AddAttributes(otellog.String("audit.decision", row.Decision), otellog.String("audit.kind", row.kind()))
	str("audit.reason", row.Reason)
	str("audit.model_alias", row.ModelAlias)
	str("audit.tool", row.Tool)
	num("audit.spend_eur", row.SpendEUR)
	if row.PolicyVersion != nil {
		r.AddAttributes(otellog.Int64("audit.policy_version", *row.PolicyVersion))
	}
	if row.OutputLimit != nil {
		r.AddAttributes(otellog.Int("audit.output_limit", *row.OutputLimit))
	}
	num("audit.hold_eur", row.HoldEUR)
	str("audit.over_hold", row.OverHold)
	str("audit.deployment", row.Deployment)
	return r
}

// upstreamBody is the body an upstream error carries (a provider's
// answer, LiteLLM's key API answer, an unreadable Replicate prediction):
// dropped from the audit log records before the shared rules, the
// gateway's booking note after it kept.
var upstreamBody = regexp.MustCompile(`(?s)(provider answered \d{3}|key (?:generate|block): status \d{3}|unreadable prediction): .*?(; cost not reported, booked the hold [\d.]+ EUR)?$`)

// scrub keeps keys and response bodies out of the audit log records.
func (t *Telemetry) scrub(v string) string {
	return t.redact.String(upstreamBody.ReplaceAllString(v, "$1: [body dropped]$2"))
}
