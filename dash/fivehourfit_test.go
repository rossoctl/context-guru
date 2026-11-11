package dash

import (
	"math"
	"math/rand"
	"testing"
)

// fhSim builds synthetic subscription traffic with known weights (#1047).
type fhSim struct {
	w          [fhTiers]float64 // the true share of the limit per token
	windows    int
	perWindow  int     // requests per window, at most
	outside    float64 // the chance, per request, of a burst of traffic outside the proxy
	burst      float64 // the largest burst, as a share of the limit
	headerLags bool    // true: a row's header does not count the row itself
	oneHour    float64 // the share of sessions on the 1-hour cache
	tokScale   float64 // multiplies every token count (1 = a normal agent)
	seed       int64
}

// realW is a set of weights that is NOT the API price mix: cache reads weigh a third of what API
// prices say, relative to input. The fit must find that, not the price list.
var realW = [fhTiers]float64{3e-7, 1e-8, 3.75e-7, 6e-7, 1.5e-6}

func (s fhSim) rows() []FiveHourRow {
	rng := rand.New(rand.NewSource(s.seed))
	scale := s.tokScale
	if scale == 0 {
		scale = 1
	}
	var out []FiveHourRow
	ts := int64(1_700_000_000_000)
	for w := 0; w < s.windows; w++ {
		reset := ts/1000 + 5*3600
		used := rng.Float64() * 0.05 // what the account used before the proxy saw a row
		prefix := 20_000.0
		oneHour := rng.Float64() < s.oneHour
		for i := 0; i < s.perWindow; i++ {
			ts += int64(5_000 + rng.Intn(60_000))
			if rng.Float64() < 0.05 {
				prefix = 20_000 + rng.Float64()*30_000 // a new session
			}
			var r FiveHourRow
			r.TsMs, r.Reset, r.Model = ts, reset, "claude-sonnet-4-5"
			newTok := 300 + rng.Float64()*6000
			if rng.Float64() < 0.08 {
				// A cold start or an expiry: the whole prefix is written again.
				write := prefix + newTok
				if oneHour {
					r.Write1h = int64(write * scale)
				} else {
					r.Write5m = int64(write * scale)
				}
			} else {
				r.CacheRead = int64(prefix * scale)
				if oneHour {
					r.Write1h = int64(newTok * scale)
				} else {
					r.Write5m = int64(newTok * scale)
				}
			}
			r.Input = int64((1 + rng.Float64()*400) * scale)
			r.Output = int64((100 + rng.Float64()*3000) * scale)
			prefix = math.Min(prefix+newTok, 180_000)
			before := used
			used += fhDot(s.w, r.tiers())
			if rng.Float64() < s.outside {
				used += rng.Float64() * s.burst
			}
			if used >= 0.99 {
				break
			}
			seen := used
			if s.headerLags {
				seen = before
			}
			r.Util = math.Floor(seen*100) / 100
			out = append(out, r)
		}
		ts += 5 * 3600 * 1000
	}
	return out
}

// apiPrice is a Sonnet-like price list per token.
func apiPrice(string) (TierPrice, bool) {
	return TierPrice{3e-6, 0.3e-6, 3.75e-6, 6e-6, 15e-6}, true
}

func fhScaled(w [fhTiers]float64, k float64) [fhTiers]float64 {
	for j := range w {
		w[j] *= k
	}
	return w
}

func fhRelErr(got, want float64) float64 { return math.Abs(got-want) / want }

func fhCheck(t *testing.T, name string, fit FiveHourFit, tol float64) {
	t.Helper()
	t.Logf("%s: ok=%v reason=%q requests=%d windows=%d spans=%d dropped=%d fit_error=%.3f header_after_row=%v",
		name, fit.OK, fit.Reason, fit.Requests, fit.Windows, fit.Spans, fit.DroppedSpans, fit.FitError, fit.HeaderAfterRow)
	if !fit.OK {
		t.Fatalf("%s: the fit failed the gate: %s", name, fit.Reason)
	}
	for j, n := range fhTierNames {
		t.Logf("  %-14s true %.3g  got %.3g  err %4.1f%%  learned=%v rel_se=%.3f", n, realW[j], fit.Weights[n],
			100*fhRelErr(fit.Weights[n], realW[j]), fit.Learned[n], fit.RelSE[n])
	}
	for _, j := range []int{fhCacheRead, fhWrite5m, fhOutput} {
		n := fhTierNames[j]
		if e := fhRelErr(fit.Weights[n], realW[j]); e > tol {
			t.Errorf("%s: %s weight %.3g, want %.3g within %.0f%% (off by %.1f%%)", name, n, fit.Weights[n], realW[j], 100*tol, 100*e)
		}
	}
}

func TestFiveHourFitRecoversKnownWeights(t *testing.T) {
	fit := FitFiveHourWeights(fhSim{w: realW, windows: 8, perWindow: 400, seed: 1}.rows(), apiPrice)
	fhCheck(t, "clean", fit, 0.10)
	if !fit.HeaderAfterRow {
		t.Error("the header counts its own row here, and the fit did not find that")
	}
	if fit.Forecast == nil {
		t.Fatal("no forecast scale with a price list")
	}
	// Reads weigh a third of the API mix, so a ping costs well under what API prices say.
	t.Logf("  forecast scale: save_5m %.2f save_1h %.2f ping %.2f", fit.Forecast.Save5m, fit.Forecast.Save1h, fit.Forecast.Ping)
	if fit.Forecast.Ping > 0.6 || fit.Forecast.Ping < 0.2 {
		t.Errorf("ping scale %.2f, want about 0.33 of the API mix (reads weigh a third)", fit.Forecast.Ping)
	}
}

func TestFiveHourFitHeaderBeforeRow(t *testing.T) {
	fit := FitFiveHourWeights(fhSim{w: realW, windows: 8, perWindow: 400, headerLags: true, seed: 2}.rows(), apiPrice)
	fhCheck(t, "header before row", fit, 0.20)
	if fit.HeaderAfterRow {
		t.Error("the header is before its row here, and the fit did not find that")
	}
}

func TestFiveHourFitOutsideTraffic(t *testing.T) {
	// 3% of requests come with a burst of up to 3% of the limit from traffic the proxy never saw.
	fit := FitFiveHourWeights(fhSim{w: realW, windows: 10, perWindow: 400, outside: 0.03, burst: 0.03, seed: 3}.rows(), apiPrice)
	fhCheck(t, "outside traffic", fit, 0.20)
	if fit.DroppedSpans == 0 {
		t.Error("no span was dropped as outside traffic")
	}
}

func TestFiveHourFitCoarseSteps(t *testing.T) {
	// Small requests: about 40 requests move the share one whole percent.
	w := fhScaled(realW, 1.0/12)
	fit := FitFiveHourWeights(fhSim{w: w, windows: 6, perWindow: 2500, seed: 4}.rows(), apiPrice)
	t.Logf("coarse: ok=%v fit_error=%.3f spans=%d requests=%d", fit.OK, fit.FitError, fit.Spans, fit.Requests)
	if !fit.OK {
		t.Fatalf("coarse steps failed the gate: %s", fit.Reason)
	}
	for _, j := range []int{fhCacheRead, fhWrite5m} {
		n := fhTierNames[j]
		if e := fhRelErr(fit.Weights[n], w[j]); e > 0.15 {
			t.Errorf("coarse: %s weight off by %.1f%%", n, 100*e)
		}
	}
}

func TestFiveHourFitOneHourFallsBackToPrice(t *testing.T) {
	// No session uses the 1-hour cache: its weight cannot be learned and comes from the price list.
	fit := FitFiveHourWeights(fhSim{w: realW, windows: 8, perWindow: 400, seed: 5}.rows(), apiPrice)
	if !fit.OK {
		t.Fatalf("gate: %s", fit.Reason)
	}
	if fit.Learned["cache_write_1h"] {
		t.Error("the 1-hour weight was learned with no 1-hour tokens")
	}
	if !(fit.Weights["cache_write_1h"] > 0) {
		t.Error("the 1-hour weight has no price fallback")
	}
}

func TestFiveHourFitLearnsOneHour(t *testing.T) {
	fit := FitFiveHourWeights(fhSim{w: realW, windows: 12, perWindow: 400, oneHour: 0.5, seed: 6}.rows(), apiPrice)
	fhCheck(t, "half on the 1-hour cache", fit, 0.15)
	if !fit.Learned["cache_write_1h"] {
		t.Error("the 1-hour weight was not learned")
	}
}

func TestFiveHourFitGate(t *testing.T) {
	cases := map[string]struct {
		rows []FiveHourRow
		want string
	}{
		"no rows":    {nil, "too few requests with a 5-hour share"},
		"one window": {fhSim{w: fhScaled(realW, 1.0/6), windows: 1, perWindow: 2000, seed: 7}.rows(), "too few 5-hour windows"},
		"few rows":   {fhSim{w: realW, windows: 3, perWindow: 50, seed: 8}.rows(), "too few requests with a 5-hour share"},
		"pure noise": {fhSim{w: [fhTiers]float64{}, windows: 8, perWindow: 400, outside: 0.5, burst: 0.01, seed: 9}.rows(), ""},
		"no variety": {fhNoWrites(), "no cache-write weight could be learned"},
	}
	for name, c := range cases {
		fit := FitFiveHourWeights(c.rows, apiPrice)
		t.Logf("%s: ok=%v reason=%q requests=%d windows=%d spans=%d fit_error=%.3f", name, fit.OK, fit.Reason, fit.Requests, fit.Windows, fit.Spans, fit.FitError)
		if fit.OK {
			t.Errorf("%s: passed the gate", name)
		}
		if c.want != "" && fit.Reason != c.want {
			t.Errorf("%s: reason %q, want %q", name, fit.Reason, c.want)
		}
		if fit.Weights != nil {
			t.Errorf("%s: weights served below the gate", name)
		}
	}
}

// fhNoWrites is traffic with cache reads and output only: no write weight can be learned.
func fhNoWrites() []FiveHourRow {
	rows := fhSim{w: realW, windows: 8, perWindow: 400, seed: 10}.rows()
	used := map[int64]float64{}
	for i := range rows {
		rows[i].Write5m, rows[i].Write1h = 0, 0
		used[rows[i].Reset] += fhDot(realW, rows[i].tiers())
		rows[i].Util = math.Floor(used[rows[i].Reset]*100) / 100
	}
	return rows
}

func TestFiveHourFitNoPrices(t *testing.T) {
	fit := FitFiveHourWeights(fhSim{w: realW, windows: 8, perWindow: 400, oneHour: 0.5, seed: 11}.rows(), nil)
	if fit.OK && fit.Forecast != nil {
		t.Error("a forecast scale with no prices")
	}
	// No 1-hour data and no price list: the fit does not guess the 1-hour weight.
	fit = FitFiveHourWeights(fhSim{w: realW, windows: 8, perWindow: 400, seed: 12}.rows(), nil)
	if fit.OK {
		t.Error("passed the gate with a tier it could neither learn nor price")
	}
}

func TestLawsonHansonKeepsWeightsNonNegative(t *testing.T) {
	// y = 2a - 1b: the unconstrained answer has a negative weight; NNLS must clamp it to 0.
	var spans []fhSpan
	for i := 1; i <= 20; i++ {
		a, b := float64(i), float64(i%3+1)
		spans = append(spans, fhSpan{dy: 2*a - b, x: [fhTiers]float64{a, b}})
	}
	w := fhNNLS(spans, [fhTiers]bool{true, true})
	if w[1] != 0 || w[0] <= 0 {
		t.Fatalf("weights %v, want the second clamped to 0", w)
	}
}
