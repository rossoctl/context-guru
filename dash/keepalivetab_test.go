package dash

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The keep-alive tab's arithmetic and its attribution, and the tests are mostly about the
// second: the delicate part of this feature is not the panels, it is that a ping must appear in
// the money and nowhere else.

// kaFixture builds a database with agent traffic, ping rows and addressable TTL expiries.
//
// Ping rows carry KeepAlive and a cost; the credit lives on the AGENT row that benefited, which
// is where the write path puts it. Both halves are needed for the attribution tests to be able
// to fail.
type kaFixture struct {
	db *DB
}

// kaAgent is one agent request.
func kaAgent(ts int64, session string, cost float64) *Event {
	e := mkEvent(ts, session, "aws/claude-sonnet-5", 100, 90)
	e.CostUSD, e.CacheRead, e.CacheWrite, e.UpstreamMs = cost, 50_000, 0, 800
	return e
}

// kaExpiry is an agent request that resumed after `gapS` of idle and paid to re-create its
// prefix. cache_write > 0 makes it ADDRESSABLE — the 385 phantom ttl_expiry rows in production
// have cache_write = 0 and are all HTTP 400.
func kaExpiry(ts int64, session string, gapS float64, cost float64, prevPrefix int64) []*Event {
	prev := kaAgent(ts-int64(gapS*1000), session, 0.05)
	prev.CacheRead, prev.CacheWrite = prevPrefix, 0
	miss := kaAgent(ts, session, cost)
	miss.CacheMissReason = "ttl_expiry"
	miss.CacheRead, miss.CacheWrite = 0, prevPrefix
	return []*Event{prev, miss}
}

// kaPing is one keep-alive ping row: a real upstream request, billed, marked.
func kaPing(ts int64, session string, cost float64, read, write int64) *Event {
	return &Event{
		TS: ts, SessionID: session, Model: "aws/claude-sonnet-5", Provider: "anthropic",
		Agent: "claude-code", Preset: "codesmart", Mode: ModeActive, Status: 200,
		KeepAlive: true, CacheRead: read, CacheWrite: write, OutputTokens: 1,
		CostUSD: cost, Meta: Meta{MaxTokens: 1}, UpstreamMs: 4000,
		TokenAccounting: AccountingComplete,
	}
}

// kaCredit is an agent row carrying a keep-alive credit — the rescued request.
func kaCredit(ts int64, session string, saved float64) *Event {
	e := kaAgent(ts, session, 0.02)
	e.KeepAliveSavedUSD = saved
	return e
}

func newKAFixture(t *testing.T, evs ...*Event) *kaFixture {
	t.Helper()
	db := openTestDB(t)
	if len(evs) > 0 {
		if err := db.insertBatch(evs); err != nil {
			t.Fatal(err)
		}
	}
	return &kaFixture{db: db}
}

// Ping rows must not reach ANY agent aggregate. One fixture, run twice — with the ping rows and
// without them — and every count, every sum over tokens, every average, every countBy map, every
// breakdown dimension and the whole series must be IDENTICAL. Only TotalSavedUSD may move, and
// only by exactly KeepAliveNetUSD.
//
// Asserted by construction rather than field by field: the two Overviews are marshalled and
// compared key by key, so a field ADDED later is covered without anyone remembering to add it
// here. That is the difference between this test and the per-aggregate ones that let /api/prompt
// and three unauthenticated routes ship.
func TestPingRowsStayOutOfAgentAggregates(t *testing.T) {
	agent := []*Event{
		kaAgent(1_000_000, "s1", 0.10), kaAgent(1_100_000, "s1", 0.20),
		kaAgent(1_200_000, "s2", 0.30), kaCredit(1_300_000, "s1", 0.40),
	}
	pings := []*Event{
		kaPing(1_050_000, "s1", 0.01, 50_000, 0), kaPing(1_150_000, "s1", 0.02, 50_000, 0),
		kaPing(1_250_000, "s2", 0.03, 0, 0),
	}
	without := newKAFixture(t, agent...)
	with := newKAFixture(t, append(append([]*Event(nil), agent...), pings...)...)
	f := Filter{TenantAll: true}

	a, err := without.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := with.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	// The keep-alive fields and the two totals they feed are the ONLY keys allowed to differ.
	mayMove := map[string]bool{
		"keepalive_pings": true, "keepalive_ping_usd": true, "keepalive_net_usd": true,
		"total_saved_usd": true,
		// The waterfall carries the ping SPEND as a step of its own, which is the whole point of
		// having it; its reconciliation is TestTheWaterfallReconcilesWithTotalSaved.
		"waterfall": true,
	}
	for k, va := range asMap(t, a) {
		vb := asMap(t, b)[k]
		if mayMove[k] {
			continue
		}
		if !jsonEqual(va, vb) {
			t.Errorf("%s moved when ping rows were present: %v -> %v — a ping is not agent "+
				"traffic and must not reach an agent aggregate", k, va, vb)
		}
	}
	if b.KeepAlivePings != 3 {
		t.Errorf("keepalive_pings = %d, want 3", b.KeepAlivePings)
	}
	// The one permitted movement, and it is exact: total_saved moves by exactly the change in
	// the keep-alive's NET, which here is the negative of what the pings cost. The credit rows
	// are in BOTH fixtures — they are agent rows — so the saving half does not move and the
	// whole difference is the ping spend. That is the assertion that fails on `+= SavedUSD`.
	if got, want := b.TotalSavedUSD-a.TotalSavedUSD, b.KeepAliveNetUSD-a.KeepAliveNetUSD; math.Abs(got-want) > 1e-9 {
		t.Errorf("total_saved moved by %.6f, want exactly the change in keepalive_net_usd %.6f",
			got, want)
	}
	if got := b.TotalSavedUSD - a.TotalSavedUSD; math.Abs(got+b.KeepAlivePingUSD) > 1e-9 {
		t.Errorf("total_saved moved by %.6f; the pings cost %.6f and nothing else changed, so the "+
			"headline must fall by exactly that", got, b.KeepAlivePingUSD)
	}
	// And the derived views, which have their own queries and their own chances to be wrong.
	for _, dim := range []string{"model", "provider", "agent", "preset", "mode", "session",
		"accounting", "cache_miss", "stop_reason"} {
		ga, err := without.db.Breakdown(f, dim)
		if err != nil {
			continue // not every dimension exists on this fixture; the ones that do must match
		}
		gb, err := with.db.Breakdown(f, dim)
		if err != nil {
			t.Fatalf("breakdown %s: %v", dim, err)
		}
		if !jsonEqual(ga, gb) {
			t.Errorf("/api/breakdown?dim=%s changed when ping rows were present", dim)
		}
	}
	sa, err := without.db.Series(f, 86_400_000)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := with.db.Series(f, 86_400_000)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(sa, sb) {
		t.Error("/api/series changed when ping rows were present")
	}
}

// The replay CEILING counts later AGENT turns, not later pings — and the correction is gated so
// it costs nothing where there is nothing to correct.
//
// Two assertions in one test because they constrain each other. The inner count originally had NO
// predicate at all, so a ping counted as a later turn that could replay a reduction; it cannot,
// since a ping is a verbatim resend carrying no new transcript. But `keepalive` is not in
// idx_requests_session, so the obvious fix (`AND p.keepalive = 0` inside the correlated count)
// turned an index-only count into a row fetch per candidate and cost 20% of Overview's whole
// runtime — 44% under -race, enough to blow the perf test's budget on a loaded box. The shipped
// form corrects from the PING side and is skipped entirely when nothing has pinged.
func TestTheReplayCeilingCountsAgentTurnsNotPings(t *testing.T) {
	// One session, three reducing agent turns, and in the second fixture a ping sitting between
	// the second and the third. The ceiling must not notice the ping.
	base := kaAgent(1_000_000, "s1", 0.10)
	base.SavedUnique = 10
	evs := []*Event{
		base,
		kaAgent(1_100_000, "s1", 0.10), kaAgent(1_200_000, "s1", 0.10),
	}
	without := newKAFixture(t, evs...)
	with := newKAFixture(t, append(append([]*Event(nil), evs...),
		kaPing(1_150_000, "s1", 0.01, 50_000, 0))...)
	f := Filter{TenantAll: true}
	a, err := without.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := with.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	// Every row in this fixture reduces by 10 (mkEvent's before/after), so the ceiling is
	// 10x2 + 10x1 + 10x0 = 30: each reduction times the later turns that could replay IT.
	if a.ReplayProjectedTokens != 30 {
		t.Fatalf("the ceiling is %d without any ping; want 10 x (2 + 1 + 0) = 30",
			a.ReplayProjectedTokens)
	}
	if b.ReplayProjectedTokens != a.ReplayProjectedTokens {
		t.Errorf("a ping changed the replay ceiling: %d -> %d. A ping is a verbatim resend of a "+
			"prefix the agent already sent; it carries no new transcript, so it cannot replay a "+
			"reduction, and counting it makes compaction read as realising less of its value than "+
			"it does.", a.ReplayProjectedTokens, b.ReplayProjectedTokens)
	}
	// The correction cannot over-subtract, either: a ping BEFORE the reducing turn inflates
	// nothing, so it must not be deducted.
	early := newKAFixture(t, append(append([]*Event(nil), evs...),
		kaPing(900_000, "s1", 0.01, 50_000, 0))...)
	c, err := early.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReplayProjectedTokens != a.ReplayProjectedTokens {
		t.Errorf("a ping BEFORE the reduction changed the ceiling: %d -> %d; only LATER rows can "+
			"replay it", a.ReplayProjectedTokens, c.ReplayProjectedTokens)
	}
	// And the correction is deliberately NOT scoped to the window, because the main query's inner
	// count is not either: it counts every later turn in the SESSION, so a ping that falls outside
	// the filtered slice still inflates a ceiling computed inside it. Scoping the correction to the
	// window is the plausible-looking change that would silently stop correcting exactly here.
	clipped := Filter{TenantAll: true, Since: 950_000, Until: 1_140_000}
	d, err := with.db.Overview(clipped)
	if err != nil {
		t.Fatal(err)
	}
	e, err := without.db.Overview(clipped)
	if err != nil {
		t.Fatal(err)
	}
	if d.ReplayProjectedTokens != e.ReplayProjectedTokens {
		t.Errorf("with a window that CLIPS THE PING OUT the ceiling still moved: %d -> %d. The "+
			"inner count spans the whole session, so a ping outside the window inflates it just "+
			"the same and the correction has to reach outside too", e.ReplayProjectedTokens,
			d.ReplayProjectedTokens)
	}
}

// The headline takes the keep-alive's NET and not its gross, so a window where the pings cost
// more than they saved makes the total go DOWN.
//
// This is the test that would fail on the tempting version of the change (`+= SavedUSD`), and
// the tempting version is dishonest in exactly one direction: CostUSD excludes ping rows, so the
// spend that bought the saving appears nowhere else in the walk.
func TestTotalSavedTakesTheKeepAliveNetNotItsGross(t *testing.T) {
	base := newKAFixture(t, kaAgent(1_000_000, "s1", 0.10))
	loss := newKAFixture(t,
		kaAgent(1_000_000, "s1", 0.10),
		kaCredit(1_100_000, "s1", 0.01),          // saved a cent
		kaPing(1_050_000, "s1", 0.50, 50_000, 0), // to spend fifty
	)
	f := Filter{TenantAll: true}
	a, err := base.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loss.db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	if b.KeepAliveNetUSD >= 0 {
		t.Fatalf("the fixture is not underwater: net = %.4f", b.KeepAliveNetUSD)
	}
	// The credit row adds its own compaction saving too, so compare against that baseline plus
	// the net rather than against the base alone.
	if b.TotalSavedUSD >= b.NetSavedUSD+b.CachesplitSavedUSD {
		t.Errorf("total_saved %.4f did not fall by the keep-alive's loss (%.4f); the gross was "+
			"used, which presents a saving without the spend that bought it",
			b.TotalSavedUSD, b.KeepAliveNetUSD)
	}
	_ = a
}

// The waterfall's signed steps must sum to total_saved to the cent, keep-alive lines included.
// A walk that does not reconcile is worse than no walk: it invites the reader to find the
// missing money and there is none to find.
func TestTheWaterfallReconcilesWithTotalSaved(t *testing.T) {
	fx := newKAFixture(t,
		kaAgent(1_000_000, "s1", 0.10), kaAgent(1_100_000, "s2", 0.20),
		kaCredit(1_200_000, "s1", 0.40),
		kaPing(1_050_000, "s1", 0.01, 50_000, 0), kaPing(1_150_000, "s2", 0.02, 50_000, 0),
	)
	o, err := fx.db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]float64{}
	for _, s := range o.waterfall() {
		steps[s.Key] = s.DeltaUSD
	}
	// total_saved = -(baseline step) + compaction + cg_llm + cachesplit + keepalive_ping +
	// keepalive_saved, in signed terms: the reductions are negative and the spends positive, so
	// the sum of the non-total steps below the baseline is the negation of what was avoided.
	sum := -(steps["compaction"] + steps["cg_llm"] + steps["cachesplit_saved"] +
		steps["keepalive_ping"] + steps["keepalive_saved"])
	if math.Abs(sum-steps["total_saved"]) > 0.005 {
		t.Errorf("the walk does not reconcile: steps sum to %.4f, total_saved is %.4f",
			sum, steps["total_saved"])
	}
}

// A session with pings and no credits reports a NEGATIVE net, not zero. The cost half comes from
// the ping rows, which the session aggregate cannot see — so a missing second query reads as
// "this session's keep-alive was free", which is the opposite of the truth.
func TestSessionRowKeepAliveCostComesFromThePingRows(t *testing.T) {
	fx := newKAFixture(t,
		kaAgent(1_000_000, "s1", 0.10),
		kaPing(1_050_000, "s1", 0.07, 50_000, 0),
	)
	rows, _, err := fx.db.Sessions(Filter{TenantAll: true}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got *SessionRow
	for _, r := range rows {
		if r.SessionID == "s1" {
			got = r
		}
	}
	if got == nil {
		t.Fatal("session s1 is missing from the list")
	}
	if got.Turns != 1 {
		t.Errorf("turns = %d, want 1 — a ping is not a turn", got.Turns)
	}
	if got.KeepAlivePings != 1 || math.Abs(got.KeepAlivePingUSD-0.07) > 1e-9 {
		t.Errorf("pings = %d at $%.4f, want 1 at $0.07", got.KeepAlivePings, got.KeepAlivePingUSD)
	}
	if got.KeepAliveNetUSD >= 0 {
		t.Errorf("net = %.4f; a session that paid for pings and was never rescued is underwater, "+
			"not free", got.KeepAliveNetUSD)
	}
}

// The coverage arithmetic, and the single error that made an earlier analysis of this mechanism
// wrong by a factor of 4.4.
//
// K*X + TTL, not K*X: the last ping is itself a cache READ, and a read refreshes the entry for
// the provider's full lifetime. A K*X implementation fails every row of this table.
func TestCoverageIncludesTheFinalPingsTTL(t *testing.T) {
	for _, tc := range []struct {
		x    float64
		k    int
		want float64
	}{
		{280, 2, 860}, {280, 1, 580}, {240, 2, 780}, {280, 3, 1140}, {280, 4, 1420},
		{240, 1, 540}, {300, 2, 900},
	} {
		if got := CoverageSeconds(tc.x, tc.k); got != tc.want {
			t.Errorf("CoverageSeconds(%g, %d) = %g, want %g (K*X + 300; a K*X implementation "+
				"under-counts reach by a whole TTL)", tc.x, tc.k, got, tc.want)
		}
	}
	// And the ping count per span, which is the other half of the same arithmetic.
	for _, tc := range []struct {
		gap, x float64
		k      int
		want   int
	}{
		{100, 280, 2, 0}, {280, 280, 2, 0}, {281, 280, 2, 1}, {559, 280, 2, 1},
		// At exactly 2X the second ping is due at the instant the span ends. The formula
		// min(K, floor((gap-X)/X)+1) counts it, and counting the boundary ping is the
		// conservative direction for a COST estimate: it over-states spend, never reach.
		{560, 280, 2, 2}, {561, 280, 2, 2}, {5000, 280, 2, 2}, {5000, 280, 4, 4},
		{5000, 280, 1, 1},
	} {
		if got := PingsPerSpan(tc.gap, tc.x, tc.k); got != tc.want {
			t.Errorf("PingsPerSpan(gap=%g, x=%g, k=%d) = %d, want %d", tc.gap, tc.x, tc.k, got, tc.want)
		}
	}
}

// The calculator prices from the ROW'S OWN MODEL, and refuses to produce a dollar figure for a
// model the operator's list does not carry.
//
// Falling back to a blended average is the defect class this project has hit five times: a
// number that looks like a measurement and is an average of unrelated things.
func TestCalculatorPricesFromTheRowsModelNotABlend(t *testing.T) {
	fx := newKAFixture(t, kaExpiry(2_000_000, "s1", 700, 1.00, 300_000)...)
	f := Filter{TenantAll: true}
	cheap := modelinfo.Price{Input: 1e-6, Output: 5e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}
	dear := modelinfo.Price{Input: 1e-5, Output: 5e-5, CacheRead: 1e-6, CacheWrite: 1.25e-5}
	price := func(m string) (modelinfo.Price, bool) {
		switch m {
		case "cheap":
			return cheap, true
		case "dear":
			return dear, true
		}
		return modelinfo.Price{}, false
	}
	a, err := fx.db.KeepAliveCalc(f, 280, 300_000, "cheap", price, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fx.db.KeepAliveCalc(f, 280, 300_000, "dear", price, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Priced || !b.Priced {
		t.Fatal("a priced model reported priced=false")
	}
	if a.PingUSDEach == b.PingUSDEach {
		t.Errorf("two models with different rates priced the same ping at $%.8f", a.PingUSDEach)
	}
	if a.PingUSDEach != 300_000*cheap.CacheRead+cheap.Output {
		t.Errorf("ping cost = %.8f, want prefix x cache_read + one output token", a.PingUSDEach)
	}
	if a.AvoidedUSDEach != 300_000*(cheap.CacheWrite-cheap.CacheRead) {
		t.Errorf("avoided = %.8f, want the WRITE PREMIUM (write - read) x prefix, not the whole "+
			"miss", a.AvoidedUSDEach)
	}
	// The unpriced model: no dollar figure at all, and it says so.
	un, err := fx.db.KeepAliveCalc(f, 280, 300_000, "nobody-prices-me", price, 2)
	if err != nil {
		t.Fatal(err)
	}
	if un.Priced {
		t.Error("an unpriced model reported priced=true")
	}
	if un.PingUSDEach != 0 || un.AvoidedUSDEach != 0 {
		t.Error("an unpriced model produced a dollar figure")
	}
	for _, row := range un.Rows {
		if row.PingUSD != 0 || row.SavedUSD != 0 || row.NetUSD != 0 {
			t.Errorf("an unpriced model produced a dollar figure on the K=%d row", row.MaxPings)
		}
		if row.Convertible < 0 {
			t.Error("counts must still be reported when the money cannot be")
		}
	}
}

// The K ladder's SHAPE, which is what the panel exists to convey: reach grows sharply from K=1
// to K=2 and then flattens, while ping cost grows roughly linearly in K.
//
// Built from a fixture whose gaps land in the production bands, so the ordering the production
// table shows is reproduced rather than asserted from memory.
func TestCalculatorGoldenAgainstTheProductionBands(t *testing.T) {
	// Gaps chosen to sit inside successive coverages at X=280: 580 (K=1), 860 (K=2), 1140
	// (K=3), 1420 (K=4). Two misses per band, so each rung gains and the gain shrinks.
	var evs []*Event
	ts := int64(10_000_000)
	for i, gap := range []float64{400, 500, 700, 800, 1000, 1100, 1300, 1350} {
		evs = append(evs, kaExpiry(ts+int64(i)*3_600_000, "s"+string(rune('a'+i)), gap, 1.0, 300_000)...)
	}
	fx := newKAFixture(t, evs...)
	price := func(string) (modelinfo.Price, bool) { return ibmSonnet, true }
	out, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 280, 300_000, "aws/claude-sonnet-5", price, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 4 {
		t.Fatalf("the ladder has %d rungs, want K=1..4", len(out.Rows))
	}
	want := []int64{2, 4, 6, 8}
	for i, row := range out.Rows {
		if row.MaxPings != i+1 {
			t.Fatalf("row %d is K=%d", i, row.MaxPings)
		}
		if row.Convertible != want[i] {
			t.Errorf("K=%d converts %d, want %d (coverage %g s)",
				row.MaxPings, row.Convertible, want[i], row.Coverage)
		}
		if row.SharePct <= 0 {
			t.Errorf("K=%d reports no share of addressable dollars", row.MaxPings)
		}
	}
	// Reach must be monotonic and its GAIN must shrink — the flattening is the finding.
	g1 := out.Rows[1].Convertible - out.Rows[0].Convertible
	g3 := out.Rows[3].Convertible - out.Rows[2].Convertible
	if g3 > g1 {
		t.Errorf("the ladder does not flatten: K1->K2 gains %d, K3->K4 gains %d", g1, g3)
	}
	// And the current row is marked, which is what the panel emphasises.
	if !out.Rows[1].Current {
		t.Error("K=2 was not marked as the current policy")
	}
}

// A window whose rows predate the keep-alive columns must report "not recorded", never $0 saved.
//
// keepalive_pings and keepalive_saved_usd arrived as ADDITIVE columns with DEFAULT 0, so a zero
// on an old row is an absence and rendering it as a measurement is a fabricated default — the
// failure this project has hit repeatedly.
func TestKeepAlivePanelsStateTheirCoverage(t *testing.T) {
	// Never ran: agent traffic only, no ping and no credit anywhere.
	never := newKAFixture(t, kaAgent(1_000_000, "s1", 0.10), kaAgent(1_100_000, "s1", 0.20))
	led, err := never.db.KeepAliveLedger(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if led.RecordedFrom != 0 {
		t.Errorf("recorded_from = %d on a database where it never ran", led.RecordedFrom)
	}
	if led.Requests != 2 {
		t.Errorf("requests = %d, want 2", led.Requests)
	}
	// Partially recorded: old rows, then the mechanism starts.
	part := newKAFixture(t,
		kaAgent(1_000_000, "s1", 0.10), kaAgent(1_100_000, "s1", 0.20),
		kaPing(1_500_000, "s1", 0.01, 50_000, 0), kaCredit(1_600_000, "s1", 0.40),
	)
	led, err = part.db.KeepAliveLedger(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if led.RecordedFrom != 1_500_000 {
		t.Errorf("recorded_from = %d, want the first ping's instant 1500000", led.RecordedFrom)
	}
	if led.RecordedRows >= led.Requests {
		t.Errorf("recorded_rows %d of requests %d: the partial state is not distinguishable",
			led.RecordedRows, led.Requests)
	}
	if led.RecordedRows == 0 {
		t.Error("recorded_rows = 0 although the mechanism has run")
	}
}

// Thin history is REFUSED, and the refusal names the count. 19 addressable expiries is one short
// of the floor, and the floor exists because a bootstrap over sessions has nothing to resample
// below it.
func TestRecommendationRefusesThinHistory(t *testing.T) {
	fx := newKAFixture(t, kaSpread(t, 19, 700, 1.0)...)
	rec, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Refused == "" {
		t.Fatalf("n=%d was not refused", rec.N)
	}
	if !strings.Contains(rec.Refused, "19") {
		t.Errorf("the refusal does not name the count: %q", rec.Refused)
	}
	if rec.MaxPings != 0 || rec.LoUSD != 0 || rec.HiUSD != 0 {
		t.Error("a refusal carried a recommendation anyway")
	}
	// And the service-wide interval is offered for scale, which is the only number a refusal
	// may give.
	if rec.ServiceLoUSD != 95 || rec.ServiceHiUSD != 237 {
		t.Errorf("the refusal did not carry the service-wide interval for scale: [%v, %v]",
			rec.ServiceLoUSD, rec.ServiceHiUSD)
	}
}

// An interval that crosses zero is refused even when the count test passes — the T2 case: n=29,
// CI [-4.49, +4.79]. Condition 3 is the one that does the work.
func TestRecommendationRefusesWhenTheIntervalCrossesZero(t *testing.T) {
	// Enough expiries and requests to clear conditions 1 and 2, but their gaps are far outside
	// any coverage at X=280 — so nothing converts, the pings cost money, and the interval sits
	// on or below zero rather than excluding it above.
	fx := newKAFixture(t, kaSpread(t, 30, 40_000, 1.0)...)
	rec, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.N < recMinMisses {
		t.Fatalf("the fixture does not clear the count test: n=%d", rec.N)
	}
	if rec.Refused == "" && rec.LoUSD <= 0 && rec.HiUSD >= 0 {
		t.Errorf("an interval spanning zero [%v, %v] was returned as a recommendation",
			rec.LoUSD, rec.HiUSD)
	}
}

// An interval that sits entirely BELOW zero is a refusal, not a recommendation.
//
// Found by looking at the page: the first live render of this route produced "Suggested: 280 s,
// 2 pings — expected -$38.63 to -$4.54", because the two-sided reading of "the interval excludes
// zero" admits an account the mechanism would simply cost money. An account whose idle gaps are
// mostly an hour long has nothing inside any coverage worth having.
func TestRecommendationRefusesAnIntervalEntirelyBelowZero(t *testing.T) {
	// 30 expiries whose gaps are far past any coverage at X=280 (2.8 hours), on sessions that
	// nevertheless have plenty of idle spans to be pinged through. Cost with no conversion.
	fx := newKAFixture(t, kaSpread(t, 30, 10_000, 2.0)...)
	rec, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.N < recMinMisses || rec.Requests < recMinRequests {
		t.Fatalf("the fixture does not clear conditions 1 and 2: n=%d requests=%d",
			rec.N, rec.Requests)
	}
	if rec.Refused == "" {
		t.Errorf("an account this policy would cost money got a recommendation: %d/%d pings, "+
			"$%.2f to $%.2f", rec.IdleSeconds, rec.MaxPings, rec.LoUSD, rec.HiUSD)
	}
	if rec.MaxPings != 0 || rec.LoUSD != 0 || rec.HiUSD != 0 {
		t.Error("a refusal carried a recommendation anyway")
	}
	// And the refusal SAYS which way: "cannot tell it apart from zero" and "this would cost you
	// money" are different findings and only one of them is actionable.
	if !strings.Contains(rec.Refused, "COST") {
		t.Errorf("the refusal does not say the mechanism would cost this account money: %q",
			rec.Refused)
	}
}

// The wire carries NO point estimate. Not a zero, not an omitted field with a name — the field
// does not exist, because a field that exists gets rendered.
func TestRecommendationPayloadCarriesNoPointEstimate(t *testing.T) {
	fx := newKAFixture(t, kaSpread(t, 25, 700, 1.0)...)
	rec, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"point_estimate", "expected_usd", "estimate", "net_usd"} {
		if strings.Contains(string(b), `"`+forbidden+`"`) {
			t.Errorf("the recommendation payload carries %q; a single confident number is exactly "+
				"what this data cannot support:\n%s", forbidden, b)
		}
	}
	// A returned recommendation is a RANGE, and it states its n.
	if rec.Refused == "" {
		if rec.LoUSD == rec.HiUSD {
			t.Error("the recommendation collapsed to a point")
		}
		if rec.N == 0 || rec.Sessions == 0 {
			t.Error("a range was returned without the n it rests on")
		}
	}
}

// K = 1 is never recommended: one ping reaches about 4.7 minutes, which is inside the free TTL,
// and it is -$71 service-wide.
func TestRecommendationNeverSuggestsOnePing(t *testing.T) {
	// Four cases, one per region of the input space rather than the full cross product: a gap
	// inside K=1's own reach, one only K=2 reaches, one only K=3 reaches, and one nothing
	// reaches. Twelve fixtures cost eight seconds under -race on a package already close to the
	// default test timeout, and added no region.
	for _, tc := range []struct {
		n   int
		gap float64
	}{{25, 400}, {40, 700}, {25, 1100}, {30, 5000}} {
		fx := newKAFixture(t, kaSpread(t, tc.n, tc.gap, 1.0)...)
		rec, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true})
		if err != nil {
			t.Fatal(err)
		}
		if rec.MaxPings == 1 || rec.AltMaxPings == 1 {
			t.Errorf("n=%d gap=%g recommended K=1", tc.n, tc.gap)
		}
	}
}

// The behaviour panels' addressability gate. A ttl_expiry row that wrote nothing is a PHANTOM —
// 385 of the 742 in production, every one an HTTP 400 — and counting it inflates every figure
// on the panel.
func TestPhantomExpiriesAreCountedApartAndNotIn(t *testing.T) {
	real1 := kaExpiry(2_000_000, "s1", 700, 1.0, 300_000)
	phantom := kaAgent(2_100_000, "s2", 0.0)
	phantom.CacheMissReason, phantom.CacheRead, phantom.CacheWrite = "ttl_expiry", 0, 0
	phantom.Status = 400
	fx := newKAFixture(t, append(real1, kaAgent(2_050_000, "s2", 0.01), phantom)...)
	b, err := fx.db.KeepAliveBehaviour(Filter{TenantAll: true}, 860)
	if err != nil {
		t.Fatal(err)
	}
	if b.Addressable != 1 {
		t.Errorf("addressable = %d, want 1; a cache_write = 0 row has no prefix to protect",
			b.Addressable)
	}
	if b.Phantom != 1 {
		t.Errorf("phantom = %d, want 1 — the difference from the Usage tab's ttl_expiry count "+
			"has to be explicable", b.Phantom)
	}
}

// The gap bands preserve their edge ORDER and mark what the current coverage cannot reach. The
// order is the meaning: sorting these by size would destroy the only thing the chart says.
func TestGapBandsKeepTheirOrderAndMarkWhatIsBeyondCoverage(t *testing.T) {
	fx := newKAFixture(t, kaExpiry(2_000_000, "s1", 700, 1.0, 300_000)...)
	b, err := fx.db.KeepAliveBehaviour(Filter{TenantAll: true}, 860)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.GapBands) != len(gapEdges) {
		t.Fatalf("%d bands for %d edges", len(b.GapBands), len(gapEdges))
	}
	var sawBeyond bool
	for i, band := range b.GapBands {
		if want := gapEdges[i] >= 860; band.Beyond != want {
			t.Errorf("band %q beyond = %v, want %v at coverage 860 s", band.Label, band.Beyond, want)
		}
		if band.Beyond {
			sawBeyond = true
		}
	}
	if !sawBeyond {
		t.Error("no band was marked beyond the coverage rule")
	}
	if len(b.HourBins) != 24 {
		t.Errorf("%d hour bins, want 24", len(b.HourBins))
	}
}

// kaSpread builds n addressable expiries across n distinct sessions, plus enough agent traffic
// to clear the request floor. Distinct sessions because the bootstrap resamples SESSIONS.
func kaSpread(t *testing.T, n int, gapS, usd float64) []*Event {
	t.Helper()
	var evs []*Event
	ts := int64(100_000_000)
	// The costs VARY across sessions, deliberately. A fixture of identical units makes every
	// bootstrap resample produce the same total, so the interval collapses to a point — which
	// would let a degenerate interval pass a test about ranges.
	for i := 0; i < n; i++ {
		evs = append(evs, kaExpiry(ts+int64(i)*7_200_000, "sess-"+itoa(int64(i)), gapS,
			usd*(0.2+1.6*float64(i%7)/6.0), 300_000)...)
	}
	// Filler, so `requests` clears recMinRequests without adding expiries.
	for i := 0; i < recMinRequests; i++ {
		evs = append(evs, kaAgent(ts+int64(i)*1000, "filler", 0.01))
	}
	return evs
}

// asMap marshals a value to a generic map, for key-by-key comparison.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// jsonEqual compares two values by their JSON encoding.
func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// The five new read routes are reachable, answer with JSON, and carry no content text. The
// content gate is asserted over the whole table by TestNoRouteServesContentTextFromAnUntrustedAddress;
// this checks they actually ANSWER, which a route that 500s on an empty database would not.
func TestKeepAliveRoutesAnswerOnAnEmptyDatabase(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	mux := http.NewServeMux()
	api.Mount(mux)
	for _, rt := range api.keepAliveRoutes() {
		path := strings.TrimPrefix(rt.pattern, "GET ")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d: %s", path, w.Code, w.Body)
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Errorf("%s did not answer with JSON: %s", path, w.Body)
		}
	}
}

// The calculator's PINGS column is a policy somebody could actually run.
//
// It was a LAG over `requests`, which is wrong in both directions at once and the errors do not
// cancel:
//
//   - a LAG span exists only BETWEEN two requests, so it charged nothing for a session-final
//     request — where a live policy must send K, because it cannot know the session ended.
//     7,782 of the 9,234 pings in the adjudicated replay were session-final.
//   - it applied no gate, charging pings on the turn-0 and small-prefix spans that the shipped
//     `turn >= 1 AND prefix >= 20k` never touches.
//
// Measured on the 19,805-request snapshot at X=280, K=2: the LAG form 1,452, this form 1,060, a
// blanket policy 9,234. The two errors partly cancel, so the old column over-charged the SHIPPED
// policy by 1.37x rather than under-charging it 6.4x — that 6.4x is the gap to a blanket policy the
// calculator does not model. NET = SAVED - PINGS x EACH either way, so a column counting a
// different population than it names is a dollar figure about nothing in particular.
//
// The fixture separates the two errors: the gated session's spans differ between the LAG and LEAD
// forms, and the small-prefix session is charged by one form and not the other.
func TestTheCalculatorChargesThePingsAPolicyWouldActuallySend(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	sec := func(n int64) int64 { return n * 1000 }
	evs := []*Event{
		// A gated session: prefix 50k throughout. turn 0, then a 300 s gap, then a 1000 s gap,
		// then nothing — the last request opens a span a live policy pings in and never closes.
		kaAgent(t0, "big", 0.10),
		kaAgent(t0+sec(300), "big", 0.10),
		kaAgent(t0+sec(1300), "big", 0.10),
		// Below the 20k prefix floor: the shipped gate never pings this session at all.
		smallPrefix(kaAgent(t0, "small", 0.10)),
		smallPrefix(kaAgent(t0+sec(1000), "small", 0.10)),
	}
	fx := newKAFixture(t, evs...)
	calc, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 280, 100_000, "aws/claude-sonnet-5",
		func(string) (modelinfo.Price, bool) {
			return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
		}, 2)
	if err != nil {
		t.Fatal(err)
	}
	var row *CalcRow
	for i := range calc.Rows {
		if calc.Rows[i].MaxPings == 2 {
			row = &calc.Rows[i]
		}
	}
	if row == nil {
		t.Fatal("no K=2 rung on the ladder")
	}
	// "big" turn 1 opens a 1000 s span (2 pings at K=2) and turn 2 opens an unbounded one (K=2).
	// turn 0 is not pingable and "small" never reaches the prefix floor.
	if row.Pings != 4 {
		t.Errorf("PINGS at K=2 = %d, want 4: two from the gated session's 1000 s span and two "+
			"from its session-final span. 3 is the LAG form (it charges turn 0's span and none "+
			"of the session-final one); 5 adds the sub-floor session the gate never touches",
			row.Pings)
	}
	// And the money follows the count, so the column cannot be right while NET is wrong.
	if want := float64(row.Pings) * calc.PingUSDEach; math.Abs(row.PingUSD-want) > 1e-12 {
		t.Errorf("PING COST = %.6f, want pings x each = %.6f", row.PingUSD, want)
	}
	if want := row.SavedUSD - row.PingUSD; math.Abs(row.NetUSD-want) > 1e-12 {
		t.Errorf("NET = %.6f, want SAVED - PING COST = %.6f", row.NetUSD, want)
	}
}

// smallPrefix drops a fixture row's billed prefix below the replay gate's floor.
func smallPrefix(e *Event) *Event {
	e.CacheRead, e.CacheWrite = 1_000, 0
	return e
}

// The replay gate is the request path's gate. A drift here is a calculator modelling a policy
// nobody runs, which is the whole of F5.
func TestTheReplayGateMatchesTheShippedPolicy(t *testing.T) {
	if kaGateMinPrefix != 20000 {
		t.Errorf("kaGateMinPrefix = %d; config.DefaultKeepAliveMinPrefix is 20000 and the replay "+
			"has to gate on what the request path gates on", kaGateMinPrefix)
	}
}

// The live panel's arithmetic: which lifetime is in force, what is left of it, and the breakeven.
//
// Three things it has to get right, and each has a wrong answer that looks plausible on screen:
//
//   - the TTL in force is the tier this session's MOST RECENT WRITE landed in, read off
//     cache_write_1h. Not configuration: a `ttl: "1h"` request that the model does not support
//     comes back a perfectly normal 200 with the entry granted for five minutes, so the billed
//     tier is the only honest source. A later 5-minute write REPLACES an earlier one-hour entry.
//   - the lifetime runs from the request's START, per the provider's documented rule, and an
//     entry whose life has already elapsed is not returned at all.
//   - the breakeven is MissUSD / PingUSDEach, and it is what the whole panel exists to say.
func TestTheLivePanelPricesOneSessionsOwnBreakeven(t *testing.T) {
	now := int64(1_700_000_000_000)
	ago := func(sec int64) int64 { return now - sec*1000 }
	// A session's last request: 100k of billed prefix, written at the tier `oneHour` names.
	live := func(ts int64, session string, read, write, write1h int64) *Event {
		e := kaAgent(ts, session, 0.10)
		e.CacheRead, e.CacheWrite, e.CacheWrite1h = read, write, write1h
		return e
	}
	fx := newKAFixture(t,
		// Five-minute entry, 100 s old: 200 s left.
		live(ago(100), "five", 0, 100_000, 0),
		// One-hour entry, 100 s old, and its last request is a pure READ — the tier is the one
		// the entry was WRITTEN at, and a read refreshes it at that same tier.
		live(ago(400), "hour", 0, 100_000, 100_000),
		live(ago(100), "hour", 100_000, 0, 0),
		// An hour-long entry REPLACED by a later five-minute write. The most recent write decides,
		// so this session has 200 s left and not 3500.
		live(ago(400), "downgraded", 0, 100_000, 100_000),
		live(ago(100), "downgraded", 0, 100_000, 0),
		// Written 400 s ago at five minutes: gone, and not a row.
		live(ago(400), "lapsed", 0, 100_000, 0),
	)
	price := func(string) (modelinfo.Price, bool) {
		// 0.1x read, 1.25x write, on a $3/MTok input rate: the shipped Anthropic shape.
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
	if _, ok := rows["lapsed"]; ok {
		t.Error("a session whose entry expired 100 s ago is on the live list; the page would be " +
			"stating a lifetime that has already elapsed")
	}
	for _, c := range []struct {
		session   string
		ttl       int64
		remaining float64
	}{
		{"five", 300, 200},
		{"hour", 3600, 3500},
		{"downgraded", 300, 200},
	} {
		r, ok := rows[c.session]
		if !ok {
			t.Errorf("session %q is missing from the live list", c.session)
			continue
		}
		if r.TTLSeconds != c.ttl {
			t.Errorf("%s: ttl_seconds = %d, want %d — the tier in force is the one this "+
				"session's MOST RECENT write was billed at", c.session, r.TTLSeconds, c.ttl)
		}
		if math.Abs(r.RemainingSeconds-c.remaining) > 0.001 {
			t.Errorf("%s: remaining_seconds = %.3f, want %.3f — measured from the request's "+
				"START, which is what the provider measures it from", c.session,
				r.RemainingSeconds, c.remaining)
		}
	}
	// The breakeven, in full, on the five-minute row. Both terms scale with the prefix, so this
	// is a ratio of the model's own rates and it comes out the same on any session on this model:
	// a lapse costs 11.49 pings, so eleven pings are cheaper than letting it go.
	r := rows["five"]
	if !r.Priced {
		t.Fatal("the row is not priced; every figure below is vacuous")
	}
	for _, c := range []struct {
		name      string
		got, want float64
		why       string
	}{
		{"ping_usd_each", r.PingUSDEach, 100_000*3e-7 + 15e-6,
			"the prefix at the cache-READ rate plus the one output token the ping asks for"},
		{"miss_usd", r.MissUSD, 100_000 * (3.75e-6 - 3e-7),
			"the avoidable WRITE PREMIUM, not the whole re-read: a resuming request pays to " +
				"read its prefix either way"},
		{"breakeven_minutes", r.BreakevenMinutes, (11*280 + 300) / 60.0,
			"K x X + TTL at the breakeven count, which is the idle time one lapse pays to bridge"},
		{"breakeven_1h_minutes", r.Breakeven1hMinutes, (18*280 + 3600) / 60.0,
			"an hour-long entry needs a ping only once an hour, so the same pings bridge " +
				"twelve times the wall clock"},
	} {
		if math.Abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s = %.9f, want %.9f: %s", c.name, c.got, c.want, c.why)
		}
	}
	if r.BreakevenPings != 11 {
		t.Errorf("breakeven_pings = %d, want 11: one lapse costs $%.6f and one ping $%.6f, so "+
			"11.49 pings are what a lapse buys and eleven of them are cheaper than it",
			r.BreakevenPings, r.MissUSD, r.PingUSDEach)
	}
	// A one-hour entry is dearer to re-create (2.0x base against 1.25x), so a lapse pays for MORE
	// pings — the figure has to move in that direction and by that amount.
	if r.Breakeven1hPings != 18 {
		t.Errorf("breakeven_1h_pings = %d, want 18: a 1h write is 2.0x base input against the "+
			"5m tier's 1.25x, so the premium at risk is larger", r.Breakeven1hPings)
	}
	// The warning's totals are the SERVER's, not a sum the browser assembled: "five" and
	// "downgraded" have 200 s left, inside the 330 s threshold; "hour" has 3500 s and is not.
	if got.Soon != 2 {
		t.Errorf("soon = %d, want 2 — the two five-minute rows are inside the %.0f s threshold "+
			"and the one-hour row is not", got.Soon, got.SoonSeconds)
	}
	if want := 2 * rows["five"].MissUSD; math.Abs(got.SoonUSD-want) > 1e-9 {
		t.Errorf("soon_usd = %.6f, want %.6f: the write premium of the two rows about to lapse",
			got.SoonUSD, want)
	}
	if want := 3 * rows["five"].MissUSD; math.Abs(got.PotentialUSD-want) > 1e-9 {
		t.Errorf("potential_usd = %.6f, want %.6f: every live row, not only the urgent ones",
			got.PotentialUSD, want)
	}
	if got.Now != now || got.IdleSeconds != 280 || got.MaxPings != 2 {
		t.Errorf("the policy and the clock the figures were computed at are not on the wire: %+v",
			struct {
				Now         int64
				IdleSeconds float64
				MaxPings    int
			}{got.Now, got.IdleSeconds, got.MaxPings})
	}
}
