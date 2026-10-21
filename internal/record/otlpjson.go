package record

// OTLP JSON: the wire form of exported spans, and the input of the offline converter.
//
// One line per span batch, each line an ExportTraceServiceRequest in the OTLP JSON encoding
// (opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding). It is the format the OTel
// collector's file exporter writes, so the converter reads a collector's output as well as the
// proxy's own local file. The encoding is written by hand, not with protojson, because OTLP JSON
// departs from protojson in two places: ids are hex, not base64, and enum values are integers.
// Both are easy to get wrong with a generic marshaller and silently produce a file no collector
// accepts.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

const (
	otlpKindClient  = 3 // SPAN_KIND_CLIENT
	otlpStatusOK    = 1 // STATUS_CODE_OK
	otlpStatusError = 2 // STATUS_CODE_ERROR
	scopeName       = "github.com/rossoctl/context-guru/internal/record"
)

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 as a decimal string, per the spec
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpWireSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpScopeSpans struct {
	Scope struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	} `json:"scope"`
	Spans []otlpWireSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource struct {
		Attributes []otlpKeyValue `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// Resource returns the resource attributes the proxy exports with, for one tenant ("" = none).
func Resource(tenantID string) map[string]any {
	r := map[string]any{
		ResService:    ServiceNameProxy,
		ResServiceVer: CollectorVersion,
		ResClient:     "claude-code",
	}
	if tenantID != "" {
		r[ResTenant] = tenantID
	}
	return r
}

// EncodeOTLPLine renders spans sharing one resource as one OTLP JSON line, newline included.
func EncodeOTLPLine(resource map[string]any, spans ...OTLPSpan) ([]byte, error) {
	ss := otlpScopeSpans{}
	ss.Scope.Name, ss.Scope.Version = scopeName, CollectorVersion
	for _, s := range spans {
		w := otlpWireSpan{
			TraceID: s.TraceID, SpanID: s.SpanID, ParentSpanID: s.ParentSpanID, Name: s.Name,
			Kind:              otlpKindClient,
			StartTimeUnixNano: unixNano(s.Start),
			EndTimeUnixNano:   unixNano(s.End),
			Attributes:        keyValues(s.Attributes),
			Status:            otlpStatus{Code: otlpStatusOK},
		}
		if s.StatusError != "" {
			w.Status = otlpStatus{Code: otlpStatusError, Message: s.StatusError}
		}
		ss.Spans = append(ss.Spans, w)
	}
	rs := otlpResourceSpans{ScopeSpans: []otlpScopeSpans{ss}}
	rs.Resource.Attributes = keyValues(resource)
	b, err := json.Marshal(otlpRequest{ResourceSpans: []otlpResourceSpans{rs}})
	if err != nil {
		return nil, fmt.Errorf("record: encode OTLP JSON: %w", err)
	}
	return append(b, '\n'), nil
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func keyValues(m map[string]any) []otlpKeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpKeyValue, 0, len(keys))
	for _, k := range keys {
		var v otlpAnyValue
		switch x := m[k].(type) {
		case string:
			v.StringValue = &x
		case bool:
			v.BoolValue = &x
		case int:
			s := strconv.Itoa(x)
			v.IntValue = &s
		case int64:
			s := strconv.FormatInt(x, 10)
			v.IntValue = &s
		case float64:
			v.DoubleValue = &x
		case nil:
			continue
		default:
			s := fmt.Sprint(x)
			v.StringValue = &s
		}
		out = append(out, otlpKeyValue{Key: k, Value: v})
	}
	return out
}

// ReadSpan is one span read back from OTLP JSON, with its resource.
type ReadSpan struct {
	OTLPSpan
	Resource map[string]any
}

// ReadOTLP reads OTLP JSON lines (blank lines skipped). A line that does not parse is an error:
// the converter would rather stop than write a corpus file with spans quietly missing.
func ReadOTLP(r io.Reader) ([]ReadSpan, error) {
	var out []ReadSpan
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<30) // a line holds whole bodies
	n := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req otlpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return nil, fmt.Errorf("record: OTLP JSON line %d: %w", n, err)
		}
		for _, rs := range req.ResourceSpans {
			res := attrMap(rs.Resource.Attributes)
			for _, ss := range rs.ScopeSpans {
				for _, w := range ss.Spans {
					s := ReadSpan{Resource: res, OTLPSpan: OTLPSpan{
						TraceID: w.TraceID, SpanID: w.SpanID, ParentSpanID: w.ParentSpanID, Name: w.Name,
						Start: fromUnixNano(w.StartTimeUnixNano), End: fromUnixNano(w.EndTimeUnixNano),
						Attributes: attrMap(w.Attributes),
					}}
					if w.Status.Code == otlpStatusError {
						s.StatusError = w.Status.Message
						if s.StatusError == "" {
							s.StatusError = "error"
						}
					}
					out = append(out, s)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("record: read OTLP JSON: %w", err)
	}
	return out, nil
}

func fromUnixNano(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func attrMap(kvs []otlpKeyValue) map[string]any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		v := kv.Value
		switch {
		case v.StringValue != nil:
			m[kv.Key] = *v.StringValue
		case v.BoolValue != nil:
			m[kv.Key] = *v.BoolValue
		case v.IntValue != nil:
			if n, err := strconv.ParseInt(*v.IntValue, 10, 64); err == nil {
				m[kv.Key] = n
			}
		case v.DoubleValue != nil:
			m[kv.Key] = *v.DoubleValue
		}
	}
	return m
}
