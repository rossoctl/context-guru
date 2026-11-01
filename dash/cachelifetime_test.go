package dash

import (
	"testing"
	"time"
)

// An OpenAI thread keeps its cache 30 minutes (the GPT-5.6+ minimum). Each dash page used to
// read it as 5 minutes. These tests fail on that old reading.

func openaiWrite(ts int64, session string) *Event {
	e := mkEvent(ts, session, "gpt-5.6", 1000, 800)
	e.Provider = "openai"
	e.Status, e.FreshInput, e.CacheRead, e.CacheWrite, e.CacheWrite1h = 200, 10, 0, 5000, 0
	return e
}

func TestCacheStateOpenAIThreadLivesThirtyMinutes(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	seed(t, rec, openaiWrite(now-60_000, "oa"))
	_, body := get(t, a, "/api/cachestate?session=oa", "")
	if body["ttl_s"].(float64) != 1800 {
		t.Errorf("ttl_s = %v, want 1800 for an OpenAI thread", body["ttl_s"])
	}
	ths, _, err := a.rec.DB().ThreadsFor(Filter{Session: "oa"}, 0)
	if err != nil || len(ths) == 0 {
		t.Fatalf("ThreadsFor: %v %v", ths, err)
	}
	if ths[0].TTLSeconds != 1800 {
		t.Errorf("thread ttl_s = %d, want 1800", ths[0].TTLSeconds)
	}
}

func TestSessionsInFlightUsesTheProviderLifetime(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	oa := mkEvent(now-10*60*1000, "oa", "gpt-5.6", 10_000, 9_000)
	oa.Provider = "openai"
	an := mkEvent(now-10*60*1000, "an", "m", 10_000, 9_000)
	if err := db.insertBatch([]*Event{oa, an}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := db.Sessions(Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.SessionID] = r.InFlight
	}
	if !got["oa"] {
		t.Error("an OpenAI session idle 10 min is not in flight; its cache lives 30 min")
	}
	if got["an"] {
		t.Error("an Anthropic session idle 10 min is in flight; its cache lives 5 min")
	}
}

func TestSpendExplainOpenAIGapUnderThirtyMinutesIsNotIdleExpiry(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(fixedPricer{})
	const m = int64(60000)
	t0 := int64(1_700_000_000_000)
	mk := func(ts int64, sess string, msgs int, read, write int64) *Event {
		e := spendEv(ts, sess, msgs, 5, read, write, 0)
		e.Provider = "openai"
		return e
	}
	seed(t, rec,
		mk(t0, "oa", 3, 0, 10000),
		mk(t0+10*m, "oa", 5, 0, 10300), // 10 min gap: inside 30 min, so not an idle expiry
		spendEv(t0, "an", 3, 5, 0, 10000, 0),
		spendEv(t0+10*m, "an", 5, 5, 0, 10300, 0), // same gap on Anthropic: idle expiry
	)
	_, body := get(t, a, "/api/spend/explain?now="+itoa(t0+16*m), "127.0.0.1:1")
	rw := body["rewrites"].(map[string]any)
	c, ok := rw["idle_expiry"].(map[string]any)
	if !ok || c["n"].(float64) != 1 {
		t.Errorf("idle_expiry = %v, want exactly the Anthropic row (all: %v)", rw["idle_expiry"], rw)
	}
}
