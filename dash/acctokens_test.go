package dash

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// tokUSD is the dollar value of a token figure: the library's sign convention.
func tokUSD(f TokenFlow) float64 {
	tt := func(t TierTokens) accountant.TierTokens {
		return accountant.TierTokens{Input: int(t.Input), CacheRead: int(t.CacheRead),
			CacheWrite5m: int(t.CacheWrite5m), CacheWrite1h: int(t.CacheWrite1h)}
	}
	return tierUSD(accTestPrice, tt(f.Saved)) - tierUSD(accTestPrice, tt(f.Caused))
}

// tokEvent is a priced event that exercises every attribution path of AccountantFigure: two
// components, a break owned by one of them, a mark premium with no owner (tailbreakpoint), a dry
// run and a spend operation (never counted), a break the client body has too (never counted), and
// the four request-level effects.
func tokEvent(ts int64, session string) *Event {
	e := &Event{TS: ts, SessionID: session, Model: "m", TenantID: "t1", FreshInput: 100, CacheRead: 5000,
		CacheWrite: 400, OutputTokens: 10, TokensBefore: 50_000, TokensAfter: 40_000,
		Components: []CompRow{
			{Component: "mask", Acted: true, Mutated: true, SavedGross: 1000, SavedUnique: 1000},
			{Component: "extract", Acted: true, Mutated: true, SavedGross: 2000, SavedUnique: 500},
			{Component: "idle"},
		}}
	e.SetAccountant(accountant.Result{
		Ops: []accountant.PricedOp{
			{OpID: "1", Operator: "context-guru.mask", ChangeID: "c1", Saved: accountant.TierTokens{CacheWrite5m: 1000}},
			{OpID: "2", Operator: "context-guru.extract", ChangeID: "c2",
				Saved: accountant.TierTokens{CacheRead: 1500, Input: 500}, Caused: accountant.TierTokens{Input: 40}},
			{OpID: "3", Operator: "context-guru.mask", ChangeID: "c3", DryRun: true, Saved: accountant.TierTokens{Input: 9999}},
			{OpID: "4", Operator: "context-guru.summarize", ChangeID: "c4", Call: &accountant.ModelCall{}, Saved: accountant.TierTokens{Input: 7777}},
		},
		CacheEffects: []accountant.CacheEffect{
			{Kind: accountant.EffectBreak, ChangeID: "c1", Saved: accountant.TierTokens{CacheRead: 300}, Caused: accountant.TierTokens{CacheWrite5m: 300}},
			{Kind: accountant.EffectBreak, ChangeID: "x", InBaseline: true, Caused: accountant.TierTokens{CacheWrite5m: 8888}},
			{Kind: accountant.EffectBreak, ChangeID: "c3", Caused: accountant.TierTokens{CacheWrite5m: 6666}}, // a dry run's: never charged
			{Kind: accountant.EffectBreak, Saved: accountant.TierTokens{CacheRead: 50}, Caused: accountant.TierTokens{CacheWrite5m: 50}},
			{Kind: accountant.EffectMarkPremium, Saved: accountant.TierTokens{Input: 200}, Caused: accountant.TierTokens{CacheWrite5m: 200}},
			{Kind: accountant.EffectRescue, ChangeID: "ka:main:w", Saved: accountant.TierTokens{CacheWrite5m: 2000}, Caused: accountant.TierTokens{CacheRead: 2000}},
			{Kind: accountant.EffectSplit, ChangeID: "cg:cachesplit:x", Saved: accountant.TierTokens{CacheWrite5m: 700}, Caused: accountant.TierTokens{CacheRead: 700}},
			{Kind: accountant.EffectTTLPremium, ChangeID: "headttl:main:x", Saved: accountant.TierTokens{CacheWrite5m: 100}, Caused: accountant.TierTokens{CacheWrite1h: 100}},
		},
	})
	return e
}

func flow(saved, caused TierTokens) TokenFlow { return TokenFlow{Saved: saved, Caused: caused} }

// Each component row gets the tokens whose dollars it gets: for every row the library priced, its
// SavedUSD is tierUSD(Saved) - tierUSD(Caused) of its tokens, except the two rows whose dollars come
// from a request-level effect (headttl, ended_change): their tokens are in AccTokens, and adding
// those gives the dollars back. The keep-alive and split dollars match their tokens the same way.
func TestAccTokens_AttributionMatchesTheDollars(t *testing.T) {
	e := tokEvent(1, "s")
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	byName := map[string]CompRow{}
	for _, c := range e.Components {
		byName[c.Component] = c
	}
	want := map[string]TokenFlow{
		"mask":               flow(TierTokens{CacheWrite5m: 1000, CacheRead: 300}, TierTokens{CacheWrite5m: 300}),
		"extract":            flow(TierTokens{CacheRead: 1500, Input: 500}, TierTokens{Input: 40}),
		"idle":               {},
		"tailbreakpoint":     flow(TierTokens{Input: 200}, TierTokens{CacheWrite5m: 200}),
		"headttl":            {},
		EndedChangeComponent: {},
	}
	if len(byName) != len(want) {
		t.Fatalf("rows %v, want %v", byName, want)
	}
	for name, w := range want {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("no %s row", name)
		}
		if c.Tokens != w {
			t.Errorf("%s tokens = %+v, want %+v", name, c.Tokens, w)
		}
		dollars := tokUSD(c.Tokens)
		switch name {
		case "headttl":
			dollars += tokUSD(e.AccTokens.TTL)
		case EndedChangeComponent:
			dollars += tokUSD(e.AccTokens.EndedBreak)
		}
		if !accNear(c.SavedUSD, dollars) {
			t.Errorf("%s: SavedUSD %v, tokens price to %v", name, c.SavedUSD, dollars)
		}
	}
	wantFx := AccEffectTokens{
		KeepAlive:  flow(TierTokens{CacheWrite5m: 2000}, TierTokens{CacheRead: 2000}),
		Split:      flow(TierTokens{CacheWrite5m: 700}, TierTokens{CacheRead: 700}),
		TTL:        flow(TierTokens{CacheWrite5m: 100}, TierTokens{CacheWrite1h: 100}),
		EndedBreak: flow(TierTokens{CacheRead: 50}, TierTokens{CacheWrite5m: 50}),
	}
	if e.AccTokens != wantFx {
		t.Fatalf("effects = %+v, want %+v", e.AccTokens, wantFx)
	}
	if !accNear(e.KeepAliveSavedUSD, tokUSD(wantFx.KeepAlive)) || !accNear(e.CachesplitSavedUSD, tokUSD(wantFx.Split)) {
		t.Fatalf("keep-alive %v split %v do not match their tokens", e.KeepAliveSavedUSD, e.CachesplitSavedUSD)
	}
}

// An unpriced operation with no tokens adds no token figure, as it adds no dollars.
func TestAccTokens_UnpricedEmptyOpHasNoTokens(t *testing.T) {
	f := figOf(accountant.Result{
		Ops:      []accountant.PricedOp{{OpID: "u", Operator: "context-guru.mask", ChangeID: "c"}},
		Unpriced: []accountant.Unpriced{{OpID: "u", Reason: "no_usage"}},
	})
	if len(f.TokByComponent) != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("tokens %v dollars %v, want none", f.TokByComponent, f.ByComponent)
	}
}

// A held estimate holds its tokens out as it holds its dollars: the component rows, the head-TTL
// legs and the ended-change break. The rescue and the split stay booked.
func TestAccTokens_HeldEstimateHoldsTheTokensOut(t *testing.T) {
	e := tokEvent(1, "s")
	e.accRes.Unpriced = []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}
	e.PriceMode(accTestPrice, true)
	if !e.EstimateHeld {
		t.Fatal("not held")
	}
	for _, c := range e.Components {
		if c.Tokens != (TokenFlow{}) {
			t.Errorf("%s keeps tokens %+v on a held estimate", c.Component, c.Tokens)
		}
	}
	if e.AccTokens.TTL != (TokenFlow{}) || e.AccTokens.EndedBreak != (TokenFlow{}) {
		t.Errorf("held legs keep tokens: %+v", e.AccTokens)
	}
	if e.AccTokens.KeepAlive == (TokenFlow{}) || e.AccTokens.Split == (TokenFlow{}) {
		t.Errorf("the rescue and the split lost their tokens: %+v", e.AccTokens)
	}
}

// Pricing again with no result (a fallback) leaves no token figure from the earlier pricing.
func TestAccTokens_AFallbackClearsTheTokens(t *testing.T) {
	e := tokEvent(1, "s")
	e.PriceMode(accTestPrice, true)
	e.accRes = nil
	e.PriceMode(accTestPrice, true)
	if e.AccountantFallback() != FallbackNoResult {
		t.Fatalf("fallback %q", e.AccountantFallback())
	}
	if e.AccTokens != (AccEffectTokens{}) {
		t.Errorf("effects kept: %+v", e.AccTokens)
	}
	for _, c := range e.Components {
		if c.Tokens != (TokenFlow{}) {
			t.Errorf("%s kept tokens %+v", c.Component, c.Tokens)
		}
	}
}

// accTokensFixture stores, in one session: two priced requests, one request the library could not
// price, one legacy row (Price only), and one keep-alive ping. A second session holds one more priced
// request, which no session-scoped read may count.
func accTokensFixture(t *testing.T, db *DB, now int64) {
	t.Helper()
	a, b := tokEvent(now-5000, "s1"), tokEvent(now-4000, "s1")
	fell := tokEvent(now-3000, "s1")
	fell.SetAccountantContext(1, false) // ops_dropped: the library's figure does not apply
	legacy := tokEvent(now-2000, "s1")
	other := tokEvent(now-1000, "s2")
	for _, e := range []*Event{a, b, fell, other} {
		e.PriceMode(accTestPrice, true)
	}
	legacy.Price(accTestPrice, true)
	if !a.AccountantPriced || fell.AccountantPriced || legacy.AccountantPriced {
		t.Fatalf("fixture: priced %v %v %v", a.AccountantPriced, fell.AccountantPriced, legacy.AccountantPriced)
	}
	ping := &Event{TS: now - 1500, SessionID: "s1", Model: "m", TenantID: "t1", KeepAlive: true,
		FreshInput: 3, CacheRead: 9000, CacheWrite: 30, CacheWrite1h: 10, OutputTokens: 1,
		TokenAccounting: AccountingComplete}
	if err := db.insertBatch([]*Event{a, b, fell, legacy, ping, other}); err != nil {
		t.Fatal(err)
	}
}

func TestAccTokens_StatsSumByFeatureAndCountTheUnmeasured(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	accTokensFixture(t, db, now)
	o, err := db.Overview(Filter{Tenant: "t1", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	two := func(f TokenFlow) TokenFlow { f.add(f); return f }
	want := map[string]TokenFlow{
		"mask":               two(flow(TierTokens{CacheWrite5m: 1000, CacheRead: 300}, TierTokens{CacheWrite5m: 300})),
		"extract":            two(flow(TierTokens{CacheRead: 1500, Input: 500}, TierTokens{Input: 40})),
		"tailbreakpoint":     two(flow(TierTokens{Input: 200}, TierTokens{CacheWrite5m: 200})),
		"keepalive":          two(flow(TierTokens{CacheWrite5m: 2000}, TierTokens{CacheRead: 2000})),
		"cachesplit":         two(flow(TierTokens{CacheWrite5m: 700}, TierTokens{CacheRead: 700})),
		"ttl":                two(flow(TierTokens{CacheWrite5m: 100}, TierTokens{CacheWrite1h: 100})),
		EndedChangeComponent: two(flow(TierTokens{CacheRead: 50}, TierTokens{CacheWrite5m: 50})),
	}
	if !reflect.DeepEqual(o.Tokens.ByFeature, want) {
		t.Fatalf("by feature:\n got %+v\nwant %+v", o.Tokens.ByFeature, want)
	}
	var total TokenFlow
	for _, f := range want {
		total.add(f)
	}
	if o.Tokens.Saved != total.Saved || o.Tokens.Caused != total.Caused {
		t.Errorf("totals %+v / %+v, want %+v", o.Tokens.Saved, o.Tokens.Caused, total)
	}
	// The fallback request and the legacy row: no token figure. The ping is not an agent request.
	if o.TokensUnmeasuredRequests != 2 {
		t.Errorf("unmeasured = %d, want 2", o.TokensUnmeasuredRequests)
	}
	if want := (TierTokens{Input: 3, CacheRead: 9000, CacheWrite5m: 20, CacheWrite1h: 10}); o.KeepAlivePingTokens != want || o.KeepAlivePingOutputTokens != 1 {
		t.Errorf("ping tokens %+v output %d, want %+v and 1", o.KeepAlivePingTokens, o.KeepAlivePingOutputTokens, want)
	}
	// The stored dollars agree with the stored tokens, feature by feature.
	if !accNear(o.KeepAliveSavedUSD, tokUSD(want["keepalive"])) || !accNear(o.CachesplitSavedUSD, tokUSD(want["cachesplit"])) {
		t.Errorf("keep-alive %v split %v, tokens price to %v and %v", o.KeepAliveSavedUSD, o.CachesplitSavedUSD,
			tokUSD(want["keepalive"]), tokUSD(want["cachesplit"]))
	}
}

// The full and the lean /api/stats report the same tokens, for a session and for a time window.
func TestAccTokens_LeanAgreesWithFull(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	accTokensFixture(t, db, now)
	for name, f := range map[string]Filter{
		"session": {Tenant: "t1", Session: "s1"},
		"window":  {Tenant: "t1", Since: now - 60_000},
	} {
		full, err := db.Overview(f)
		if err != nil {
			t.Fatal(err)
		}
		lean, err := db.OverviewLean(f, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(full.Tokens.ByFeature) == 0 {
			t.Fatalf("%s: no tokens at all", name)
		}
		fj, _ := json.Marshal(full.Tokens)
		lj, _ := json.Marshal(lean.Tokens)
		if string(fj) != string(lj) || full.TokensUnmeasuredRequests != lean.TokensUnmeasuredRequests ||
			full.KeepAlivePingTokens != lean.KeepAlivePingTokens || full.KeepAlivePingOutputTokens != lean.KeepAlivePingOutputTokens {
			t.Errorf("%s: full %s (%d unmeasured)\nlean %s (%d unmeasured)", name, fj, full.TokensUnmeasuredRequests,
				lj, lean.TokensUnmeasuredRequests)
		}
	}
}

// Rows written before the token columns read 0 and count as unmeasured; an empty period answers an
// empty object, not null.
func TestAccTokens_EmptyAndOldRows(t *testing.T) {
	db := openTestDB(t)
	o, err := db.Overview(Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(o.Tokens)
	if string(b) != `{"saved":{"input":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0},"caused":{"input":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0},"by_feature":{}}` {
		t.Errorf("empty tokens = %s", b)
	}
	old := mkEvent(1000, "s", "m", 100, 90)
	old.TenantID = "t1"
	if err := db.insertBatch([]*Event{old}); err != nil {
		t.Fatal(err)
	}
	o, err = db.Overview(Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.TokensUnmeasuredRequests != 1 || len(o.Tokens.ByFeature) != 0 {
		t.Errorf("old row: unmeasured %d, features %v", o.TokensUnmeasuredRequests, o.Tokens.ByFeature)
	}
}

// The schema's ddl and the additive migration name the same token columns, so a fresh database and
// an upgraded one have the same shape.
func TestAccTokenColumnsExistOnAFreshDatabase(t *testing.T) {
	db := openTestDB(t)
	has := func(table string) map[string]bool {
		rows, err := db.sql.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[n] = true
		}
		return out
	}
	req, comp := has("requests"), has("request_components")
	for _, c := range requestTokenColumns() {
		if !req[c] {
			t.Errorf("requests has no %s", c)
		}
	}
	for _, c := range compTokenColumns {
		if !comp[c] {
			t.Errorf("request_components has no %s", c)
		}
	}
	if n := len(requestTokenColumns()); n != 32 || len(requestTokenArgs(AccEffectTokens{})) != n {
		t.Errorf("request token columns %d, args %d, want 32", n, len(requestTokenArgs(AccEffectTokens{})))
	}
}

// A database from before the token columns gets them on open and keeps its rows.
func TestAccTokenColumnsAreAddedToAnOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 5)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range requestTokenColumns() {
		if _, err := db.sql.Exec(`ALTER TABLE requests DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range compTokenColumns {
		if _, err := db.sql.Exec(`ALTER TABLE request_components DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	o, err := db2.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 1 || o.TokensUnmeasuredRequests != 1 {
		t.Fatalf("requests %d unmeasured %d, want 1 and 1", o.Requests, o.TokensUnmeasuredRequests)
	}
}

// A stored request reads back the tokens the live event showed: by id (/api/requests/{id}) with
// each component's tokens, and in the list (/api/requests).
func TestAccTokens_RoundTripThroughTheStore(t *testing.T) {
	db := openTestDB(t)
	e := tokEvent(1000, "s1")
	e.PriceMode(accTestPrice, true)
	if e.AccTokens == (AccEffectTokens{}) {
		t.Fatal("fixture: no effect tokens")
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Request(e.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccTokens != e.AccTokens {
		t.Errorf("acc_tokens read back %+v, wrote %+v", got.AccTokens, e.AccTokens)
	}
	if len(got.Components) != len(e.Components) {
		t.Fatalf("components read back %d, wrote %d", len(got.Components), len(e.Components))
	}
	for i, c := range e.Components {
		if got.Components[i].Component != c.Component || got.Components[i].Tokens != c.Tokens {
			t.Errorf("component %d: read back %s %+v, wrote %s %+v", i, got.Components[i].Component,
				got.Components[i].Tokens, c.Component, c.Tokens)
		}
	}
	page, err := db.Requests(Filter{TenantAll: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 || page.Requests[0].AccTokens != e.AccTokens {
		t.Errorf("list read back %+v, wrote %+v", page.Requests, e.AccTokens)
	}
}
