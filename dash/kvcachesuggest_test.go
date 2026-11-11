package dash

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// findSuggestCell returns the cell for one (user, hour), or nil.
func findSuggestCell(out *KVCacheSuggestions, user string, hour int) *KVCacheSuggestion {
	for i := range out.Cells {
		if out.Cells[i].User == user && out.Cells[i].HourUTC == hour {
			return &out.Cells[i]
		}
	}
	return nil
}

// 2023-01-01 is a Sunday, 2023-01-06 a Friday and 2023-01-07 a Saturday — the fixed points every
// test below is built from, so a reader can check the weekday without running `date` themselves.

// A user whose ENTIRE history in this window is Friday and Saturday must not be read at all:
// not shown with a lower confidence, not folded into a neighbouring cell, simply absent.
func TestKVCacheSuggestExcludesFridayAndSaturday(t *testing.T) {
	sunday10 := time.Date(2023, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	friday10 := time.Date(2023, 1, 6, 10, 0, 0, 0, time.UTC).UnixMilli()
	saturday11 := time.Date(2023, 1, 7, 11, 0, 0, 0, time.UTC).UnixMilli()
	db := seedKV(t,
		kvEvent("weekday-user", "s1", "m", sunday10, 0, 100_000),
		kvEvent("weekend-user", "s2", "m", friday10, 0, 100_000),
		kvEvent("weekend-user", "s2", "m", saturday11, 0, 100_000),
	)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range out.Users {
		if u == "weekend-user" {
			t.Fatalf("weekend-user has only Friday/Saturday requests; must not appear: %v", out.Users)
		}
	}
	for _, c := range out.Cells {
		if c.User == "weekend-user" {
			t.Errorf("a cell attributes requests to weekend-user: %+v", c)
		}
	}
	c := findSuggestCell(out, "weekday-user", 10)
	if c == nil {
		t.Fatal("weekday-user's Sunday request produced no cell")
	}
	if c.Requests != 1 {
		t.Errorf("weekday-user hour 10 requests = %d, want 1", c.Requests)
	}
}

// A cell below the request floor still reports every candidate's own numbers — it is not
// hidden — but is flagged so a caller does not act on a couple of requests as a pattern.
func TestKVCacheSuggestFlagsInsufficientData(t *testing.T) {
	base := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	db := seedKV(t,
		kvEvent("sparse", "s1", "m", base, 0, 100_000),
		kvEvent("sparse", "s1", "m", base+120_000, 0, 100_000),
	)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := findSuggestCell(out, "sparse", 9)
	if c == nil {
		t.Fatal("no cell for sparse user, hour 9")
	}
	if !c.InsufficientData {
		t.Errorf("2 requests is below min_requests (%d); got %+v", out.MinRequests, c)
	}
	if len(c.Candidates) == 0 {
		t.Error("a below-floor cell must still report every candidate's own numbers")
	}
}

// On gaps that consistently outlive the 5-minute tier's own lifetime, holding the entry with
// keep-alives (or some other arm) is cheaper than the naive fixed-5m default, which pays a full
// re-creation on every turn for nothing: this is the case the suggester exists to catch.
func TestKVCacheSuggestPicksACheaperArmThanTheNaiveBaseline(t *testing.T) {
	start := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	var evs []*Event
	for i := int64(0); i < 6; i++ {
		evs = append(evs, kvEvent("heavy", "s1", "m", start+i*600_000, 0, 100_000)) // 10 min apart
	}
	db := seedKV(t, evs...)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := findSuggestCell(out, "heavy", 9)
	if c == nil {
		t.Fatal("no cell for heavy user, hour 9")
	}
	if c.InsufficientData {
		t.Fatalf("6 requests should clear the floor: %+v", c)
	}
	if c.Baseline != KVStrategyFixed5m {
		t.Fatalf("baseline = %q, want the registry's own default %q", c.Baseline, KVStrategyFixed5m)
	}
	if c.SavingUSD <= 0 || !c.SavingKnown {
		t.Errorf("expected some arm to beat fixed-5m on 10-minute gaps over a 100k prefix; "+
			"saving = %.6f known=%v best=%s", c.SavingUSD, c.SavingKnown, c.BestStrategy)
	}
	if c.BestStrategy == KVStrategyFixed5m {
		t.Error("fixed-5m cannot be the cheapest arm when every gap outlives its own lifetime")
	}
	// The winner must be checkable: its own Savings entry is in Candidates, and it agrees with
	// the top-level fields rather than being a second, driftable copy of them.
	found := false
	for _, s := range c.Candidates {
		if s.Strategy == c.BestStrategy {
			found = true
			if s.AbsoluteUSD != c.SavingUSD {
				t.Errorf("candidate saving %.6f disagrees with the reported best %.6f",
					s.AbsoluteUSD, c.SavingUSD)
			}
		}
	}
	if !found {
		t.Errorf("best strategy %q is not among its own cell's candidates", c.BestStrategy)
	}
}

// The suggestion is never worse than doing nothing: on gaps that never come back within any
// horizon this simulator knows, nothing beats the baseline, and the cell says so rather than
// picking an arm at random among equally-bad options.
func TestKVCacheSuggestFallsBackToBaselineWhenNothingWins(t *testing.T) {
	start := time.Date(2023, 1, 1, 8, 0, 0, 0, time.UTC).UnixMilli() // Sunday 08:00
	// One request per user per week, same hour: every gap is days long, far past any tier's
	// lifetime or keep-alive schedule, so caching never pays for itself.
	var evs []*Event
	for i := int64(0); i < 5; i++ {
		evs = append(evs, kvEvent("cold", "s1", "m", start+i*7*24*3600_000, 0, 50_000))
	}
	db := seedKV(t, evs...)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := findSuggestCell(out, "cold", 8)
	if c == nil {
		t.Fatal("no cell for cold user, hour 8")
	}
	if c.BestStrategy != KVStrategyNoCache && c.SavingUSD > 1e-9 {
		t.Errorf("with no reuse at all, no-cache (or the baseline itself) should win; got "+
			"best=%s saving=%.6f", c.BestStrategy, c.SavingUSD)
	}
}

// A baseline that is a real, buildable arm but not one kvSuggestCandidates() would ever offer
// on its own — `optimal`, the exact ceiling — must still be IN the comparison, or a cell can
// report a "best" that costs more than a baseline nothing was ever measured against.
func TestKVCacheSuggestNeverBeatsAnUnofferedBaseline(t *testing.T) {
	start := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	var evs []*Event
	for i := int64(0); i < 6; i++ {
		evs = append(evs, kvEvent("heavy", "s1", "m", start+i*600_000, 0, 100_000))
	}
	db := seedKV(t, evs...)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{Baseline: KVStrategyOptimal})
	if err != nil {
		t.Fatal(err)
	}
	if out.Baseline != KVStrategyOptimal {
		t.Fatalf("baseline = %q, want %q", out.Baseline, KVStrategyOptimal)
	}
	c := findSuggestCell(out, "heavy", 9)
	if c == nil {
		t.Fatal("no cell for heavy user, hour 9")
	}
	if c.SavingUSD < 0 {
		t.Errorf("nothing can beat the exact ceiling; saving must be 0, got %.6f (best=%s)",
			c.SavingUSD, c.BestStrategy)
	}
	foundBaseline := false
	for _, s := range c.Candidates {
		if s.Strategy == KVStrategyOptimal {
			foundBaseline = true
			if s.AbsoluteUSD != 0 {
				t.Errorf("the baseline's own saving against itself must be exactly 0, got %.6f",
					s.AbsoluteUSD)
			}
		}
	}
	if !foundBaseline {
		t.Error("the baseline itself is not among its own cell's candidates")
	}
}

// The exact ceiling is computed and frozen beside the winner for every cell, never as one
// of the candidates a sweep could pick (see kvSuggestCandidates' own doc comment) — so a
// reader always has a fixed "how much was there to capture" figure next to "how much this
// cell's own recommendation actually captures".
func TestKVCacheSuggestFreezesTheOptimalCeilingBesideTheWinner(t *testing.T) {
	start := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	var evs []*Event
	for i := int64(0); i < 6; i++ {
		evs = append(evs, kvEvent("heavy", "s1", "m", start+i*600_000, 0, 100_000)) // 10 min apart
	}
	db := seedKV(t, evs...)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := findSuggestCell(out, "heavy", 9)
	if c == nil {
		t.Fatal("no cell for heavy user, hour 9")
	}
	if !c.OptimalKnown {
		t.Fatalf("optimal should be priced on the same rows the baseline was: %+v", c)
	}
	// Nothing can cost less than the exact ceiling, including this cell's own winner.
	if c.OptimalUSD > c.BestUSD+1e-9 {
		t.Errorf("optimal_usd %.6f costs more than the cell's own best %.6f", c.OptimalUSD, c.BestUSD)
	}
	if c.OptimalSavingUSD < c.SavingUSD-1e-9 {
		t.Errorf("optimal_saving_usd %.6f is less than the winner's own saving %.6f — the "+
			"ceiling must never be BELOW what was actually recommended", c.OptimalSavingUSD, c.SavingUSD)
	}
	// A reference figure, not a result: `optimal` must never be reportable as this cell's
	// own winner or appear in its candidate list.
	if c.BestStrategy == KVStrategyOptimal {
		t.Error("optimal must never be reported as a cell's own best_strategy")
	}
	for _, s := range c.Candidates {
		if s.Strategy == KVStrategyOptimal {
			t.Error("optimal must never appear among a cell's own candidates")
		}
	}
}

// An unresolvable baseline must fail the whole request, the same way it fails
// /api/kvcache/simulate — not degrade into a 200 full of empty cells.
func TestKVCacheSuggestRejectsAnUnknownBaseline(t *testing.T) {
	db := seedKV(t, kvEvent("t", "s", "m", kvBase, 0, 100_000))
	_, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{Baseline: "not-a-real-strategy"})
	if err == nil {
		t.Fatal("an unknown baseline must be an error, not a silently-empty result")
	}
	if !strings.Contains(err.Error(), "unknown baseline strategy") {
		t.Errorf("error = %q, want it to name the baseline as unknown (matching KVCacheSimulate's "+
			"own wording, which kvCacheSuggest's handler string-matches for its 400)", err.Error())
	}
}

// The HTTP route must map that same error to 400, exactly as /api/kvcache/simulate does.
func TestKVCacheSuggestRouteRejectsAnUnknownBaselineAs400(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, kvEvent("", "s", "m", kvBase, 0, 100_000))
	w, _ := get(t, a, "/api/kvcache/suggest?baseline=not-a-real-strategy", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("/api/kvcache/suggest?baseline=bogus = %d, want %d: %s",
			w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// Every route this page mounts must answer, and this one specifically must not regress into
// panicking on the empty-candidate or empty-window path.
func TestKVCacheSuggestRouteAnswersOnRealData(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	sunday9 := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli()
	seed(t, rec, kvEvent("", "s", "m", sunday9, 0, 100_000))
	w, body := get(t, a, "/api/kvcache/suggest", "")
	if w.Code != 200 {
		t.Fatalf("/api/kvcache/suggest = %d: %s", w.Code, w.Body.String())
	}
	if body == nil {
		t.Fatal("/api/kvcache/suggest served no JSON object")
	}
	if _, ok := body["cells"]; !ok {
		t.Errorf("response has no \"cells\" key: %v", body)
	}
}

// A NAMED CANDIDATE LIST IS THE WHOLE COMPARISON. With Strategies set, no other arm may win a cell —
// an operator who rules out no-cache and the 1h tier must never be handed one — and every cell
// still reports the baseline. An arm the sweep cannot pick fails the request.
func TestKVCacheSuggestPicksOnlyFromTheNamedCandidates(t *testing.T) {
	base := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	var evs []*Event
	for i := int64(0); i < 10; i++ {
		evs = append(evs, kvEvent("u", "s1", "m", base+i*420_000, 0, 100_000)) // 7-minute gaps
	}
	db := seedKV(t, evs...)
	allowed := []string{KVStrategyFixed5m, KVStrategyKeepAlive5mOnce, "keepalive-5m-x3"}
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{Strategies: allowed})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Cells) == 0 {
		t.Fatal("no cells")
	}
	for _, c := range out.Cells {
		if !slices.Contains(allowed, c.BestStrategy) {
			t.Errorf("cell %s/%d chose %q, outside the named candidates %v", c.User, c.HourUTC, c.BestStrategy, allowed)
		}
		for _, cand := range c.Candidates {
			if !slices.Contains(allowed, cand.Strategy) {
				t.Errorf("cell %s/%d scored %q, outside the named candidates", c.User, c.HourUTC, cand.Strategy)
			}
		}
	}
	for _, bad := range []string{"optimal", "no-such-arm", KVStrategyCustom} {
		if _, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
			KVCacheSimConfig{Strategies: []string{bad}}); err == nil {
			t.Errorf("Strategies=[%q] was accepted", bad)
		}
	}
}
