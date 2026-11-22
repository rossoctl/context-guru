package apply

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// #1279: the top-level cache_control of Anthropic's automatic caching is a mark for every reader.
const (
	root1hOnly = `{"model":"claude-sonnet-5-5","cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hi"}]}`
	root5mOnly = `{"model":"claude-sonnet-5-5","cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"hi"}]}`
	root5mMix  = `{"cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`
)

func TestRootCacheControlIsReadAsOneHour(t *testing.T) {
	b := []byte(root1hOnly)
	if !bodyAsksExtendedTTL(b) || !AllBreakpointsAskExtendedTTL(b) {
		t.Fatal("a root-only 1h body is not read as 1h")
	}
	if d, _ := CacheLifetime(bschemas.Anthropic, "claude-sonnet-5-5", b); d != extendedTTL {
		t.Errorf("CacheLifetime = %v, want %v", d, extendedTTL)
	}
	if got := ttlTier(bschemas.Anthropic, b); got != TTLEphemeral1h {
		t.Errorf("ttlTier = %q, want 1h", got)
	}
	if got := ttlTier(bschemas.Anthropic, []byte(root5mOnly)); got != TTLEphemeral5m {
		t.Errorf("root 5m ttlTier = %q, want 5m", got)
	}
}

func TestRootFiveMinuteMarkBesideOneHourBlockMarksIsFiveMinutes(t *testing.T) {
	b := []byte(root5mMix)
	if AllBreakpointsAskExtendedTTL(b) {
		t.Error("a root 5m mark next to a 1h block mark gave all-1h")
	}
	mixed := `{"cache_control":{"type":"ephemeral","ttl":"1h"},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`
	if AllBreakpointsAskExtendedTTL([]byte(mixed)) {
		t.Error("a root 1h mark next to a 5m block mark gave all-1h")
	}
}

func TestRootMarkCountsAgainstTheCap(t *testing.T) {
	b := []byte(`{"cache_control":{"type":"ephemeral"},` +
		`"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`)
	if n := CountBreakpoints(b).Total(); n != 4 {
		t.Fatalf("total = %d, want 4", n)
	}
	var st placerStats
	out, ok := placeBreakpoint(b, "messages.1.content.0", `{"type":"ephemeral"}`, "", &st)
	if ok || string(out) != string(b) {
		t.Error("the proxy added a fifth breakpoint")
	}
}

func TestRelabelAllTTL1hRelabelsTheRootMark(t *testing.T) {
	out, up := RelabelAllTTL1h([]byte(root5mOnly), bschemas.Anthropic)
	if !up {
		t.Fatal("a root-only body reported not upgraded")
	}
	if got := gjson.GetBytes(out, "cache_control.ttl").String(); got != "1h" {
		t.Errorf("root ttl = %q, want 1h", got)
	}
	if n := CountBreakpoints(out).Total(); n != 1 {
		t.Errorf("breakpoints = %d, want 1", n)
	}
	// A root mark next to block marks: all become 1h.
	out, _ = RelabelAllTTL1h([]byte(root5mMix), bschemas.Anthropic)
	if !AllBreakpointsAskExtendedTTL(out) {
		t.Error("a mark is left at 5m")
	}
	// No root mark: unchanged behavior.
	if _, up := RelabelAllTTL1h([]byte(`{"messages":[]}`), bschemas.Anthropic); up {
		t.Error("a body with no mark was upgraded")
	}
}
