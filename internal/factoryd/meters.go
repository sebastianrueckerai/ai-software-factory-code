package factoryd

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
)

// OpenActivities is the worker interceptor behind
// factory.activity.open_age: it keeps the start of every running activity
// and reports, per activity name, the age of the oldest one running. A
// human wait is no activity, so it never shows here.
type OpenActivities struct {
	interceptor.WorkerInterceptorBase
	now     func() time.Time
	mu      sync.Mutex
	next    int64
	running map[int64]openActivity
}

type openActivity struct {
	name  string
	since time.Time
}

func NewOpenActivities(meter metric.Meter) (*OpenActivities, error) {
	o := &OpenActivities{now: time.Now, running: map[int64]openActivity{}}
	_, err := meter.Float64ObservableGauge("factory.activity.open_age", metric.WithUnit("s"),
		metric.WithDescription("age of the oldest running activity per activity name"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			for name, age := range o.oldest() {
				obs.Observe(age, metric.WithAttributes(attribute.String("activity", name)))
			}
			return nil
		}))
	if err != nil {
		return nil, err
	}
	return o, nil
}

func (o *OpenActivities) start(name string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	o.running[o.next] = openActivity{name: name, since: o.now()}
	return o.next
}

func (o *OpenActivities) finish(id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.running, id)
}

func (o *OpenActivities) oldest() map[string]float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	ages := map[string]float64{}
	for _, a := range o.running {
		ages[a.name] = max(ages[a.name], now.Sub(a.since).Seconds())
	}
	return ages
}

func (o *OpenActivities) InterceptActivity(ctx context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &openActivityInbound{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}, o: o}
}

type openActivityInbound struct {
	interceptor.ActivityInboundInterceptorBase
	o *OpenActivities
}

func (i *openActivityInbound) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	id := i.o.start(activity.GetInfo(ctx).ActivityType.Name)
	defer i.o.finish(id)
	return i.Next.ExecuteActivity(ctx, in)
}

// RegisterAlertCount exports factory.alert.count{kind,verdict}, read from
// the alerts table at each export. Until the table exists it logs once and
// reports nothing.
func RegisterAlertCount(meter metric.Meter, counts func(context.Context) ([]AlertCount, error)) error {
	var missing sync.Once
	_, err := meter.Int64ObservableUpDownCounter("factory.alert.count",
		metric.WithDescription("alerts per kind and verdict (open until one is set)"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			rows, err := counts(ctx)
			if errors.Is(err, ErrNoAlertsTable) {
				missing.Do(func() { log.Printf("alert count: no alerts table yet; skipped until it exists") })
				return nil
			}
			if err != nil {
				return err
			}
			for _, r := range rows {
				obs.Observe(r.N, metric.WithAttributes(attribute.String("kind", r.Kind), attribute.String("verdict", r.Verdict)))
			}
			return nil
		}))
	return err
}
