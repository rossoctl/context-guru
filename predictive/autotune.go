package predictive

import (
	"math"
	"sort"
)

// Auto campaigns: pick each user's keep-alive plan from a small menu by shrinking that user's own evidence toward the fleet.
//
// A naive per-user campaign (each user gets the plan that did best on that user's past) wins in-sample and loses held-out: most users have a few dozen
// sessions, the value sits in a handful of them, and the in-sample winner is mostly noise. The tuner below is safe by construction:
//
//   - a user's score for plan k is the posterior mean (S_u[k] + kappa * g[k]) / (N_u + kappa): its own recency-weighted rows S_u, N_u plus kappa pseudo-rows
//     of the fleet mean g. Few rows, or kappa large, means the fleet plan; kappa = +Inf is the fleet plan for everyone.
//   - kappa and the recency half-life are chosen on the training rows only: fit on the first 70% (by response end), score on the rest, safest
//     setting first, so a less shrunk setting is taken only if it is strictly better on rows it was not fitted on.
//
// It is a plan chooser, not a predictor: the menu is "ping k times, 270 s apart" for k = 0..K, so the client signals (SessionEnd, /clear) compose with it the
// same way they compose with a blanket plan.

// AutoRow is one decision point: whose it is, when its response ended, and the net $ of each menu plan on it (Gain[0] = never ping = 0).
type AutoRow struct {
	Scope   string
	Session string
	EndMs   int64
	Gain    []float64
}

// AutoGrid is what the tuner may choose from. Zero values take the defaults below.
type AutoGrid struct {
	Kappas        []float64 // pseudo-rows of the fleet prior; +Inf = the fleet plan for everyone
	HalfLivesDays []float64 // recency half-life of a row's weight; +Inf = no decay
	InnerFrac     float64   // the inner split point of the training rows
}

var (
	defaultKappas    = []float64{0, 30, 100, 300, 1000, 3000, 10000, 30000, math.Inf(1)}
	defaultHalfLives = []float64{1, 3, 10, 30, math.Inf(1)}
)

// AutoFit is the tuner's answer.
type AutoFit struct {
	HalfLifeDays float64 // +Inf = no decay
	Kappa        float64 // +Inf = the fleet plan for everyone
	FleetPlan    int
	Plan         map[string]int     // per scope; a scope with no rows is not listed and gets FleetPlan
	Weight       map[string]float64 // recency-weighted rows behind each scope's choice
}

// PlanFor is the plan for one scope.
func (f *AutoFit) PlanFor(scope string) int {
	if k, ok := f.Plan[scope]; ok {
		return k
	}
	return f.FleetPlan
}

// fitAuto is one (half-life, kappa) fit on rows, weights relative to refMs.
func fitAuto(rows []AutoRow, refMs float64, hl, kappa float64) *AutoFit {
	if len(rows) == 0 {
		return &AutoFit{Plan: map[string]int{}, Weight: map[string]float64{}}
	}
	a := len(rows[0].Gain)
	g := make([]float64, a)
	var wg float64
	S := map[string][]float64{}
	N := map[string]float64{}
	for _, r := range rows {
		w := 1.0
		if !math.IsInf(hl, 1) {
			w = math.Exp2(-(refMs - float64(r.EndMs)) / (hl * 86400e3))
		}
		s := S[r.Scope]
		if s == nil {
			s = make([]float64, a)
			S[r.Scope] = s
		}
		for k, v := range r.Gain {
			g[k] += w * v
			s[k] += w * v
		}
		wg += w
		N[r.Scope] += w
	}
	for k := range g {
		g[k] /= wg
	}
	f := &AutoFit{HalfLifeDays: hl, Kappa: kappa, FleetPlan: argmax(g), Plan: map[string]int{}, Weight: map[string]float64{}}
	post := make([]float64, a)
	for sc, s := range S {
		for k := range post {
			if math.IsInf(kappa, 1) {
				post[k] = g[k]
			} else {
				post[k] = (s[k] + kappa*g[k]) / (N[sc] + kappa)
			}
		}
		f.Plan[sc], f.Weight[sc] = argmax(post), N[sc]
	}
	return f
}

func argmax(v []float64) int {
	b := 0
	for i := range v {
		if v[i] > v[b] {
			b = i
		}
	}
	return b
}

// resolveMs is how long after a response a row's outcome is known (the last menu ping plus the cache lifetime); a row that ends later than
// resolveMs before a cut is not scored or fitted at that cut.
const resolveMs = 2700e3

// AutoTune chooses (half-life, kappa) on an inner split of rows and refits on all of them. rows must be resolved by nowMs (the build's cut).
func AutoTune(rows []AutoRow, nowMs int64, grid AutoGrid) *AutoFit {
	if len(grid.Kappas) == 0 {
		grid.Kappas = defaultKappas
	}
	if len(grid.HalfLivesDays) == 0 {
		grid.HalfLivesDays = defaultHalfLives
	}
	if grid.InnerFrac <= 0 || grid.InnerFrac >= 1 {
		grid.InnerFrac = 0.7
	}
	hl, kappa := math.Inf(1), math.Inf(1)
	if len(rows) > 1 {
		ends := make([]int64, len(rows))
		for i, r := range rows {
			ends[i] = r.EndMs
		}
		sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
		cut := quantileMs(ends, grid.InnerFrac)
		var fit, score []AutoRow
		for _, r := range rows {
			if float64(r.EndMs)+resolveMs <= cut {
				fit = append(fit, r)
			}
			if float64(r.EndMs) >= cut {
				score = append(score, r)
			}
		}
		if len(fit) > 0 && len(score) > 0 {
			best := math.Inf(-1)
			// safest first (largest kappa, then longest half-life): a less shrunk setting has to be strictly better to replace it
			for i := len(grid.Kappas) - 1; i >= 0; i-- {
				for j := len(grid.HalfLivesDays) - 1; j >= 0; j-- {
					f := fitAuto(fit, cut, grid.HalfLivesDays[j], grid.Kappas[i])
					v := 0.0
					for _, r := range score {
						v += r.Gain[f.PlanFor(r.Scope)]
					}
					if v > best+1e-9 {
						best, hl, kappa = v, grid.HalfLivesDays[j], grid.Kappas[i]
					}
				}
			}
		}
	}
	return fitAuto(rows, float64(nowMs), hl, kappa)
}

// quantileMs is numpy's default (linear) quantile of sorted values.
func quantileMs(sorted []int64, q float64) float64 {
	p := q * float64(len(sorted)-1)
	lo := int(math.Floor(p))
	hi := min(lo+1, len(sorted)-1)
	return float64(sorted[lo]) + (p-float64(lo))*float64(sorted[hi]-sorted[lo])
}
