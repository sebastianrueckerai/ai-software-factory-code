package factoryd

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

func testMetrics(t *testing.T) (*telemetry.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	m, err := telemetry.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	return m, reader
}

type histPoint struct {
	Count uint64
	Sum   float64
}

func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Aggregation, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data, true
			}
		}
	}
	return nil, false
}

func attrKey(s attribute.Set) string { return s.Encoded(attribute.DefaultEncoder()) }

// histograms maps each attribute set ("k=v,k=v", sorted) to its count and sum.
func histograms(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]histPoint {
	t.Helper()
	out := map[string]histPoint{}
	data, ok := collect(t, reader, name)
	if !ok {
		return out
	}
	for _, dp := range data.(metricdata.Histogram[float64]).DataPoints {
		out[attrKey(dp.Attributes)] = histPoint{dp.Count, dp.Sum}
	}
	return out
}

func sums(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	data, ok := collect(t, reader, name)
	if !ok {
		return out
	}
	switch d := data.(type) {
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = dp.Value
		}
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = float64(dp.Value)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			out[attrKey(dp.Attributes)] = dp.Value
		}
	default:
		t.Fatalf("%s: unexpected %T", name, data)
	}
	return out
}
