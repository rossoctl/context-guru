package proxy

import (
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

const cfSample = `{"tools":[{"name":"a"}],"system":"sys",
 "messages":[{"role":"user","content":"hi"},
 {"role":"assistant","content":[{"type":"text","text":"yo","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`

func TestCfBodies_SectionsAndSizes(t *testing.T) {
	b := cfBodies([]byte(cfSample))
	if b.Start != [3]int{0, 1, 2} || len(b.Pos) != 4 {
		t.Fatalf("start = %v pos = %d, want [0 1 2] and 4", b.Start, len(b.Pos))
	}
	if int(b.Pos[2].Bytes) != len(`{"role":"user","content":"hi"}`) {
		t.Fatalf("message 0 bytes = %d", b.Pos[2].Bytes)
	}
}

func TestCfBodies_EqualInputEqualHashes(t *testing.T) {
	a, b := cfBodies([]byte(cfSample)), cfBodies([]byte(cfSample))
	for i := range a.Pos {
		if a.Pos[i] != b.Pos[i] {
			t.Fatalf("position %d differs", i)
		}
	}
}

func TestCfBodies_SystemArrayOneBlockEach(t *testing.T) {
	b := cfBodies([]byte(`{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"x"}]}`))
	if b.Start != [3]int{0, 0, 2} || len(b.Pos) != 3 {
		t.Fatalf("start = %v pos = %d", b.Start, len(b.Pos))
	}
}

func TestCfBodies_NoMessagesAndInvalidJSON(t *testing.T) {
	for _, in := range []string{``, `not json`, `{"model":"x"}`, `{"messages":`} {
		b := cfBodies([]byte(in))
		if len(b.Pos) != 0 {
			t.Fatalf("%q: pos = %d, want 0", in, len(b.Pos))
		}
		_ = cfBreakpoints([]byte(in))
	}
}

func TestCfBodies_OpenAIShapeLeavesToolsAndSystemEmpty(t *testing.T) {
	b := cfBodies([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`))
	if b.Start != [3]int{0, 0, 0} || len(b.Pos) != 2 {
		t.Fatalf("start = %v pos = %d", b.Start, len(b.Pos))
	}
}

func TestCfBreakpoints_FindsBlockLevelMarkAndTTL(t *testing.T) {
	got := cfBreakpoints([]byte(cfSample))
	want := []cf.Breakpoint{{Pos: cf.Position{Section: cf.SectionMessages, Index: 1}, TTL: "1h"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestCfBreakpoints_AllSectionsInBodyOrder(t *testing.T) {
	raw := `{"tools":[{"name":"a","cache_control":{"type":"ephemeral"}}],
	 "system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],
	 "messages":[{"role":"user","content":"x","cache_control":{"type":"ephemeral"}}]}`
	got := cfBreakpoints([]byte(raw))
	want := []cf.Breakpoint{
		{Pos: cf.Position{Section: cf.SectionTools, Index: 0}, TTL: "5m"},
		{Pos: cf.Position{Section: cf.SectionSystem, Index: 0}, TTL: "1h"},
		{Pos: cf.Position{Section: cf.SectionMessages, Index: 0}, TTL: "5m"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// S3: an element mark and a block mark in one message are one breakpoint, with the longer TTL,
// whichever order they come in.
func TestCfBreakpointsOnePerPositionLongerTTL(t *testing.T) {
	for _, raw := range []string{
		`{"messages":[{"role":"user","cache_control":{"type":"ephemeral"},"content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
		`{"messages":[{"role":"user","cache_control":{"type":"ephemeral","ttl":"1h"},"content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
	} {
		bps := cfBreakpoints([]byte(raw))
		if len(bps) != 1 || bps[0].TTL != "1h" || bps[0].Pos != (cf.Position{Section: cf.SectionMessages, Index: 0}) {
			t.Fatalf("%s: breakpoints = %+v, want one 1h at messages[0]", raw, bps)
		}
	}
}
