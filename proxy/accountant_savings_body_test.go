package proxy

import (
	"testing"

	"github.com/rossoctl/accountant"
)

const accSample = `{"tools":[{"name":"a"}],"system":"sys",
 "messages":[{"role":"user","content":"hi"},
 {"role":"assistant","content":[{"type":"text","text":"yo","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`

func TestCfBodies_SectionsAndSizes(t *testing.T) {
	b := accBodies([]byte(accSample))
	if b.Start != [3]int{0, 1, 2} || len(b.Pos) != 4 {
		t.Fatalf("start = %v pos = %d, want [0 1 2] and 4", b.Start, len(b.Pos))
	}
	if int(b.Pos[2].Bytes) != len(`{"role":"user","content":"hi"}`) {
		t.Fatalf("message 0 bytes = %d", b.Pos[2].Bytes)
	}
}

func TestCfBodies_EqualInputEqualHashes(t *testing.T) {
	a, b := accBodies([]byte(accSample)), accBodies([]byte(accSample))
	for i := range a.Pos {
		if a.Pos[i] != b.Pos[i] {
			t.Fatalf("position %d differs", i)
		}
	}
}

func TestCfBodies_SystemArrayOneBlockEach(t *testing.T) {
	b := accBodies([]byte(`{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"x"}]}`))
	if b.Start != [3]int{0, 0, 2} || len(b.Pos) != 3 {
		t.Fatalf("start = %v pos = %d", b.Start, len(b.Pos))
	}
}

func TestCfBodies_NoMessagesAndInvalidJSON(t *testing.T) {
	for _, in := range []string{``, `not json`, `{"model":"x"}`, `{"messages":`} {
		b := accBodies([]byte(in))
		if len(b.Pos) != 0 {
			t.Fatalf("%q: pos = %d, want 0", in, len(b.Pos))
		}
		_ = accBreakpoints([]byte(in))
	}
}

func TestCfBodies_OpenAIShapeLeavesToolsAndSystemEmpty(t *testing.T) {
	b := accBodies([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`))
	if b.Start != [3]int{0, 0, 0} || len(b.Pos) != 2 {
		t.Fatalf("start = %v pos = %d", b.Start, len(b.Pos))
	}
}

func TestCfBreakpoints_FindsBlockLevelMarkAndTTL(t *testing.T) {
	got := accBreakpoints([]byte(accSample))
	want := []accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionMessages, Index: 1}, TTL: "1h"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestCfBreakpoints_AllSectionsInBodyOrder(t *testing.T) {
	raw := `{"tools":[{"name":"a","cache_control":{"type":"ephemeral"}}],
	 "system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],
	 "messages":[{"role":"user","content":"x","cache_control":{"type":"ephemeral"}}]}`
	got := accBreakpoints([]byte(raw))
	want := []accountant.Breakpoint{
		{Pos: accountant.Position{Section: accountant.SectionTools, Index: 0}, TTL: "5m"},
		{Pos: accountant.Position{Section: accountant.SectionSystem, Index: 0}, TTL: "1h"},
		{Pos: accountant.Position{Section: accountant.SectionMessages, Index: 0}, TTL: "5m"},
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
		bps := accBreakpoints([]byte(raw))
		if len(bps) != 1 || bps[0].TTL != "1h" || bps[0].Pos != (accountant.Position{Section: accountant.SectionMessages, Index: 0}) {
			t.Fatalf("%s: breakpoints = %+v, want one 1h at messages[0]", raw, bps)
		}
	}
}

// The client moves its cache mark: turn 2 has the mark on message 1 and none on message 0, turn 3 moves it to
// message 2. The text of messages 0 and 1 did not change, so their hashes must not change. If the mark were
// in the hash, message 1 would look edited on turn 3 and the library would find no common prefix after the
// system block (accountant#21, context-guru#768 B1).
func TestAccBodies_AMovedCacheMarkDoesNotChangeTheHash(t *testing.T) {
	const mark = `,"cache_control":{"type":"ephemeral"}`
	turn := func(m0, m1, m2 string) []byte {
		return []byte(`{"system":"s","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"a"` + m0 + `}]},` +
			`{"role":"assistant","content":[{"type":"text","text":"b"` + m1 + `}]},` +
			`{"role":"user","content":[{"type":"text","text":"c"` + m2 + `}]}]}`)
	}
	t2, t3 := accBodies(turn("", mark, "")), accBodies(turn("", "", mark))
	for i := 0; i < 3; i++ { // system, message 0, message 1: the same text on both turns
		if t2.Pos[i].Hash != t3.Pos[i].Hash {
			t.Errorf("position %d hashes differ only by a moved cache mark", i)
		}
	}
	// The size is the wire size: the mark is bytes the provider received.
	if with, without := t2.Pos[2].Bytes, t3.Pos[2].Bytes; with <= without || int(with-without) != len(mark) {
		t.Errorf("message 1 is %d bytes with the mark and %d without, want a difference of %d", with, without, len(mark))
	}
}

// A mark on the element itself (not on a content block) is left out of the hash too.
func TestAccBodies_AnElementMarkIsLeftOutOfTheHash(t *testing.T) {
	a := accBodies([]byte(`{"messages":[{"role":"user","content":"x","cache_control":{"type":"ephemeral"}}]}`))
	b := accBodies([]byte(`{"messages":[{"role":"user","content":"x"}]}`))
	if a.Pos[0].Hash != b.Pos[0].Hash {
		t.Fatal("an element mark changed the hash")
	}
	if a.Pos[0].Bytes <= b.Pos[0].Bytes {
		t.Fatal("the marked element should be larger on the wire")
	}
}

// A different text still changes the hash: leaving out the mark must not hide a real edit.
func TestAccBodies_ARealEditStillChangesTheHash(t *testing.T) {
	a := accBodies([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`))
	b := accBodies([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]}]}`))
	if a.Pos[0].Hash == b.Pos[0].Hash {
		t.Fatal("an edited text kept the same hash")
	}
}
