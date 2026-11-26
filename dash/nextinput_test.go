package dash

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

type nextInPricer struct{ p modelinfo.Price }

func (f nextInPricer) Price(context.Context, string) (modelinfo.Price, bool) { return f.p, true }

func seedFresh(t *testing.T, rec *Recorder, session string, fresh ...int) {
	seedFreshWrite(t, rec, session, 0, fresh...)
}

func seedFreshWrite(t *testing.T, rec *Recorder, session string, write int64, fresh ...int) {
	t.Helper()
	now := time.Now().UnixMilli()
	for i, f := range fresh {
		e := mkEvent(now-int64(len(fresh)-i)*1000, session, "gpt-5.6", 1000, 800)
		e.Status, e.FreshInput, e.CacheRead, e.CacheWrite = 200, int64(f), 5000, write
		seed(t, rec, e)
	}
}

// An Anthropic-like session bills the new turn as cache write: fresh 2, write 3,000.
func TestNextInputTokensCountsCacheWrite(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	seedFreshWrite(t, rec, "an", 3_000, 2, 2, 2)
	if got := rec.DB().NextInputTokens(Filter{Session: "an"}, ""); got != 3_002 {
		t.Errorf("got %d, want 3002", got)
	}
}

// The next request's size is the median of the session's last billed requests.
func TestNextInputTokensIsTheMedian(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	seedFresh(t, rec, "s1", 100, 5_000, 6_000)
	if got := rec.DB().NextInputTokens(Filter{Session: "s1"}, ""); got != 5_000 {
		t.Errorf("got %d, want 5000", got)
	}
}

// Only the last NextInputWindow requests count.
func TestNextInputTokensUsesTheLastWindow(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	fresh := []int{9_000, 9_000, 9_000, 9_000, 9_000, 9_000}
	for i := 0; i < NextInputWindow; i++ {
		fresh = append(fresh, 40)
	}
	seedFresh(t, rec, "s1", fresh...)
	if got := rec.DB().NextInputTokens(Filter{Session: "s1"}, ""); got != 40 {
		t.Errorf("got %d, want 40", got)
	}
}

// A session with no history gets the fallback, which is not the ping constant.
func TestNextInputTokensFallsBack(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	got := rec.DB().NextInputTokens(Filter{Session: "none"}, "")
	if got != DefaultNextInputTokens {
		t.Errorf("got %d, want %d", got, DefaultNextInputTokens)
	}
	if DefaultNextInputTokens <= 7 {
		t.Errorf("fallback %d must exceed the ping constant", DefaultNextInputTokens)
	}
}

// A prefix of 268,000 plus a median of 5,000 is over the 272,000 line: the higher tier.
func TestNextRequestUSDPricesAtTheTierItsInputReaches(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	seedFresh(t, rec, "s1", 100, 5_000, 6_000)
	p := modelinfo.Price{Input: 4e-6, Output: 20e-6, CacheRead: 0.4e-6, CacheWrite: 5e-6}.WithTiers("gpt-5.6")
	n := rec.DB().NextInputTokens(Filter{Session: "s1"}, "")
	warm, cold := NextRequestUSD(p, 268_000, n, 1800)
	hi := p.At(272_001)
	if warm != 268_000*hi.CacheRead || cold != 268_000*hi.CacheWrite {
		t.Errorf("warm=%v cold=%v, want the tier rates %v/%v", warm, cold, hi.CacheRead, hi.CacheWrite)
	}
	if warm == 268_000*p.CacheRead {
		t.Error("priced at the base tier")
	}
}

// A ping row and a non-2xx row inside the window do not move the size.
func TestNextInputTokensSkipsPingsAndFailures(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	mk := func(age int64, fresh int64, status int, ping bool) *Event {
		e := mkEvent(now-age, "s1", "gpt-5.6", 1000, 800)
		e.Status, e.FreshInput, e.CacheRead, e.CacheWrite, e.KeepAlive = status, fresh, 5000, 0, ping
		return e
	}
	seed(t, rec, mk(6000, 5_000, 200, false), mk(5000, 5_000, 200, false), mk(4000, 6_000, 200, false),
		mk(3000, 7, 200, true), mk(2000, 7, 200, true), mk(1900, 7, 200, true),
		mk(1000, 9, 500, false), mk(900, 9, 500, false), mk(800, 9, 500, false))
	if got := rec.DB().NextInputTokens(Filter{Session: "s1"}, ""); got != 5_000 {
		t.Errorf("got %d, want 5000", got)
	}
}

// Each model of a session has its own size; a model with no rows takes the session median.
func TestNextInputTokensPerModel(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	mk := func(age int64, model string, fresh int64) *Event {
		e := mkEvent(now-age, "s1", model, 1000, 800)
		e.Status, e.FreshInput, e.CacheRead, e.CacheWrite = 200, fresh, 5000, 0
		return e
	}
	seed(t, rec, mk(6000, "main-model", 8_000), mk(5000, "main-model", 8_000), mk(4000, "main-model", 8_000),
		mk(3000, "small-model", 50), mk(2000, "small-model", 50))
	db := rec.DB()
	if got := db.NextInputTokens(Filter{Session: "s1"}, "main-model"); got != 8_000 {
		t.Errorf("main = %d, want 8000", got)
	}
	if got := db.NextInputTokens(Filter{Session: "s1"}, "small-model"); got != 50 {
		t.Errorf("small = %d, want 50", got)
	}
	if got := db.NextInputTokens(Filter{Session: "s1"}, "new-model"); got != 8_000 {
		t.Errorf("unseen model = %d, want the session median 8000", got)
	}
}

// /api/cachestate prices the next request with the median, so a prefix just under the line
// reaches the higher tier.
func TestCacheStateUsesTheNextInputMedian(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(nextInPricer{modelinfo.Price{Input: 4e-6, CacheRead: 0.4e-6, CacheWrite: 5e-6}.WithTiers("gpt-5.6")})
	now := time.Now().UnixMilli()
	e := mkEvent(now-1000, "s1", "gpt-5.6", 1000, 800)
	e.Provider, e.Status, e.FreshInput, e.CacheRead, e.CacheWrite = "openai", 200, 5_000, 263_000, 0
	seed(t, rec, e)
	_, body := get(t, a, "/api/cachestate?session=s1", "")
	warm, _ := body["next_warm_usd"].(float64)
	if want := 268_000 * 0.8e-6; math.Abs(warm-want) > 1e-9 {
		t.Errorf("next_warm_usd = %v, want %v (higher tier)", body["next_warm_usd"], want)
	}
}
