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
