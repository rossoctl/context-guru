package dash

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// A dashboard row prices its 1-hour writes at the rate the price list states. An entry that
// bills them at the input rate costs the same with or without the 1-hour split.
func TestEventPriceUsesThePriceListOneHourRate(t *testing.T) {
	tb, err := modelinfo.ParseTable([]byte("models:\n" +
		"  - {match: acme/flat-1, in: 3, out: 15, cache_write_1h_frac: 1.0}\n" +
		"  - {match: claude-sonnet*, in: 3, out: 15}\n"))
	if err != nil {
		t.Fatal(err)
	}
	row := func(model string) *Event {
		p, ok := tb.Price(context.Background(), model)
		if !ok {
			t.Fatalf("%s not priced", model)
		}
		e := &Event{TS: 1, SessionID: "s", Model: model,
			FreshInput: 4, CacheWrite: 20315, CacheWrite1h: 20157, OutputTokens: 12}
		e.Price(p, true)
		return e
	}
	if got, want := row("acme/flat-1").CostUSD, 20319*3e-6+12*15e-6; math.Abs(got-want) > 1e-12 {
		t.Errorf("acme/flat-1 row cost = %v, want %v", got, want)
	}
	want := 4*3e-6 + 20315*3.75e-6 + 12*15e-6 + 20157*0.75*3e-6
	if got := row("claude-sonnet-4-6").CostUSD; math.Abs(got-want) > 1e-12 {
		t.Errorf("Claude row cost = %v, want %v", got, want)
	}
}

// The live keep-alive row prices a 1-hour lapse at the entry's 1-hour rate, not at 2.0x.
func TestTheLivePanelUsesThePriceListOneHourRate(t *testing.T) {
	now := int64(1_700_000_000_000)
	e := kaAgent(now-100_000, "hour", 0.10)
	e.CacheWrite, e.CacheWrite1h = 100_000, 100_000
	fx := newKAFixture(t, e)
	price := func(string) (modelinfo.Price, bool) {
		return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6, Write1hFrac: 1.5}, true
	}
	got, err := fx.db.KeepAliveLive(Filter{TenantAll: true}, now, 280, 2, price)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Rows))
	}
	// A lapse: 100k x (4.5e-6 - 3e-7) = $0.42. A ping: 100k x 3e-7 + 15e-6 = $0.030015.
	// 0.42 / 0.030015 = 13.99, so 13 pings. At 2.0x it would be 18.
	if r := got.Rows[0]; r.Breakeven1hPings != 13 {
		t.Errorf("breakeven_1h_pings = %d, want 13 (the entry's 1.5x)", r.Breakeven1hPings)
	}
}
