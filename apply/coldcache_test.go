package apply

import (
	"context"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
)

// The direction of error here is not symmetric, and the asymmetry is the whole design.
//
// Calling a WARM cache cold makes a component rewrite deep history, which invalidates a
// live prefix and forces a cache-WRITE of the whole suffix at 1.25x the fresh rate. Calling
// a COLD cache warm merely forgoes a saving. So every uncertain case must answer "warm".
func TestCacheIsCold(t *testing.T) {
	const now = int64(1_000_000_000)
	ttl := 5 * time.Minute

	for _, tc := range []struct {
		name   string
		prevAt int64
		want   bool
	}{
		{"no previous turn on record is UNKNOWN, not cold", 0, false},
		{"a negative/absent timestamp is unknown", -1, false},
		{"one second ago", now - 1000, false},
		{"just inside the TTL", now - ms(4*time.Minute), false},
		{"exactly at the TTL, inside the margin", now - ms(5*time.Minute), false},
		{"inside the margin", now - ms(5*time.Minute+30*time.Second), false},
		{"past the TTL and the margin", now - ms(6*time.Minute+1), true},
		{"an hour ago", now - ms(time.Hour), true},
		{"clock skew: previous turn in the future", now + 5000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheIsCold(tc.prevAt, now, ttl); got != tc.want {
				t.Fatalf("cacheIsCold(prev=%d, now=%d, ttl=%v) = %v, want %v",
					tc.prevAt, now, ttl, got, tc.want)
			}
		})
	}
}

// The TTL comes from the request where the request declares it, and from a documented outer
// bound where it does not.
func TestCacheTTLPerProvider(t *testing.T) {
	bare := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi",
		"cache_control":{"type":"ephemeral"}}]}]}`)
	hour := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi",
		"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	sysHour := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages":[{"role":"user","content":"hi"}]}`)

	for _, tc := range []struct {
		name     string
		provider bschemas.ModelProvider
		body     []byte
		want     time.Duration
	}{
		{"anthropic, bare ephemeral is the 5m tier", bschemas.Anthropic, bare, anthropicDefaultTTL},
		{"anthropic, explicit 1h", bschemas.Anthropic, hour, extendedTTL},
		{"anthropic, 1h on a system block", bschemas.Anthropic, sysHour, extendedTTL},
		{"bedrock follows the anthropic shape", bschemas.Bedrock, bare, anthropicDefaultTTL},
		{"vertex follows the anthropic shape", bschemas.Vertex, bare, anthropicDefaultTTL},
		{"openai declares no lifetime, so the outer bound applies", bschemas.OpenAI, bare, extendedTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheTTL(tc.provider, tc.body); got != tc.want {
				t.Fatalf("cacheTTL = %v, want %v", got, tc.want)
			}
		})
	}
}

// A tool output that merely CONTAINS the text "1h" must not extend our idea of the cache
// lifetime. Over-stating the TTL is the safe direction for coldness, but it is
// attacker-influenced content deciding a cost decision, so it must be structural — the same
// reason hasCacheBreakpoint does gjson paths instead of a substring scan.
func TestExtendedTTLDetectionIsStructural(t *testing.T) {
	body := []byte(`{"messages":[{"role":"tool","content":"cache_control ttl 1h expires in 1h"},
		{"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"ephemeral"}}]}]}`)
	if bodyAsksExtendedTTL(body) {
		t.Fatal("text inside a tool output was read as a cache_control ttl")
	}
	if got := cacheTTL(bschemas.Anthropic, body); got != anthropicDefaultTTL {
		t.Fatalf("cacheTTL = %v, want the 5m tier", got)
	}
}

// A BYPASSED turn must refresh the session's timestamp, and this goes through apply so it
// tests the real path rather than the Tracker's contract.
//
// A bypassed request is forwarded IN FULL, so the provider caches it exactly as a compacted
// turn's would be — it is activity on this session's prefix. The bypass branch used to call
// Turn(), which records the length but not the time, so a session that had been bypassing for
// ten minutes looked ten minutes IDLE and a cold sweep would rewrite a prefix the provider had
// just cached: the 1.25x suffix re-write the whole design exists to avoid.
//
// PROVEN TO FAIL WITHOUT THE FIX: with the bypass branch back on Turn(), prevAt is still the
// FIRST turn's timestamp (1700000000000 against a now of 1700000420000), so seven minutes read
// as idle when only one had passed.
func TestBypassedTurnStillRefreshesTheIdleClock(t *testing.T) {
	tr := modes.NewTracker(0)
	pipe, st := components.NewPipeline(nil, nil), store.NewMemory(store.Options{})
	body := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":"more"}]}`)
	base := time.Unix(1_700_000_000, 0)

	run := func(at time.Time, bypass bool) {
		BodyOpts(context.Background(), pipe, st, Opts{
			Provider: bschemas.Anthropic, Body: body, Session: "explicit-session",
			Bypass: bypass, Tracker: tr, Now: at,
		})
	}
	run(base, false)                   // a normal turn
	run(base.Add(6*time.Minute), true) // then a BYPASSED one, six minutes later

	// Reading the tracker the way apply does: what time did the bypassed turn leave behind?
	now := base.Add(7 * time.Minute)
	_, prevAt := tr.TurnAt(session.Scoped("", "explicit-session", "", ""), 2, now.UnixMilli())
	if prevAt == 0 {
		t.Fatal("the bypassed turn recorded no timestamp at all")
	}
	if cacheIsCold(prevAt, now.UnixMilli(), anthropicDefaultTTL) {
		t.Fatalf("the cache read COLD one minute after a bypassed turn (prevAt=%d, now=%d): "+
			"a live cached prefix would be rewritten", prevAt, now.UnixMilli())
	}
	if !cacheIsCold(prevAt, base.Add(20*time.Minute).UnixMilli(), anthropicDefaultTTL) {
		t.Fatal("a genuine 13-minute gap after the bypassed turn did not read cold")
	}
}

func ms(d time.Duration) int64 { return int64(d / time.Millisecond) }
