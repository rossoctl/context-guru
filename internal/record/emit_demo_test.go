package record

import (
	"os"
	"testing"
	"time"
)

// TestWriteDemoFile is an escape hatch, not a unit test: with -run TestWriteDemoFile and
// CG_RECORD_DEMO_OUT set, it writes one realistic two-turn trace file so the EXISTING Python
// analysis pipeline -- claude-record's analyze_trace.py and the Eval team's traces-analyzer --
// can be pointed at it. Compatibility asserted in Go is a claim about my reading of the corpus;
// a file that their real scripts load and report on is evidence.
//
// It no-ops without the env var, so it costs nothing in a normal run.
//
// THE BODIES BELOW ARE RAW ANTHROPIC WIRE JSON, and that matters more than it looks. An earlier
// version of this file hand-wrote the *already-translated* corpus shape into RequestBody, which
// quietly made the demo prove nothing: it skipped the translator entirely, so the one file
// offered as end-to-end evidence exercised none of the code the evidence was about. Run through
// the Eval team's own message_type_counts analyzer, that version reported `part_types: {}` and a
// single role -- against `text: 4259, tool_call: 2295` and four roles for a real claude-record
// file. Exactly the silently-empty-column failure this package exists to avoid, reproduced in
// the demo rather than in the emitter.
//
// So the demo now feeds raw bodies through InputMessages / OutputMessages / ToolDefinitions,
// which is what the proxy will do. If those translations regress, this file's analyzer output
// goes empty and the regression is visible in their tooling, not just in ours.
func TestWriteDemoFile(t *testing.T) {
	out := os.Getenv("CG_RECORD_DEMO_OUT")
	if out == "" {
		t.Skip("set CG_RECORD_DEMO_OUT to write a demo trace file")
	}

	// Turn 1: a healthy turn that exercises every translation rule worth demonstrating --
	// a system block list, a tool_use, and the tool_result that has to become role "tool".
	// The planted key is a synthetic placeholder, and it is here to show redaction working.
	req1 := []byte(`{
	  "model": "claude-sonnet-5",
	  "max_tokens": 64000,
	  "output_config": {"effort": "high", "format": {"schema": {"type": "object"}}},
	  "system": [
	    {"type": "text", "text": "You are a helpful assistant."},
	    {"type": "text", "text": "Prefer short answers."}
	  ],
	  "tools": [
	    {"name": "Read", "description": "Read a file.",
	     "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}}
	  ],
	  "messages": [
	    {"role": "user", "content": "read config.toml -- my key is sk-ant-api03-SECRETSECRETSECRETSECRET-XX"},
	    {"role": "assistant", "content": [
	      {"type": "text", "text": "Reading it now."},
	      {"type": "tool_use", "id": "toolu_01", "name": "Read", "input": {"path": "config.toml"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_01", "content": "port = 8080"}
	    ]}
	  ]
	}`)
	resp1 := []byte(`{
	  "role": "assistant",
	  "stop_reason": "end_turn",
	  "content": [{"type": "text", "text": "The port is 8080."}],
	  "usage": {"input_tokens": 1200, "output_tokens": 350,
	            "cache_read_input_tokens": 18000, "cache_creation_input_tokens": 400}
	}`)

	// Turn 2: the unrecoverable path. A request was recorded, the response was not, and the
	// turn says so rather than presenting an empty answer as a real one.
	req2 := []byte(`{
	  "model": "claude-sonnet-5",
	  "max_tokens": 64000,
	  "messages": [{"role": "user", "content": "continue"}]
	}`)

	start := time.Date(2026, 9, 28, 14, 0, 0, 896000000, time.UTC)

	in1, err := InputMessages(req1)
	if err != nil {
		t.Fatalf("InputMessages(req1): %v", err)
	}
	inBody1, err := MarshalMessages(in1)
	if err != nil {
		t.Fatalf("MarshalMessages(in1): %v", err)
	}
	out1, err := OutputMessages(resp1)
	if err != nil {
		t.Fatalf("OutputMessages(resp1): %v", err)
	}
	outBody1, err := MarshalMessages(out1)
	if err != nil {
		t.Fatalf("MarshalMessages(out1): %v", err)
	}
	tools1, err := ToolDefinitions(req1)
	if err != nil {
		t.Fatalf("ToolDefinitions(req1): %v", err)
	}
	meta1, err := RequestMeta(req1)
	if err != nil {
		t.Fatalf("RequestMeta(req1): %v", err)
	}
	fresh1, cacheRead1, cacheWrite1, outTok1, stop1, err := UsageFromResponse(resp1)
	if err != nil {
		t.Fatalf("UsageFromResponse(resp1): %v", err)
	}

	in2, err := InputMessages(req2)
	if err != nil {
		t.Fatalf("InputMessages(req2): %v", err)
	}
	inBody2, err := MarshalMessages(in2)
	if err != nil {
		t.Fatalf("MarshalMessages(in2): %v", err)
	}
	meta2, err := RequestMeta(req2)
	if err != nil {
		t.Fatalf("RequestMeta(req2): %v", err)
	}

	turns := []Turn{
		{
			SessionID: "cg-demo-0001", SpanID: "aaaa000000000001",
			Model: meta1.Model, System: "anthropic",
			Start: start, End: start.Add(2 * time.Second),
			MaxTokens: meta1.MaxTokens, Effort: meta1.Effort, OutputTy: meta1.OutputType,
			RequestBody:  inBody1,
			ResponseBody: outBody1,
			ToolDefs:     tools1,
			FreshInput:   fresh1, CacheRead: cacheRead1,
			CacheWrite: cacheWrite1, Output: outTok1,
			StopReason: stop1, UpstreamMS: 1900, FirstByteMS: 380, Rounds: 1,
			ResponseSize: len(resp1),
		},
		{
			SessionID: "cg-demo-0001", SpanID: "aaaa000000000002", ParentID: "aaaa000000000001",
			Model: meta2.Model, System: "anthropic",
			Start: start.Add(3 * time.Second), End: start.Add(5 * time.Second),
			MaxTokens:   meta2.MaxTokens,
			RequestBody: inBody2,
			// No response body, plus a reason: the unrecoverable path, emitted honestly.
			Incomplete: IncompleteRetainCap,
			FreshInput: 800, CacheRead: 19500, Output: 120,
			UpstreamMS: 2100, Rounds: 2,
		},
	}

	spans := make([]Span, 0, len(turns))
	for i := range turns {
		turns[i].Redact(1 << 20)
		spans = append(spans, turns[i].ToSpan())
	}
	b, err := NewFile("cg-demo-0001", spans, start.Add(6*time.Second)).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote %d bytes to %s", len(b), out)
}
