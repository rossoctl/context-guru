package dash

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

// The one class of wire break that NOMINAL checks cannot see: a key that keeps its name and
// changes its MEANING.
//
// Three separate guards now protect the contract between this package's payloads and the page
// that reads them, and all three are nominal — the receiver-scoped read check in
// uikvcache_test.go catches a key that vanished from the payloads, kvcache's own golden set
// catches one that moved between shapes or gained an omitempty, and the JSON-serialization test
// catches a shape that stopped being emitted. None of them would notice `hit_rate_pct` becoming
// a fraction, or `total_usd` becoming a premium. The field is still there, still spelled the
// same, and every consumer silently renders it wrong: 0.766 formatted as "0.8%", or a cost
// comparison whose winner inverts.
//
// This file is pointed at the meanings instead. Every assertion is an INVARIANT the value must
// satisfy whatever it is called, checked over a production-shaped replay, so a unit change or a
// redefinition fails here rather than reaching a reader.
//
// It is a new file rather than an addition to kvcacheshape_test.go deliberately: that one asserts
// the SHAPE of a distribution (are the reuse probabilities plausible, is the ceiling really a
// ceiling), which catches some of this class by accident. Accidental coverage is worth having and
// is not a guard. These are by design.

// Every field whose name ends in `_pct` is a SHARE, and a share is in [0,100].
//
// WHAT THIS ALONE CANNOT CATCH, measured rather than assumed: a share expressed as a FRACTION.
// 0.766 is inside [0,100], so dropping the ×100 from a hit rate passes this check untouched — I
// verified that by dropping it, and this test stayed green. Only recomputing a percentage from its
// own numerator and denominator catches it, which is what
// TestEveryRateIsRecomputableFromItsOwnCounts below does, and which is why KVCacheGroup carries
// `hits` at all. The range check earns its place on the fields where no count exists to
// recompute from; it is not the whole guard.
//
// Walked by reflection over the real payload values rather than a hand-listed set of fields, so a
// new percentage is covered the day it lands and cannot be forgotten. The naming convention is
// load-bearing and is the reason a signed comparison is called `percent_usd` and not
// `percent_usd_pct`: that one is a difference against a baseline, legitimately negative and
// legitimately past 100, and it must not be filed under the same rule.
func TestEveryPercentageFieldIsAShareAndNotAFraction(t *testing.T) {
	db := seedKV(t, productionShaped()...)
	an, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sim, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.KVCacheRows(allTenants(), KVCacheOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, root := range []any{an, sim, page} {
		walkPercentages(t, reflect.ValueOf(root), "", &checked)
	}
	// A positive assertion that the walk found something: a reflection check that silently
	// traversed nothing is the failure mode that makes this whole file worthless.
	if checked < 12 {
		t.Errorf("only %d percentage fields were checked; the payloads carry more than that, so "+
			"the walk is not reaching them", checked)
	}
	t.Logf("checked %d percentage fields", checked)
}

// walkPercentages descends a payload and asserts the range of every `_pct` float it finds.
func walkPercentages(t *testing.T, v reflect.Value, path string, n *int) {
	t.Helper()
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			walkPercentages(t, v.Elem(), path, n)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkPercentages(t, v.Index(i), path+"[]", n)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walkPercentages(t, v.MapIndex(k), path+"."+k.String(), n)
		}
	case reflect.Struct:
		typ := v.Type()
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath != "" {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				name = f.Name
			}
			child := path + "." + name
			if v.Field(i).Kind() == reflect.Float64 && strings.HasSuffix(name, "_pct") {
				*n++
				got := v.Field(i).Float()
				if got < 0 || got > 100 {
					t.Errorf("%s = %v, which is not a share. A percentage field outside [0,100] "+
						"is either a fraction wearing a percentage's name — every consumer then "+
						"renders 0.77 as \"0.8%%\" — or a signed comparison that belongs under a "+
						"different name.", strings.TrimPrefix(child, "."), got)
				}
				continue
			}
			walkPercentages(t, v.Field(i), child, n)
		}
	}
}

// Every rate is recomputable from the counts beside it, which is the only thing that catches a
// share silently expressed as a fraction.
//
// The range check above is blind to it — 0.766 is a legal value in [0,100] — so this is where a
// missing ×100 fails. It is also where a swapped denominator fails: an observed group's hit rate is
// over its OWN requests, and a rate computed against some wider total would read plausibly and be
// wrong everywhere.
func TestEveryRateIsRecomputableFromItsOwnCounts(t *testing.T) {
	db := seedKV(t, productionShaped()...)
	an, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	near2(t, "cards hit_rate_pct against its own counts", an.Cards.HitRatePct,
		100*float64(an.Cards.Hits)/float64(an.Cards.Requests))
	groups := map[string][]KVCacheGroup{"by_ttl": an.ByTTL, "by_bucket": an.ByBucket,
		"by_user": an.ByUser, "by_model": an.ByModel}
	var checked int
	for name, rows := range groups {
		for _, g := range rows {
			if g.Requests == 0 {
				continue
			}
			checked++
			near2(t, name+"["+g.Key+"] hit_rate_pct against its own counts", g.HitRatePct,
				100*float64(g.Hits)/float64(g.Requests))
			if g.Hits > g.Requests {
				t.Errorf("%s[%s]: %d hits out of %d requests", name, g.Key, g.Hits, g.Requests)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no group carried any requests, so nothing here was checked")
	}
	// The groups' hits sum to the whole dataset's, for the groupings that partition it.
	for _, name := range []string{"by_ttl", "by_bucket", "by_user", "by_model"} {
		var sum int64
		for _, g := range groups[name] {
			sum += g.Hits
		}
		if sum != an.Cards.Hits {
			t.Errorf("%s holds %d hits, the dataset has %d", name, sum, an.Cards.Hits)
		}
	}
}

// The COST identities — total_usd against its five parts, the premium, and the three savings
// fields — are asserted in kvcacheidentity_test.go, as costIdentityFailures(), and deliberately
// not restated here.
//
// Two definitions of an identity are worse than none: they drift, and then a reader has two green
// lines and no way to tell which one describes the arithmetic. That file states them once, proves
// each one FIRES against a hand-built Result with exactly one field wrong, and checks them over a
// real replay so the negative cases cannot pass over a function that is simply wrong about what
// the fields mean.
//
// What stays in this file is the half that file does not reach: the OBSERVED dataset. Its
// identities are per-arm over kvcache.Result, and KVCacheCards and KVCacheGroup are a different
// measurement — what happened, rather than what a strategy would have cost.

// The reuse percentages are recomputable from their own numerator and denominator, and the two
// horizons NEST.
//
// Two meanings that could drift without the name changing: the denominator becoming Requests
// rather than WithNext — which silently folds every conversation's last request in as a
// zero-second return, the exact lie this page is built to avoid — and the horizons ceasing to
// nest, which would mean one of them is no longer measuring "arrived within".
func TestTheReuseSharesAreRecomputableAndTheHorizonsNest(t *testing.T) {
	db := seedKV(t, productionShaped()...)
	an, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := an.Cards
	if c.WithNext == 0 {
		t.Fatal("the fixture produced no gaps, so nothing here is being checked")
	}
	near2(t, "within_5m_pct against its own counts", c.Within5mPct,
		100*float64(c.Within5m)/float64(c.WithNext))
	near2(t, "within_1h_pct against its own counts", c.Within1hPct,
		100*float64(c.Within1h)/float64(c.WithNext))
	// The denominator is WithNext and NOT Requests. Stated as its own assertion because the
	// difference is invisible in the value alone on a corpus with few final requests, and this
	// corpus has many.
	if c.FinalRequests == 0 {
		t.Fatal("the fixture has no final requests, so it cannot tell the two denominators apart")
	}
	if wrong := 100 * float64(c.Within5m) / float64(c.Requests); sameFloat(c.Within5mPct, wrong) {
		t.Errorf("within_5m_pct (%.4f) is indistinguishable from the same count over REQUESTS "+
			"(%.4f); the denominator must be the requests that have a successor", c.Within5mPct,
			wrong)
	}
	for _, g := range append(append([]KVCacheGroup{}, an.ByTTL...), an.ByModel...) {
		if g.WithNext == 0 {
			continue
		}
		if g.Within1hPct < g.Within5mPct {
			t.Errorf("%s: within_1h_pct %.2f < within_5m_pct %.2f; an hour contains five minutes, "+
				"so the shares must nest", g.Key, g.Within1hPct, g.Within5mPct)
		}
	}
	// The survival curve is a CDF: non-decreasing, and its last rung is the widest.
	var prev float64
	for _, p := range an.Survival {
		if p.ArrivedPct < prev {
			t.Errorf("the survival curve falls at %s: %.2f%% after %.2f%%. A cumulative share "+
				"cannot decrease.", p.Label, p.ArrivedPct, prev)
		}
		prev = p.ArrivedPct
		if p.N > 0 && !sameFloat(p.ArrivedPct, 100*float64(p.Arrived)/float64(p.N)) {
			t.Errorf("survival at %s: arrived_pct %.4f does not match %d of %d", p.Label,
				p.ArrivedPct, p.Arrived, p.N)
		}
	}
	// And it agrees with the cards at the two horizons, which is the cross-panel check: two views
	// of one measurement that disagree is the defect this dashboard has shipped before.
	at := map[float64]SurvivalPoint{}
	for _, p := range an.Survival {
		at[p.Seconds] = p
	}
	near2(t, "the survival curve at five minutes against the summary card",
		at[kvcache.Horizon5m.Seconds()].ArrivedPct, c.Within5mPct)
	near2(t, "the survival curve at one hour against the summary card",
		at[kvcache.Horizon1h.Seconds()].ArrivedPct, c.Within1hPct)
}

// sameFloat is the tolerance the recomputation checks above use. Named apart from meta_test.go's
// own `near`, which this package already owns for a different comparison.
func sameFloat(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
