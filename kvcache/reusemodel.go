package kvcache

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The FIXED reuse model: a frozen survival fit, compiled in, answering Predictor.
//
// # Why this file exists
//
// `Predictor` has been the seam a learned model plugs into since the simulator was written,
// and until now nothing implemented it outside a test. That had a consequence bigger than a
// missing feature: `BudgetPolicy` — the arm that turns reuse probabilities into a
// per-conversation keep-alive budget — was kept out of `Registry()` because "it cannot be
// built from a name alone: it needs a Predictor", so no dashboard page could reach it and no
// operator could ask what it would have done. The registry's promise is that every name in it
// resolves; an arm needing an injected dependency could not keep it.
//
// A model compiled in as constants resolves from a name. That is the whole idea here, and it
// is also why the model is a dot product rather than the gradient-boosted ensemble the
// reference fit uses (deploy/harbor/kv_ttl_keepalive_policy.py's `fit()`):
//
//   - It has to be REVIEWABLE. Thirty-odd coefficients can be read in a diff and argued with.
//     Thirty-seven trees of thresholds cannot, so nobody could tell a re-fit that improved the
//     model from one that broke it.
//   - It has to be FIXED. Re-fitting at startup would make yesterday's dashboard figure
//     unreproducible, and the KV-cache page's whole claim is that replaying a historical
//     window gives the same answer every time.
//   - It has to be cheap enough for the request path, because the live pinger consults the
//     same model. proxy/keepalivestrategy.go states that rule for itself: "a rule, or ... a
//     portable logistic-regression dot product — never an embedded model or a call out of the
//     hot path."
//
// What going linear costs against the ensemble is measured on every re-fit and printed beside
// the coefficients in reusemodel_v1_gen.go, rather than asserted once here.
//
// # What it answers, and why that is not the obvious thing
//
// The model is fitted on CONDITIONAL survival over the keep-alive sweep grid —
// `s_k = P(still idle at sweep k+1 | still idle at sweep k)` — not on "will this conversation
// come back within five minutes". That is because of the population: a keep-alive fires only
// after a conversation has already gone quiet for one interval, so the decision is never faced
// by the ~92.5% of gaps that close inside five minutes. Elapsed silence is not a feature here,
// it is the sample definition. `ReuseProbability` composes those conditionals forwards into the
// cumulative answer the interface promises.
//
// # Where the numbers live
//
// The coefficients are in reusemodel_v1_gen.go, generated. This file is the arithmetic that
// reads them, and it is hand-written and reviewed. The two are pinned to the Python that
// produced them by TestReuseModelV1AgreesWithThePort, which drives
// `kv_ttl_keepalive_policy.py --score` over the checked-in reusemodel_v1.json and compares the
// per-sweep survival vector, the CDF at horizons on and off the knot grid, and the budget that
// results — all three, because a budget that agrees for the wrong reason is a guard that has
// already stopped working.

// ReuseTerm is one numeric feature's coefficient and the standardisation the fit applied.
//
// Center/Scale are on the wire because the coefficient is meaningless without them: the fit
// standardised every numeric column, so Go has to apply the identical transform or a
// coefficient of -1.0 means something different on each side. A Scale of 0 is read as 1,
// which is the same guard the fit applies — see the zero-variance note in the generated file.
type ReuseTerm struct {
	Name   string
	Coef   float64
	Center float64
	Scale  float64
}

// ReuseLevel is one one-hot level's coefficient. The empty Level is the CATCH-ALL: a level the
// fit never saw scores as that rather than as zero, because zero is not "unknown" — it is
// "exactly the reference level", which is a different and much stronger claim.
type ReuseLevel struct {
	Level string
	Coef  float64
}

// ReuseCategory is one one-hot feature and its levels.
type ReuseCategory struct {
	Name   string
	Levels []ReuseLevel
}

// ReuseModel is a frozen discrete-time survival model over the keep-alive sweep grid.
//
// Immutable after construction and safe for concurrent use: every method takes the model by
// pointer but writes nothing through it, which matters because one *ReuseModel is shared by
// every dashboard request and every live ping sweep.
type ReuseModel struct {
	// Version and TrainedOn are provenance, and they are on the type rather than only in a
	// comment so that a result can carry them: a saving attributed to "the model" is not
	// checkable unless the reader can tell WHICH model produced it.
	Version   string
	TrainedOn string

	// IntervalS, LifeS and MaxK are the schedule the model was FITTED on. The grid the
	// conditionals live on is a property of the fit, not of the caller — see grid().
	IntervalS float64
	LifeS     float64
	MaxK      int

	Intercept   float64
	Numeric     []ReuseTerm
	Categorical []ReuseCategory
}

// The shipped feature names. Strings rather than struct fields because they are a wire
// contract with the Python: the generated table names each term, and a term this code cannot
// evaluate is a fitted coefficient silently applied to zero. Refused loudly instead — see
// Validate.
const (
	featLogPrefix  = "log_prefix"
	featLogPrevGap = "log_prev_gap"
	featTurn       = "turn"
	featHourSin    = "hour_sin"
	featHourCos    = "hour_cos"
	featDowSin     = "dow_sin"
	featDowCos     = "dow_cos"
	featStatP      = "stat_p"
	featLogStatN   = "log_stat_n"

	featCategoricalModel = "model"
	// featCategoricalSweep is the BASELINE HAZARD, one coefficient per sweep index, and its
	// level is the index as a decimal string.
	//
	// It is a one-hot rather than a numeric `sweep_k` term, and that is the one place the
	// shipped model's specification departs from the reference ensemble's. The reference reads
	// `sweep_k`, `log_elapsed` and `log_secs_to_deadline` — all three exact functions of the
	// sweep index, and the third a constant at any schedule with interval < life. A tree
	// ensemble treats that as redundancy. A linear model cannot: it is exact collinearity, the
	// coefficients are unidentified, and the first fit here produced -4.77 and +4.09 on two
	// features that move together, summing to a hazard of ~14% at every sweep where the corpus
	// measures 4.86% at the first and 0.24% by the seventh. The arm bought its maximum budget
	// on 97% of decisions and saved less than a flat MaxPings=2.
	//
	// A free coefficient per sweep is strictly more general than the linear term, and the shape
	// it recovers is not monotone — which is exactly why assuming monotone was wrong.
	featCategoricalSweep = "sweep"
)

// hourPeriod and dowPeriod are the cycle lengths the sin/cos encoding uses.
//
// Hour and day-of-week are encoded as a sin/cos PAIR rather than as the raw integers the
// reference ensemble reads, and the reason is that this model is linear: a linear term on raw
// hour reads 23:00 and 00:00 as 23 units apart, which is not a fact about time. The ensemble
// did not care because it splits rather than multiplies. The pair costs one extra coefficient
// each; the measured effect on this corpus is inside the seed-noise floor either way.
const (
	hourPeriod = 24.0
	dowPeriod  = 7.0
)

// reuseSpan is the half of the feature vector that does NOT move with the sweep index.
//
// Hoisted out of the per-sweep loop deliberately: composing the CDF walks the whole grid, and
// `Windows` asks for four horizons per sweep, so the span block would otherwise be recomputed
// tens of times per decision — including the Stats lookup, which is the only part that touches
// a map.
type reuseSpan struct {
	nowMs      int64
	logPrefix  float64
	logPrevGap float64
	turn       float64
	statP      float64
	logStatN   float64
	model      string
}

// spanOf reads the span-level features off an Observation.
//
// Every line here has a counterpart in the Python's `load_spans`, and the pairs have to agree
// or the model is served features it was not fitted on:
//
//	log_prefix    log1p(CachedTokens)      = log1p(cache_read + cache_write) offline
//	log_prev_gap  log1p(SinceLastMs/1000)  = log1p(the gap that already closed), 0 at turn 1
//	turn          Turn                     = requests served so far, including this one
//	stat_p/n      Stats.ReuseWithin(5m)    = the same leak-free accumulator, same 6-gap floor
//	model         lower(Model)             = case-folded, because the corpus spells one
//	                                         upstream two ways and two levels for one model
//	                                         split its evidence in half
//
// A nil Stats yields (0, 0), which is exactly what the accumulator returns early in a replay
// before any gap has closed — so it is a value the fit saw, not a missing one.
func (m *ReuseModel) spanOf(o Observation) reuseSpan {
	var p float64
	var n int
	if o.Stats != nil {
		p, n, _ = o.Stats.ReuseWithin(o.User, o.Model, o.Bucket, Horizon5m)
	}
	return reuseSpan{
		nowMs:      o.Now,
		logPrefix:  math.Log1p(float64(o.CachedTokens)),
		logPrevGap: math.Log1p(float64(o.SinceLastMs) / 1000.0),
		turn:       float64(o.Turn),
		statP:      p,
		logStatN:   math.Log1p(float64(n)),
		model:      strings.ToLower(o.Model),
	}
}

// reuseState is the half of the feature vector that DOES move with the sweep index.
type reuseState struct {
	sweep   int
	hourSin float64
	hourCos float64
	dowSin  float64
	dowCos  float64
}

// stateAt is the span's state as seen at sweep k, mirroring the Python's `state_features`.
//
// The calendar fields are the hour and weekday AT THE SWEEP INSTANT, not at the request:
// a conversation that goes quiet at 23:58 is decided about at 00:03, and Observation.HourUTC
// is the wrong one of those two. That is why this recomputes them from Now rather than reading
// the field the Observation already carries.
func (m *ReuseModel) stateAt(sp reuseSpan, k int) reuseState {
	tk := float64(k) * m.interval()
	// Epoch seconds at the sweep. Floored rather than truncated-toward-zero: the corpus is
	// post-1970 so the two agree, but the Python uses `//` and this has to be the same
	// function, not the same answer on the data that happens to be in front of it.
	secs := float64(sp.nowMs)/1000.0 + tk
	hour := math.Mod(math.Floor(secs/3600.0), 24.0)
	dow := math.Mod(math.Floor(secs/86400.0)+4.0, 7.0)
	return reuseState{
		sweep:   k,
		hourSin: math.Sin(2.0 * math.Pi * hour / hourPeriod),
		hourCos: math.Cos(2.0 * math.Pi * hour / hourPeriod),
		dowSin:  math.Sin(2.0 * math.Pi * dow / dowPeriod),
		dowCos:  math.Cos(2.0 * math.Pi * dow / dowPeriod),
	}
}

// value resolves one named feature. An unknown name yields (0, false) so Validate can refuse
// the model rather than scoring a fitted coefficient against a silent zero.
func value(sp reuseSpan, st reuseState, name string) (float64, bool) {
	switch name {
	case featLogPrefix:
		return sp.logPrefix, true
	case featLogPrevGap:
		return sp.logPrevGap, true
	case featTurn:
		return sp.turn, true
	case featStatP:
		return sp.statP, true
	case featLogStatN:
		return sp.logStatN, true
	case featHourSin:
		return st.hourSin, true
	case featHourCos:
		return st.hourCos, true
	case featDowSin:
		return st.dowSin, true
	case featDowCos:
		return st.dowCos, true
	}
	return 0, false
}

// levelOf reads the one-hot level for a categorical feature.
func levelOf(sp reuseSpan, st reuseState, name string) (string, bool) {
	switch name {
	case featCategoricalModel:
		return sp.model, true
	case featCategoricalSweep:
		return strconv.Itoa(st.sweep), true
	}
	return "", false
}

// catMovesWithSweep says whether a categorical feature's LEVEL depends on the sweep index, so
// that only the ones that do are re-resolved per sweep. The numeric counterpart is
// movesWithSweep; both are switches rather than sets so that adding a feature without
// classifying it fails to compile rather than being silently evaluated at the wrong sweep.
func catMovesWithSweep(name string) bool { return name == featCategoricalSweep }

// coefFor is a level's coefficient, falling back to the catch-all.
//
// A linear scan, not a map: there are a dozen levels, the slice is contiguous, and a map here
// would allocate at construction for no measurable gain on a lookup this small.
func (c ReuseCategory) coefFor(level string) float64 {
	fallback := 0.0
	for _, l := range c.Levels {
		if l.Level == level {
			return l.Coef
		}
		if l.Level == "" {
			fallback = l.Coef
		}
	}
	return fallback
}

// Validate refuses a model this code cannot evaluate exactly.
//
// It exists because the failure it prevents is silent. A generated table naming a feature this
// build does not implement would otherwise score that coefficient against zero, which is not
// an error and not a crash — it is a slightly different model, producing slightly different
// budgets, with nothing to notice. TestReuseModelV1IsValid calls this on the shipped table, so
// a re-fit that adds a feature fails the build's tests rather than the deployment.
func (m *ReuseModel) Validate() error {
	if m == nil {
		return fmt.Errorf("kvcache: nil reuse model")
	}
	if len(m.Numeric) == 0 && len(m.Categorical) == 0 {
		return fmt.Errorf("kvcache: reuse model %q has no terms", m.Version)
	}
	var sp reuseSpan
	st := m.stateAt(sp, 1)
	for _, t := range m.Numeric {
		if _, ok := value(sp, st, t.Name); !ok {
			return fmt.Errorf("kvcache: reuse model %q has unknown numeric feature %q; this "+
				"build cannot score it, and scoring it as zero would be a different model",
				m.Version, t.Name)
		}
	}
	for _, c := range m.Categorical {
		if _, ok := levelOf(sp, st, c.Name); !ok {
			return fmt.Errorf("kvcache: reuse model %q has unknown categorical feature %q; "+
				"this build cannot score it, and scoring it as zero would be a different model",
				m.Version, c.Name)
		}
	}
	if m.IntervalS <= 0 || m.LifeS <= 0 {
		return fmt.Errorf("kvcache: reuse model %q has a non-positive schedule "+
			"(interval %gs, life %gs)", m.Version, m.IntervalS, m.LifeS)
	}
	return nil
}

func (m *ReuseModel) interval() float64 {
	if m.IntervalS > 0 {
		return m.IntervalS
	}
	return float64(DefaultPingIdle / time.Second)
}

func (m *ReuseModel) life() float64 {
	if m.LifeS > 0 {
		return m.LifeS
	}
	return float64(TTL5m.Lifetime() / time.Second)
}

func (m *ReuseModel) maxK() int {
	if m.MaxK > 0 {
		return m.MaxK
	}
	return DefaultBudgetMaxK
}

// survival is `s_k`: the dot product, standardised, through the logistic.
//
// zSpan is passed in rather than recomputed because the span block is the same at every sweep
// and this is the innermost loop of the whole arm.
func (m *ReuseModel) survival(sp reuseSpan, zSpan float64, k int) float64 {
	st := m.stateAt(sp, k)
	z := zSpan
	for _, t := range m.Numeric {
		if !movesWithSweep(t.Name) {
			continue
		}
		v, ok := value(sp, st, t.Name)
		if !ok {
			continue
		}
		z += t.Coef * (v - t.Center) / scaleOf(t)
	}
	for _, c := range m.Categorical {
		if !catMovesWithSweep(c.Name) {
			continue
		}
		level, ok := levelOf(sp, st, c.Name)
		if !ok {
			continue
		}
		z += c.coefFor(level)
	}
	return sigmoid(z)
}

// zSpanOf is the intercept plus every term that does not move with the sweep index.
func (m *ReuseModel) zSpanOf(sp reuseSpan) float64 {
	z := m.Intercept
	var st reuseState // unused by the span-level features, by construction
	for _, t := range m.Numeric {
		if movesWithSweep(t.Name) {
			continue
		}
		v, ok := value(sp, st, t.Name)
		if !ok {
			continue
		}
		z += t.Coef * (v - t.Center) / scaleOf(t)
	}
	for _, c := range m.Categorical {
		if catMovesWithSweep(c.Name) {
			continue
		}
		level, ok := levelOf(sp, st, c.Name)
		if !ok {
			continue
		}
		z += c.coefFor(level)
	}
	return z
}

// movesWithSweep says whether a feature is in the state block. A switch rather than a set
// lookup, so adding a feature to value() without classifying it here fails to compile the
// same day rather than scoring at the wrong sweep.
func movesWithSweep(name string) bool {
	switch name {
	case featHourSin, featHourCos, featDowSin, featDowCos:
		return true
	}
	return false
}

func scaleOf(t ReuseTerm) float64 {
	if t.Scale == 0 {
		return 1.0
	}
	return t.Scale
}

// sigmoid is 1/(1+e^-z), arranged so a large-magnitude z cannot overflow in either direction.
func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1.0 / (1.0 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1.0 + e)
}

// ReuseProbability implements Predictor: P(this conversation has returned within horizon).
//
// CUMULATIVE, as the interface promises, composed forwards from the fitted conditionals:
//
//	F(t_(j+1)) = 1 - prod_(i<=j) s_i
//
// on the grid {t_j} u {t_j + life}, linearly interpolated between knots and CLAMPED outside
// it. Two properties of that are worth stating because BudgetPolicy depends on both:
//
//   - F(t_1) = 0 by construction. This CDF is already conditioned on having reached the first
//     sweep, which is exactly the population `Windows` divides by — so the conditioning is not
//     applied twice.
//   - the grid is the FIT's, not the caller's. A caller replaying on a different interval gets
//     the model's own knots interpolated to its horizons rather than a silent re-fit, which is
//     the honest failure mode: the numbers degrade visibly instead of the model pretending to
//     have been trained on a schedule it was not.
//
// ok is false only when there is no model to consult. Everything else a decision needs to be
// refused — unknown rates, no prefix, a schedule the arm cannot price — is BudgetPolicy's own
// business and is already checked there; duplicating it here would put the same policy in two
// places and let them disagree.
func (m *ReuseModel) ReuseProbability(o Observation, horizon time.Duration) (float64, bool) {
	if m == nil || (len(m.Numeric) == 0 && len(m.Categorical) == 0) {
		return 0, false
	}
	seconds := horizon.Seconds()
	sp := m.spanOf(o)
	zSpan := m.zSpanOf(sp)
	iv, life, mk := m.interval(), m.life(), m.maxK()

	// The knot grid, built exactly as the Python builds it: a map keyed by the knot, so that a
	// schedule where t_(j+1) coincides with t_j+life resolves the collision the same way on
	// both sides. Sorted afterwards, because {t_j} and {t_j + life} interleave whenever
	// life > interval — which is the shipped case (300 > 280).
	cum := make(map[float64]float64, 2*mk+3)
	cum[0] = 0
	surv := 1.0
	for j := 1; j <= mk+1; j++ {
		tj := float64(j) * iv
		cum[tj] = 1.0 - surv
		surv *= m.survival(sp, zSpan, j)
		cum[tj+life] = 1.0 - surv
		// Early exit once the grid brackets the horizon. Every later knot is larger, so it
		// could only ever be selected by the upper clamp — which cannot fire now that a knot
		// above the horizon exists. Same answer, up to 8x less work per call, and `Windows`
		// makes four calls per sweep.
		if tj > seconds {
			break
		}
	}
	xs := make([]float64, 0, len(cum))
	for at := range cum {
		xs = append(xs, at)
	}
	sort.Float64s(xs)
	ys := make([]float64, len(xs))
	for i, at := range xs {
		ys[i] = cum[at]
	}
	p := interp(seconds, xs, ys)
	// Clamped, not trusted. A non-monotone composition cannot arise from a product of
	// probabilities, but the interpolation is floating point and BudgetPolicy divides by
	// 1-F: a p of 1+1e-16 there is a negative survivor share and a budget with no meaning.
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return p, true
}

// interp is numpy.interp for a sorted grid: linear inside, CLAMPED to the endpoints outside.
//
// The clamping is load-bearing rather than defensive. A horizon past the last knot must read
// as the last cumulative value, not extrapolate past 1.0 — `Windows` asks about
// t_maxk + life, which is the last knot, and about t_(maxk+1), which is not. The Python's
// `_interp` is the same function, and the drift test compares them on horizons deliberately
// placed outside the grid.
func interp(x float64, xs, ys []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	if x <= xs[0] {
		return ys[0]
	}
	if x >= xs[len(xs)-1] {
		return ys[len(ys)-1]
	}
	lo, hi := 0, len(xs)-1
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if xs[mid] <= x {
			lo = mid
		} else {
			hi = mid
		}
	}
	span := xs[hi] - xs[lo]
	if span <= 0 {
		return ys[hi]
	}
	return ys[lo] + (ys[hi]-ys[lo])*(x-xs[lo])/span
}

// Describe is the one-sentence provenance a result carries beside the arm's numbers.
func (m *ReuseModel) Describe() string {
	if m == nil {
		return "no reuse model"
	}
	return fmt.Sprintf("reuse model %s, fitted on %s", m.Version, m.TrainedOn)
}
