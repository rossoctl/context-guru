package record

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testTurn(sess, span, parent string, at time.Time, input string) Turn {
	return Turn{
		SessionID: sess, SpanID: span, ParentID: parent, Model: "claude-opus-5", System: "anthropic",
		Start: at, End: at.Add(time.Second),
		RequestBody: input,
		ResponseBody: `[{"role":"assistant","finish_reason":"tool_use","parts":[` +
			`{"type":"tool_call","id":"toolu_1","name":"Bash","arguments":"{\"command\":\"ls\"}"}]}]`,
		FreshInput: 1, CacheRead: 2, CacheWrite: 3, Output: 4,
	}
}

// What the proxy exports, read back and converted, must equal what the proxy used to write
// directly: same trace id, same attributes, same messages. This is the guarantee that moving the
// corpus offline changes nothing for Inference-Perf or the analysis scripts.
func TestConvertGivesBackTheCorpusSpan(t *testing.T) {
	const sess = "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33"
	tr := testTurn(sess, "0123456789abcdef", "", time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC),
		`[{"role":"user","parts":[{"type":"text","content":"fix it"}]}]`)
	direct := tr.ToSpan()

	line, err := EncodeOTLPLine(Resource(""), tr.ToOTLP())
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadOTLP(bytes.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	sessions := GroupSessions(read)
	if len(sessions) != 1 || sessions[0].ID != sess {
		t.Fatalf("want one session %q, got %+v", sess, sessions)
	}
	got := CorpusSpan(sessions[0].ID, sessions[0].Spans[0])

	if got.TraceID != direct.TraceID || got.SpanID != direct.SpanID || got.Name != direct.Name ||
		got.StartTime != direct.StartTime || got.EndTime != direct.EndTime {
		t.Errorf("span envelope differs\n got: %+v\nwant: %+v", got, direct)
	}
	if got.ResourceAttr["service.name"] != "claude-code" {
		t.Errorf("corpus service.name = %v, want claude-code (what the corpus has always had)", got.ResourceAttr)
	}
	for k, want := range direct.Attributes {
		if g := got.Attributes[k]; g != want {
			t.Errorf("attribute %s = %#v, want %#v", k, g, want)
		}
	}
	for k := range got.Attributes {
		if _, ok := direct.Attributes[k]; !ok {
			t.Errorf("converted span has extra attribute %s (an OTLP-only key not dropped?)", k)
		}
	}
}

// A session split over two files (a proxy restart) is one session in the corpus, in time order.
func TestGroupSessionsMergesAndOrders(t *testing.T) {
	const sess = "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33"
	t0 := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
	later := testTurn(sess, "2222222222222222", "1111111111111111", t0.Add(time.Minute), "[]")
	earlier := testTurn(sess, "1111111111111111", "", t0, "[]")
	var buf bytes.Buffer
	for _, tr := range []Turn{later, earlier} { // written out of order, as two processes might
		line, err := EncodeOTLPLine(Resource(""), tr.ToOTLP())
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
	}
	read, err := ReadOTLP(&buf)
	if err != nil {
		t.Fatal(err)
	}
	s := GroupSessions(read)
	if len(s) != 1 || len(s[0].Spans) != 2 {
		t.Fatalf("want one session of 2 spans, got %+v", s)
	}
	if s[0].Spans[0].SpanID != "1111111111111111" {
		t.Errorf("spans not in time order: first is %s", s[0].Spans[0].SpanID)
	}
}

// Two tenants with the same session UUID are two sessions, never one.
func TestGroupSessionsKeepsTenantsApart(t *testing.T) {
	const sess = "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33"
	var buf bytes.Buffer
	for i, who := range []string{"alice", "bob"} {
		tr := testTurn(sess, []string{"1111111111111111", "2222222222222222"}[i], "", time.Now(), "[]")
		line, _ := EncodeOTLPLine(Resource(who), tr.ToOTLP())
		buf.Write(line)
	}
	read, err := ReadOTLP(&buf)
	if err != nil {
		t.Fatal(err)
	}
	s := GroupSessions(read)
	if len(s) != 2 || s[0].TenantID == s[1].TenantID {
		t.Fatalf("want two sessions, one per tenant, got %+v", s)
	}
}

// The converter must stop on a broken line rather than write a corpus file with spans quietly
// missing.
func TestReadOTLPRejectsABrokenLine(t *testing.T) {
	_, err := ReadOTLP(strings.NewReader(`{"resourceSpans":[]}` + "\n" + `{"resourceSpans":[` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("want an error naming line 2, got %v", err)
	}
}

// Spans from other services in a collector's file are skipped, not converted.
func TestGroupSessionsSkipsOtherSpans(t *testing.T) {
	read := []ReadSpan{{OTLPSpan: OTLPSpan{TraceID: "a", Name: "GET /", Attributes: map[string]any{}}}}
	if s := GroupSessions(read); len(s) != 0 {
		t.Errorf("a non-recording span became a session: %+v", s)
	}
}
