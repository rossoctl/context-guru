package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The differential test against the REFERENCE IMPLEMENTATION.
//
// Every other test in this package checks that the emitter matches my reading of the corpus.
// This one checks it against claude-record's converter.py itself: the Python was executed over
// 294 real Anthropic bodies (183 requests, 111 responses) captured from actual sessions, its
// output recorded in testdata/reference_digests.json, and the Go port must reproduce it
// byte for byte.
//
// This is the test that earns the parity claim. A schema test can only say "the value is a
// JSON string where a JSON string belongs"; this says "given the same request, the two
// recorders write the same characters." It is also how six divergences were found and fixed --
// each of which would have produced a file that passed every schema check and still failed to
// compare:
//
//  1. A user message with list content splits into ONE MESSAGE PER BLOCK, and a tool_result
//     block becomes role "tool", not "user". (This is the origin of all 82,677 role=tool
//     messages in the corpus; Anthropic has no tool role.)
//  2. A system block LIST joins into one {role, content} string -- it does not become parts.
//  3. Tool definitions are RESHAPED to {type, name, description, parameters}, not forwarded
//     with Anthropic's input_schema key.
//  4. finish_reason falls back to the literal "stop" when a response has no stop_reason.
//  5. gen_ai.request.effort is read verbatim from output_config.effort. An earlier version
//     derived it from a thinking budget on low/medium/high tiers; the corpus contains only
//     "high" and "xhigh", so that would have invented values it has never held.
//  6. gen_ai.usage.input_tokens sums ALL THREE token components, matching
//     total_input_tokens(). An earlier comment here claimed the collector under-reported.
//
// THE FIXTURE STORES DIGESTS, NOT TEXT, and that is a deliberate constraint rather than an
// optimisation. The first version of it held converter.py's actual output: 51 MB containing
// 13,960 mentions of a developer's username and 17,697 of an internal project path, i.e. verbatim
// internal session content. This repo is public and Apache-2.0, and the whole point of the
// recording effort is to obtain clearance BEFORE such data moves anywhere -- so committing it
// here, even on a local branch, would have been the exact exposure the effort exists to avoid.
//
// A digest answers the only question this test asks -- "does the Go port produce the same bytes
// as converter.py?" -- so the payload was redundant to the test while being the whole risk. The
// fixture is now 134 KB and carries no session content (verified: zero hits for the username, the
// project path, the org name, and any key prefix). Role and part COUNTS are kept in the clear:
// they are the coverage guard's evidence, and a count is not content.
//
// The cost is that a failure reports two hashes rather than a readable diff. That is a real loss,
// mitigated by also printing the emitted value -- which is ours, not session data -- so the got
// side is still inspectable. To debug a mismatch against the want side, regenerate the full
// fixture locally with cmp-redact/ref_convert.py and diff it outside the repo.
//
// If the corpus format changes, regenerate with cmp-redact/ref_convert.py then
// cmp-redact/shrink_fixture.py rather than editing the fixture, and expect this test to be where
// the change shows up first.

type refCase struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`

	// Expected values are stored as DIGESTS, never as text -- see the fixture note above.
	InputMessagesSHA   string `json:"input_messages_sha"`
	ToolDefinitionsSHA string `json:"tool_definitions_sha"`
	OutputSchemaSHA    string `json:"output_schema_sha"`
	OutputMessagesSHA  string `json:"output_messages_sha"`

	// Scalars stay in the clear: tiny, non-sensitive, and far more useful readable.
	Model      string `json:"model"`
	MaxTokens  int    `json:"max_tokens"`
	Effort     string `json:"effort"`
	OutputType string `json:"output_type"`

	// Structure without content, so the coverage guard still has evidence to stand on.
	Roles map[string]int `json:"roles"`
	Parts map[string]int `json:"parts"`
}

// bodyDir is where the raw Anthropic bodies live. They are session data and stay outside this
// repo, so the test SKIPS rather than fails when they are absent -- a CG developer running
// `go test ./...` must not see a red suite over a corpus they do not have, and will not.
//
// The location comes only from the environment. An earlier version defaulted to a hardcoded
// absolute path, which would have written one developer's username and an internal project
// layout into a public repository for no benefit: anyone who has the bodies can set one variable.
func bodyDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("CG_RECORD_BODIES"); d != "" {
		return d
	}
	t.Skip("raw Anthropic bodies not available; set CG_RECORD_BODIES to run the differential test")
	return ""
}

func loadRefCases(t *testing.T) []refCase {
	t.Helper()
	b, err := os.ReadFile("testdata/reference_digests.json")
	if err != nil {
		t.Skipf("reference fixture absent: %v", err)
	}
	var cases []refCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatalf("parse reference fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("reference fixture is empty; it is not evidence of anything")
	}
	return cases
}

// digestJSON reduces a JSON document to the digest the fixture stores.
//
// The canonicalisation is the load-bearing part, and it is not cosmetic pedantry. Key order and
// whitespace differ between the Go and Python encoders, and -- less obviously -- the `arguments`
// and `response` fields hold JSON *as a string*, whose spelling differs while its meaning does
// not: Python defaults to ensure_ascii=True and writes \u2014 where Go writes the raw em dash,
// and Go escapes < as \u003c for HTML safety. Verified over all 294 reference cases:
// re-encoding changes no value, only its spelling. Comparing raw bytes would have reported 259
// of 294 as mismatched for a reason that means nothing -- and a test that cries wolf gets
// deleted rather than trusted.
func digestJSON(s string) string {
	if s == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "sha256:" + hashHex(s)
	}
	// SetEscapeHTML(false) is REQUIRED, not a preference. json.Marshal escapes <, > and & as
	// \u003c, \u003e and \u0026 for safe embedding in HTML; Python writes them raw. Measured
	// on a probe value: Go produced {"a":"x \u003c y \u0026 z"} where Python produced
	// {"a":"x < y & z"} -- identical values, different bytes, so every digest over a body
	// mentioning a tag or an ampersand would differ. Claude Code's own tool descriptions talk
	// about <system-reminder>, so that is most of the corpus, and it showed up as 34/183 rather
	// than as an obvious total failure.
	//
	// The encoder also appends a newline, which Encode does and Marshal does not; it is trimmed
	// rather than left to silently change every hash.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonValue(v)); err != nil {
		return "sha256:" + hashHex(s)
	}
	return "sha256:" + hashHex(strings.TrimRight(buf.String(), "\n"))
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// canonValue walks a decoded document, replacing any string that itself parses as a JSON object
// or array with its parsed form, so `arguments` compares structurally rather than by spelling.
// Only objects and arrays are unwrapped: treating every numeric-looking string as a number would
// erase the real difference between "1" and 1.
//
// It must stay in lockstep with canon() in cmp-redact/shrink_fixture.py, which computes the
// other side. If the two drift, every case fails at once -- which is the failure mode to want,
// since a silent partial drift would be far worse.
func canonValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = canonValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = canonValue(val)
		}
		return out
	case string:
		t := strings.TrimSpace(x)
		if len(t) > 1 && (t[0] == '{' || t[0] == '[') {
			var inner any
			if err := json.Unmarshal([]byte(t), &inner); err == nil {
				return canonValue(inner)
			}
		}
		return x
	default:
		return v
	}
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestInputMessagesMatchTheReferenceConverter is the big one: 183 real request bodies through
// both implementations.
func TestInputMessagesMatchTheReferenceConverter(t *testing.T) {
	dir := bodyDir(t)
	cases := loadRefCases(t)

	var checked, mismatched int
	for _, c := range cases {
		if c.Kind != "request" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, c.ID+".request.json"))
		if err != nil {
			continue // body pruned; the fixture outlives individual files
		}
		msgs, err := InputMessages(raw)
		if err != nil {
			t.Errorf("%s: InputMessages: %v", c.ID, err)
			continue
		}
		got, err := MarshalMessages(msgs)
		if err != nil {
			t.Errorf("%s: marshal: %v", c.ID, err)
			continue
		}
		checked++
		if digestJSON(got) != c.InputMessagesSHA {
			mismatched++
			if mismatched <= 3 { // a few concrete diffs beat 183 walls of JSON
				t.Errorf("%s: input messages differ from converter.py\n want digest: %s\n  got digest: %s\n  got: %s",
					c.ID, c.InputMessagesSHA, digestJSON(got), clipStr(got, 500))
			}
		}
	}
	if checked == 0 {
		t.Skip("no request bodies available to compare")
	}
	t.Logf("input messages: %d/%d request bodies match converter.py exactly", checked-mismatched, checked)
}

// TestOutputMessagesMatchTheReferenceConverter: 111 real response bodies.
func TestOutputMessagesMatchTheReferenceConverter(t *testing.T) {
	dir := bodyDir(t)
	cases := loadRefCases(t)

	var checked, mismatched int
	for _, c := range cases {
		if c.Kind != "response" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, c.ID+".response.json"))
		if err != nil {
			continue
		}
		msgs, err := OutputMessages(raw)
		if err != nil {
			t.Errorf("%s: OutputMessages: %v", c.ID, err)
			continue
		}
		got, err := MarshalMessages(msgs)
		if err != nil {
			t.Errorf("%s: marshal: %v", c.ID, err)
			continue
		}
		checked++
		if digestJSON(got) != c.OutputMessagesSHA {
			mismatched++
			if mismatched <= 3 {
				t.Errorf("%s: output messages differ from converter.py\n want digest: %s\n  got digest: %s\n  got: %s",
					c.ID, c.OutputMessagesSHA, digestJSON(got), clipStr(got, 500))
			}
		}
	}
	if checked == 0 {
		t.Skip("no response bodies available to compare")
	}
	t.Logf("output messages: %d/%d response bodies match converter.py exactly", checked-mismatched, checked)
}

// TestToolDefinitionsMatchTheReferenceConverter pins the reshaping. This is the divergence
// most likely to be reintroduced by someone "simplifying" the code to forward req.tools
// unchanged, which would move the schema from `parameters` to `input_schema`.
func TestToolDefinitionsMatchTheReferenceConverter(t *testing.T) {
	dir := bodyDir(t)
	cases := loadRefCases(t)

	var checked, withTools, mismatched int
	for _, c := range cases {
		if c.Kind != "request" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, c.ID+".request.json"))
		if err != nil {
			continue
		}
		got, err := ToolDefinitions(raw)
		if err != nil {
			t.Errorf("%s: ToolDefinitions: %v", c.ID, err)
			continue
		}
		checked++
		if c.ToolDefinitionsSHA != "" {
			withTools++
		}
		if c.ToolDefinitionsSHA == "" && got == "" {
			continue
		}
		if digestJSON(got) != c.ToolDefinitionsSHA {
			mismatched++
			if mismatched <= 2 {
				t.Errorf("%s: tool definitions differ\n want digest: %s\n  got digest: %s\n  got: %s",
					c.ID, c.ToolDefinitionsSHA, digestJSON(got), clipStr(got, 400))
			}
		}
	}
	if checked == 0 {
		t.Skip("no request bodies available")
	}
	t.Logf("tool definitions: %d/%d match (%d bodies carried tools)", checked-mismatched, checked, withTools)
}

// TestRequestMetaMatchesTheReferenceConverter covers the scalar attributes, including the
// effort field that an earlier version of RequestMeta derived instead of reading.
func TestRequestMetaMatchesTheReferenceConverter(t *testing.T) {
	dir := bodyDir(t)
	cases := loadRefCases(t)

	var checked int
	seenEffort := map[string]int{}
	for _, c := range cases {
		if c.Kind != "request" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, c.ID+".request.json"))
		if err != nil {
			continue
		}
		meta, err := RequestMeta(raw)
		if err != nil {
			t.Errorf("%s: RequestMeta: %v", c.ID, err)
			continue
		}
		checked++
		if meta.Model != c.Model {
			t.Errorf("%s: model = %q, reference %q", c.ID, meta.Model, c.Model)
		}
		if meta.MaxTokens != c.MaxTokens {
			t.Errorf("%s: max_tokens = %d, reference %d", c.ID, meta.MaxTokens, c.MaxTokens)
		}
		if meta.Effort != c.Effort {
			t.Errorf("%s: effort = %q, reference %q", c.ID, meta.Effort, c.Effort)
		}
		if meta.OutputType != c.OutputType {
			t.Errorf("%s: output.type = %q, reference %q", c.ID, meta.OutputType, c.OutputType)
		}
		if c.OutputSchemaSHA == "" {
			if meta.OutputSchema != "" {
				t.Errorf("%s: emitted an output schema the reference does not have", c.ID)
			}
		} else if digestJSON(meta.OutputSchema) != c.OutputSchemaSHA {
			t.Errorf("%s: output schema differs\n want digest: %s\n  got: %s",
				c.ID, c.OutputSchemaSHA, clipStr(meta.OutputSchema, 300))
		}
		seenEffort[meta.Effort]++
	}
	if checked == 0 {
		t.Skip("no request bodies available")
	}
	t.Logf("request meta: %d bodies checked; effort values seen: %v", checked, seenEffort)
}

// TestTheReferenceCorpusExercisesTheInterestingPaths guards against a false pass. A
// differential test over bodies that happen to contain no tool calls and no tool results would
// agree perfectly while testing none of the translation rules that actually differ, so the
// fixture's own coverage is asserted rather than assumed.
func TestTheReferenceCorpusExercisesTheInterestingPaths(t *testing.T) {
	cases := loadRefCases(t)

	roles, parts := map[string]int{}, map[string]int{}
	var reqs, resps int
	for _, c := range cases {
		switch c.Kind {
		case "request":
			reqs++
		case "response":
			resps++
		}
		for r, n := range c.Roles {
			roles[r] += n
		}
		for p, n := range c.Parts {
			parts[p] += n
		}
	}
	t.Logf("fixture: %d requests, %d responses; roles %v; parts %v", reqs, resps, roles, parts)

	// The synthesised "tool" role and the tool_call_response part are the two rules most likely
	// to be got wrong, so their presence is what makes the comparison meaningful.
	if roles["tool"] == 0 {
		t.Error("no role=tool messages: the tool_result -> role:tool rule is untested")
	}
	if roles["system"] == 0 {
		t.Error("no system messages: the block-list join rule is untested")
	}
	if parts[PartToolCall] == 0 {
		t.Error("no tool_call parts: the tool_use rename is untested")
	}
	if parts[PartToolResponse] == 0 {
		t.Error("no tool_call_response parts: the tool_result rename is untested")
	}
	if parts[PartText] == 0 {
		t.Error("no text parts")
	}
}
