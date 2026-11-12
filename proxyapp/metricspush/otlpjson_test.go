package metricspush

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// collectJSON records some metrics and returns the OTLP/JSON body the exporter would send.
func collectJSON(t *testing.T, record func(p *sdk.MeterProvider)) map[string]any {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	exp := newJSONExporter(srv.URL+"/v1/metrics", "", 5*time.Second)
	res := resource.NewSchemaless(attribute.String("proxy", "context-guru"), attribute.String("service.instance.id", "abc"))
	r := sdk.NewManualReader()
	p := sdk.NewMeterProvider(sdk.WithResource(res), sdk.WithReader(r))
	record(p)
	rm := resourceMetricsOf(t, r)
	if err := exp.Export(context.Background(), rm); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, got)
	}
	return doc
}

func firstMetric(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	rms := doc["resourceMetrics"].([]any)
	sms := rms[0].(map[string]any)["scopeMetrics"].([]any)
	return sms[0].(map[string]any)["metrics"].([]any)[0].(map[string]any)
}

// A counter is a cumulative, monotonic sum, and a 64-bit integer is a decimal STRING in OTLP/JSON.
func TestCounterEncodesAsCumulativeSumWithStringInt(t *testing.T) {
	doc := collectJSON(t, func(p *sdk.MeterProvider) {
		c, _ := p.Meter("m").Int64Counter("llm.test.tokens")
		c.Add(context.Background(), 12345678901234)
	})
	m := firstMetric(t, doc)
	if m["name"] != "llm.test.tokens" {
		t.Fatalf("name = %v", m["name"])
	}
	sum := m["sum"].(map[string]any)
	if sum["aggregationTemporality"].(float64) != 2 || sum["isMonotonic"] != true {
		t.Errorf("sum = %v, want cumulative (2) and monotonic", sum)
	}
	dp := sum["dataPoints"].([]any)[0].(map[string]any)
	if dp["asInt"] != "12345678901234" {
		t.Errorf("asInt = %#v, want the string \"12345678901234\"", dp["asInt"])
	}
	if _, ok := dp["timeUnixNano"].(string); !ok {
		t.Errorf("timeUnixNano = %#v, want a string", dp["timeUnixNano"])
	}
}

func TestFloatCounterUsesAsDouble(t *testing.T) {
	doc := collectJSON(t, func(p *sdk.MeterProvider) {
		c, _ := p.Meter("m").Float64Counter("llm.test.usd")
		c.Add(context.Background(), 1.5)
	})
	dp := firstMetric(t, doc)["sum"].(map[string]any)["dataPoints"].([]any)[0].(map[string]any)
	if dp["asDouble"].(float64) != 1.5 {
		t.Errorf("asDouble = %#v", dp["asDouble"])
	}
	if _, has := dp["asInt"]; has {
		t.Error("a float point must not carry asInt")
	}
}

func TestAttributesUseTypedValues(t *testing.T) {
	doc := collectJSON(t, func(p *sdk.MeterProvider) {
		c, _ := p.Meter("m").Int64Counter("llm.test.n")
		c.Add(context.Background(), 1, metricAttrs(
			attribute.String("tier", "input"), attribute.Int64("n", 7), attribute.Bool("ok", true), attribute.Float64("f", 0.5)))
	})
	dp := firstMetric(t, doc)["sum"].(map[string]any)["dataPoints"].([]any)[0].(map[string]any)
	got := map[string]map[string]any{}
	for _, a := range dp["attributes"].([]any) {
		kv := a.(map[string]any)
		got[kv["key"].(string)] = kv["value"].(map[string]any)
	}
	if got["tier"]["stringValue"] != "input" || got["n"]["intValue"] != "7" || got["ok"]["boolValue"] != true || got["f"]["doubleValue"].(float64) != 0.5 {
		t.Errorf("attributes = %v", got)
	}
}

func TestResourceAttributesAreSent(t *testing.T) {
	doc := collectJSON(t, func(p *sdk.MeterProvider) {
		c, _ := p.Meter("m").Int64Counter("llm.test.n")
		c.Add(context.Background(), 1)
	})
	attrs := doc["resourceMetrics"].([]any)[0].(map[string]any)["resource"].(map[string]any)["attributes"].([]any)
	seen := map[string]string{}
	for _, a := range attrs {
		kv := a.(map[string]any)
		seen[kv["key"].(string)] = kv["value"].(map[string]any)["stringValue"].(string)
	}
	if seen["proxy"] != "context-guru" || seen["service.instance.id"] != "abc" {
		t.Errorf("resource attributes = %v", seen)
	}
}

func TestHistogramEncodesBucketsAsStrings(t *testing.T) {
	doc := collectJSON(t, func(p *sdk.MeterProvider) {
		h, _ := p.Meter("m").Float64Histogram("llm.test.seconds")
		h.Record(context.Background(), 0.2)
		h.Record(context.Background(), 3)
	})
	h := firstMetric(t, doc)["histogram"].(map[string]any)
	dp := h["dataPoints"].([]any)[0].(map[string]any)
	if dp["count"] != "2" {
		t.Errorf("count = %#v, want \"2\"", dp["count"])
	}
	bc := dp["bucketCounts"].([]any)
	bounds := dp["explicitBounds"].([]any)
	if len(bc) != len(bounds)+1 {
		t.Errorf("%d bucket counts for %d bounds, want one more count than bounds", len(bc), len(bounds))
	}
	if _, ok := bc[0].(string); !ok {
		t.Errorf("bucketCounts[0] = %#v, want a string", bc[0])
	}
}

// NaN and infinity cannot be written in JSON, so they must not break the whole push.
func TestNaNDoesNotBreakTheBody(t *testing.T) {
	doc := collectJSON(t, func(p *sdk.MeterProvider) {
		g, _ := p.Meter("m").Float64Gauge("llm.test.g")
		g.Record(context.Background(), nanValue())
	})
	if firstMetric(t, doc)["gauge"] == nil {
		t.Fatal("no gauge in the body")
	}
}

func TestExporterReportsStatusAndSendsToken(t *testing.T) {
	var auth, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	exp := newJSONExporter(srv.URL, "tok", 5*time.Second)
	err := exp.Export(context.Background(), emptyMetrics())
	if err == nil || reasonOf(err) != "unauthorized" {
		t.Fatalf("err = %v, reason = %q, want unauthorized", err, reasonOf(err))
	}
	if auth != "Bearer tok" || ctype != "application/json" {
		t.Errorf("headers: Authorization=%q Content-Type=%q", auth, ctype)
	}
}

func TestStatusReasons(t *testing.T) {
	for code, want := range map[int]string{401: "unauthorized", 403: "unauthorized", 429: "rate_limited", 500: "collector_error", 503: "collector_error", 400: "rejected"} {
		if got := statusReason(code); got != want {
			t.Errorf("statusReason(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestExportAfterShutdownIsRefused(t *testing.T) {
	exp := newJSONExporter("http://127.0.0.1:1", "", time.Second)
	_ = exp.Shutdown(context.Background())
	if err := exp.Export(context.Background(), emptyMetrics()); err != sdk.ErrExporterShutdown {
		t.Fatalf("err = %v, want ErrExporterShutdown", err)
	}
}
