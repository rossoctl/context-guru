package campaign

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

func nearf(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// A row whose entry was billed at the 1-hour tier has a miss at the 1-hour write rate, as the keeper
// values it. The stake stays at the 5-minute rate: it is in the calibrated units of the replay.
func TestReplayMissOfAOneHourEntryIsAOneHourWrite(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	price := func(string, int64) (float64, float64, float64, float64, bool) {
		return 3e-6, 15e-6, 0.3e-6, 3.75e-6, true
	}
	mk := func(w1h int64) *kvcache.Request {
		return &kvcache.Request{ID: 1, User: "u", ConversationID: "s", Model: "claude-sonnet-5", TS: t0, UpstreamMs: 1000,
			InputTokens: 10, CacheRead: 90_000, CacheWrite: 10_000, CacheWrite1h: w1h, CachedContext: 100_000, CostKnown: true}
	}
	_, five := Rows([]*kvcache.Request{mk(0)}, price, false, t0+10*3600e3)
	_, hour := Rows([]*kvcache.Request{mk(10_000)}, price, false, t0+10*3600e3)
	if len(five) != 1 || len(hour) != 1 {
		t.Fatalf("rows %d %d", len(five), len(hour))
	}
	if want := 100_000 * (3.75e-6 - 0.3e-6); !nearf(five[0].Miss, want) {
		t.Errorf("5m miss %v want %v", five[0].Miss, want)
	}
	if want := 100_000 * (6e-6 - 0.3e-6); !nearf(hour[0].Miss, want) { // the 1-hour write is 2.0x input
		t.Errorf("1h miss %v want %v", hour[0].Miss, want)
	}
	if !nearf(hour[0].Stake, five[0].Stake) {
		t.Errorf("stake moved with the tier: %v against %v", hour[0].Stake, five[0].Stake)
	}
	if want := 100_000 * (3.75e-6 - 0.3e-6); !nearf(hour[0].Stake, want) {
		t.Errorf("stake %v want the 5-minute %v", hour[0].Stake, want)
	}
}

// The ping reads the prefix plus its own input, so a prefix just below a price line prices the ping above
// it. The stake is at the request's own size.
func TestPriceDecisionPricesThePingAtItsOwnSize(t *testing.T) {
	price := func(_ string, n int64) (float64, float64, float64, float64, bool) {
		if n > 272_000 {
			return 10, 45, 1, 10, true
		}
		return 5, 30, 0.5, 5, true
	}
	// Prefix 271,995 + 7 = 272,002: the ping is above the line. The request itself (272,000) is not.
	d, ok := PriceDecision(price, "m", 271_995, 272_000, false)
	if !ok {
		t.Fatal("not priced")
	}
	if want := 271_995*1.0 + 7*10 + 45; !nearf(d.Ping, want) {
		t.Errorf("ping %v want %v", d.Ping, want)
	}
	if want := 271_995 * (10.0 - 1); !nearf(d.Miss, want) { // the miss is at the ping's size too
		t.Errorf("miss %v want %v", d.Miss, want)
	}
	if want := 271_995 * (5.0 - 0.5); !nearf(d.Stake, want) {
		t.Errorf("stake %v want %v", d.Stake, want)
	}
	if _, ok := PriceDecision(func(string, int64) (float64, float64, float64, float64, bool) { return 0, 0, 0, 0, false }, "m", 1000, 1000, false); ok {
		t.Error("an unpriced model must not price")
	}
}

// A write rate below the read rate gives no stake and no miss, never a negative one.
func TestStakeAndMissNeverBelowZero(t *testing.T) {
	if got := Stake(1000, 3, 1); got != 0 {
		t.Errorf("stake %v want 0", got)
	}
	d, ok := PriceDecision(func(string, int64) (float64, float64, float64, float64, bool) { return 1, 1, 3, 1, true }, "m", 1000, 1000, false)
	if !ok || d.Stake != 0 || d.Miss != 0 {
		t.Errorf("got %+v ok %v", d, ok)
	}
}

// A mixed write (a 1-hour head, a 5-minute tail): the stake and the miss are the tail rewrite while the head lives.
func TestPriceDecisionHeadPricesTheTail(t *testing.T) {
	price := func(string, int64) (float64, float64, float64, float64, bool) {
		return 3e-6, 15e-6, 0.3e-6, 3.75e-6, true
	}
	const head, tail = 20_157, 150
	d, ok := PriceDecisionHead(price, "claude-sonnet-5", head+tail, head+tail, true, head)
	if !ok {
		t.Fatal("not priced")
	}
	want := float64(tail) * (3.75e-6 - 0.3e-6)
	if d.Miss < want*0.999 || d.Miss > want*1.001 || d.Stake < want*0.999 || d.Stake > want*1.001 {
		t.Errorf("miss %v stake %v, want the tail %v", d.Miss, d.Stake, want)
	}
	whole, _ := PriceDecisionHead(price, "claude-sonnet-5", head+tail, head+tail, false, 0)
	if whole.Miss <= d.Miss*10 {
		t.Errorf("whole-prefix miss %v is not far above the tail miss %v", whole.Miss, d.Miss)
	}
}
