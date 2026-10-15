package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/schema"
)

// prettyJSON is a tool output `format` re-serialises compactly: the one candidate whose effect
// is certain, so the arithmetic below is about pricing, not about a component's judgement.
func prettyJSON() string {
	rows := make([]map[string]any, 40)
	for i := range rows {
		rows[i] = map[string]any{"id": i, "name": "item-name", "status": "active", "tags": []string{"a", "b"}}
	}
	b, _ := json.MarshalIndent(map[string]any{"items": rows}, "", "        ")
	return string(b)
}

type adaptFixtureReq struct {
	ts                         int64
	fresh, read, write, write1 int64
	blocks                     map[string]string // path -> before text
}

func adaptFixtureDB(t *testing.T, tenant string, sessions map[string][]adaptFixtureReq) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for sess, reqs := range sessions {
		for _, r := range reqs {
			res, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id, model, provider,
				fresh_input, cache_read, cache_write, cache_write_1h, output_tokens, cost_usd, token_accounting)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, r.ts, tenant, sess, "m", "anthropic",
				r.fresh, r.read, r.write, r.write1, 10, 1.0, AccountingComplete)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			seq := 0
			for path, text := range r.blocks {
				tok := schema.TextTokens(text)
				if strings.Contains(text, "…[+") {
					tok += 500 // a clipped capture: the stored count is the full string's
				}
				if _, err := db.sql.Exec(`INSERT INTO request_content(request_id, seq, path,
					before_tokens, after_tokens, before_gz, components) VALUES (?,?,?,?,?,?,?)`,
					id, seq, path, tok, tok, gzipText(text), "dedup"); err != nil {
					t.Fatal(err)
				}
				seq++
			}
		}
	}
	return db
}

var adaptTestPrice = modelinfo.Price{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6, Output: 5e-6}

// One block, three turns: tail on a turn that wrote it, a hit, a full-prefix rewrite. The
// current config (nothing) sent the block as it was; `format` shrinks it, and that difference
// must be priced at write, read, and write again — by hand here.
func TestAdaptPricesEachAppearanceAtTheRateThatTurnPaid(t *testing.T) {
	text := prettyJSON()
	blocks := map[string]string{"messages.1.content.0.content": text}
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {
		{ts: 1_000_000, fresh: 10, read: 0, write: 5000, blocks: blocks},
		{ts: 1_060_000, fresh: 10, read: 5000, write: 50, blocks: blocks},
		{ts: 9_000_000, fresh: 10, read: 0, write: 5050, blocks: blocks},
	}})
	sessions, err := db.adaptDataset(context.Background(), Filter{Tenant: "acct"},
		staticTable{"m": adaptTestPrice}, &AdaptCompactionCoverage{})
	if err != nil || len(sessions) != 1 || len(sessions[0].reqs) != 3 {
		t.Fatalf("dataset: %v %+v", err, sessions)
	}
	none, err := replayAdapt(context.Background(), adaptSpec{}, sessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	if none.deltaUSD != 0 || none.reproduced != 3 {
		t.Fatalf("the current arm must reproduce what was sent: %+v", none)
	}
	fm, err := replayAdapt(context.Background(), adaptSpec{comps: []string{"format"}}, sessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	orig := schema.TextTokens(text)
	if fm.deltaUSD >= 0 {
		t.Fatalf("format should have shrunk pretty JSON: %+v", fm)
	}
	factor, _ := tokens.BilledDeltaFactor("m")
	perToken := fm.deltaUSD / (factor * (adaptTestPrice.CacheWrite + adaptTestPrice.CacheRead + adaptTestPrice.CacheWrite))
	d := math.Round(perToken)
	if math.Abs(perToken-d) > 1e-6 || d >= 0 || -d >= float64(orig) {
		t.Fatalf("delta %.9f is not one token change priced write+read+write (per-token %.4f, orig %d)",
			fm.deltaUSD, perToken, orig)
	}
	if fm.busts != 0 {
		t.Fatalf("a frozen block must not bust: %+v", fm)
	}
}

func TestAdaptPriceReqBustAndTiers(t *testing.T) {
	r := &adaptReq{fresh: 10, read: 1000, write: 200, write1h: 100, price: adaptTestPrice, factor: 1,
		blocks: []*adaptBlock{{key: "k", sent: 100}}}
	tail, repeat, write := adaptRates(r)
	wantWrite := 0.5*adaptTestPrice.CacheWrite + 0.5*2*adaptTestPrice.Input
	// A mixed-TTL write: the rewrite rate blends both tiers, the tail is the 5m entry.
	if math.Abs(write-wantWrite) > 1e-15 || tail != adaptTestPrice.CacheWrite || repeat != adaptTestPrice.CacheRead {
		t.Fatalf("rates tail=%g repeat=%g write=%g", tail, repeat, write)
	}
	all1h := *r
	all1h.write1h = all1h.write
	if tail1h, _, _ := adaptRates(&all1h); tail1h != 2*adaptTestPrice.Input {
		t.Fatalf("an all-1h write's tail = %g", tail1h)
	}
	seen := map[string]adaptSeen{"k": {got: 100, sent: 100}}
	d, _, bust := priceAdaptReq(r, []int{60}, seen, false, 0)
	want := -40*repeat + 1000*(write-adaptTestPrice.CacheRead)
	if !bust || math.Abs(d-want) > 1e-12 {
		t.Fatalf("re-sent block changed on a cache hit: bust=%v d=%g want %g", bust, d, want)
	}
	// Same size as last time: no bust, and a declaration added on the opening turn is tail.
	d, decl, bust := priceAdaptReq(r, []int{60}, seen, true, 30)
	if bust || math.Abs(d-(-40*repeat+30*tail)) > 1e-12 || math.Abs(decl-30*tail) > 1e-12 {
		t.Fatalf("unchanged block: bust=%v d=%g", bust, d)
	}
	// A turn that missed (no read) cannot bust: it rewrote everything anyway.
	miss := *r
	miss.read = 0
	if _, _, bust := priceAdaptReq(&miss, []int{10}, map[string]adaptSeen{"k": {100, 100}}, false, 0); bust {
		t.Fatal("a full-prefix rewrite was charged a bust")
	}
	// The history itself re-sent the block at a new size: that rewrite is in the bill already.
	if _, _, bust := priceAdaptReq(r, []int{10}, map[string]adaptSeen{"k": {100, 90}}, false, 0); bust {
		t.Fatal("a rewrite the history made was charged again")
	}
}

func TestAdaptCoverageCountsWhatCannotBeReplayed(t *testing.T) {
	text := prettyJSON()
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{
		"s1": {{ts: 1, write: 100, blocks: map[string]string{
			"messages.1.content.0.content": text,
			"messages.3.content.0.content": "head of a long output…[+9000 bytes]",
		}}},
		"s2": {{ts: 2, write: 100}}, // no content at all
	})
	if _, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id, model, cost_usd) VALUES (3,'other','x','m',5)`); err != nil {
		t.Fatal(err)
	}
	var cov AdaptCompactionCoverage
	sessions, err := db.adaptDataset(context.Background(), Filter{Tenant: "acct"}, staticTable{"m": adaptTestPrice}, &cov)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Requests != 2 || cov.RequestsWithContent != 1 || cov.CoveredRequests != 1 ||
		cov.Blocks != 2 || cov.ClippedBlocks != 1 || cov.BilledUSD != 2 || cov.CoveredBilledUSD != 1 {
		t.Fatalf("coverage %+v", cov)
	}
	if len(sessions) != 1 || len(sessions[0].reqs[0].blocks) != 2 {
		t.Fatalf("both blocks are kept, the clipped one modelled: %+v", sessions)
	}
	var m *adaptBlock
	for _, b := range sessions[0].reqs[0].blocks {
		if b.modelled {
			m = b
		}
	}
	if m == nil || m.text != "" || !reflect.DeepEqual(m.touched, []string{"dedup"}) {
		t.Fatalf("clipped block: %+v", m)
	}
}

// A clipped block is never run. Keeping the component that rewrote it costs what was sent;
// dropping it costs the original size — so removing an offloader that acted on large outputs is
// visible even though those outputs could not be replayed.
func TestAdaptModelledBlockChargesDroppingTheComponentThatRewroteIt(t *testing.T) {
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {
		{ts: 1, fresh: 10, write: 5000, blocks: map[string]string{
			"messages.1.content.0.content": "head of a long output…[+9000 bytes]"}},
	}})
	if _, err := db.sql.Exec(`UPDATE request_content SET after_tokens = 20`); err != nil {
		t.Fatal(err)
	}
	sessions, err := db.adaptDataset(context.Background(), Filter{Tenant: "acct"},
		staticTable{"m": adaptTestPrice}, &AdaptCompactionCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	b := sessions[0].reqs[0].blocks[0]
	keep, _ := replayAdapt(context.Background(), adaptSpec{comps: []string{"format", "dedup"}}, sessions, 0)
	drop, _ := replayAdapt(context.Background(), adaptSpec{comps: []string{"format"}}, sessions, 0)
	if keep.deltaUSD != 0 {
		t.Fatalf("keeping dedup must cost what was sent: %+v", keep)
	}
	factor, _ := tokens.BilledDeltaFactor("m")
	want := float64(b.tokens-20) * adaptTestPrice.CacheWrite * factor
	if math.Abs(drop.deltaUSD-want) > 1e-12 || keep.compared != 0 {
		t.Fatalf("dropping dedup: got %g want %g (%+v)", drop.deltaUSD, want, drop)
	}
}

func TestAdaptMergeKeepsOtherComponentsInPlace(t *testing.T) {
	cur := []string{"format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract_llm", "extract", "cachesplit", "toolfilter"}
	got := AdaptCompactionMerge(cur, []string{"format", "dedup", "searchfold", "textclean"})
	// extract left, so extract_llm, cachesplit and toolfilter re-anchor on textclean.
	want := []string{"format", "dedup", "searchfold", "textclean", "extract_llm", "cachesplit", "toolfilter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got = AdaptCompactionMerge([]string{"cachesplit"}, []string{"format", "extract"})
	if !reflect.DeepEqual(got, []string{"cachesplit", "format", "extract"}) {
		t.Fatalf("head-anchored component: %v", got)
	}
}

// End to end: an account with nothing but cachesplit, whose history is pretty JSON, is told
// format would have saved money, and the applied pipeline keeps cachesplit.
func TestAdaptCompactionRecommendsWhatSaves(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {
		{ts: 1_000_000, fresh: 10, write: 5000, blocks: blocks},
		{ts: 1_060_000, fresh: 10, read: 5000, write: 50, blocks: blocks},
	}})
	// Rates x1000 so the saving clears the recommendation floor on a two-request fixture.
	p := modelinfo.Price{Input: 1e-3, CacheRead: 0.1e-3, CacheWrite: 1.25e-3, Output: 5e-3}
	rep, err := db.AdaptCompaction(context.Background(), Filter{Tenant: "acct"},
		staticTable{"m": p}, "pipeline: [cachesplit]\n", defaultAdaptDocForTest)
	if err != nil {
		t.Fatal(err)
	}
	best := adaptOpt(t, rep, rep.Best)
	if rep.Recommendation == nil || rep.Recommendation.Option != rep.Best || rep.Recommendation.NetDeltaUSD <= 0 ||
		!slices.Contains(best.spec.comps, "format") {
		t.Fatalf("best %+v recommendation %+v summary %q", best, rep.Recommendation, rep.Summary)
	}
	if !slices.Contains(best.Pipeline, "cachesplit") {
		t.Fatalf("applied pipeline dropped a non-candidate: %v", best.Pipeline)
	}
	cur := adaptOpt(t, rep, rep.Current)
	if cur.BilledUSD != rep.Coverage.CoveredBilledUSD || *cur.NetUSD != 0 || cur.ID != "current" {
		t.Fatalf("current (nothing) replays to %+v, billed %g", cur, rep.Coverage.CoveredBilledUSD)
	}
	// net = saved - spend, and the short id maps to exactly what the option replayed.
	if math.Abs(*best.NetUSD-(best.SavedUSD-best.SpendUSD)) > 1e-12 {
		t.Fatalf("net %g saved %g spend %g", *best.NetUSD, best.SavedUSD, best.SpendUSD)
	}
	ch := rep.choices[best.ID]
	if !strings.HasPrefix(best.ID, "c") || !reflect.DeepEqual(ch.Candidates, best.spec.comps) ||
		ch.ConfigHash != AdaptConfigHash("pipeline: [cachesplit]\n") {
		t.Fatalf("option %q maps to %+v", best.ID, ch)
	}
}

func adaptOpt(t *testing.T, rep *AdaptCompactionReport, id string) AdaptCompactionOption {
	t.Helper()
	for _, o := range rep.Options {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no option %q in %+v", id, rep.Options)
	return AdaptCompactionOption{}
}

// An apply may name only an option of the latest report this account fetched, while fresh.
func TestAdaptOffersAreOneAccountsLatestReport(t *testing.T) {
	a := &API{}
	b1 := a.OfferAdapt("compaction", "t1", map[string]any{"c1": 1})
	if v, ok := a.AdaptOffer("compaction", "t1", b1, "c1"); !ok || v != 1 {
		t.Fatalf("fresh offer: %v %v", v, ok)
	}
	for _, c := range [][4]string{{"compaction", "t2", b1, "c1"}, {"tools", "t1", b1, "c1"},
		{"compaction", "t1", b1, "c2"}, {"compaction", "t1", "x", "c1"}} {
		if _, ok := a.AdaptOffer(c[0], c[1], c[2], c[3]); ok {
			t.Errorf("%v was accepted", c)
		}
	}
	if _, ok := a.AdaptOffer("compaction", "t1", "", "c1"); ok {
		t.Error("an apply without a basis was accepted")
	}
	b2 := a.OfferAdapt("compaction", "t1", map[string]any{"c1": 2})
	if _, ok := a.AdaptOffer("compaction", "t1", b1, "c1"); ok || b1 == b2 {
		t.Error("a superseded report's basis still applies")
	}
	a.offers.m["compaction|t1"] = adaptOffer{basis: b2, opts: map[string]any{"c1": 2}, exp: time.Now().Add(-time.Second)}
	if _, ok := a.AdaptOffer("compaction", "t1", b2, "c1"); ok {
		t.Error("an expired offer applies")
	}
}

func TestAdaptMovesPrefersTheCurrentShape(t *testing.T) {
	cur := adaptSpec{comps: []string{"format", "dedup", "extract"}, settings: map[string]map[string]int{"extract": {"min_tokens": 400}}}
	for _, c := range []struct {
		s adaptSpec
		n int
	}{
		{cur, 0},
		{adaptSpec{comps: []string{"dedup", "format", "extract"}, settings: cur.settings}, 1},
		{adaptSpec{comps: []string{"format", "extract"}, settings: cur.settings}, 1},
		{adaptSpec{comps: []string{"format", "dedup", "extract"}, settings: map[string]map[string]int{"extract": {"min_tokens": 800}}}, 1},
	} {
		if got := c.s.moves(cur); got != c.n {
			t.Errorf("%s: %d moves, want %d", c.s.key(), got, c.n)
		}
	}
}

// The capture cap hides the newest blocks of a long session, which is where an offloader acts.
// The product booked that saving in request_components; an arm dropping the component must be
// charged what the replay could not see of it.
func TestAdaptChargesDroppingAComponentForTheSavingTheReplayCannotSee(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {
		{ts: 1_000_000, fresh: 10, write: 5000, blocks: blocks},
	}})
	// Booked in tokens; the stored dollars are deliberately wrong, to show they are not read.
	if _, err := db.sql.Exec(`INSERT INTO request_components(request_id, component, saved_gross, saved_unique, saved_usd)
		SELECT id, 'dedup', 100000, 100000, 99 FROM requests`); err != nil {
		t.Fatal(err)
	}
	rep, err := db.AdaptCompaction(context.Background(), Filter{Tenant: "acct"},
		staticTable{"m": adaptTestPrice}, "pipeline: [format, dedup, cachesplit]\n", defaultAdaptDocForTest)
	if err != nil {
		t.Fatal(err)
	}
	factor, _ := tokens.BilledDeltaFactor("m")
	want := 100000 * adaptTestPrice.CacheWrite * factor // the unique part, first written as tail
	if len(rep.Measured) != 2 || rep.Measured[1].Component != "dedup" || math.Abs(rep.Measured[1].MeasuredUSD-want) > 1e-12 {
		t.Fatalf("measured %+v, want dedup %g", rep.Measured, want)
	}
	for _, o := range rep.Options {
		if !o.spec.has("dedup") && o.UnseenUSD < want-1e-9 {
			t.Fatalf("%s drops dedup but is charged %g of its unseen saving", o.Label, o.UnseenUSD)
		}
	}
	if rep.Recommendation != nil && !adaptOpt(t, rep, rep.Best).spec.has("dedup") {
		t.Fatalf("recommended dropping dedup: %+v", rep.Recommendation)
	}
}

func TestAdaptExpandDeclarationIsPriced(t *testing.T) {
	if n := expandDeclTokens(); n < 50 {
		t.Fatalf("expand declaration measured at %d tokens", n)
	}
}

// The route compares ONE account's history with that account's configuration: a plain user
// gets their own, a manager's service-wide default narrows to their own account, and a
// manager naming an account gets that account's history against that account's configuration.
func TestAdaptRouteIsOneAccountAgainstItsOwnConfig(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	db := adaptFixtureDB(t, "t1", map[string][]adaptFixtureReq{"s1": {{ts: 1000, write: 5000, blocks: blocks}}})
	rec := &Recorder{db: db}
	api := NewAPI(rec)
	api.SetPricer(staticTable{"m": adaptTestPrice})
	api.SetAuth(func(r *http.Request) (Principal, bool) {
		switch r.Header.Get("X-Who") {
		case "t1":
			return Principal{TenantID: "t1"}, true
		case "boss":
			return Principal{TenantID: "boss", Manager: true}, true
		}
		return Principal{}, false
	})
	var asked []string
	api.SetAdaptCompactionConfig(func(id string) (string, string, bool, string) {
		asked = append(asked, id)
		return "pipeline: [cachesplit]\n", defaultAdaptDocForTest, true, ""
	})
	for _, tc := range []struct {
		who, query, wantID string
		covered            int
	}{{"t1", "", "t1", 1}, {"boss", "", "boss", 0}, {"boss", "&tenant=t1", "t1", 1}} {
		asked = nil
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/adapt/compaction?since=1"+tc.query, nil)
		req.Header.Set("X-Who", tc.who)
		api.adaptCompaction(rr, req)
		var rep AdaptCompactionReport
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &rep) != nil {
			t.Fatalf("%s%s: %d %s", tc.who, tc.query, rr.Code, rr.Body)
		}
		if len(asked) != 1 || asked[0] != tc.wantID || rep.Coverage.CoveredRequests != tc.covered {
			t.Errorf("%s%s: config of %v, %d covered; want %q, %d", tc.who, tc.query, asked,
				rep.Coverage.CoveredRequests, tc.wantID, tc.covered)
		}
	}
	rr := httptest.NewRecorder()
	api.adaptCompaction(rr, httptest.NewRequest("GET", "/api/adapt/compaction", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no principal: %d", rr.Code)
	}
	for _, rt := range api.adaptRoutes() {
		if rt.class != scopeTenant {
			t.Errorf("%s is not tenant-scoped", rt.pattern)
		}
	}
}

// A window that opens mid-session: the block was first sent BEFORE the window, so its first
// in-window appearance is a cache read, not the uncached tail. The lead-in is replayed for its
// state and is neither priced nor counted.
func TestAdaptLeadInBeforeTheWindowIsReplayedNotPriced(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {
		{ts: 1_000_000, fresh: 10, write: 5000, blocks: blocks},
		{ts: 2_000_000, fresh: 10, read: 5000, write: 50, blocks: blocks},
	}})
	var cov AdaptCompactionCoverage
	sessions, err := db.adaptDataset(context.Background(), Filter{Tenant: "acct", Since: 1_500_000},
		staticTable{"m": adaptTestPrice}, &cov)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Requests != 1 || cov.CoveredRequests != 1 || len(sessions) != 1 || len(sessions[0].reqs) != 2 {
		t.Fatalf("coverage %+v, sessions %+v", cov, sessions)
	}
	e, err := replayAdapt(context.Background(), adaptSpec{comps: []string{"format"}}, sessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	factor, _ := tokens.BilledDeltaFactor("m")
	perToken := e.deltaUSD / (factor * adaptTestPrice.CacheRead)
	if d := math.Round(perToken); math.Abs(perToken-d) > 1e-6 || d >= 0 || e.compared != 1 {
		t.Fatalf("in-window appearance not priced as one read: %+v (per-token %.4f)", e, perToken)
	}
}

func TestAdaptReportSlotIsOnePerAccountAndBudgeted(t *testing.T) {
	a := &API{}
	done, ok := a.adaptReportSlot("k")
	if !ok {
		t.Fatal("first report refused")
	}
	if _, ok := a.adaptReportSlot("k"); ok {
		t.Error("a second concurrent report for one account was admitted")
	}
	if d, ok := a.adaptReportSlot("other"); !ok {
		t.Error("another account was blocked")
	} else {
		d()
	}
	done()
	for i := 1; i < adaptReportBurst; i++ {
		d, ok := a.adaptReportSlot("k")
		if !ok {
			t.Fatalf("report %d refused inside the budget", i+1)
		}
		d()
	}
	if _, ok := a.adaptReportSlot("k"); ok {
		t.Error("the hourly budget was not enforced")
	}
}

// A client that disconnects stops the replay, and a cut-short replay is an error, not numbers.
func TestAdaptReplayStopsWhenTheClientGoesAway(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {{ts: 1, write: 5000, blocks: blocks}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.AdaptCompaction(ctx, Filter{Tenant: "acct"}, staticTable{"m": adaptTestPrice},
		"pipeline: [format]\n", defaultAdaptDocForTest); err == nil {
		t.Fatal("a cancelled report returned numbers")
	}
}

// No global slot before the client gives up: a 503 with a retry hint, never an empty 200.
func TestAdaptReportBusyIs503(t *testing.T) {
	db := adaptFixtureDB(t, "t1", nil)
	api := NewAPI(&Recorder{db: db})
	api.SetPricer(staticTable{"m": adaptTestPrice})
	api.SetAdaptCompactionConfig(func(string) (string, string, bool, string) {
		return "pipeline: [format]\n", defaultAdaptDocForTest, true, ""
	})
	for i := 0; i < cap(kvCacheSlots); i++ {
		kvCacheSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(kvCacheSlots); i++ {
			<-kvCacheSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rr := httptest.NewRecorder()
	api.adaptCompaction(rr, httptest.NewRequest("GET", "/api/adapt/compaction?since=1", nil).WithContext(ctx))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" || !strings.Contains(rr.Body.String(), "error") {
		t.Fatalf("busy: %d %q %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body)
	}
	if n := len(api.offers.started["compaction|"]); n != 0 {
		t.Errorf("a 503 spent %d of the account's hourly reports", n)
	}
}

// The same history gives the same report: ids, order and best, on every run — including when
// many options tie, which is the common case (components that act on nothing reorder freely).
func TestAdaptReportIsDeterministicOnTies(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	sess := map[string][]adaptFixtureReq{}
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		sess[id] = []adaptFixtureReq{{ts: 1_000_000, fresh: 10, write: 5000, blocks: blocks},
			{ts: 1_060_000, fresh: 10, read: 5000, write: 50, blocks: blocks}}
	}
	db := adaptFixtureDB(t, "acct", sess)
	p := modelinfo.Price{Input: 1e-3, CacheRead: 0.1e-3, CacheWrite: 1.25e-3, Output: 5e-3}
	var first string
	for i := 0; i < 4; i++ {
		rep, err := db.AdaptCompaction(context.Background(), Filter{Tenant: "acct"}, staticTable{"m": p},
			"pipeline: [dedup, cachesplit]\n", defaultAdaptDocForTest)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString(rep.Best + "|")
		for _, o := range rep.Options {
			fmt.Fprintf(&b, "%s=%s:%.9f;", o.ID, o.Label, o.BilledUSD)
		}
		if i == 0 {
			first = b.String()
		} else if b.String() != first {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, b.String())
		}
	}
	if !strings.HasPrefix(first, "c") {
		t.Fatalf("expected a searched option to win on this history: %s", first)
	}
}

func TestAdaptTieBreakIsATotalOrder(t *testing.T) {
	cur := adaptSpec{comps: []string{"format"}}
	a := adaptSpec{comps: []string{"format", "dedup"}}
	b := adaptSpec{comps: []string{"format", "toon"}}
	c := adaptSpec{comps: []string{"searchfold"}}
	if !adaptTieLess(a, b, cur) || adaptTieLess(b, a, cur) {
		t.Error("equal moves and size must fall back to the spelling")
	}
	if !adaptTieLess(a, c, cur) {
		t.Error("fewer moves from current must win") // a: +1; c: -1 +1 = 2
	}
}

// flipCtx reports itself cancelled after its first Err call: the replay has already dispatched
// its sessions when the client goes away.
type flipCtx struct {
	context.Context
	n int
}

func (c *flipCtx) Err() error {
	c.n++
	if c.n > 1 {
		return context.Canceled
	}
	return nil
}

func TestAdaptReplayCancelledMidwayIsAnError(t *testing.T) {
	blocks := map[string]string{"messages.1.content.0.content": prettyJSON()}
	db := adaptFixtureDB(t, "acct", map[string][]adaptFixtureReq{"s1": {{ts: 1, write: 5000, blocks: blocks},
		{ts: 2, read: 5000, blocks: blocks}}})
	sessions, err := db.adaptDataset(context.Background(), Filter{Tenant: "acct"}, staticTable{"m": adaptTestPrice}, &AdaptCompactionCoverage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayAdapt(&flipCtx{Context: context.Background()}, adaptSpec{comps: []string{"format"}}, sessions, 0); err == nil {
		t.Fatal("a replay cut short after dispatch returned numbers")
	}
}
