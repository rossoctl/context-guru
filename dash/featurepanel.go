package dash

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/kvcache/predictor"
)

// The feature and predictor inspection panel: every feature kvcache/predictor's registry
// knows about (including the ones this schema genuinely cannot build, listed rather than
// hidden), a correlation view over a sibling job's own output, and a side-by-side comparison
// of every registered predictor's calibration and net dollars against BOTH the never-ping and
// the ping-everyone baseline.
//
// # Reuse, not reinvention
//
// Nothing here computes a cost, runs a replay loop, or defines a second feature catalog.
// predictor.Features/predictor.Predictors/predictor.Replay (kvcache/predictor) are the single
// source for what a feature or a predictor IS; dash.KVCacheSimulate (kvcachesim.go) is the
// single source for what a policy is WORTH. This file is the glue: it reads the registry, and
// it hands predictor.Registered.Predictor to the SAME simulator the KV-cache and keep-alive
// pages already use, exactly the seam KVCacheSimConfig.Predictor's own doc comment describes
// for "an in-process caller... a benchmark harness, a test".
//
// # Honesty, enforced on the type
//
// FPHonesty is not a caption: every struct below that carries a number the reader could act on
// also carries the FPHonesty that number earned, on the SAME struct, so a caller cannot forward
// the number without the tag travelling with it.

// FPHonesty tags how one number on this panel was produced.
type FPHonesty string

const (
	// FPObserved is read directly off a real row — a fact, not a measurement with variance.
	FPObserved FPHonesty = "observed"
	// FPReplayed is kvcache.Simulate or predictor.Replay run once over real history — exact
	// for that window, but a different window (or a different baseline) can and does disagree.
	FPReplayed FPHonesty = "replayed"
	// FPEstimated carries sampling uncertainty — a proportion computed from a finite bin.
	FPEstimated FPHonesty = "estimated"
	// FPHindsight reads the true future. No feature or predictor on this panel is allowed to
	// carry it — see FeaturePanelCatalog's own check — because this project has exactly one
	// thing that is (kvcache.Optimal, a kvcache.Strategy, not a predictor.Registered), and it
	// is already marked Unreachable in kvcache's own registry rather than routed through here.
	FPHindsight FPHonesty = "hindsight"
)

// ── the feature browser ─────────────────────────────────────────────────────

// FPFeature is one row of the feature browser: predictor.Feature's own catalog entry,
// translated to strings for the wire, plus — when an example session was given — a real value
// from it.
//
// A feature this schema cannot build is NEVER omitted: Buildable is false and AbsentReason
// names why, transcribed from the registry's own AvailableAt rather than restated — a second,
// drifted copy of that sentence is exactly the trap kvcache/predictor's own doc comment warns
// against.
type FPFeature struct {
	ID           string `json:"id"`
	Description  string `json:"description"`
	SourceTable  string `json:"source_table,omitempty"`
	SourceColumn string `json:"source_column,omitempty"`
	AvailableAt  string `json:"available_at"`
	Availability string `json:"availability"`
	Missing      string `json:"missing,omitempty"`
	Privacy      string `json:"privacy"`
	Cost         string `json:"cost,omitempty"`
	// Buildable is true for Live and OfflineDerivable features — everything else
	// (NeedsInstrumentation, Impossible) is listed anyway, with Buildable false.
	Buildable    bool   `json:"buildable"`
	AbsentReason string `json:"absent_reason,omitempty"`
	// Example and ExampleHonesty are set only when a caller supplied a session to draw one
	// from (see FeaturePanelExample) and this feature had an answer there.
	Example        string    `json:"example,omitempty"`
	ExampleHonesty FPHonesty `json:"example_honesty,omitempty"`
}

// FeaturePanelCatalog lists every feature the given registry knows about, in ID order —
// predictor.Features.List()'s own order, so a feature registered by anyone else (feat-impl's
// fuller catalog, a project-specific addition) appears here without this file changing.
func FeaturePanelCatalog(freg *predictor.Features) []FPFeature {
	if freg == nil {
		return nil
	}
	feats := freg.List()
	out := make([]FPFeature, 0, len(feats))
	for _, f := range feats {
		fp := FPFeature{
			ID: f.ID, Description: f.Description,
			SourceTable: f.SourceTable, SourceColumn: f.SourceColumn,
			AvailableAt: f.AvailableAt, Availability: string(f.Availability),
			Missing: string(f.Missing), Privacy: string(f.Privacy), Cost: string(f.Cost),
			Buildable: f.Availability == predictor.Live || f.Availability == predictor.OfflineDerivable,
		}
		if !fp.Buildable {
			fp.AbsentReason = f.AvailableAt
		}
		out = append(out, fp)
	}
	return out
}

// fpFormatValue turns one predictor.Value into wire text: the string half for a string-valued
// feature, the plain number otherwise. Absent (Present=false) formats as "".
func fpFormatValue(v predictor.Value) string {
	if !v.Present {
		return ""
	}
	if v.Str != "" {
		return v.Str
	}
	return strconv.FormatFloat(v.Num, 'g', -1, 64)
}

// FeaturePanelExample fills each catalog feature's real value from ONE session's own history,
// at its LAST decision point — the point with the most closed history behind it, so a lag-1 or
// a stats feature is the most likely to have an answer. reqs must already be one session's
// rows, chronological (KVCacheDataset's own contract); predictor.Replay is the leak-free walk,
// never re-derived here.
func FeaturePanelExample(reqs []*kvcache.Request, freg *predictor.Features) (map[string]string, error) {
	if freg == nil || len(reqs) == 0 {
		return nil, nil
	}
	dps, err := predictor.Replay(reqs, freg, nil)
	if err != nil {
		return nil, err
	}
	if len(dps) == 0 {
		return nil, nil
	}
	last := dps[len(dps)-1]
	out := make(map[string]string, len(last.Features))
	for id, v := range last.Features {
		if s := fpFormatValue(v); s != "" {
			out[id] = s
		}
	}
	return out, nil
}

// FeaturePanelWithExample is FeaturePanelCatalog with FeaturePanelExample's answers merged in
// — the one call the HTTP handler needs. reqs may be nil/empty (no example session chosen),
// in which case every feature is listed with no example rather than a made-up one.
func FeaturePanelWithExample(reqs []*kvcache.Request, freg *predictor.Features) ([]FPFeature, error) {
	out := FeaturePanelCatalog(freg)
	ex, err := FeaturePanelExample(reqs, freg)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if v, ok := ex[out[i].ID]; ok {
			out[i].Example, out[i].ExampleHonesty = v, FPObserved
		}
	}
	return out, nil
}

// ── the correlation view ─────────────────────────────────────────────────────

// FPStrategyCorrelation is one feature's correlation against one strategy's outcome.
type FPStrategyCorrelation struct {
	Strategy    string  `json:"strategy"`
	Correlation float64 `json:"correlation"`
}

// FPCorrelation is one feature's row in the correlation view.
type FPCorrelation struct {
	Feature string `json:"feature"`
	// VsNextBucket is this feature's correlation with the time bucket of the NEXT request.
	VsNextBucket float64 `json:"vs_next_bucket,omitempty"`
	// Low/High bound VsNextBucket when the sweep reported an interval; Ranged is false, and
	// both zero, when it did not — never fabricated here.
	Low               float64                 `json:"low,omitempty"`
	High              float64                 `json:"high,omitempty"`
	Ranged            bool                    `json:"ranged,omitempty"`
	VsStrategyOutcome []FPStrategyCorrelation `json:"vs_strategy_outcome,omitempty"`
}

// FPCorrelations is the correlation panel's whole payload.
type FPCorrelations struct {
	// Computed is false until the sweep has produced its file. The page must show "not yet
	// computed" then, never an empty table that reads as a zero result.
	Computed    bool            `json:"computed"`
	Note        string          `json:"note,omitempty"`
	GeneratedAt string          `json:"generated_at,omitempty"`
	Honesty     FPHonesty       `json:"honesty"`
	Features    []FPCorrelation `json:"features,omitempty"`
}

const fpCorrelationsNotComputedNote = "The correlation sweep (deploy/harbor/kv_ttl_featcorr.py) " +
	"has not produced exp4/featcorr/correlations.json yet on this host. This panel starts " +
	"showing results the next time that job completes; nothing below is a zero measurement."

// featCorrFile is the correlation sweep's own JSON shape, as read off disk. Kept permissive —
// an unrecognised or partially populated file degrades to whatever parsed, rather than failing
// the whole panel, because this file is written by a job this change does not own or control
// the timing of.
type featCorrFile struct {
	GeneratedAt string `json:"generated_at"`
	Features    []struct {
		Feature           string  `json:"feature"`
		VsNextBucket      float64 `json:"vs_next_bucket"`
		Low               float64 `json:"low"`
		High              float64 `json:"high"`
		VsStrategyOutcome []struct {
			Strategy    string  `json:"strategy"`
			Correlation float64 `json:"correlation"`
		} `json:"vs_strategy_outcome"`
	} `json:"features"`
}

// FeaturePanelCorrelations reads the correlation sweep's own output at path, if a sibling job
// has produced it. It never blocks on that job, never computes a correlation itself, and never
// fails the caller: a missing or unparseable file is reported as "not yet computed", not as an
// error — the sweep runs on its own schedule and this panel must not wait on it.
func FeaturePanelCorrelations(path string) FPCorrelations {
	b, err := os.ReadFile(path)
	if err != nil {
		return FPCorrelations{Computed: false, Note: fpCorrelationsNotComputedNote, Honesty: FPEstimated}
	}
	var f featCorrFile
	if err := json.Unmarshal(b, &f); err != nil {
		return FPCorrelations{Computed: false, Honesty: FPEstimated, Note: fmt.Sprintf(
			"exp4/featcorr/correlations.json did not parse (%v); treated as not yet computed "+
				"rather than guessed at.", err)}
	}
	out := FPCorrelations{Computed: true, GeneratedAt: f.GeneratedAt, Honesty: FPEstimated}
	for _, ff := range f.Features {
		c := FPCorrelation{Feature: ff.Feature, VsNextBucket: ff.VsNextBucket}
		if ff.Low != 0 || ff.High != 0 {
			c.Low, c.High, c.Ranged = ff.Low, ff.High, true
		}
		for _, s := range ff.VsStrategyOutcome {
			c.VsStrategyOutcome = append(c.VsStrategyOutcome,
				FPStrategyCorrelation{Strategy: s.Strategy, Correlation: s.Correlation})
		}
		out.Features = append(out.Features, c)
	}
	return out
}

// ── the predictor comparison panel ──────────────────────────────────────────

// FPCalibrationBin is one decile of predicted probability: how many decision points fell in
// it, what the predictor said on average, and what actually happened — with a 95% Wilson-score
// interval on the observed rate, because a bin's observed share is an ESTIMATE from a finite
// sample, not a fact, and a point estimate alone overstates how well-calibrated a thin bin is.
type FPCalibrationBin struct {
	Horizon       string    `json:"horizon"` // "5m" | "1h"
	BinLow        float64   `json:"bin_low"`
	BinHigh       float64   `json:"bin_high"`
	N             int       `json:"n"`
	MeanPredicted float64   `json:"mean_predicted"`
	ObservedRate  float64   `json:"observed_rate"`
	ObservedLow   float64   `json:"observed_low,omitempty"`
	ObservedHigh  float64   `json:"observed_high,omitempty"`
	Ranged        bool      `json:"ranged"`
	Honesty       FPHonesty `json:"honesty"`
}

// FPNetDollars is one predictor's net dollars against one named baseline, from
// dash.KVCacheSimulate — the same simulation the KV-cache and keep-alive pages already use,
// never re-derived here.
type FPNetDollars struct {
	Baseline   string    `json:"baseline"`
	NetUSD     float64   `json:"net_usd"`
	NetPercent float64   `json:"net_pct"`
	Known      bool      `json:"known"`
	Honesty    FPHonesty `json:"honesty"`
}

// FPPredictorComparison is one registered predictor's calibration and net-dollar standing.
type FPPredictorComparison struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Validated/Enforceable mirror predictor.Registered's own gate (see its doc comment):
	// visible for comparison either way, armable for real traffic only when Enforceable.
	Validated   bool `json:"validated"`
	Enforceable bool `json:"enforceable"`

	Calibration []FPCalibrationBin `json:"calibration,omitempty"`
	// VsNeverPing scores against kvcache.StrategyFixed5m (never pings — "the two rungs
	// everybody compares against", per Fixed's own doc comment). VsPingEveryone scores
	// against kvcache.StrategyKeepAlive5m, which "pings unconditionally" on every eligible
	// idle span (per KeepAlive's own doc comment) — pinging every eligible row with NO
	// feature already beats never-ping by 5.7%, so a predictor shown only against never-ping
	// is flattered by that margin before it has done anything.
	VsNeverPing    *FPNetDollars `json:"vs_never_ping,omitempty"`
	VsPingEveryone *FPNetDollars `json:"vs_ping_everyone,omitempty"`
}

// wilsonInterval is the 95% Wilson score interval for a binomial proportion — closed-form and
// well-behaved for a bin with as few as a handful of points, unlike a normal (Wald) interval,
// which can run outside [0,1].
func wilsonInterval(hits, n int) (low, high float64, ok bool) {
	if n == 0 {
		return 0, 0, false
	}
	const z = 1.959963984540054 // 95%
	p := float64(hits) / float64(n)
	nf := float64(n)
	denom := 1 + z*z/nf
	center := p + z*z/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	low, high = (center-margin)/denom, (center+margin)/denom
	// Clamp away the floating-point epsilon that can otherwise put a proportion's own
	// interval very slightly outside [0,1] — a range on a probability must never read as
	// "less than impossible" or "more than certain".
	return math.Max(0, low), math.Min(1, high), true
}

// fpCalibrationBins buckets dps' predictions for one predictor at one horizon into deciles by
// predicted probability, and reports the observed outcome rate per bin.
//
// Only NON-CENSORED decision points count — a censored row's Within5m/Within1h is meaningless
// (see predictor.Outcome's own doc comment) and must not be scored as a miss it never was.
func fpCalibrationBins(dps []predictor.DecisionPoint, id, version, horizon string) []FPCalibrationBin {
	type pt struct {
		p   float64
		hit bool
	}
	var pts []pt
	key := id + "@" + version + "@" + horizon
	for _, dp := range dps {
		if dp.Outcome.Censored {
			continue
		}
		pr, ok := dp.Predictions[key]
		if !ok || !pr.OK {
			continue
		}
		hit := dp.Outcome.Within5m
		if horizon == "1h" {
			hit = dp.Outcome.Within1h
		}
		pts = append(pts, pt{p: pr.P, hit: hit})
	}
	if len(pts) == 0 {
		return nil
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].p < pts[j].p })
	const nBins = 10
	n := len(pts)
	out := make([]FPCalibrationBin, 0, nBins)
	for b := 0; b < nBins; b++ {
		lo, hi := b*n/nBins, (b+1)*n/nBins
		if hi <= lo {
			continue
		}
		seg := pts[lo:hi]
		var sumP float64
		var hits int
		for _, s := range seg {
			sumP += s.p
			if s.hit {
				hits++
			}
		}
		bin := FPCalibrationBin{
			Horizon: horizon, N: len(seg), BinLow: seg[0].p, BinHigh: seg[len(seg)-1].p,
			MeanPredicted: sumP / float64(len(seg)), ObservedRate: float64(hits) / float64(len(seg)),
			Honesty: FPEstimated,
		}
		bin.ObservedLow, bin.ObservedHigh, bin.Ranged = wilsonInterval(hits, len(seg))
		out = append(out, bin)
	}
	return out
}

// fpPredictorNetDollars runs one registered predictor as dash's custom arm (KVCacheSimConfig.
// Predictor — the in-process seam that field's own doc comment names for exactly this: "a
// benchmark harness, a test") and scores it against baseline, via dash.KVCacheSimulate.
//
// allowUnvalidated is implicit and permanent here: an unvalidated predictor must still be
// SCOREABLE (visible for comparison), never armed for real traffic — this function only ever
// scores, through the read-only simulator, so that distinction does not need a flag at this
// call site the way predictor.ToStrategy needs one at ITS call sites.
func (d *DB) fpPredictorNetDollars(f Filter, o KVCacheOptions, price modelinfo.Pricer,
	r predictor.Registered, baseline string) (*FPNetDollars, error) {
	cfg := KVCacheSimConfig{
		Strategies: []string{baseline, KVStrategyCustom},
		Baseline:   baseline,
		Predictor:  r.Predictor,
	}
	sim, err := d.KVCacheSimulate(f, o, price, cfg)
	if err != nil {
		return nil, err
	}
	out := &FPNetDollars{Baseline: baseline, Honesty: FPReplayed}
	for i, res := range sim.Results {
		if res.Strategy == KVStrategyCustom && i < len(sim.Savings) {
			sav := sim.Savings[i]
			out.NetUSD, out.NetPercent, out.Known = sav.AbsoluteUSD, sav.PercentUSD, sav.Known && res.Valued
			break
		}
	}
	return out, nil
}

// FeaturePanelPredictors builds the predictor comparison panel: every registered predictor's
// calibration and net dollars against both baselines, over one filtered window.
func (d *DB) FeaturePanelPredictors(f Filter, o KVCacheOptions, price modelinfo.Pricer,
	freg *predictor.Features, preg *predictor.Predictors) ([]FPPredictorComparison, error) {
	if preg == nil || freg == nil {
		return nil, nil
	}
	registered := preg.List()
	if len(registered) == 0 {
		return nil, nil
	}
	reqs, _, err := d.KVCacheDataset(f, o)
	if err != nil {
		return nil, err
	}
	dps, err := predictor.Replay(reqs, freg, preg)
	if err != nil {
		return nil, err
	}
	out := make([]FPPredictorComparison, 0, len(registered))
	for _, r := range registered {
		fpc := FPPredictorComparison{ID: r.ID, Version: r.Version, Description: r.Description,
			Validated: r.Validated, Enforceable: r.Validated}
		fpc.Calibration = append(fpCalibrationBins(dps, r.ID, r.Version, "5m"),
			fpCalibrationBins(dps, r.ID, r.Version, "1h")...)
		if nd, err := d.fpPredictorNetDollars(f, o, price, r, kvcache.StrategyFixed5m); err == nil {
			fpc.VsNeverPing = nd
		}
		if nd, err := d.fpPredictorNetDollars(f, o, price, r, kvcache.StrategyKeepAlive5m); err == nil {
			fpc.VsPingEveryone = nd
		}
		out = append(out, fpc)
	}
	return out, nil
}

// FeaturePanelDefaultPredictors registers the concrete kvcache.Predictor implementations this
// build already ships — today, kvcache.ReuseModelV1, the fitted, frozen model the
// keepalive-budget arm reads (see reusemodel_v1_gen.go for its holdout AUC/logloss) — so the
// comparison panel has at least one real predictor before any sibling registers more into the
// SAME *predictor.Predictors this function returns.
func FeaturePanelDefaultPredictors() *predictor.Predictors {
	p := predictor.NewPredictors()
	_ = p.Register(predictor.Registered{
		ID: "reuse-model", Version: kvcache.ReuseModelV1.Version,
		Description: "Logistic reuse-probability model compiled into this binary; backs the " +
			"keepalive-budget arm. See reusemodel_v1_gen.go for its holdout AUC/logloss.",
		Validated:  true,
		Provenance: predictor.Provenance{TrainingWindow: kvcache.ReuseModelV1.TrainedOn},
		Predictor:  kvcache.ReuseModelV1,
	})
	return p
}
