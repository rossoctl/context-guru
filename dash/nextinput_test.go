package dash

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func seedFresh(t *testing.T, rec *Recorder, session string, fresh ...int) {
	t.Helper()
	now := time.Now().UnixMilli()
	for i, f := range fresh {
		e := mkEvent(now-int64(len(fresh)-i)*1000, session, "gpt-5.6", 1000, 800)
		e.Status, e.FreshInput, e.CacheRead = 200, int64(f), 5000
		seed(t, rec, e)
	}
}

// The next request's size is the median of the session's last billed requests.
func TestNextInputTokensIsTheMedian(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	seedFresh(t, rec, "s1", 100, 5_000, 6_000)
	if got := rec.DB().NextInputTokens(Filter{Session: "s1"}); got != 5_000 {
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
	if got := rec.DB().NextInputTokens(Filter{Session: "s1"}); got != 40 {
		t.Errorf("got %d, want 40", got)
	}
}

// A session with no history gets the fallback, which is not the ping constant.
func TestNextInputTokensFallsBack(t *testing.T) {
	_, rec := newTestAPI(t, Options{})
	got := rec.DB().NextInputTokens(Filter{Session: "none"})
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
	n := rec.DB().NextInputTokens(Filter{Session: "s1"})
	warm, cold := NextRequestUSD(p, 268_000, n, 1800)
	hi := p.At(272_001)
	if warm != 268_000*hi.CacheRead || cold != 268_000*hi.CacheWrite {
		t.Errorf("warm=%v cold=%v, want the tier rates %v/%v", warm, cold, hi.CacheRead, hi.CacheWrite)
	}
	if warm == 268_000*p.CacheRead {
		t.Error("priced at the base tier")
	}
}
