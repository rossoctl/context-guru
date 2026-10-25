package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/store"
)

// #715. An OpenAI Responses cache lives 30 minutes and the keeper pings it at 28. The substitute's
// phase check used to treat every cache off the one-hour tier as a 5-minute cache, so at 28 minutes
// it read Cold, and a pre_expiry summary never took an OpenAI ping. At 28 of 30 minutes, with a
// 3-minute pre-expiry window, the phase is PreExpiry.
func TestAnOpenAIThreadAtTwentyEightMinutesIsNotCold(t *testing.T) {
	prev := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	at := prev.Add(modelinfo.OpenAIDefaultKeepAliveIdle)
	got := cachePhaseAt(prev, at, cacheLifetime(bschemas.OpenAI, false), 3*time.Minute)
	if got != components.CachePhasePreExpiry {
		t.Fatalf("an OpenAI cache 28 min into its 30-min life reads %v, want PreExpiry", got)
	}
	// The retry guard and the phase check use one rule (#715). cacheTTL calls cacheLifetime today, so
	// this cannot fail now; it pins the two against drifting apart again.
	e := &kaEntry{provider: bschemas.OpenAI}
	if e.cacheTTL() != cacheLifetime(bschemas.OpenAI, false) {
		t.Fatal("cacheTTL and cacheLifetime disagree on OpenAI")
	}
}

// The same, end to end, with the DEFAULT pre-expiry window (pre_expiry_seconds unset): a
// pre_expiry summary candidate on an OpenAI thread takes the ping at 28 minutes. The cache then has
// 2 minutes left, more than the 60 s default, so without the keeper's own slack in the window the
// phase read Warm and the substitute was still refused (#717 review).
func TestAPreExpirySubstituteTakesAnOpenAIPing(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>fires inside the OpenAI window</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 0)

	pol := kaPolicy()
	pol.Idle = modelinfo.OpenAIDefaultKeepAliveIdle
	body := `{"model":"azure/gpt-5.6-luna","max_output_tokens":2048,"input":[{"role":"user","content":"hi"}]}`
	for _, at := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
		k.Record(Record{Key: sess1, StartedAt: at, Body: []byte(body), Provider: bschemas.OpenAI,
			Route: "/v1/responses", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(sess1)})
	}
	k.Sweep(clock.advance(modelinfo.OpenAIDefaultKeepAliveIdle + time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if n := k.summarySubstitutePhaseMismatch.Load(); n != 0 {
		t.Fatalf("the substitute was refused as a phase mismatch %d time(s): at 28 of 30 min, either "+
			"the cache was read as a 5-min one or the default window did not cover the ping", n)
	}
	if k.summarySubstituted.Load() != 1 || fs.n() != 0 {
		t.Fatalf("substituted %d, ordinary pings %d: want the summary to take the OpenAI ping",
			k.summarySubstituted.Load(), fs.n())
	}
}

// The default window follows the keeper's own slack; an operator's value is kept as set. On
// Anthropic at the defaults nothing changes: 300 s - 270 s is inside the 60 s default.
func TestTheSubstituteWindowCoversTheKeepersOwnSlack(t *testing.T) {
	anthropic := substituteWindow(0, cacheLifetime(bschemas.Anthropic, false), 270*time.Second)
	if anthropic != components.DefaultPreExpiry {
		t.Errorf("Anthropic default window = %v, want the unchanged %v", anthropic, components.DefaultPreExpiry)
	}
	openai := substituteWindow(0, cacheLifetime(bschemas.OpenAI, false), modelinfo.OpenAIDefaultKeepAliveIdle)
	if openai < 2*time.Minute {
		t.Errorf("OpenAI default window = %v, want at least the keeper's 2-minute slack", openai)
	}
	if got := substituteWindow(30, cacheLifetime(bschemas.OpenAI, false), modelinfo.OpenAIDefaultKeepAliveIdle); got != 30*time.Second {
		t.Errorf("an operator's pre_expiry_seconds 30 became %v, want it kept as set", got)
	}
}

// The Anthropic end to end, with the default window, so the change is shown not to move it.
func TestAPreExpirySubstituteStillTakesAnAnthropicPingWithTheDefaultWindow(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>fires inside the Anthropic window</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 0)
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 || fs.n() != 0 {
		t.Fatalf("substituted %d, ordinary pings %d: the default window must still cover an Anthropic ping",
			k.summarySubstituted.Load(), fs.n())
	}
}
