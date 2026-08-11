package dash

import (
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// TestCaptureDropsRatherThanBlocks is the property that lets the dashboard exist
// at all: with the queue full and the writer wedged, Record must return
// immediately and count the loss. If this ever blocks, enabling observability
// becomes a latency incident.
func TestCaptureDropsRatherThanBlocks(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", QueueSize: 4,
		BatchSize: 1000, FlushInterval: time.Hour}) // writer will not drain
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const n = 500
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			r.Record(&Event{SessionID: "s"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a full queue; capture must never block a request")
	}

	s := r.Stats()
	if s.Captured != n {
		t.Errorf("captured = %d; want %d", s.Captured, n)
	}
	if s.Dropped == 0 {
		t.Fatal("a full queue dropped nothing; either the drop is not counted or the queue grew")
	}
	// With a depth-4 queue and a writer that never flushes, the overwhelming
	// majority must be dropped — proving the queue is genuinely bounded and not
	// quietly growing to absorb the burst.
	if s.Dropped < int64(n)/2 {
		t.Errorf("only %d of %d dropped through a depth-%d queue; is the queue unbounded?",
			s.Dropped, n, s.QueueCap)
	}
	// The drop must be VISIBLE, not merely survived — every other number depends on
	// the viewer knowing coverage was incomplete.
	if s.QueueCap != 4 {
		t.Errorf("queue cap = %d; want the configured 4", s.QueueCap)
	}
}

func TestCloseDrainsQueuedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	r, err := NewRecorder(Options{DBPath: path, QueueSize: 1024,
		BatchSize: 10000, FlushInterval: time.Hour}) // nothing flushes until Close
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		r.Record(mkEvent(int64(1000+i), "s", "m", 100, 90))
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	page, err := db.Requests(Filter{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 25 {
		t.Errorf("Close persisted %d of 25 queued events", page.Total)
	}
}

func TestObserveDetectsColdStartOncePerSessionAndModel(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	seenSess, seenModel, since := r.Observe("s1", "m1", 1000)
	if seenSess || seenModel {
		t.Error("first request for a session+model must report both unseen (cold start)")
	}
	if since != 0 {
		t.Errorf("since = %d on a first request; want 0", since)
	}
	seenSess, seenModel, since = r.Observe("s1", "m1", 4000)
	if !seenSess || !seenModel {
		t.Error("second request must report the session and model as seen")
	}
	if since != 3000 {
		t.Errorf("since = %d; want 3000", since)
	}
	// A NEW session on an already-seen model is still a session cold start.
	if s, m, _ := r.Observe("s2", "m1", 5000); s || !m {
		t.Errorf("new session on known model: seenSession=%v seenModel=%v; want false,true", s, m)
	}
}

func TestAttributeCacheBuckets(t *testing.T) {
	const ttl = 300_000
	cases := []struct {
		name                   string
		read                   int64
		seenSession, seenModel bool
		sinceMs                int64
		prefixChanged          bool
		want                   string
	}{
		{"a cache read is a hit", 5000, true, true, 1000, false, CacheHit},
		{"first request of a session is a cold start", 0, false, true, 0, true, CacheColdStart},
		{"first request for a model is a cold start", 0, true, false, 0, true, CacheColdStart},
		{"a gap past the TTL is expiry", 0, true, true, ttl + 1, false, CacheTTLExpiry},
		{"TTL wins the tie against a changed prefix", 0, true, true, ttl + 1, true, CacheTTLExpiry},
		{"a changed prefix inside the TTL is a bust", 0, true, true, 1000, true, CachePrefixChange},
		{"otherwise unknown", 0, true, true, 1000, false, CacheUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Event{CacheRead: tc.read}
			e.AttributeCache(tc.seenSession, tc.seenModel, tc.sinceMs, ttl, tc.prefixChanged)
			if e.CacheMissReason != tc.want {
				t.Errorf("got %q, want %q", e.CacheMissReason, tc.want)
			}
		})
	}
}

func TestMarkUniqueDedupsByContentKey(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// First sighting: all of it is new.
	if got := r.MarkUnique("extract", []string{"k1", "k2"}, 200); got != 200 {
		t.Errorf("first sighting = %d; want 200", got)
	}
	// Same compaction re-sent on the next turn: nothing new.
	if got := r.MarkUnique("extract", []string{"k1", "k2"}, 200); got != 0 {
		t.Errorf("re-sent compaction = %d; want 0 (this is what stops gross from lying)", got)
	}
	// Half new: proportional attribution.
	if got := r.MarkUnique("extract", []string{"k2", "k3"}, 200); got != 100 {
		t.Errorf("half-new = %d; want 100", got)
	}
	// Keys are namespaced per component, so two components stashing the same content
	// each get credit for their own work.
	if got := r.MarkUnique("dedup", []string{"k1"}, 50); got != 50 {
		t.Errorf("other component = %d; want 50 (keys must be per-component)", got)
	}
	// No keys at all: count the run once (the Aggregator's rule).
	if got := r.MarkUnique("collapse", nil, 70); got != 70 {
		t.Errorf("keyless run = %d; want 70", got)
	}
}

func TestFromTraceMapsPipelineOutcome(t *testing.T) {
	tr := apply.Trace{
		Session: "s1", CacheAware: true, MaxCachedIdx: 3, Messages: 8,
		AttemptedTokens: 400, FrozenTokens: 600,
		Run: &components.RunReport{
			TokensBefore: 1000, TokensAfter: 800,
			Components: []components.Report{
				{Component: "extract", Kind: "offload", TokensBefore: 1000, TokensAfter: 800},
				{Component: "dedup", Kind: "offload", TokensBefore: 800, TokensAfter: 800, Skipped: true},
				{Component: "boom", Kind: "offload", TokensBefore: 800, TokensAfter: 800, Reverted: true},
			},
		},
		Changes: []apply.Change{{Path: "messages.4", BeforeTokens: 200, AfterTokens: 0,
			Before: "big output", After: ""}},
	}
	var e Event
	e.FromTrace(tr, map[string]int{"extract": 150})

	if e.SessionID != "s1" || !e.CacheAware || e.Messages != 8 {
		t.Errorf("header fields wrong: %+v", e)
	}
	if e.TokensBefore != 1000 || e.TokensAfter != 800 || e.Saved() != 200 {
		t.Errorf("token accounting wrong: %d -> %d", e.TokensBefore, e.TokensAfter)
	}
	if e.AttemptedTokens != 400 || e.FrozenTokens != 600 {
		t.Errorf("eligibility wrong: attempted=%d frozen=%d", e.AttemptedTokens, e.FrozenTokens)
	}
	if e.SavedUnique != 150 {
		t.Errorf("unique saved = %d; want the deduped 150, not the gross 200", e.SavedUnique)
	}
	if e.Reverts != 1 {
		t.Errorf("reverts = %d; want 1", e.Reverts)
	}
	if len(e.Components) != 3 {
		t.Fatalf("component rows = %d; want 3", len(e.Components))
	}
	if !e.Components[0].Acted || !e.Components[0].Mutated {
		t.Error("the acting component was not marked acted+mutated")
	}
	if e.Components[1].Mutated || !e.Components[1].Skipped {
		t.Error("a skipped component must not be marked mutated")
	}
	if e.Components[2].Mutated {
		t.Error("a reverted component must not be marked mutated")
	}
	if len(e.Content) != 1 || e.Content[0].Path != "messages.4" {
		t.Errorf("content not carried: %+v", e.Content)
	}
	if e.UncompressedReason != "" {
		t.Errorf("a request that saved tokens must have no uncompressed reason, got %q", e.UncompressedReason)
	}
}

func TestUncompressedReasonBuckets(t *testing.T) {
	cases := []struct {
		name string
		tr   apply.Trace
		want string
	}{
		{"bypassed", apply.Trace{Bypassed: true}, ReasonBypassed},
		{"no run", apply.Trace{}, ReasonNoMessages},
		{"no messages", apply.Trace{Messages: 0, Run: &components.RunReport{}}, ReasonNoMessages},
		{"nothing triggered", apply.Trace{Messages: 3, Run: &components.RunReport{
			TokensBefore: 100, TokensAfter: 100,
			Components: []components.Report{{Component: "extract", Skipped: true}},
		}}, ReasonBelowTrigger},
		{"all frozen", apply.Trace{Messages: 3, CacheAware: true, AttemptedTokens: 0,
			Run: &components.RunReport{TokensBefore: 100, TokensAfter: 100,
				Components: []components.Report{{Component: "extract"}}}}, ReasonAllFrozen},
		{"all reverted", apply.Trace{Messages: 3, Run: &components.RunReport{
			TokensBefore: 100, TokensAfter: 100,
			Components: []components.Report{{Component: "extract", Reverted: true}},
		}}, ReasonReverted},
		{"ran, found nothing", apply.Trace{Messages: 3, AttemptedTokens: 100,
			Run: &components.RunReport{TokensBefore: 100, TokensAfter: 100,
				Components: []components.Report{{Component: "extract"}}}}, ReasonNoSavings},
		{"compacted", apply.Trace{Messages: 3, AttemptedTokens: 100,
			Run: &components.RunReport{TokensBefore: 100, TokensAfter: 50,
				Components: []components.Report{{Component: "extract", TokensBefore: 100, TokensAfter: 50}}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e Event
			e.FromTrace(tc.tr, nil)
			if e.UncompressedReason != tc.want {
				t.Errorf("reason = %q; want %q", e.UncompressedReason, tc.want)
			}
		})
	}
}

func TestPriceNeverReportsUnknownCostAsFree(t *testing.T) {
	p := modelinfo.Price{Input: 2e-06, Output: 1e-05, CacheRead: 2e-07, CacheWrite: 2.5e-06}

	// Complete accounting: cost, plus a baseline that prices the UNIQUE removed
	// tokens at the cache-WRITE rate they would have entered as and the re-sent
	// remainder at the cache-READ rate the provider would have served it from. Here
	// every removed token is unique, so the whole 200 gets the write rate.
	e := &Event{TokensBefore: 1000, TokensAfter: 800, SavedUnique: 200, FreshInput: 10,
		CacheRead: 5000, CacheWrite: 500, OutputTokens: 100}
	e.Price(p, true)
	if e.TokenAccounting != AccountingComplete {
		t.Errorf("accounting = %q; want complete", e.TokenAccounting)
	}
	// Compare with a tolerance, not ==. Price.Cost is a sum of four products, and
	// Go PERMITS the compiler to contract `x*y + z` into a fused multiply-add (FMA),
	// which rounds once instead of twice. Whether it does so differs between the two
	// evaluations here — the test's literal arguments fold differently from the
	// struct-field loads inside Price — so on arm64 the two mathematically identical
	// sums land one bit apart (0.00327 vs 0.0032700000000000003) and an exact
	// comparison fails. amd64 has no FMA contraction for this shape, so CI is green
	// and the failure only appears on Apple Silicon. The property under test is the
	// accounting, which a 1-ULP difference does not affect.
	const eps = 1e-12
	wantCost := p.Cost(10, 5000, 500, 100)
	if math.Abs(e.CostUSD-wantCost) > eps {
		t.Errorf("cost = %v; want %v", e.CostUSD, wantCost)
	}
	if math.Abs(e.BaselineCostUSD-(wantCost+200*p.CacheWrite)) > eps {
		t.Errorf("baseline = %v; want cost + 200 unique removed tokens at the cache-write rate", e.BaselineCostUSD)
	}
	if e.BaselineCostUSD <= e.CostUSD {
		t.Error("baseline must exceed actual when tokens were removed")
	}

	// No usage data: partial, and NOT priced — a cost we cannot compute must read as
	// unknown, never as zero.
	e2 := &Event{TokensBefore: 1000, TokensAfter: 800}
	e2.Price(p, false)
	if e2.TokenAccounting != AccountingPartial {
		t.Errorf("accounting = %q; want partial", e2.TokenAccounting)
	}
	if e2.CostUSD != 0 || e2.BaselineCostUSD != 0 {
		t.Error("an unpriceable request must leave costs at zero AND be flagged, not be priced")
	}

	// No pricing table and no content either: missing.
	e3 := &Event{}
	e3.Price(modelinfo.Price{}, true)
	if e3.TokenAccounting != AccountingMissing {
		t.Errorf("accounting = %q; want missing", e3.TokenAccounting)
	}
}

// TestDollarsDeriveFromUniqueNotGrossSavings is the regression test for the defect
// this dashboard shipped with: `net_dollars_saved` was priced off GROSS savings, so a
// single compaction re-sent on every later turn was paid for once per turn. The tile
// read $7.00 beside an `overcount_ratio` of 13.1x — the dashboard displayed the
// correction factor for its own headline and did not apply it.
//
// The fixture is DELIBERATELY overcounted: one 1,000-token compaction, unique on turn
// 1, re-sent unchanged on nine further turns. Gross savings are 10,000 tokens;
// genuinely-never-sent content is 1,000. Both pricing bugs are pinned:
//
//   - the denominator (unique, not gross), and
//   - the tier (the re-sent remainder is a cache READ, not a cache WRITE; on this
//     price table a write is 12.5x a read, so mispricing it inflates on top of the
//     overcount).
//
// A gross+write implementation reports ~11.4x the correct figure here and fails.
func TestDollarsDeriveFromUniqueNotGrossSavings(t *testing.T) {
	p := modelinfo.Price{Input: 2e-06, Output: 1e-05, CacheRead: 2e-07, CacheWrite: 2.5e-06}
	const turns, saved, uniqueTurn = 10, 1000, 0

	db := openTestDB(t)
	var events []*Event
	for i := range turns {
		e := &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m",
			TokensBefore: 50_000, TokensAfter: 50_000 - saved,
			// Unique only on the first turn: every later turn re-sends the same content,
			// which is exactly what MarkUnique's dedup reports.
			FreshInput: 10, CacheRead: 49_000, CacheWrite: 100, OutputTokens: 50,
		}
		if i == uniqueTurn {
			e.SavedUnique = saved
		}
		e.Price(p, true)
		events = append(events, e)
	}
	if err := db.insertBatch(events); err != nil {
		t.Fatal(err)
	}

	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}

	// The fixture's own overcount factor, as the dashboard computes and displays it.
	if o.SavedGross != turns*saved || o.SavedUnique != saved {
		t.Fatalf("fixture: gross=%d unique=%d; want %d/%d", o.SavedGross, o.SavedUnique, turns*saved, saved)
	}
	if o.OvercountRatio != float64(turns) {
		t.Fatalf("overcount_ratio = %v; want %v", o.OvercountRatio, float64(turns))
	}

	// The dollar figure the tile renders, from first principles: the unique 1,000
	// tokens would have been new input (cache-write), the 9,000 re-sent ones would
	// have been served as cache reads.
	wantDelta := float64(saved)*p.CacheWrite + float64((turns-1)*saved)*p.CacheRead
	if got := o.BaselineCostUSD - o.CostUSD; math.Abs(got-wantDelta) > 1e-12 {
		t.Errorf("baseline − actual = %.10f; want %.10f", got, wantDelta)
	}
	if math.Abs(o.NetSavedUSD-wantDelta) > 1e-12 {
		t.Errorf("net_saved_usd = %.10f; want %.10f", o.NetSavedUSD, wantDelta)
	}

	// And the explicit anti-regression: the figure the old code produced. Pricing all
	// 10,000 gross tokens at the cache-write rate is 11.36x the honest number, so a
	// reversion cannot slip through as a rounding difference.
	grossWritePriced := float64(turns*saved) * p.CacheWrite
	if math.Abs(o.NetSavedUSD-grossWritePriced) < 1e-9 {
		t.Fatal("net_saved_usd still prices GROSS savings at the cache-write rate")
	}
	if o.NetSavedUSD >= grossWritePriced {
		t.Errorf("net_saved_usd %.10f should be far below the gross-priced %.10f",
			o.NetSavedUSD, grossWritePriced)
	}
}

// TestBaselineNeverPricesMoreThanTheRequestSaved guards the clamp: SavedUnique is
// attributed per component and can exceed a single request's own gross saving when
// several components stash the same content key. Without the clamp the "re-sent
// remainder" term goes negative and the baseline inflates.
func TestBaselineNeverPricesMoreThanTheRequestSaved(t *testing.T) {
	p := modelinfo.Price{CacheRead: 2e-07, CacheWrite: 2.5e-06}
	e := &Event{TokensBefore: 1000, TokensAfter: 900, SavedUnique: 5000}
	e.Price(p, true)
	if want := 100 * p.CacheWrite; math.Abs(e.BaselineCostUSD-e.CostUSD-want) > 1e-12 {
		t.Errorf("baseline delta = %.12f; want %.12f (clamped to the 100 tokens actually removed)",
			e.BaselineCostUSD-e.CostUSD, want)
	}
}

func TestAgentClassification(t *testing.T) {
	cases := map[string]string{
		"claude-cli/2.0.14 (external, cli)": "claude-cli",
		"claude-code/1.2.3":                 "claude-code",
		"codex_cli_rs/0.4.0":                "codex",
		"Gemini-CLI/1.0":                    "gemini-cli",
		"":                                  "unknown",
		"curl/8.5.0":                        "curl",
		"SomeVeryLongUnknownAgentNameWithNoDelimiterAtAllThatKeepsGoingForever": "someverylongunknownagentnamewith",
	}
	for ua, want := range cases {
		if got := AgentFor(ua); got != want {
			t.Errorf("AgentFor(%q) = %q; want %q", ua, got, want)
		}
	}
}

// TestConcurrentCaptureIsRaceFree drives every shared structure at once. Run under
// -race, this is the mandatory check on the capture path.
func TestConcurrentCaptureIsRaceFree(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", QueueSize: 256, BatchSize: 16,
		FlushInterval: 2 * time.Millisecond, CaptureContent: true, ContentCap: 1024})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				e := mkEvent(int64(1000+i), "sess", "model", 1000, 900)
				e.Content = []ContentRow{{Path: "messages.1", Before: "before", After: "after"}}
				r.Observe(e.SessionID, e.Model, e.TS)
				r.MarkUnique("extract", []string{"k1", "k2"}, 100)
				r.Record(e)
				if i%20 == 0 {
					_ = r.Stats()
				}
			}
		}(g)
	}
	// Concurrent readers, racing the writer's commits.
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				if _, err := r.DB().Overview(Filter{}); err != nil {
					t.Errorf("Overview during concurrent writes: %v", err)
					return
				}
				if _, err := r.DB().Requests(Filter{}, 0, 10); err != nil {
					t.Errorf("Requests during concurrent writes: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
