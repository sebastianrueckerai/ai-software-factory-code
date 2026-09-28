package factoryd

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// ActivitySpans is the worker interceptor that runs every activity inside
// a span named after it. An activity of a task's workflow (WorkflowID)
// carries the task's id as task.id, the key the gateway's and the worker's
// spans and the audit log records carry too. A failed activity's span is
// in error, described by its name only.
type ActivitySpans struct {
	interceptor.WorkerInterceptorBase
	Tracer trace.Tracer
}

func (a *ActivitySpans) InterceptActivity(ctx context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &activitySpanInbound{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}, tracer: a.Tracer}
}

type activitySpanInbound struct {
	interceptor.ActivityInboundInterceptorBase
	tracer trace.Tracer
}

func (i *activitySpanInbound) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	info := activity.GetInfo(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("temporal.workflow_id", info.WorkflowExecution.ID),
		attribute.Int("temporal.attempt", int(info.Attempt)),
	}
	if id, ok := strings.CutPrefix(info.WorkflowExecution.ID, WorkflowID("")); ok && id != "" {
		attrs = append(attrs, telemetry.TaskID.String(id))
	}
	ctx, span := i.tracer.Start(ctx, info.ActivityType.Name, trace.WithAttributes(attrs...))
	defer span.End()
	out, err := i.Next.ExecuteActivity(ctx, in)
	if err != nil {
		// the error's text (Forgejo's and the API server's answers) stays
		// in the workflow history, not in a span
		span.SetStatus(codes.Error, info.ActivityType.Name+" failed")
	}
	return out, err
}
