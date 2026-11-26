package dash

import (
	"github.com/rossoctl/context-guru/keepalive"
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
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
		e.Provider, e.Model = "openai", "gpt-5.6"
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

// A pre-GPT-5.6 OpenAI model has no documented retention. Every page keeps the 5-minute reading.

func TestCacheStateOldOpenAIModelKeepsFiveMinutes(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	e := openaiWrite(now-60_000, "old")
	e.Model = "gpt-4o"
	seed(t, rec, e)
	_, body := get(t, a, "/api/cachestate?session=old", "")
	if body["ttl_s"].(float64) != 300 {
		t.Errorf("ttl_s = %v, want 300 for gpt-4o", body["ttl_s"])
	}
	ths, _, err := a.rec.DB().ThreadsFor(Filter{Session: "old"}, 0)
	if err != nil || len(ths) == 0 {
		t.Fatalf("ThreadsFor: %v %v", ths, err)
	}
	if ths[0].TTLSeconds != 300 {
		t.Errorf("thread ttl_s = %d, want 300 for gpt-4o", ths[0].TTLSeconds)
	}
}

func TestSessionsInFlightOldOpenAIModelKeepsFiveMinutes(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	old := mkEvent(now-10*60*1000, "old", "gpt-4o", 10_000, 9_000)
	old.Provider = "openai"
	if err := db.insertBatch([]*Event{old}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := db.Sessions(Filter{}, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Sessions: %v %v", rows, err)
	}
	if rows[0].InFlight {
		t.Error("a gpt-4o session idle 10 min is in flight; only GPT-5.6+ has a 30 min lifetime")
	}
}

func TestSpendExplainOldOpenAIModelGapIsIdleExpiry(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(fixedPricer{})
	const m = int64(60000)
	t0 := int64(1_700_000_000_000)
	mk := func(ts int64, msgs int, write int64) *Event {
		e := spendEv(ts, "old", msgs, 5, 0, write, 0)
		e.Provider, e.Model = "openai", "gpt-4o"
		return e
	}
	seed(t, rec, mk(t0, 3, 10000), mk(t0+10*m, 5, 10300))
	_, body := get(t, a, "/api/spend/explain?now="+itoa(t0+16*m), "127.0.0.1:1")
	c, ok := body["rewrites"].(map[string]any)["idle_expiry"].(map[string]any)
	if !ok || c["n"].(float64) != 1 {
		t.Errorf("idle_expiry = %v, want 1 for a 10 min gap on gpt-4o", body["rewrites"])
	}
}

func TestKeepAliveLiveUsesTheProviderLifetime(t *testing.T) {
	now := int64(1_700_000_000_000)
	ago := func(sec int64) int64 { return now - sec*1000 }
	live := func(session, provider, model string, sec int64) *Event {
		e := kaAgent(ago(sec), session, 0.10)
		e.Provider, e.Model = provider, model
		e.CacheRead, e.CacheWrite, e.CacheWrite1h = 0, 100_000, 0
		return e
	}
	fx := newKAFixture(t,
		live("oa", "openai", "gpt-5.6", 400),   // 30 min lifetime: 1400 s left
		live("oldoa", "openai", "gpt-4o", 400), // 5 min lifetime: gone, not a row
		live("an", "anthropic", "aws/claude-sonnet-5", 100),
	)
	price := func(string) (modelinfo.Price, bool) {
		return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
	}
	got, err := fx.db.KeepAliveLive(Filter{TenantAll: true}, now, 280, 2, price)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]KeepAliveLiveRow{}
	for _, r := range got.Rows {
		rows[r.SessionID] = r
	}
	if r, ok := rows["oa"]; !ok || r.TTLSeconds != 1800 || math.Abs(r.RemainingSeconds-1400) > 0.001 || r.Model != "gpt-5.6" || r.Prefix != 100_000 {
		t.Errorf("oa row = %+v (ok %v), want ttl 1800, 1400 s left, model gpt-5.6, prefix 100000", r, ok)
	}
	// 11 break-even pings, so the reach is 11*280 + the row's own lifetime.
	if r := rows["oa"]; r.BreakevenPings != 11 || math.Abs(r.BreakevenMinutes-(11*280+1800)/60.0) > 0.001 {
		t.Errorf("oa break-even = %d pings, %.3f min, want 11 and %.3f (1800 s lifetime)",
			r.BreakevenPings, r.BreakevenMinutes, (11*280+1800)/60.0)
	}
	if r := rows["an"]; math.Abs(r.BreakevenMinutes-(11*280+300)/60.0) > 0.001 {
		t.Errorf("an break-even = %.3f min, want %.3f", r.BreakevenMinutes, (11*280+300)/60.0)
	}
	if _, ok := rows["oldoa"]; ok {
		t.Error("a gpt-4o entry 400 s old is on the live list; its lifetime is 5 min")
	}
	if r := rows["an"]; r.TTLSeconds != 300 || math.Abs(r.RemainingSeconds-200) > 0.001 {
		t.Errorf("an row = %+v, want ttl 300, 200 s left", r)
	}
}

func TestLiveSessionsTTLFollowsTheProvider(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	mk := func(session, provider, model string) *Event {
		e := spendEv(now-60_000, session, 3, 5, 0, 10000, 0)
		e.Provider, e.Model = provider, model
		return e
	}
	if err := db.insertBatch([]*Event{
		mk("oa", "openai", "gpt-5.6"), mk("old", "openai", "gpt-4o"), mk("an", "anthropic", "m"),
	}); err != nil {
		t.Fatal(err)
	}
	live, err := db.liveSessions(Filter{TenantAll: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, s := range live {
		got[s.Session] = s.TTLMs
	}
	for s, want := range map[string]int64{"oa": 1_800_000, "old": 300_000, "an": 300_000} {
		if got[s] != want {
			t.Errorf("%s ttl_ms = %d, want %d", s, got[s], want)
		}
	}
}

func TestCoverageSecondsForUsesTheRowLifetime(t *testing.T) {
	if got := CoverageSecondsFor(280, 2, modelinfo.CacheLifetime("openai", "gpt-5.6", false)); got != 2360 {
		t.Errorf("GPT-5.6 coverage = %v, want 2*280+1800", got)
	}
	if got := CoverageSecondsFor(280, 2, modelinfo.CacheLifetime("openai", "gpt-4o", false)); got != 860 {
		t.Errorf("gpt-4o coverage = %v, want 860", got)
	}
	if CoverageSeconds(280, 2) != 860 {
		t.Error("the account-wide CoverageSeconds must keep the 5-minute default")
	}
}

// An expiry after a 20 minute gap is out of the 5-minute policy's reach on Anthropic and gpt-4o,
// and inside a GPT-5.6 entry's 30-minute life, so only the GPT-5.6 one is convertible.
func TestKeepAliveCalcJudgesAnExpiryAgainstItsOwnLifetime(t *testing.T) {
	now := int64(1_700_000_000_000)
	exp := func(session, provider, model string) []*Event {
		evs := kaExpiry(now, session, 20*60, 0.50, 100_000)
		for _, e := range evs {
			e.Provider, e.Model = provider, model
		}
		return evs
	}
	price := func(string) (modelinfo.Price, bool) {
		return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
	}
	for _, c := range []struct {
		provider, model string
		want            int64
	}{
		{"openai", "gpt-5.6", 1},
		{"openai", "gpt-4o", 0},
		{"anthropic", "aws/claude-sonnet-5", 0},
	} {
		fx := newKAFixture(t, exp("s", c.provider, c.model)...)
		out, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 280, 100_000, c.model, price, 2, keepalive.Policy{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows[1].Convertible != c.want { // K=2: 560 s + the lifetime
			t.Errorf("%s/%s: convertible = %d, want %d", c.provider, c.model, out.Rows[1].Convertible, c.want)
		}
	}
}
