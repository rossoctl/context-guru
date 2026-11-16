package metricspush

import (
	"context"
	"math"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func metricAttrs(kv ...attribute.KeyValue) metric.AddOption {
	return metric.WithAttributeSet(attribute.NewSet(kv...))
}

func nanValue() float64 { return math.NaN() }

func resourceMetricsOf(t *testing.T, r *sdk.ManualReader) *metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return &rm
}

func emptyMetrics() *metricdata.ResourceMetrics { return &metricdata.ResourceMetrics{} }
