package proxy

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rossoctl/context-guru/internal/record"
)

// The exported span must carry the ids the turn already has, not ones the SDK made up: the
// trace id is the session's, and a turn's parent is the previous turn. Otherwise Phoenix shows
// one trace per call instead of one per session, and the converter cannot thread turns.
func TestRecExportKeepsTheTurnsIDsAndTimes(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	x := newRecExportWith(mem)
	start := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
	tr := record.Turn{
		SessionID: testSessionUUID, SpanID: "0123456789abcdef", ParentID: "fedcba9876543210",
		Model: "claude-opus-5", System: "anthropic", Start: start, End: start.Add(2 * time.Second),
		FreshInput: 1, CacheRead: 2, CacheWrite: 3, Output: 4,
	}
	if err := x.export("", tr.ToOTLP()); err != nil {
		t.Fatal(err)
	}
	// Flush, not Shutdown: the in-memory exporter forgets its spans when shut down.
	if err := x.flush(); err != nil {
		t.Fatal(err)
	}
	defer x.Shutdown()
	got := mem.GetSpans()
	if len(got) != 1 {
		t.Fatalf("want 1 exported span, got %d", len(got))
	}
	sp := got[0]
	if g, w := sp.SpanContext.TraceID().String(), record.TraceIDFor(testSessionUUID); g != w {
		t.Errorf("trace id = %s, want %s (the session's)", g, w)
	}
	if g := sp.SpanContext.SpanID().String(); g != tr.SpanID {
		t.Errorf("span id = %s, want %s", g, tr.SpanID)
	}
	if g := sp.Parent.SpanID().String(); g != tr.ParentID {
		t.Errorf("parent span id = %s, want %s", g, tr.ParentID)
	}
	if !sp.StartTime.Equal(tr.Start) || !sp.EndTime.Equal(tr.End) {
		t.Errorf("times = %v..%v, want the turn's %v..%v", sp.StartTime, sp.EndTime, tr.Start, tr.End)
	}
	if sp.Status.Code != codes.Ok {
		t.Errorf("status = %v, want Ok", sp.Status.Code)
	}
	res := map[string]string{}
	for _, kv := range sp.Resource.Attributes() {
		res[string(kv.Key)] = kv.Value.Emit()
	}
	if res["service.name"] != record.ServiceNameProxy {
		t.Errorf("service.name = %q, want %q: the proxy produces these spans, not Claude Code",
			res["service.name"], record.ServiceNameProxy)
	}
	if _, ok := res[record.ResTenant]; ok {
		t.Error("single-tenant span carries a tenant id")
	}
}

// Hosted mode: one collector receives several accounts, so each span says whose it is, as a
// resource attribute.
func TestRecExportPutsTheTenantOnTheResource(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	x := newRecExportWith(mem)
	for i, who := range []string{"alice", "bob"} {
		tr := record.Turn{SessionID: testSessionUUID, SpanID: []string{"1111111111111111", "2222222222222222"}[i],
			Model: "m", Start: time.Now(), End: time.Now()}
		if err := x.export(who, tr.ToOTLP()); err != nil {
			t.Fatal(err)
		}
	}
	// Flush, not Shutdown: the in-memory exporter forgets its spans when shut down.
	if err := x.flush(); err != nil {
		t.Fatal(err)
	}
	defer x.Shutdown()
	seen := map[string]bool{}
	for _, sp := range mem.GetSpans() {
		for _, kv := range sp.Resource.Attributes() {
			if string(kv.Key) == record.ResTenant {
				seen[kv.Value.AsString()] = true
			}
		}
	}
	if !seen["alice"] || !seen["bob"] {
		t.Errorf("tenant ids on exported resources = %v, want alice and bob", seen)
	}
}

func TestRecExportRejectsABadEndpoint(t *testing.T) {
	for _, bad := range []string{"localhost:4318", "not a url", ""} {
		if _, err := newRecExport(bad); err == nil {
			t.Errorf("newRecExport(%q) accepted a URL with no host", bad)
		}
	}
}
