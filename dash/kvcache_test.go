package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// The KV-cache page's DB and API layer.
//
// The arithmetic itself is asserted in package kvcache, which needs no database. What is
// asserted here is the part only this layer can get wrong: the SQL derivation of each
// request's successor, the tier reconstruction over history that recorded none, the derived
// filters, the JSON on the wire, and the two guards this dashboard has broken before — a
// keep-alive ping row inside an agent aggregate, and a cap applied without saying so.

// kvEvent builds one request row for the KV-cache dataset. Deliberately not mkEvent: that one
// carries a component report and a compaction, and every field here is about cache tiers.
func kvEvent(tenant, session, model string, ts int64, read, write int64) *Event {
	return &Event{TS: ts, TenantID: tenant, SessionID: session, Model: model,
		Provider: "anthropic", Agent: "claude-code", Mode: ModeActive,
		TokensBefore: 1000, TokensAfter: 1000, FreshInput: 100, OutputTokens: 50,
		CacheRead: read, CacheWrite: write, CostUSD: 0.5, UpstreamMs: 1000,
		TokenAccounting: AccountingComplete, CacheMissReason: CacheHit,
		CacheTTL: "ephemeral_5m"}
}

// seedKV writes rows and returns the DB.
func seedKV(t *testing.T, evs ...*Event) *DB {
	t.Helper()
	db := openTestDB(t)
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	return db
}

const kvBase = int64(1_786_967_311_185)

func allTenants() Filter { return Filter{TenantAll: true} }

// The successor of a request comes from SQL, over (tenant, session) — the PAIR. A session id
// is client-supplied, so two accounts can present the same one; partitioned on the session
// alone this dataset derives a 1-second gap where the truth is that neither account came back.
func TestKVCacheDatasetDerivesTheSuccessorPerAccountAndSession(t *testing.T) {
	shared := "same-session-id"
	db := seedKV(t,
		kvEvent("tenant-a", shared, "m", kvBase, 1000, 0),
		kvEvent("tenant-b", shared, "m", kvBase+1_000, 1000, 0),
		kvEvent("tenant-a", shared, "m", kvBase+60_000, 1000, 0),
	)
	rows, total, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("read %d of %d rows, want 3 of 3", len(rows), total)
	}
	byKey := map[kvcache.Conversation][]*kvcache.Request{}
	for _, r := range rows {
		byKey[r.Key()] = append(byKey[r.Key()], r)
	}
	a := byKey[kvcache.Conversation{User: "tenant-a", Conversation: shared, Model: "m"}]
	if len(a) != 2 {
		t.Fatalf("tenant-a has %d rows", len(a))
	}
	if idle, ok := a[0].Idle(); !ok || idle != 60*time.Second {
		t.Errorf("tenant-a's gap = %v (ok=%v), want 60s — tenant-b must not shorten it", idle, ok)
	}
	b := byKey[kvcache.Conversation{User: "tenant-b", Conversation: shared, Model: "m"}]
	if len(b) != 1 || b[0].HasNext {
		t.Errorf("tenant-b has one request and no successor; got %d rows, has_next=%v",
			len(b), len(b) > 0 && b[0].HasNext)
	}
}

// A keep-alive PING row is not in this dataset. Counted, it would split one real idle gap into
// two short ones and make every reuse probability on the page wrong in the flattering
// direction — which is the whole reason Filter.where() has that predicate.
func TestKVCacheDatasetExcludesKeepAlivePings(t *testing.T) {
	ping := kvEvent("t", "s", "m", kvBase+280_000, 1000, 0)
	ping.KeepAlive = true
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 1000, 0),
		ping,
		kvEvent("t", "s", "m", kvBase+600_000, 1000, 0),
	)
	rows, total, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("the ping is in the dataset: %d of %d rows", len(rows), total)
	}
	if idle, _ := rows[0].Idle(); idle != 600*time.Second {
		t.Errorf("the gap is %v; the ping split one 600 s idle span into two", idle)
	}
	for _, r := range rows {
		if r.KeepAlive {
			t.Error("a ping row reached the analysis dataset")
		}
	}
}

// The tier is reconstructed with three different degrees of confidence, and the page counts
// each separately: `cache_ttl` arrived as an ADDITIVE column defaulting to ”, so on a row
// written before it a blank means NOT RECORDED, and reporting that as "no cache_control" would
// say a deployment that has always cached never cached at all.
func TestObservedTTLSaysHowWellItKnows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recorded   string
		write1h    int64
		cached     int64
		wantTTL    kvcache.TTL
		wantSource string
	}{
		{"the row recorded its own tier", "ephemeral_1h", 0, 50_000,
			kvcache.TTL1h, kvcache.TTLSourceConfigured},
		{"a 5m tier recorded", "ephemeral_5m", 0, 50_000,
			kvcache.TTL5m, kvcache.TTLSourceConfigured},
		{"no tier, but the provider billed at the 1h tier", "", 40_000, 50_000,
			kvcache.TTL1h, kvcache.TTLSourceObserved},
		{"no tier, something was cached", "", 0, 50_000,
			kvcache.TTL5m, kvcache.TTLSourceUnknown},
		{"no tier and nothing cached at all", "", 0, 0,
			kvcache.TTLNone, kvcache.TTLSourceObserved},
		{"an unrecognised tier is not trusted", "ephemeral_10m", 0, 50_000,
			kvcache.TTL5m, kvcache.TTLSourceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, src := observedTTL(tc.recorded, tc.write1h, tc.cached)
			if got != tc.wantTTL || src != tc.wantSource {
				t.Errorf("observedTTL(%q, %d, %d) = %q/%s, want %q/%s",
					tc.recorded, tc.write1h, tc.cached, got, src, tc.wantTTL, tc.wantSource)
			}
		})
	}
}

// The coverage panel counts the three tier states rather than blending them.
func TestAnalysisCountsTheThreeTierStates(t *testing.T) {
	configured := kvEvent("t", "s1", "m", kvBase, 1000, 100)
	observed1h := kvEvent("t", "s2", "m", kvBase+1000, 1000, 100)
	observed1h.CacheTTL, observed1h.CacheWrite1h = "", 100
	unknown := kvEvent("t", "s3", "m", kvBase+2000, 1000, 100)
	unknown.CacheTTL = ""
	uncached := kvEvent("t", "s4", "m", kvBase+3000, 0, 0)
	uncached.CacheTTL = ""
	db := seedKV(t, configured, observed1h, unknown, uncached)

	out, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := out.Coverage
	if c.TTLConfigured != 1 || c.TTLObserved != 2 || c.TTLUnknown != 1 {
		t.Errorf("coverage = %+v; want 1 configured, 2 observed (one 1h, one uncached), 1 unknown", c)
	}
	if c.SingleRequestConversations != 4 {
		t.Errorf("single-request conversations = %d, want 4 — the page must say so, or it looks "+
			"like most of the traffic was thrown away", c.SingleRequestConversations)
	}
}

// The summary cards: every idle average is over the requests that HAVE a successor, and the
// final requests are reported beside them rather than folded in as zero gaps.
func TestSummaryCardsKeepFinalRequestsOutOfEveryAverage(t *testing.T) {
	// One conversation: gaps of 10 s, 600 s, 10 s. Four requests, three gaps, one final.
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 100_000, 0),
		kvEvent("t", "s", "m", kvBase+10_000, 100_000, 0),
		kvEvent("t", "s", "m", kvBase+610_000, 100_000, 0),
		kvEvent("t", "s", "m", kvBase+620_000, 100_000, 0),
	)
	out, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := out.Cards
	if c.Requests != 4 || c.WithNext != 3 || c.FinalRequests != 1 {
		t.Fatalf("requests=%d with_next=%d final=%d, want 4/3/1",
			c.Requests, c.WithNext, c.FinalRequests)
	}
	if c.Conversations != 1 {
		t.Errorf("conversations = %d, want 1", c.Conversations)
	}
	// Mean over the three real gaps is (10 + 600 + 10)/3 = 206.67 s. Counting the final
	// request as a zero would give 155 s.
	if want := 206_666.6667; c.MeanIdleMs < want-1 || c.MeanIdleMs > want+1 {
		t.Errorf("mean idle = %.1f ms, want ~%.1f (three gaps, not four)", c.MeanIdleMs, want)
	}
	if c.MedianIdleMs != 10_000 {
		t.Errorf("median idle = %.0f ms, want 10000", c.MedianIdleMs)
	}
	// Two of three gaps are inside five minutes; all three are inside an hour.
	if c.Within5m != 2 || c.Within1h != 3 {
		t.Errorf("within 5m/1h = %d/%d, want 2/3", c.Within5m, c.Within1h)
	}
	if got, want := c.Within5mPct, 200.0/3; got < want-0.01 || got > want+0.01 {
		t.Errorf("within_5m_pct = %.4f, want %.4f (denominator is with_next, not requests)",
			got, want)
	}
	if c.Within1hPct != 100 {
		t.Errorf("within_1h_pct = %.2f, want 100", c.Within1hPct)
	}
	if c.CachedContextP50 != 100_000 {
		t.Errorf("median cached context = %d, want 100000", c.CachedContextP50)
	}
	near2(t, "cost", c.CostUSD, 4*0.5)
}

// The idle histogram's 5-minute and 1-hour edges are load-bearing: every percentage on the
// page is measured against them, so a band that straddled either would make the chart and the
// cards disagree.
func TestIdleHistogramEdgesLineUpWithTheHorizons(t *testing.T) {
	var have5m, have1h bool
	for _, e := range idleEdges {
		if e == kvcache.Horizon5m.Seconds() {
			have5m = true
		}
		if e == kvcache.Horizon1h.Seconds() {
			have1h = true
		}
	}
	if !have5m || !have1h {
		t.Fatalf("idleEdges %v does not have an edge at 300 s and at 3600 s", idleEdges)
	}
	// A gap of exactly five minutes belongs to the band STARTING at five minutes, and that
	// band is the first one marked beyond a five-minute lifetime.
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 1000, 0),
		kvEvent("t", "s", "m", kvBase+300_000, 1000, 0),
	)
	out, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, b := range out.IdleBands {
		n += b.N
		if strings.HasPrefix(b.Label, "5m") && (b.N != 1 || !b.Beyond) {
			t.Errorf("the 5m band holds %d gaps (beyond=%v), want 1 and beyond", b.N, b.Beyond)
		}
	}
	if n != 1 {
		t.Errorf("the histogram holds %d gaps; only the request WITH a successor has one", n)
	}
}

// The survival view is the empirical CDF of the idle gap, and the two provider horizons are ON
// its ladder rather than interpolated between rungs.
func TestSurvivalCurveSamplesTheTwoHorizonsExactly(t *testing.T) {
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 1000, 0),
		kvEvent("t", "s", "m", kvBase+10_000, 1000, 0),    // 10 s gap
		kvEvent("t", "s", "m", kvBase+1_810_000, 1000, 0), // 30 min gap
		kvEvent("t", "s", "m", kvBase+9_000_000, 1000, 0), // ~2 h gap
	)
	out, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	at := map[float64]SurvivalPoint{}
	for _, p := range out.Survival {
		at[p.Seconds] = p
	}
	for _, s := range []float64{kvcache.Horizon5m.Seconds(), kvcache.Horizon1h.Seconds()} {
		if _, ok := at[s]; !ok {
			t.Fatalf("the survival ladder has no rung at %.0f s", s)
		}
	}
	if got := at[300]; got.Arrived != 1 || got.ArrivedPct < 33 || got.ArrivedPct > 34 {
		t.Errorf("by 5 minutes: %d of 3 gaps (%.1f%%), want 1 (33.3%%)", got.Arrived, got.ArrivedPct)
	}
	if got := at[3600]; got.Arrived != 2 {
		t.Errorf("by an hour: %d of 3 gaps, want 2", got.Arrived)
	}
	if got := at[21600]; got.Arrived != 3 || got.ArrivedPct != 100 {
		t.Errorf("by six hours: %d of 3 gaps (%.1f%%), want all of them",
			got.Arrived, got.ArrivedPct)
	}
}

// The three derived filters narrow the same dataset the summary is computed from, so the table
// and the cards cannot disagree.
func TestDerivedFiltersNarrowTheDataset(t *testing.T) {
	long := kvEvent("t", "s", "m", kvBase, 1000, 0)
	long.CacheTTL = "ephemeral_1h"
	// A request that carried no cache_control at all: no recorded tier and nothing billed at a
	// cache tier either, which is the one combination that reads as "cached nothing".
	uncached := kvEvent("u", "s2", "m", kvBase+20_000, 0, 0)
	uncached.CacheTTL = ""
	db := seedKV(t, long,
		kvEvent("t", "s", "m", kvBase+10_000, 1000, 0),
		uncached,
	)
	for _, tc := range []struct {
		name string
		o    KVCacheOptions
		want int
	}{
		{"unfiltered", KVCacheOptions{}, 3},
		{"only requests with a successor", KVCacheOptions{HasNext: "yes"}, 1},
		{"only final requests", KVCacheOptions{HasNext: "no"}, 2},
		{"only the 1h tier", KVCacheOptions{TTL: string(kvcache.TTL1h)}, 1},
		{"only the 5m tier", KVCacheOptions{TTL: string(kvcache.TTL5m)}, 1},
		{"only requests that cached nothing", KVCacheOptions{TTL: "none"}, 1},
		{"one time-of-day band", KVCacheOptions{Bucket: string(kvcache.BucketAt(kvBase))}, 3},
		{"a different band", KVCacheOptions{Bucket: otherBucket(kvBase)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, err := db.KVCacheDataset(allTenants(), tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.want {
				t.Errorf("got %d rows, want %d", len(rows), tc.want)
			}
		})
	}
}

// otherBucket names a UTC band that is not the one an instant falls in.
func otherBucket(ts int64) string {
	in := kvcache.BucketAt(ts)
	for _, b := range kvcache.Buckets {
		if b != in {
			return string(b)
		}
	}
	return ""
}

// A request with no successor sorts LAST in BOTH directions. Letting a nil idle sort as zero
// would put every final request at the top of an ascending sort as though it had returned
// instantly, which is the same lie as averaging it in.
func TestTheDetailTableSortsFinalRequestsLastInBothDirections(t *testing.T) {
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 1000, 0),
		kvEvent("t", "s", "m", kvBase+10_000, 1000, 0),
		kvEvent("t", "s2", "m", kvBase+20_000, 1000, 0),
	)
	for _, dir := range []string{"asc", "desc"} {
		page, err := db.KVCacheRows(allTenants(), KVCacheOptions{Sort: "idle", Dir: dir, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) != 3 {
			t.Fatalf("%s: %d rows", dir, len(page.Rows))
		}
		if !page.Rows[0].HasNext {
			t.Errorf("%s: a request with no idle time sorted first", dir)
		}
		for _, r := range page.Rows[1:] {
			if r.HasNext {
				t.Errorf("%s: a request WITH an idle time sorted after one without", dir)
			}
		}
	}
}

// Every row carries the way back to the request and to the conversation it came from, so the
// analysis is never a dead end.
func TestEveryTableRowLinksBackToItsRequestAndConversation(t *testing.T) {
	db := seedKV(t, kvEvent("t", "sess/with space", "m", kvBase, 1000, 0))
	page, err := db.KVCacheRows(allTenants(), KVCacheOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("%d rows", len(page.Rows))
	}
	r := page.Rows[0]
	if !strings.HasPrefix(r.RequestURL, "#requests?req=") || r.RequestURL == "#requests?req=0" {
		t.Errorf("request link = %q", r.RequestURL)
	}
	if want := "#sessions?diff=sess%2Fwith%20space"; r.ConversationURL != want {
		t.Errorf("conversation link = %q, want %q — a session id is client-supplied and has to "+
			"be encoded", r.ConversationURL, want)
	}
}

// The whole simulation, end to end over rows in a database: the arms run, the savings are
// measured against one baseline, and a strategy that costs more reports a negative saving.
func TestSimulationRunsEveryArmAndReportsNegativeSavings(t *testing.T) {
	// A conversation that always comes back in ten seconds: the 1-hour tier pays 2.0x input to
	// protect something a 1.25x write already held.
	var evs []*Event
	for i := int64(0); i < 8; i++ {
		evs = append(evs, kvEvent("t", "s", "m", kvBase+i*10_000, 0, 100_000))
	}
	db := seedKV(t, evs...)
	cfg := KVCacheSimConfig{Baseline: KVStrategyFixed5m}
	out, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Baseline != KVStrategyFixed5m {
		t.Fatalf("baseline = %q", out.Baseline)
	}
	if len(out.Results) != len(KVCacheDefaultStrategies) {
		t.Fatalf("ran %d arms, want %d", len(out.Results), len(KVCacheDefaultStrategies))
	}
	if len(out.Savings) != len(out.Results) {
		t.Fatalf("%d savings for %d results", len(out.Savings), len(out.Results))
	}
	byName := map[string]kvcache.Savings{}
	for _, s := range out.Savings {
		byName[s.Strategy] = s
	}
	if s := byName[KVStrategyFixed5m]; s.AbsoluteUSD != 0 || !s.Known {
		t.Errorf("the baseline's own saving must be exactly zero and defined: %+v", s)
	}
	if s := byName[KVStrategyFixed1h]; s.AbsoluteUSD >= 0 {
		t.Errorf("the 1h arm should cost MORE on 10-second gaps; saving = %.6f", s.AbsoluteUSD)
	}
	if s := byName[KVStrategyNoCache]; s.AbsoluteUSD >= 0 {
		t.Errorf("not caching a 100k prefix at all should cost more: %.6f", s.AbsoluteUSD)
	}
	// The observed-policy arm reports how much of itself rested on a recorded tier.
	for _, r := range out.Results {
		if r.Strategy != KVStrategyObserved {
			continue
		}
		if r.ObservedCoverage == nil || r.ObservedCoverage.Recorded != 8 {
			t.Errorf("observed coverage = %+v, want 8 recorded", r.ObservedCoverage)
		}
	}
	if out.WindowEnd != kvBase+7*10_000 {
		t.Errorf("window end = %d; an open idle span must be bounded by the data, never by "+
			"time.Now()", out.WindowEnd)
	}
}

// A baseline the caller did not ask to see is still replayed, because every percentage on the
// page is divided by its total — and an unknown one is an error rather than a substitution.
func TestAnUnrunBaselineIsStillComputedAndAnUnknownOneIsRefused(t *testing.T) {
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 0, 100_000),
		kvEvent("t", "s", "m", kvBase+10_000, 100_000, 0),
	)
	out, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{
			Strategies: []string{KVStrategyFixed1h}, Baseline: KVStrategyNoCache})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Strategy != KVStrategyFixed1h {
		t.Fatalf("the arms on screen changed: %d results", len(out.Results))
	}
	if out.Baseline != KVStrategyNoCache || out.Savings[0].BaselineUSD <= 0 {
		t.Errorf("the unrun baseline was not priced: %+v", out.Savings[0])
	}
	if _, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, nil,
		KVCacheSimConfig{Baseline: "not-a-strategy"}); err == nil {
		t.Error("an unknown baseline was accepted; every percentage is divided by it")
	}
	// And an unknown ARM is named rather than silently replaced.
	named, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, nil,
		KVCacheSimConfig{Strategies: []string{KVStrategyFixed5m, "wishful-thinking"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(named.Unknown) != 1 || named.Unknown[0] != "wishful-thinking" {
		t.Errorf("unknown strategies = %v", named.Unknown)
	}
}

// An unpriced model produces no dollars anywhere, and says so. An unpriced request is not a
// free one — the defect class this project has shipped five times.
func TestAnUnpricedModelIsReportedNotZeroed(t *testing.T) {
	db := seedKV(t,
		kvEvent("t", "s", "mystery-model", kvBase, 0, 100_000),
		kvEvent("t", "s", "mystery-model", kvBase+10_000, 100_000, 0),
	)
	out, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.TotalUSD != 0 || r.Unpriced != 2 || r.Requests != 2 {
			t.Errorf("%s: total=%v unpriced=%d requests=%d", r.Strategy, r.TotalUSD,
				r.Unpriced, r.Requests)
		}
	}
	for _, s := range out.Savings {
		if s.Known {
			t.Errorf("%s reported a percentage against a zero baseline", s.Strategy)
		}
	}
	if len(out.Pricing.Models) != 1 || out.Pricing.Models[0].Known {
		t.Errorf("the price list did not report the model as unpriced: %+v", out.Pricing.Models)
	}
}

// A per-model rate typed into the page reaches the simulation, and it is read in USD per
// MILLION tokens — the unit every price page uses, converted once at the boundary.
func TestARateTypedIntoThePageRepricesTheWindow(t *testing.T) {
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 0, 100_000),
		kvEvent("t", "s", "m", kvBase+10_000, 100_000, 0),
	)
	// input $3/MTok, output $15/MTok, read $0.30/MTok, 5m write $3.75/MTok, 1h write $6/MTok.
	req := httptest.NewRequest(http.MethodGet,
		"/api/kvcache/simulate?rate=m:3:15:0.30:3.75:6&strategy=fixed-5m&baseline=no-cache", nil)
	cfg := kvCacheConfigFrom(req)
	ov, ok := cfg.Overrides["m"]
	if !ok {
		t.Fatal("the rate override did not parse")
	}
	if ov.Input == nil || *ov.Input != 3e-6 {
		t.Errorf("input override = %v, want 3e-6 (per-token, from $3 per MTok)", ov.Input)
	}
	if ov.Write1h == nil || *ov.Write1h != 6e-6 {
		t.Errorf("write_1h override = %v, want 6e-6", ov.Write1h)
	}
	out, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("%d arms", len(out.Results))
	}
	r := out.Results[0]
	// A miss writing 100k at $3.75/MTok plus a hit reading 100k at $0.30/MTok, plus 100 fresh
	// input and 50 output on each.
	want := (100*3e-6 + 100_000*3.75e-6 + 50*15e-6) + (100*3e-6 + 100_000*0.3e-6 + 50*15e-6)
	near2(t, "the repriced total", r.TotalUSD, want)
	if out.Pricing.Models[0].Source != "override" {
		t.Errorf("source = %q; nobody may read a typed number as a configured one",
			out.Pricing.Models[0].Source)
	}
}

// A malformed rate is DROPPED rather than half-applied: a partially parsed price is a wrong
// price, and the panel shows what it got back so an ignored edit is visible.
func TestAMalformedRateIsDroppedWholeAndAModelNameMayContainSlashes(t *testing.T) {
	for _, spec := range []string{"m:3:15", "m:3:15:0.3:3.75", ":1:2:3:4:5", "m:x:15:0.3:3.75:6",
		"m:-1:15:0.3:3.75:6", "m:::::"} {
		if _, _, ok := parseRateOverride(spec); ok {
			t.Errorf("%q was accepted", spec)
		}
	}
	// A gateway route name carries a slash and a dot, and needs no escaping.
	model, ov, ok := parseRateOverride("us.anthropic/claude-opus-5:3:15::3.75:6")
	if !ok {
		t.Fatal("a qualified model id was rejected")
	}
	if model != "us.anthropic/claude-opus-5" {
		t.Errorf("model = %q", model)
	}
	if ov.CacheRead != nil {
		t.Error("an empty field must mean 'not overridden', not zero")
	}
	if ov.Write5m == nil || *ov.Write5m != 3.75e-6 {
		t.Errorf("write_5m = %v", ov.Write5m)
	}
}

// The keep-alive schedule mirrors the one the keep-alive tab's own calculator uses, and the
// two arithmetics cannot drift because this asserts they agree.
//
// They are separate functions because the dependency runs one way: dash imports kvcache, so
// kvcache cannot call back into this package. A shared table is the alternative to a shared
// function, and this is that table.
func TestPingScheduleMatchesTheKeepAliveTab(t *testing.T) {
	for _, gap := range []float64{0, 1, 279, 280, 281, 559, 560, 600, 1800, 3600, 14400, 86400} {
		for _, idle := range []float64{60, 280, 300, 3360} {
			for k := 0; k <= 4; k++ {
				want := PingsPerSpan(gap, idle, k)
				got := kvcache.PingsPerSpan(time.Duration(gap)*time.Second,
					time.Duration(idle)*time.Second, k)
				if got != want {
					t.Fatalf("gap=%.0f idle=%.0f k=%d: kvcache says %d, the keep-alive tab says %d",
						gap, idle, k, got, want)
				}
			}
		}
	}
}

// The four routes answer on an EMPTY database, which is the state every new deployment is in
// and the one an empty-panel bug hides in.
func TestKVCacheRoutesAnswerOnAnEmptyDatabase(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	for _, path := range []string{"/api/kvcache", "/api/kvcache/rows",
		"/api/kvcache/simulate", "/api/kvcache/pricing"} {
		w, body := get(t, a, path, "")
		if w.Code != http.StatusOK {
			t.Errorf("%s -> %d: %s", path, w.Code, w.Body.String())
		}
		if body == nil {
			t.Errorf("%s served no JSON object", path)
		}
	}
}

// The wire shape: the fields the page reads are present and spelled as the UI expects, and an
// idle time is `null` on a final request rather than 0.
func TestKVCacheJSONSerialization(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec,
		kvEvent("", "s", "aws/claude-sonnet-5", kvBase, 0, 100_000),
		kvEvent("", "s", "aws/claude-sonnet-5", kvBase+10_000, 100_000, 0),
	)
	w, body := get(t, a, "/api/kvcache", "")
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"cards", "idle_bands", "by_ttl", "by_user", "by_model",
		"by_bucket", "survival", "coverage", "assumptions", "pricing", "scanned", "total",
		"truncated"} {
		if _, ok := body[key]; !ok {
			t.Errorf("/api/kvcache has no %q", key)
		}
	}
	cards, _ := body["cards"].(map[string]any)
	for _, key := range []string{"requests", "conversations", "with_next", "final_requests",
		"median_idle_ms", "mean_idle_ms", "within_5m_pct", "within_1h_pct", "hit_rate_pct",
		"cost_usd", "cached_context_p50"} {
		if _, ok := cards[key]; !ok {
			t.Errorf("cards has no %q", key)
		}
	}
	assumptions, _ := body["assumptions"].(map[string]any)
	if assumptions["time_zone"] != "UTC" {
		t.Errorf("the page does not state its timezone: %v", assumptions["time_zone"])
	}
	if f, _ := assumptions["formulas"].([]any); len(f) < 8 {
		t.Errorf("the cost formulas are not on the wire: %d of them", len(f))
	}

	// The rows route, and the nil idle.
	rw, _ := get(t, a, "/api/kvcache/rows?limit=10&sort=ts&dir=asc", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("rows -> %d: %s", rw.Code, rw.Body.String())
	}
	var page struct {
		Rows []struct {
			ID       int64  `json:"id"`
			HasNext  bool   `json:"has_next"`
			IdleMs   *int64 `json:"idle_ms"`
			TTL      string `json:"ttl"`
			Source   string `json:"ttl_source"`
			Cached   int64  `json:"cached_context_tokens"`
			ReqURL   string `json:"request_url"`
			ConvURL  string `json:"conversation_url"`
			Within5m bool   `json:"within_5m"`
		} `json:"rows"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 2 || page.Total != 2 {
		t.Fatalf("%d of %d rows", len(page.Rows), page.Total)
	}
	if page.Rows[0].IdleMs == nil || *page.Rows[0].IdleMs != 10_000 || !page.Rows[0].Within5m {
		t.Errorf("the first row's idle = %v", page.Rows[0].IdleMs)
	}
	if page.Rows[1].HasNext || page.Rows[1].IdleMs != nil {
		t.Errorf("the final row must serialize idle as null, got %v", page.Rows[1].IdleMs)
	}
	if !strings.Contains(rw.Body.String(), `"idle_ms":null`) {
		t.Error("a final request's idle time is not null on the wire; 0 would read as " +
			"'it came back instantly'")
	}
	if page.Rows[0].Cached != 100_000 || page.Rows[0].TTL != "ephemeral_5m" {
		t.Errorf("row = %+v", page.Rows[0])
	}
}

// The pricing route shows the configured rate AND what it comes to on this window's own
// median prefix. A per-token rate is not a number anybody can act on.
func TestPricingRouteShowsBothTheRateAndItsCost(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	seed(t, rec,
		kvEvent("", "s", "aws/claude-sonnet-5", kvBase, 0, 100_000),
		kvEvent("", "s", "aws/claude-sonnet-5", kvBase+10_000, 100_000, 0),
	)
	w, body := get(t, a, "/api/kvcache/pricing", "")
	if w.Code != http.StatusOK {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	if got, _ := body["prefix_tokens"].(float64); got != 100_000 {
		t.Errorf("median prefix = %v, want 100000", got)
	}
	costs, _ := body["costs"].([]any)
	if len(costs) != 1 {
		t.Fatalf("%d cost rows", len(costs))
	}
	c, _ := costs[0].(map[string]any)
	for _, key := range []string{"uncached_usd", "read_usd", "write_5m_usd", "write_1h_usd",
		"keep_alive_usd", "late_5m_usd", "late_1h_usd", "hold_5m_usd", "hold_1h_usd"} {
		if _, ok := c[key]; !ok {
			t.Errorf("the cost row has no %q", key)
		}
	}
	// One hit on 100k at the IBM read rate.
	near2(t, "read cost", c["read_usd"].(float64), 100_000*ibmSonnet.CacheRead)
	// The 1h tier is 2.0x input, derived because no gateway publishes it.
	near2(t, "1h write cost", c["write_1h_usd"].(float64), 100_000*2*ibmSonnet.Input)
	if c["write_1h_usd"].(float64) <= c["write_5m_usd"].(float64) {
		t.Error("the 1h write is not dearer than the 5m one")
	}
}

// The cap is announced. A silent top-N would read as "this is your whole history", which is
// the failure this dashboard has already shipped once.
func TestATruncatedAnalysisSaysSo(t *testing.T) {
	db := seedKV(t, kvEvent("t", "s", "m", kvBase, 1000, 0))
	out, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Truncated || out.Scanned != out.Total || out.Total != 1 {
		t.Errorf("an untruncated window reported scanned=%d total=%d truncated=%v",
			out.Scanned, out.Total, out.Truncated)
	}
	if kvCacheMaxRows < 100_000 {
		t.Errorf("the analysis cap is %d rows, which is below the live service's own window",
			kvCacheMaxRows)
	}
}

// near2 is a float comparison for this file. Named apart from package kvcache's own helper
// because the two files are in different packages and the tolerance here is a money one.
func near2(t *testing.T, what string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("%s = %.12f, want %.12f", what, got, want)
	}
}
