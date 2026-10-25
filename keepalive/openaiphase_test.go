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
	// The retry guard and the phase check use one rule (#715).
	e := &kaEntry{provider: bschemas.OpenAI}
	if e.cacheTTL() != cacheLifetime(bschemas.OpenAI, false) {
		t.Fatal("cacheTTL and cacheLifetime disagree on OpenAI")
	}
}

// The same, end to end: a pre_expiry summary candidate on an OpenAI thread takes the ping at
// 28 minutes, where it used to be refused as a phase mismatch.
func TestAPreExpirySubstituteTakesAnOpenAIPing(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>fires inside the OpenAI window</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 180)

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
		t.Fatalf("the substitute was refused as a phase mismatch %d time(s): the 30-min OpenAI cache "+
			"was read as a 5-min one", n)
	}
	if k.summarySubstituted.Load() != 1 || fs.n() != 0 {
		t.Fatalf("substituted %d, ordinary pings %d: want the summary to take the OpenAI ping",
			k.summarySubstituted.Load(), fs.n())
	}
}
