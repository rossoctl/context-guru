package proxy

// The OTLP side of --record: one SDK span per recorded turn, sent once by a batch exporter.
//
// WHY AN SDK TRACER AND NOT HAND-BUILT PROTOBUF. A turn's span is built after the request is
// over, with a start and end time that already happened, and ids that are already decided (the
// trace id is the session's). The SDK supports exactly that through trace.WithTimestamp and a
// parent span context; the BatchSpanProcessor then gives us the two properties the recorder
// needs for free: export off the writer goroutine, and a bounded queue that DROPS when full
// rather than blocking, which keeps "drop, never delay".
//
// Ids: the SDK draws random ids unless told otherwise, so recExport installs an IDGenerator
// that hands back the ids the turn already carries. That is what makes a turn's span id in
// Phoenix the same value its child spans and the converter refer to.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rossoctl/context-guru/internal/record"
)

// Batch settings. The exporter's own defaults (2048-span queue, 512 per batch) assume small
// spans; a recorded span carries up to two 1 MiB bodies, so batches are kept small enough that
// one OTLP/HTTP request stays well under a collector's usual body limit (Phoenix and the OTel
// collector default to tens of MB; 16 spans x 2 MiB = 32 MiB worst case).
const (
	recExportQueue     = 256
	recExportBatch     = 16
	recExportTimeout   = 30 * time.Second
	recExportBatchWait = 5 * time.Second
)

// recExport owns the tracer provider for one recorder.
type recExport struct {
	ids    *fixedIDs
	mu     sync.Mutex // one span at a time through the fixed id generator
	tracer map[string]trace.Tracer
	tpByT  map[string]*sdktrace.TracerProvider
	exp    sdktrace.SpanExporter
}

// newRecExport builds an OTLP/HTTP exporter for endpoint, a full URL such as
// http://localhost:6006/v1/traces (Phoenix) or http://collector:4318/v1/traces.
func newRecExport(endpoint string) (*recExport, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("record-otlp %q: want a URL such as http://localhost:6006/v1/traces", endpoint)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithTimeout(recExportTimeout)}
	if u.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("record-otlp: %w", err)
	}
	return newRecExportWith(exp), nil
}

// newRecExportWith is the constructor tests use, with an in-memory exporter.
func newRecExportWith(exp sdktrace.SpanExporter) *recExport {
	return &recExport{
		ids:    &fixedIDs{},
		exp:    exp,
		tracer: map[string]trace.Tracer{},
		tpByT:  map[string]*sdktrace.TracerProvider{},
	}
}

// tracerFor returns the tracer for one tenant. The tenant id is a RESOURCE attribute (Dudi's
// point 5: one collector receives several accounts, so each span must say whose it is), and a
// resource belongs to a provider, so each tenant gets its own provider over the shared exporter.
func (x *recExport) tracerFor(tenantID string) trace.Tracer {
	if t, ok := x.tracer[tenantID]; ok {
		return t
	}
	attrs := []attribute.KeyValue{
		attribute.String(record.ResService, record.ServiceNameProxy),
		attribute.String(record.ResServiceVer, record.CollectorVersion),
		attribute.String(record.ResClient, "claude-code"),
	}
	if tenantID != "" {
		attrs = append(attrs, attribute.String(record.ResTenant, tenantID))
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attrs...)),
		sdktrace.WithIDGenerator(x.ids),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(x.exp,
			sdktrace.WithMaxQueueSize(recExportQueue),
			sdktrace.WithMaxExportBatchSize(recExportBatch),
			sdktrace.WithBatchTimeout(recExportBatchWait),
			sdktrace.WithExportTimeout(recExportTimeout))),
	)
	t := tp.Tracer("github.com/rossoctl/context-guru/internal/record")
	x.tracer[tenantID] = t
	x.tpByT[tenantID] = tp
	return t
}

// export sends one span. Called on the writer goroutine only.
func (x *recExport) export(tenantID string, s record.OTLPSpan) error {
	tid, err := trace.TraceIDFromHex(s.TraceID)
	if err != nil {
		return fmt.Errorf("record: trace id %q: %w", s.TraceID, err)
	}
	sid, err := trace.SpanIDFromHex(s.SpanID)
	if err != nil {
		return fmt.Errorf("record: span id %q: %w", s.SpanID, err)
	}
	start, end := s.Start, s.End
	if end.Before(start) {
		end = start
	}

	ctx := context.Background()
	if s.ParentSpanID != "" {
		if pid, err := trace.SpanIDFromHex(s.ParentSpanID); err == nil {
			ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: tid, SpanID: pid, TraceFlags: trace.FlagsSampled}))
		}
	}
	tr := x.tracerFor(tenantID)

	x.mu.Lock()
	x.ids.next(tid, sid)
	_, span := tr.Start(ctx, s.Name, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attributesOf(s.Attributes)...))
	x.mu.Unlock()
	if s.StatusError != "" {
		span.SetStatus(codes.Error, s.StatusError)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End(trace.WithTimestamp(end))
	return nil
}

// flush sends every tenant's queued spans now, without shutting down.
func (x *recExport) flush() error {
	if x == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), recExportTimeout)
	defer cancel()
	var first error
	for _, tp := range x.tpByT {
		if err := tp.ForceFlush(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Shutdown flushes every tenant's queued spans. Bounded, so a collector that is down cannot hold
// proxy shutdown hostage.
func (x *recExport) Shutdown() error {
	if x == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), recExportTimeout)
	defer cancel()
	var first error
	for _, tp := range x.tpByT {
		if err := tp.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// attributesOf converts the record package's plain map into OTel attributes, keys sorted so a
// span's attribute order is stable.
func attributesOf(m map[string]any) []attribute.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]attribute.KeyValue, 0, len(m))
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			out = append(out, attribute.String(k, v))
		case bool:
			out = append(out, attribute.Bool(k, v))
		case int:
			out = append(out, attribute.Int(k, v))
		case int64:
			out = append(out, attribute.Int64(k, v))
		case float64:
			out = append(out, attribute.Float64(k, v))
		case nil:
		default:
			out = append(out, attribute.String(k, fmt.Sprint(v)))
		}
	}
	return out
}

// fixedIDs is an IDGenerator that returns the ids set by next. The tracer asks for a span id
// (and, for a root span, a trace id) inside Start; export sets them just before, under mu.
type fixedIDs struct {
	tid trace.TraceID
	sid trace.SpanID
}

func (f *fixedIDs) next(tid trace.TraceID, sid trace.SpanID) { f.tid, f.sid = tid, sid }

func (f *fixedIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID) { return f.tid, f.sid }

func (f *fixedIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID { return f.sid }
