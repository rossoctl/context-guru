package dash

import (
	"math"
	"sort"
)

// The 5-hour weight fit (#1047).
//
// A Claude subscription does not bill per token. It counts usage against a 5-hour limit, and the
// provider does not publish how the limit weighs each token kind. Since #984 every request row
// keeps the used share of the 5-hour window (limit_5h_utilization, 0..1) beside its billed tokens.
// This file learns the weights from those two: how much of the limit one token of each kind uses.
//
// The method, in short:
//
//  1. Group the rows by window (the same limit_5h_reset) and sort each window by time.
//  2. The provider reports the share in whole percents. A row where the share changes from the row
//     before is a CHANGE POINT: the true usage crossed that whole percent within one request. So
//     the usage at a change point is known to within one request, far better than one percent.
//  3. A SPAN runs from one change point to a later one, at least fhMinSpanStep higher. Its share
//     change is explained by the tokens billed between the two change points.
//  4. Fit non-negative weights per tier (input, cache read, 5-minute write, 1-hour write, output)
//     by least squares over all spans (Lawson-Hanson NNLS).
//  5. Traffic that does not go through the proxy (claude.ai, another machine) moves the share with no
//     tokens on our rows. It can only push a span UP. After the first fit, spans whose residual is far
//     above the rest (a robust 3.5-sigma test on the median absolute deviation) are dropped, and the
//     fit runs again. At most fhMaxDropShare of the spans can be dropped.
//  6. It is not known whether a row's header counts the row itself. The fit runs both ways (the
//     header after the row, and the header before the row) and keeps the one with the smaller error.
//
// The gate (FiveHourFit.OK) asks for enough rows, windows and spans, a fit error (the root mean square
// of the span errors over the mean span change) of at most fhMaxFitError, and a learned cache-read
// and write weight. The fit error, not R²: most spans move the share by exactly two percents, so the
// share changes barely vary and R² would measure nothing. The information is in how much the token
// counts behind the same change vary.
// Below it, the answer is "not enough data" and every screen keeps its API-price figures.
//
// Pure: the caller reads the rows (DB.FiveHourRows) and gives the prices.

// The tiers of the fit, in a fixed order.
const (
	fhInput = iota
	fhCacheRead
	fhWrite5m
	fhWrite1h
	fhOutput
	fhTiers
)

// fhTierNames are the JSON names of the tiers, in fit order. The first four match TierTokens.
var fhTierNames = [fhTiers]string{"input", "cache_read", "cache_write_5m", "cache_write_1h", "output"}

// The gate and the fit's settings.
const (
	// fhMinRows is the fewest rows (with a 5-hour share) the spans may cover.
	fhMinRows = 200
	// fhMinWindows is the fewest 5-hour windows the spans must come from. One window can be one
	// odd day: three show the weights hold across time.
	fhMinWindows = 3
	// fhMinSpans is the fewest spans after the outlier drop.
	fhMinSpans = 30
	// fhMaxFitError is the largest fit error the gate takes: the weights must predict a span's
	// change to within 25% on average (root mean square).
	fhMaxFitError = 0.25
	// fhMaxRelSE is the largest relative standard error a weight may have to count as learned.
	fhMaxRelSE = 0.5
	// fhMinSpanStep is the smallest share change of one span: two whole percents.
	fhMinSpanStep = 0.02
	// fhMaxDropShare is the most spans the outlier drop may remove.
	fhMaxDropShare = 0.2
	// fhOutlierSigma is the outlier test, in robust standard deviations.
	fhOutlierSigma = 3.5
)

// FiveHourRow is one request row for the fit: numbers only.
type FiveHourRow struct {
	// ID is the row id: the order of two rows in the same millisecond.
	ID    int64
	TsMs  int64
	Reset int64   // limit_5h_reset, unix seconds: the window's key
	Util  float64 // limit_5h_utilization, 0..1
	// Billed tokens. Write5m is cache_write less cache_write_1h.
	Input, CacheRead, Write5m, Write1h, Output int64
	Model                                      string
}

func (r FiveHourRow) tiers() [fhTiers]float64 {
	return [fhTiers]float64{float64(r.Input), float64(r.CacheRead), float64(r.Write5m), float64(r.Write1h), float64(r.Output)}
}

// TierPrice is one model's API price per token, by fit tier.
type TierPrice [fhTiers]float64

// ForecastScale turns a dollar figure at API prices into one at the learned weights, for the cache
// strategy forecast (#1042). Each factor is the learned weight over the API price, both relative to
// the account's whole spend, so the total scale of the dollars does not change, only the mix:
//   - Save5m, Save1h: a rescued expiry saves a write and pays a read of the same prefix;
//   - Ping: a ping pays a read of the prefix.
//
// 1 is "the same as API prices".
type ForecastScale struct {
	Save5m float64 `json:"save_5m"`
	Save1h float64 `json:"save_1h"`
	Ping   float64 `json:"ping"`
}

// FiveHourFit is the fit's answer, served at GET /api/me/fivehour-weights.
type FiveHourFit struct {
	// OK is true when the fit passed the gate. Every other screen reads the weights only then.
	OK bool `json:"ok"`
	// Reason says why the gate failed. "" when OK.
	Reason string `json:"reason,omitempty"`
	// Weights is the share of the 5-hour limit (0..1) that one token of each tier uses. A tier the
	// fit could not learn (Learned false) carries the API price, scaled to the account's own spend.
	Weights map[string]float64 `json:"weights,omitempty"`
	// Learned says, by tier, whether the weight came from the fit.
	Learned map[string]bool `json:"learned,omitempty"`
	// RelSE is each learned weight's relative standard error.
	RelSE map[string]float64 `json:"rel_se,omitempty"`
	// Requests, Windows and Spans are what the fit used. DroppedSpans were dropped as traffic
	// that did not go through the proxy.
	Requests     int `json:"requests"`
	Windows      int `json:"windows"`
	Spans        int `json:"spans"`
	DroppedSpans int `json:"dropped_spans"`
	// FitError is the fit quality: the root mean square of the span errors over the mean span
	// change. 0.1 means the weights predict a span's change to within about 10%.
	FitError float64 `json:"fit_error"`
	// HeaderAfterRow is true when the fit found that a row's header counts the row itself.
	HeaderAfterRow bool `json:"header_after_row"`
	// Forecast is the cache strategy forecast's scale. nil without prices.
	Forecast *ForecastScale `json:"forecast_scale,omitempty"`
}

// fhSpan is one span: its share change and its tokens by tier, and the rows it covers.
type fhSpan struct {
	dy     float64
	x      [fhTiers]float64
	window int64
	rows   int
}

// FitFiveHourWeights fits the weights. price gives a model's API price per tier; nil, or a model it
// does not know, leaves the forecast scale and the fallback weights out.
func FitFiveHourWeights(rows []FiveHourRow, price func(model string) (TierPrice, bool)) FiveHourFit {
	windows := map[int64][]FiveHourRow{}
	for _, r := range rows {
		if r.Reset <= 0 || r.Util < 0 || math.IsNaN(r.Util) {
			continue
		}
		windows[r.Reset] = append(windows[r.Reset], r)
	}
	keys := make([]int64, 0, len(windows))
	for k, w := range windows {
		sort.Slice(w, func(i, j int) bool {
			if w[i].TsMs != w[j].TsMs {
				return w[i].TsMs < w[j].TsMs
			}
			return w[i].ID < w[j].ID
		})
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var best *fhResult
	for _, after := range []bool{true, false} {
		var spans []fhSpan
		for _, k := range keys {
			spans = append(spans, fhSpans(windows[k], after)...)
		}
		res := fhFit(spans)
		res.after = after
		if best == nil || res.better(best) {
			r := res
			best = &r
		}
	}
	return best.answer(rows, price)
}

// fhSpans cuts one window, sorted by time, into spans. after says the header counts its own row.
func fhSpans(w []FiveHourRow, after bool) []fhSpan {
	const eps = 1e-6
	var cps []int
	for i := 1; i < len(w); i++ {
		if math.Abs(w[i].Util-w[i-1].Util) > eps {
			cps = append(cps, i)
		}
	}
	var out []fhSpan
	if len(cps) < 2 {
		return nil
	}
	a := cps[0]
	for _, b := range cps[1:] {
		dy := w[b].Util - w[a].Util
		if dy < -eps {
			// The share fell inside one window: not something tokens explain. Start again here.
			a = b
			continue
		}
		if dy < fhMinSpanStep-eps {
			continue
		}
		// The usage crossed w[a]'s percent within row a (header after the row) or within the row
		// before a (header before the row). So the tokens between are rows a+1..b, or a..b-1.
		lo, hi := a+1, b
		if !after {
			lo, hi = a, b-1
		}
		s := fhSpan{dy: dy, window: w[a].Reset}
		for i := lo; i <= hi; i++ {
			t := w[i].tiers()
			for j := range s.x {
				s.x[j] += t[j]
			}
			s.rows++
		}
		out = append(out, s)
		a = b
	}
	return out
}

// fhResult is one fit (one header alignment).
type fhResult struct {
	after   bool
	w       [fhTiers]float64
	relSE   [fhTiers]float64
	present [fhTiers]bool
	kept    []fhSpan
	dropped int
	fitErr  float64
	ok      bool
}

func (r fhResult) better(o *fhResult) bool {
	if r.ok != o.ok {
		return r.ok
	}
	if len(r.kept) == 0 {
		return false
	}
	if len(o.kept) == 0 {
		return true
	}
	return r.fitErr < o.fitErr
}

// fhFit fits the spans, drops the outliers, and fits again.
func fhFit(spans []fhSpan) fhResult {
	var res fhResult
	if len(spans) == 0 {
		return res
	}
	// A tier is present when enough spans carry it to be learned at all.
	var count [fhTiers]int
	for _, s := range spans {
		for j, v := range s.x {
			if v > 0 {
				count[j]++
			}
		}
	}
	need := max(10, len(spans)/10)
	for j := range count {
		res.present[j] = count[j] >= need
	}
	kept := spans
	maxDrop := int(fhMaxDropShare * float64(len(spans)))
	var w [fhTiers]float64
	for round := 0; round < 3; round++ {
		w = fhNNLS(kept, res.present)
		resid := make([]float64, len(kept))
		for i, s := range kept {
			resid[i] = s.dy - fhDot(w, s.x)
		}
		sigma := 1.4826 * fhMAD(resid)
		if sigma <= 0 || round == 2 {
			break
		}
		var next []fhSpan
		for i, s := range kept {
			// Only UP: traffic outside the proxy adds share with no tokens of ours.
			if resid[i] > fhOutlierSigma*sigma && res.dropped < maxDrop {
				res.dropped++
				continue
			}
			next = append(next, s)
		}
		if len(next) == len(kept) {
			break
		}
		kept = next
	}
	res.w, res.kept = w, kept
	var ssr, mean float64
	for _, s := range kept {
		e := s.dy - fhDot(w, s.x)
		ssr += e * e
		mean += s.dy
	}
	mean /= float64(len(kept))
	res.fitErr = math.Inf(1)
	if mean > 0 {
		res.fitErr = math.Sqrt(ssr/float64(len(kept))) / mean
	}
	res.relSE = fhRelSE(kept, w, ssr)
	res.ok = true
	return res
}

func fhDot(w, x [fhTiers]float64) float64 {
	var s float64
	for j := range w {
		s += w[j] * x[j]
	}
	return s
}

// fhMAD is the median absolute deviation from the median.
func fhMAD(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	med := fhMedian(xs)
	d := make([]float64, len(xs))
	for i, x := range xs {
		d[i] = math.Abs(x - med)
	}
	return fhMedian(d)
}

func fhMedian(xs []float64) float64 {
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// fhNNLS is the Lawson-Hanson non-negative least squares over the present tiers. The columns are
// scaled to unit RMS first: token counts are in the millions and weights near 1e-8.
func fhNNLS(spans []fhSpan, present [fhTiers]bool) [fhTiers]float64 {
	var out [fhTiers]float64
	var cols []int
	for j, p := range present {
		if p {
			cols = append(cols, j)
		}
	}
	n := len(cols)
	if n == 0 || len(spans) == 0 {
		return out
	}
	scale := fhColScale(spans, cols)
	// G = AᵀA and h = Aᵀb on the scaled columns.
	G := make([][]float64, n)
	h := make([]float64, n)
	for a := range G {
		G[a] = make([]float64, n)
	}
	for _, sp := range spans {
		for a, ja := range cols {
			xa := sp.x[ja] / scale[a]
			h[a] += xa * sp.dy
			for b, jb := range cols {
				G[a][b] += xa * sp.x[jb] / scale[b]
			}
		}
	}
	x := lawsonHanson(G, h)
	for k, j := range cols {
		out[j] = x[k] / scale[k]
	}
	return out
}

// fhColScale is each column's root mean square over the spans (1 for an all-zero column). The fit
// and its standard errors both work on columns divided by it: token counts reach the millions, so
// the raw XᵀX reaches 1e16 and loses precision in the solve.
func fhColScale(spans []fhSpan, cols []int) []float64 {
	scale := make([]float64, len(cols))
	for k, j := range cols {
		var s float64
		for _, sp := range spans {
			s += sp.x[j] * sp.x[j]
		}
		scale[k] = math.Sqrt(s / float64(len(spans)))
		if scale[k] == 0 {
			scale[k] = 1
		}
	}
	return scale
}

// lawsonHanson solves min |Ax-b|² with x >= 0, given G = AᵀA and h = Aᵀb.
func lawsonHanson(G [][]float64, h []float64) []float64 {
	n := len(h)
	x := make([]float64, n)
	passive := make([]bool, n)
	grad := func() []float64 {
		g := make([]float64, n)
		for i := range g {
			g[i] = h[i]
			for j := range x {
				g[i] -= G[i][j] * x[j]
			}
		}
		return g
	}
	const tol = 1e-10
	for iter := 0; iter < 3*n+10; iter++ {
		g := grad()
		best, bj := tol, -1
		for j := range g {
			if !passive[j] && g[j] > best {
				best, bj = g[j], j
			}
		}
		if bj < 0 {
			break
		}
		passive[bj] = true
		for inner := 0; inner < 3*n+10; inner++ {
			z := solvePassive(G, h, passive)
			allPos := true
			for j := range z {
				if passive[j] && z[j] <= tol {
					allPos = false
				}
			}
			if allPos {
				copy(x, z)
				break
			}
			alpha := math.Inf(1)
			for j := range z {
				if passive[j] && z[j] <= tol {
					if d := x[j] - z[j]; d > 0 {
						alpha = math.Min(alpha, x[j]/d)
					} else {
						alpha = 0
					}
				}
			}
			if math.IsInf(alpha, 1) {
				alpha = 0
			}
			for j := range x {
				x[j] += alpha * (z[j] - x[j])
				if passive[j] && x[j] <= tol {
					passive[j], x[j] = false, 0
				}
			}
		}
	}
	return x
}

// solvePassive solves the normal equations on the passive set; the other entries are 0.
func solvePassive(G [][]float64, h []float64, passive []bool) []float64 {
	var idx []int
	for j, p := range passive {
		if p {
			idx = append(idx, j)
		}
	}
	m := len(idx)
	A := make([][]float64, m)
	for a, i := range idx {
		A[a] = make([]float64, m+1)
		for b, j := range idx {
			A[a][b] = G[i][j]
		}
		A[a][m] = h[i]
	}
	sol := gaussSolve(A)
	out := make([]float64, len(h))
	for a, i := range idx {
		out[i] = sol[a]
	}
	return out
}

// gaussSolve solves the augmented system A (m rows, m+1 columns) by elimination with partial
// pivoting. A singular column reads 0: a pivot below 1e-12 of the largest entry.
func gaussSolve(A [][]float64) []float64 {
	m := len(A)
	var big float64
	for _, row := range A {
		for _, v := range row[:m] {
			big = math.Max(big, math.Abs(v))
		}
	}
	tol := 1e-12 * big
	if tol == 0 {
		tol = 1e-300
	}
	for c := 0; c < m; c++ {
		p := c
		for r := c + 1; r < m; r++ {
			if math.Abs(A[r][c]) > math.Abs(A[p][c]) {
				p = r
			}
		}
		A[c], A[p] = A[p], A[c]
		if math.Abs(A[c][c]) < tol {
			continue
		}
		for r := 0; r < m; r++ {
			if r == c {
				continue
			}
			f := A[r][c] / A[c][c]
			for k := c; k <= m; k++ {
				A[r][k] -= f * A[c][k]
			}
		}
	}
	out := make([]float64, m)
	for c := 0; c < m; c++ {
		if math.Abs(A[c][c]) >= tol {
			out[c] = A[c][m] / A[c][c]
		}
	}
	return out
}

// fhRelSE is each positive weight's relative standard error, from σ²(XᵀX)⁻¹ on the positive tiers.
// +Inf for a tier at 0 or one the fit could not resolve. It solves on the scaled columns (fhColScale),
// as the fit does: with X = X̃·D, (XᵀX)⁻¹ = D⁻¹(X̃ᵀX̃)⁻¹D⁻¹, so Var(w_j) = σ²[(X̃ᵀX̃)⁻¹]_kk / d_k².
func fhRelSE(spans []fhSpan, w [fhTiers]float64, ssr float64) [fhTiers]float64 {
	var out [fhTiers]float64
	for j := range out {
		out[j] = math.Inf(1)
	}
	var cols []int
	for j := range w {
		if w[j] > 0 {
			cols = append(cols, j)
		}
	}
	n := len(cols)
	if n == 0 || len(spans) <= n {
		return out
	}
	sigma2 := ssr / float64(len(spans)-n)
	scale := fhColScale(spans, cols)
	G := make([][]float64, n)
	for a, ja := range cols {
		G[a] = make([]float64, n)
		for b, jb := range cols {
			for _, s := range spans {
				G[a][b] += s.x[ja] / scale[a] * s.x[jb] / scale[b]
			}
		}
	}
	for k, j := range cols {
		// Solve (X̃ᵀX̃) v = e_k and read v_k.
		A := make([][]float64, n)
		for a := range cols {
			A[a] = append(append([]float64(nil), G[a]...), 0)
			if a == k {
				A[a][n] = 1
			}
		}
		v := gaussSolve(A)
		if v[k] > 0 {
			out[j] = math.Sqrt(sigma2*v[k]) / scale[k] / w[j]
		}
	}
	return out
}

// answer applies the gate and fills the fallbacks and the forecast scale.
func (r *fhResult) answer(rows []FiveHourRow, price func(string) (TierPrice, bool)) FiveHourFit {
	out := FiveHourFit{Spans: len(r.kept), DroppedSpans: r.dropped, HeaderAfterRow: r.after}
	if !math.IsInf(r.fitErr, 0) {
		out.FitError = r.fitErr
	}
	wins := map[int64]bool{}
	for _, s := range r.kept {
		wins[s.window] = true
		out.Requests += s.rows
	}
	out.Windows = len(wins)
	learned := [fhTiers]bool{}
	for j := range learned {
		learned[j] = r.present[j] && r.w[j] > 0 && r.relSE[j] <= fhMaxRelSE
	}
	switch {
	case out.Requests < fhMinRows:
		out.Reason = "too few requests with a 5-hour share"
	case out.Windows < fhMinWindows:
		out.Reason = "too few 5-hour windows"
	case out.Spans < fhMinSpans:
		out.Reason = "too few changes of the 5-hour share"
	case !(r.fitErr <= fhMaxFitError):
		out.Reason = "the token counts do not explain the 5-hour share well enough"
	case !learned[fhCacheRead]:
		out.Reason = "the cache-read weight could not be learned"
	case !learned[fhWrite5m] && !learned[fhWrite1h]:
		out.Reason = "no cache-write weight could be learned"
	}
	if out.Reason != "" {
		return out
	}
	w := r.w
	// The account's mean API price per tier, token-weighted over the models of its rows.
	var pbar [fhTiers]float64
	priced := price != nil
	if priced {
		var tot float64
		for _, row := range rows {
			p, ok := price(row.Model)
			if !ok {
				continue
			}
			t := row.tiers()
			var n float64
			for _, v := range t {
				n += v
			}
			for j := range pbar {
				pbar[j] += p[j] * n
			}
			tot += n
		}
		if tot > 0 {
			for j := range pbar {
				pbar[j] /= tot
			}
		} else {
			priced = false
		}
	}
	// k is the share of the limit per API dollar, over the learned tiers of the kept spans.
	var k float64
	if priced {
		var num, den float64
		for _, s := range r.kept {
			for j := range w {
				if learned[j] {
					num += w[j] * s.x[j]
					den += pbar[j] * s.x[j]
				}
			}
		}
		if den > 0 && num > 0 {
			k = num / den
		} else {
			priced = false
		}
	}
	for j := range w {
		if learned[j] {
			continue
		}
		if !priced {
			// A tier the fit could not learn and no price to scale: the gate fails rather than guess.
			if r.present[j] || j != fhOutput {
				return FiveHourFit{Reason: "no API price to fill the weights the fit could not learn",
					Spans: out.Spans, DroppedSpans: out.DroppedSpans, FitError: out.FitError, Requests: out.Requests,
					Windows: out.Windows, HeaderAfterRow: out.HeaderAfterRow}
			}
			continue
		}
		w[j] = k * pbar[j]
	}
	out.OK = true
	out.Weights, out.Learned, out.RelSE = map[string]float64{}, map[string]bool{}, map[string]float64{}
	for j, name := range fhTierNames {
		out.Weights[name] = w[j]
		out.Learned[name] = learned[j]
		if learned[j] {
			out.RelSE[name] = r.relSE[j]
		}
	}
	if priced && pbar[fhCacheRead] > 0 {
		ratio := func(wt, pt float64) float64 {
			if pt <= 0 {
				return 1
			}
			return math.Max(0, wt/k/pt)
		}
		out.Forecast = &ForecastScale{
			Save5m: ratio(w[fhWrite5m]-w[fhCacheRead], pbar[fhWrite5m]-pbar[fhCacheRead]),
			Save1h: ratio(w[fhWrite1h]-w[fhCacheRead], pbar[fhWrite1h]-pbar[fhCacheRead]),
			Ping:   ratio(w[fhCacheRead], pbar[fhCacheRead]),
		}
	}
	return out
}
