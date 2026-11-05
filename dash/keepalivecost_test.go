package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// keepaliveEvent is a real request that resumed after `gapMs` of idle with `pings` keep-alive
// pings behind it, billed the given tiers.
func keepaliveEvent(gapMs int64, pings int, refreshed, read, write int64) *Event {
	e := &Event{
		TS: 1000, SessionID: "s", Model: "aws/claude-sonnet-5",
		TokensBefore: 90_000, TokensAfter: 90_000,
		CacheRead: read, CacheWrite: write, OutputTokens: 50,
		SinceLastMs: gapMs, KeepAlivePings: pings, KeepAliveRefreshed: refreshed,
	}
	e.Price(ibmSonnet, true)
	return e
}

// A ping row must be priced like any other request — its cost is the whole point of the
// audit trail — while contributing nothing to the saving side of the ledger.
func TestKeepAlivePingRowIsPricedAndNotCredited(t *testing.T) {
	p := &Event{TS: 1000, SessionID: "s", Model: "aws/claude-sonnet-5", KeepAlive: true,
		CacheRead: 48_576, OutputTokens: 1}
	p.Price(ibmSonnet, true)
	want := 48_576*ibmSonnet.CacheRead + 1*ibmSonnet.Output
	if p.CostUSD < want*0.999 || p.CostUSD > want*1.001 {
		t.Errorf("ping cost = %.6f, want %.6f", p.CostUSD, want)
	}
	if p.KeepAliveSavedUSD != 0 {
		t.Errorf("a ping credited itself $%.6f", p.KeepAliveSavedUSD)
	}
	// The economics this rests on: one ping buys back about 11.5 of itself. Stated as an
	// assertion so a price list that broke the ratio would be caught here rather than in a
	// month of bills.
	miss := 48_576 * (ibmSonnet.CacheWrite - ibmSonnet.CacheRead)
	if ratio := miss / (48_576 * ibmSonnet.CacheRead); ratio < 11.0 || ratio > 12.0 {
		t.Errorf("saving:ping ratio = %.2f, expected ~11.5 at the Anthropic-family multiples", ratio)
	}
}

// The 1-hour write tier costs 2.0x base input where the 5-minute tier costs 1.25x. Pricing one
// as the other understates the row by 0.75x of its whole written prefix, and a cost that is
// wrong in the flattering direction argues for spending.
//
// The numbers are a live measurement, not a construction: a request asking for the 1h head on
// aws/claude-haiku-4-5 came back with 36,251 of 36,574 written tokens on the 1h tier.
func TestOneHourWriteIsPricedAtItsOwnTier(t *testing.T) {
	haiku := modelinfo.Price{Input: 0.76e-6, Output: 3.8e-6,
		CacheRead: 0.076e-6, CacheWrite: 0.95e-6}
	mk := func(write1h int64) *Event {
		e := &Event{TS: 1, SessionID: "s", Model: "aws/claude-haiku-4-5",
			CacheWrite: 36_574, CacheWrite1h: write1h, OutputTokens: 32}
		e.Price(haiku, true)
		return e
	}
	asFiveMin, asOneHour := mk(0), mk(36_251)
	// 5m only: 36,574 x 0.95/MTok + output.
	want5 := 36_574*haiku.CacheWrite + 32*haiku.Output
	if asFiveMin.CostUSD < want5*0.999 || asFiveMin.CostUSD > want5*1.001 {
		t.Errorf("5m-only cost = %.6f, want %.6f", asFiveMin.CostUSD, want5)
	}
	// With the 1h tier: the same, plus (2.0x input - 5m write rate) on the 1h tokens.
	want1 := want5 + 36_251*(2*haiku.Input-haiku.CacheWrite)
	if asOneHour.CostUSD < want1*0.999 || asOneHour.CostUSD > want1*1.001 {
		t.Errorf("1h cost = %.6f, want %.6f", asOneHour.CostUSD, want1)
	}
	if asOneHour.CostUSD <= asFiveMin.CostUSD {
		t.Error("the 1h tier priced no higher than the 5m tier; the premium is the whole reason " +
			"a blanket 1h TTL loses money")
	}
	// And the ratio the mixed-TTL argument rests on: 2.0x versus 1.25x is a 60% premium on the
	// written tokens.
	if r := asOneHour.CostUSD / asFiveMin.CostUSD; r < 1.5 || r > 1.7 {
		t.Errorf("1h/5m cost ratio = %.3f, expected ~1.6 on a fully-1h write", r)
	}
}

// A ping is a request WE made on the user's behalf, not traffic their agent sent. Counted into
// an aggregate it inflates the request count, drags every per-request average towards a
// one-token response, and makes an account's own traffic statistics wrong. It must still be
// visible as a row, because it is the audit trail for money spent while nobody was watching.
func TestPingsAreVisibleAsRowsAndExcludedFromAggregates(t *testing.T) {
	db := openTestDB(t)
	agent := &Event{TS: 1000, SessionID: "s", Model: "aws/claude-sonnet-5",
		TokensBefore: 90_000, TokensAfter: 90_000, CacheRead: 48_576, OutputTokens: 500}
	agent.Price(ibmSonnet, true)
	ping := &Event{TS: 2000, SessionID: "s", Model: "aws/claude-sonnet-5", KeepAlive: true,
		CacheRead: 48_576, OutputTokens: 1}
	ping.Price(ibmSonnet, true)
	if err := db.insertBatch([]*Event{agent, ping}); err != nil {
		t.Fatal(err)
	}

	o, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 1 {
		t.Errorf("Overview counted %d requests; the ping is not agent traffic", o.Requests)
	}
	if o.OutputTokens != 500 {
		t.Errorf("output_tokens = %d, want 500 — the ping's single token must not move an average",
			o.OutputTokens)
	}
	// Both halves of the ledger, and they must come from the two different populations.
	if o.KeepAlivePings != 1 {
		t.Errorf("keepalive_pings = %d, want 1", o.KeepAlivePings)
	}
	if o.KeepAlivePingUSD <= 0 {
		t.Error("the ping cost nothing; a ledger that hides the spend is the thing this exists to prevent")
	}
	if got, want := o.KeepAliveNetUSD, o.KeepAliveSavedUSD-o.KeepAlivePingUSD; got != want {
		t.Errorf("net = %.6f, want saved-ping = %.6f", got, want)
	}

	// The row list SHOWS it, flagged, so the spend is auditable.
	page, err := db.Requests(Filter{TenantAll: true}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 2 {
		t.Fatalf("the request list returned %d rows; the ping must be visible", len(page.Requests))
	}
	pings := 0
	for _, r := range page.Requests {
		if r.KeepAlive {
			pings++
		}
	}
	if pings != 1 {
		t.Errorf("%d rows flagged keepalive in the list, want 1", pings)
	}
}

// A ping reads the whole prefix by design, so every figure derived from `cache_read` has to
// exclude it or the mechanism credits itself. `cache_saved_usd` is the one that bites: it is a
// headline figure elsewhere on the page, and at production ping volumes an unguarded ping row
// fabricates it on the order of a thousand dollars.
func TestPingsDoNotCreditProviderCacheSavings(t *testing.T) {
	ping := &Event{TS: 1, SessionID: "s", Model: "aws/claude-sonnet-5", KeepAlive: true,
		CacheRead: 48_576, OutputTokens: 1}
	ping.Price(ibmSonnet, true)
	if ping.CacheSavedUSD != 0 {
		t.Errorf("a ping booked $%.4f of provider cache saving; without the keep-alive that "+
			"request does not exist, so there is nothing it saved against", ping.CacheSavedUSD)
	}
	// It still costs what it costs — the guard must suppress the phantom saving, not the spend.
	if ping.CostUSD <= 0 {
		t.Error("the ping's own cost was suppressed too")
	}
	// And an identical AGENT request still earns the credit, so the guard is on the ping and not
	// on the shape of the row.
	agent := &Event{TS: 1, SessionID: "s", Model: "aws/claude-sonnet-5",
		CacheRead: 48_576, OutputTokens: 1}
	agent.Price(ibmSonnet, true)
	if agent.CacheSavedUSD <= 0 {
		t.Error("an ordinary cache hit lost its provider-cache diagnostic")
	}
}
