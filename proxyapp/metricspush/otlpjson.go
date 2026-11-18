package metricspush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// This file is an OTLP/HTTP exporter that sends JSON. The standard exporter sends protobuf, and its
// generated types link gRPC and protobuf (+11.9 MB), which the default build must not (see
// depguard_test.go). The OTLP specification allows JSON on the same path, /v1/metrics. This code
// uses the standard library only.
//
// The rules of OTLP/JSON that matter here:
//   - a 64-bit integer is a decimal string ("12"), not a number;
//   - a time is Unix nanoseconds, also as a decimal string;
//   - field names are lowerCamelCase;
//   - an attribute value is an object with one key: stringValue, intValue, doubleValue or boolValue.

// jsonExporter implements sdk.Exporter.
type jsonExporter struct {
	url    string
	token  string
	client *http.Client
	closed atomic.Bool
}

func newJSONExporter(url, token string, timeout time.Duration) *jsonExporter {
	return &jsonExporter{url: url, token: token, client: &http.Client{Timeout: timeout}}
}

// The SDK keeps cumulative totals, so a lost push loses no count (R20). Aggregation is the default.
func (*jsonExporter) Temporality(sdk.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}
func (*jsonExporter) Aggregation(k sdk.InstrumentKind) sdk.Aggregation {
	return sdk.DefaultAggregationSelector(k)
}
func (*jsonExporter) ForceFlush(context.Context) error { return nil }

func (e *jsonExporter) Shutdown(context.Context) error {
	e.closed.Store(true)
	e.client.CloseIdleConnections()
	return nil
}

// Export encodes the data and sends it once. The SDK calls it again at the next interval with the
// totals, so there is no queue and no retry loop here.
func (e *jsonExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if e.closed.Load() {
		return sdk.ErrExporterShutdown
	}
	body, err := json.Marshal(encode(rm))
	if err != nil {
		return fmt.Errorf("otlp json: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) // drain a little so the connection can be reused
	return &statusError{code: resp.StatusCode, body: string(snippet)}
}

// statusError carries the HTTP status so reasonOf can name it without guessing at text.
type statusError struct {
	code int
	body string
}

func (s *statusError) Error() string {
	return fmt.Sprintf("collector answered status %d: %s", s.code, s.body)
}

var _ sdk.Exporter = (*jsonExporter)(nil)

// --- OTLP/JSON shapes (only the fields we send) ---

type jRequest struct {
	ResourceMetrics []jResourceMetrics `json:"resourceMetrics"`
}

type jResourceMetrics struct {
	Resource     jResource       `json:"resource"`
	ScopeMetrics []jScopeMetrics `json:"scopeMetrics"`
}

type jResource struct {
	Attributes []jKV `json:"attributes"`
}

type jScopeMetrics struct {
	Scope   jScope    `json:"scope"`
	Metrics []jMetric `json:"metrics"`
}

type jScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type jMetric struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Unit        string      `json:"unit,omitempty"`
	Sum         *jSum       `json:"sum,omitempty"`
	Gauge       *jGauge     `json:"gauge,omitempty"`
	Histogram   *jHistogram `json:"histogram,omitempty"`
}

type jSum struct {
	DataPoints             []jNumberPoint `json:"dataPoints"`
	AggregationTemporality int            `json:"aggregationTemporality"`
	IsMonotonic            bool           `json:"isMonotonic"`
}

type jGauge struct {
	DataPoints []jNumberPoint `json:"dataPoints"`
}

type jHistogram struct {
	DataPoints             []jHistPoint `json:"dataPoints"`
	AggregationTemporality int          `json:"aggregationTemporality"`
}

// jNumberPoint: exactly one of AsInt and AsDouble is set. AsInt is a string (int64 rule).
type jNumberPoint struct {
	Attributes        []jKV    `json:"attributes,omitempty"`
	StartTimeUnixNano string   `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string   `json:"timeUnixNano"`
	AsInt             *string  `json:"asInt,omitempty"`
	AsDouble          *float64 `json:"asDouble,omitempty"`
}

type jHistPoint struct {
	Attributes        []jKV     `json:"attributes,omitempty"`
	StartTimeUnixNano string    `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string    `json:"timeUnixNano"`
	Count             string    `json:"count"`
	Sum               *float64  `json:"sum,omitempty"`
	BucketCounts      []string  `json:"bucketCounts"`
	ExplicitBounds    []float64 `json:"explicitBounds"`
}

type jKV struct {
	Key   string `json:"key"`
	Value jAny   `json:"value"`
}

type jAny struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

// OTLP aggregationTemporality: 1 = delta, 2 = cumulative.
const temporalityCumulative = 2

func encode(rm *metricdata.ResourceMetrics) jRequest {
	out := jResourceMetrics{}
	if rm.Resource != nil {
		out.Resource.Attributes = kvs(rm.Resource.Iter())
	}
	for _, sm := range rm.ScopeMetrics {
		js := jScopeMetrics{Scope: jScope{Name: sm.Scope.Name, Version: sm.Scope.Version}}
		for _, m := range sm.Metrics {
			if jm, ok := encodeMetric(m); ok {
				js.Metrics = append(js.Metrics, jm)
			}
		}
		if len(js.Metrics) > 0 {
			out.ScopeMetrics = append(out.ScopeMetrics, js)
		}
	}
	return jRequest{ResourceMetrics: []jResourceMetrics{out}}
}

func encodeMetric(m metricdata.Metrics) (jMetric, bool) {
	jm := jMetric{Name: m.Name, Description: m.Description, Unit: m.Unit}
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		jm.Sum = &jSum{DataPoints: intPoints(d.DataPoints), AggregationTemporality: temporalityCumulative, IsMonotonic: d.IsMonotonic}
	case metricdata.Sum[float64]:
		jm.Sum = &jSum{DataPoints: floatPoints(d.DataPoints), AggregationTemporality: temporalityCumulative, IsMonotonic: d.IsMonotonic}
	case metricdata.Gauge[int64]:
		jm.Gauge = &jGauge{DataPoints: intPoints(d.DataPoints)}
	case metricdata.Gauge[float64]:
		jm.Gauge = &jGauge{DataPoints: floatPoints(d.DataPoints)}
	case metricdata.Histogram[float64]:
		jm.Histogram = &jHistogram{DataPoints: histPoints(d.DataPoints), AggregationTemporality: temporalityCumulative}
	case metricdata.Histogram[int64]:
		jm.Histogram = &jHistogram{DataPoints: histPointsInt(d.DataPoints), AggregationTemporality: temporalityCumulative}
	default:
		return jm, false // a kind we do not send (exponential histogram, summary)
	}
	return jm, true
}

func intPoints(dps []metricdata.DataPoint[int64]) []jNumberPoint {
	out := make([]jNumberPoint, 0, len(dps))
	for _, dp := range dps {
		v := strconv.FormatInt(dp.Value, 10)
		out = append(out, jNumberPoint{Attributes: kvs(dp.Attributes.Iter()), StartTimeUnixNano: nanos(dp.StartTime),
			TimeUnixNano: nanos(dp.Time), AsInt: &v})
	}
	return out
}

func floatPoints(dps []metricdata.DataPoint[float64]) []jNumberPoint {
	out := make([]jNumberPoint, 0, len(dps))
	for _, dp := range dps {
		v := finite(dp.Value)
		out = append(out, jNumberPoint{Attributes: kvs(dp.Attributes.Iter()), StartTimeUnixNano: nanos(dp.StartTime),
			TimeUnixNano: nanos(dp.Time), AsDouble: &v})
	}
	return out
}

func histPoints(dps []metricdata.HistogramDataPoint[float64]) []jHistPoint {
	out := make([]jHistPoint, 0, len(dps))
	for _, dp := range dps {
		s := finite(dp.Sum)
		out = append(out, histPoint(dp.Attributes, dp.StartTime, dp.Time, dp.Count, &s, dp.Bounds, dp.BucketCounts))
	}
	return out
}

func histPointsInt(dps []metricdata.HistogramDataPoint[int64]) []jHistPoint {
	out := make([]jHistPoint, 0, len(dps))
	for _, dp := range dps {
		s := float64(dp.Sum)
		out = append(out, histPoint(dp.Attributes, dp.StartTime, dp.Time, dp.Count, &s, dp.Bounds, dp.BucketCounts))
	}
	return out
}

func histPoint(attrs attribute.Set, start, at time.Time, count uint64, sum *float64, bounds []float64, buckets []uint64) jHistPoint {
	bc := make([]string, len(buckets))
	for i, c := range buckets {
		bc[i] = strconv.FormatUint(c, 10)
	}
	return jHistPoint{Attributes: kvs(attrs.Iter()), StartTimeUnixNano: nanos(start), TimeUnixNano: nanos(at),
		Count: strconv.FormatUint(count, 10), Sum: sum, BucketCounts: bc, ExplicitBounds: bounds}
}

// nanos gives a time as Unix nanoseconds in a decimal string. A zero time is left out.
func nanos(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

// finite keeps NaN and infinity out of the JSON, which cannot carry them.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func kvs(it attribute.Iterator) []jKV {
	var out []jKV
	for it.Next() {
		a := it.Attribute()
		out = append(out, jKV{Key: string(a.Key), Value: anyValue(a.Value)})
	}
	return out
}

func anyValue(v attribute.Value) jAny {
	switch v.Type() {
	case attribute.BOOL:
		b := v.AsBool()
		return jAny{BoolValue: &b}
	case attribute.INT64:
		s := strconv.FormatInt(v.AsInt64(), 10)
		return jAny{IntValue: &s}
	case attribute.FLOAT64:
		f := finite(v.AsFloat64())
		return jAny{DoubleValue: &f}
	}
	s := v.Emit() // strings, and the slice types as their text form
	return jAny{StringValue: &s}
}

// statusReason maps an HTTP status to a label value.
func statusReason(code int) string {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return "unauthorized"
	case code == http.StatusTooManyRequests:
		return "rate_limited"
	case code >= 500:
		return "collector_error"
	}
	return "rejected"
}
