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

// frozen_tokens is what cache safety cost us, so it must read the same gate the
// components read — including the cold lift. Before this, `attemptedTokens` called
// TailOnly and reported a cold turn's whole prefix as frozen even though every
// deterministic offloader was free to rewrite it: 742 production `ttl_expiry` requests
// reported 38.4M frozen tokens (90.8% of their context) for a cache that had expired.
func TestAttemptedTokensCountsTheWholePrefixOnAColdTurn(t *testing.T) {
	tmsg := func(text string) bschemas.ChatMessage {
		t := text
		return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
			Content: &bschemas.ChatMessageContent{ContentStr: &t}}
	}
	msgs := []bschemas.ChatMessage{tmsg("first tool output here"), tmsg("second tool output here"),
		tmsg("third tool output here")}
	warm := &components.Ctx{CacheAware: true, MaxCachedIdx: 1}
	cold := &components.Ctx{CacheAware: true, MaxCachedIdx: 1, ColdCache: true}

	gotWarm, gotCold := attemptedTokens(msgs, warm), attemptedTokens(msgs, cold)
	all := attemptedTokens(msgs, &components.Ctx{})
	if gotWarm >= all {
		t.Fatalf("warm attempted=%d must be less than the whole transcript (%d) — the tail gate is the point", gotWarm, all)
	}
	if gotCold != all {
		t.Fatalf("cold attempted=%d, want the whole transcript %d: nothing is frozen for cache safety on a turn whose cache has expired", gotCold, all)
	}
}

// THE -$708 CASE, named so nobody has to rediscover it.
//
// proxy/promexport.go records a measured verdict on acting when frozen_tokens reads zero:
// 3,092 requests whose OWN prefix tracker had been reset (a restart, an evicted entry)
// still cache-HIT for 404,376,878 cache-read tokens, and treating that reading as "the
// cache is cold, deep history is safe to rewrite" was worth about -$708 on sonnet-5
// against +$0.62 of upside.
//
// The cold sweep is a different claim, and this is where the difference lives: a reset
// tracker has no previous turn on record, cacheIsCold answers FALSE for it, and the sweep
// cannot fire. The -$708 outcome needs a WARM cache misread as cold; ColdCache only reads
// true when a recorded previous turn is older than the provider TTL plus a margin.
//
// Every row here is a way the tracker can come back empty or wrong. All must answer warm.
func TestColdSweepCannotFireOnTheMinus708Case(t *testing.T) {
	const now = int64(1_000_000_000)
	ttl := 5 * time.Minute
	for _, tc := range []struct {
		name   string
		prevAt int64
	}{
		{"process restart: no previous turn on record", 0},
		{"evicted tracker entry: same, reported as absent", 0},
		{"a corrupt/negative timestamp", -1},
		{"a first turn recorded in the future (clock skew)", now + 60_000},
		{"a long-lived session whose provider cache is warm", now - ms(2*time.Minute)},
		{"idle past the TTL but inside the safety margin", now - ms(5*time.Minute+30*time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cacheIsCold(tc.prevAt, now, ttl) {
				t.Fatal("read COLD; this is the reading that cost -$708 — a warm provider " +
					"cache rewritten at depth forces a cache-write of the whole suffix at 1.25x")
			}
			// And the accounting must agree: a warm turn still reports its prefix frozen,
			// so the dashboard cannot be read as "we swept this".
			c := &components.Ctx{CacheAware: true, MaxCachedIdx: 1,
				ColdCache: cacheIsCold(tc.prevAt, now, ttl)}
			msgs := []bschemas.ChatMessage{tmsgT("one output here"), tmsgT("two output here"),
				tmsgT("three output here")}
			if attemptedTokens(msgs, c) == attemptedTokens(msgs, &components.Ctx{}) {
				t.Fatal("a warm turn reported its whole transcript as attempted")
			}
		})
	}
}

func tmsgT(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		Content: &bschemas.ChatMessageContent{ContentStr: &t}}
}

// The two false-cold paths docs/design.md warned about, both of which produce a POPULATED,
// PLAUSIBLE, STALE prevAt — the one shape TestColdSweepCannotFireOnTheMinus708Case does not
// reach, since all six of its rows are variants of "the tracker came back empty or too
// recent". Both are the expensive direction: a live prefix rewritten at depth costs a
// cache-write of the whole suffix at 1.25x fresh.

// Path 1: the TTL is read out of THIS request, so a session that asked ttl:"1h" once and
// sends a bare ephemeral mark afterwards would be judged cold at six minutes.
func TestSessionTTLRemembersTheLongestLifetimeAsked(t *testing.T) {
	st := store.NewMemory(store.Options{})
	// Turn 1 asks for the extended tier.
	if got := sessionTTL(st, "s", time.Hour); got != time.Hour {
		t.Fatalf("turn 1 = %v, want 1h", got)
	}
	// Turn 2 sends a bare ephemeral mark. The prefix may still be held for an hour.
	if got := sessionTTL(st, "s", 5*time.Minute); got != time.Hour {
		t.Fatalf("turn 2 = %v, want 1h — this is the false-cold read", got)
	}
	// And a 20-minute idle gap must therefore NOT read cold.
	const now = int64(1_000_000_000)
	if cacheIsCold(now-ms(20*time.Minute), now, sessionTTL(st, "s", 5*time.Minute)) {
		t.Fatal("read COLD on a prefix the provider may hold for an hour")
	}
	// A session that never asked for it keeps the short TTL, so the sweep still works.
	if got := sessionTTL(st, "other", 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("unrelated session = %v, want 5m", got)
	}
	if !cacheIsCold(now-ms(20*time.Minute), now, sessionTTL(st, "other", 5*time.Minute)) {
		t.Fatal("a genuinely cold 5m session stopped being swept")
	}
}

// Path 2: one conversation reaching us under two session ids. An explicit header present on
// some turns and absent on others resolves to the header on one turn and to
// sha256(system+firstUser) on the next, so turns under id B keep the provider's
// content-keyed entry warm while id A's clock ages past the TTL.
func TestAliasSessionClockKeepsAWarmPrefixWarm(t *testing.T) {
	st := store.NewMemory(store.Options{})
	tr := modes.NewTracker(0)
	const sys, firstUser = "you are a helpful agent", "please fix the failing test"
	explicit := session.Scoped("t", "client-supplied-id", sys, firstUser)
	alias := session.Scoped("t", "", sys, firstUser)
	if explicit == alias {
		t.Fatal("fixture broken: the two ids must differ for this to be the aliased case")
	}
	base := int64(1_700_000_000_000)
	// Turn 1 under the explicit id.
	tr.TurnAt(explicit, 10, base)
	aliasSeen(st, alias, base)
	// Turn 2, three minutes later, under the ALIAS — this is what keeps the provider warm.
	aliasSeen(st, alias, base+ms(3*time.Minute))
	// Turn 3, seven minutes after turn 1, back under the explicit id. Its own clock says
	// seven minutes idle, which is past 5m + the margin: the old code read COLD here.
	now := base + ms(7*time.Minute)
	_, prevAt := tr.TurnAt(explicit, 14, now)
	if !cacheIsCold(prevAt, now, 5*time.Minute) {
		t.Fatal("fixture broken: the un-aliased clock must read cold, or this proves nothing")
	}
	// With the alias's clock folded in, the later of the two wins and the turn reads WARM.
	aliasAt := aliasSeen(st, alias, now)
	if aliasAt > prevAt {
		prevAt = aliasAt
	}
	if cacheIsCold(prevAt, now, 5*time.Minute) {
		t.Fatal("read COLD while another id had touched the same prefix 4 minutes ago")
	}
}

// Path 2 through the REAL call site, which is where the previous version of this guard was
// wrong: TestAliasSessionClockKeepsAWarmPrefixWarm calls aliasSeen directly and so never
// exercised the `alias != sessionID` condition that skipped it on header-less turns.
//
// Four turns over one stable prefix, the session header present on turns 2 and 3 only — the
// shape three of thirteen production tenants actually send. Turn 4's own tracker id was last
// touched at turn 1, seven minutes earlier, so its private clock says COLD; the provider's
// content-keyed entry was refreshed 60 seconds ago by turn 3.
func TestAliasClockIsReadOnHeaderlessTurnsToo(t *testing.T) {
	var cold []bool
	pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	body := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the failing test","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":"more"}]}`)
	base := time.Unix(1_700_000_000, 0)

	for _, turn := range []struct {
		at     time.Time
		header string
	}{
		{base, ""},                            // header-less
		{base.Add(3 * time.Minute), "cc-abc"}, // header
		{base.Add(6 * time.Minute), "cc-abc"}, // header — refreshes the provider's entry
		{base.Add(7 * time.Minute), ""},       // header-less again: 7m on its OWN clock
	} {
		BodyOpts(context.Background(), pipe, st, Opts{
			Provider: bschemas.Anthropic, Body: body, Session: turn.header,
			Tracker: tr, Now: turn.at,
		})
	}
	if len(cold) != 4 {
		t.Fatalf("the pipeline ran %d times, want 4", len(cold))
	}
	if cold[3] {
		t.Fatalf("turn 4 read COLD while the alias clock held a timestamp 60 seconds old "+
			"(ColdCache per turn: %v) — a live prefix rewritten at depth costs a cache-write "+
			"of the whole suffix at 1.25x fresh", cold)
	}
	// And the guard must not have jammed the sweep permanently warm: a genuine long gap on
	// every identity still reads cold.
	cold = nil
	BodyOpts(context.Background(), pipe, st, Opts{
		Provider: bschemas.Anthropic, Body: body, Tracker: tr,
		Now: base.Add(40 * time.Minute),
	})
	if len(cold) != 1 || !cold[0] {
		t.Fatalf("a genuine 33-minute gap stopped reading cold (%v)", cold)
	}
}

// coldSpy records Ctx.ColdCache as the real pipeline hands it over, so the test observes the
// decision apply actually made rather than re-deriving it.
type coldSpy struct{ seen *[]bool }

func (coldSpy) Name() string                 { return "coldspy" }
func (coldSpy) Enabled(*components.Ctx) bool { return true }
func (s coldSpy) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.seen = append(*s.seen, c.ColdCache)
	return nil
}

// The two guards have to COMPOSE, not just each close its own path. The clock guard is
// alias-aware; a TTL record keyed under one id only is invisible to a turn arriving under the
// other, so it falls back to the 5m tier and can declare cold on a prefix held for an hour —
// path (a) reached through the door path (b) opens. This was also masked before the clock fix:
// such a turn used to have prevAt == 0 and decline itself under the "no record reads warm"
// rule, so the TTL comparison was never reached at all.
//
// Every direction the flip can happen in, each ending on a turn whose own id has never seen
// the 1h mark. The last two are the ones a single key cannot cover from either side.
func TestSessionTTLComposesAcrossBothIDs(t *testing.T) {
	body1h := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the failing test",` +
		`"cache_control":{"type":"ephemeral","ttl":"1h"}}]},{"role":"user","content":"more"}]}`)
	bodyBare := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the failing test",` +
		`"cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"more"}]}`)
	// Post-compaction: the agent replaced its transcript head with a summary, so
	// sha256(system+firstUser) — and therefore the alias — is a different id entirely.
	bodyCompacted := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"[summary] we were fixing a test",` +
		`"cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"continue"}]}`)
	base := time.Unix(1_700_000_000, 0)

	for _, tc := range []struct {
		name  string
		turns []struct {
			at     time.Time
			header string
			body   []byte
		}
	}{
		{"the 1h grant was recorded under the explicit id", []struct {
			at     time.Time
			header string
			body   []byte
		}{
			{base, "cc-abc", body1h},
			{base.Add(5 * time.Minute), "cc-abc", body1h},
			{base.Add(20 * time.Minute), "", bodyBare},
		}},
		{"and with the header arriving mid-conversation", []struct {
			at     time.Time
			header string
			body   []byte
		}{
			{base, "", bodyBare},
			{base.Add(2 * time.Minute), "cc-abc", body1h},
			{base.Add(30 * time.Minute), "", bodyBare},
		}},
		// The reverse: recorded under the ALIAS, then arriving under the explicit id. An
		// alias-only key cannot serve this one, which is why the pair is read rather than
		// either id being picked as canonical.
		{"recorded header-less, then the header appears", []struct {
			at     time.Time
			header string
			body   []byte
		}{
			{base, "", body1h},
			{base.Add(20 * time.Minute), "cc-abc", bodyBare},
		}},
		{"and back and forth across both ids", []struct {
			at     time.Time
			header string
			body   []byte
		}{
			{base, "cc-abc", body1h},
			{base.Add(2 * time.Minute), "", bodyBare},
			{base.Add(25 * time.Minute), "cc-abc", bodyBare},
		}},
		// The case the ALIAS alone cannot serve, and the reason both keys are read rather than
		// the content hash being picked as canonical: the agent compacts its own context, so
		// the transcript head changes and sha256(system+firstUser) moves with it. An explicit
		// id survives that (see metaSessionKeys); the derived one does not.
		{"the agent compacts its context, moving the alias", []struct {
			at     time.Time
			header string
			body   []byte
		}{
			{base, "cc-abc", body1h},
			{base.Add(20 * time.Minute), "cc-abc", bodyCompacted},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cold []bool
			pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
			st, tr := store.NewMemory(store.Options{PinPrefixes: store.DefaultPinPrefixes}), modes.NewTracker(0)
			for _, turn := range tc.turns {
				BodyOpts(context.Background(), pipe, st, Opts{
					Provider: bschemas.Anthropic, Body: turn.body, Session: turn.header,
					Tracker: tr, Now: turn.at,
				})
			}
			if last := len(cold) - 1; last < 0 || cold[last] {
				t.Fatalf("the final turn read COLD on a prefix the provider may hold for an "+
					"hour (ColdCache per turn: %v) — the 1h grant has to be keyed the way the "+
					"provider keys its cache, on content", cold)
			}
		})
	}
}

// And the guards must not have jammed the sweep warm: a prefix that only ever asked for the
// 5m tier still reads cold after a real gap, whichever id the turns arrive under.
func TestColdSweepStillFiresOnAFiveMinuteTierPrefix(t *testing.T) {
	var cold []bool
	pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
	st, tr := store.NewMemory(store.Options{PinPrefixes: store.DefaultPinPrefixes}), modes.NewTracker(0)
	body := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the failing test",` +
		`"cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"more"}]}`)
	base := time.Unix(1_700_000_000, 0)
	for _, turn := range []struct {
		at     time.Time
		header string
	}{{base, "cc-abc"}, {base.Add(9 * time.Minute), ""}} {
		BodyOpts(context.Background(), pipe, st, Opts{
			Provider: bschemas.Anthropic, Body: body, Session: turn.header,
			Tracker: tr, Now: turn.at,
		})
	}
	if len(cold) != 2 || !cold[1] {
		t.Fatalf("a genuine 9-minute gap on the 5m tier stopped reading cold (%v); the guards "+
			"are only allowed to move the estimate toward warm, not to disable the sweep", cold)
	}
}
