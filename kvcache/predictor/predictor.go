// Package predictor is the feature registry, the predictor registry, and the historical
// replay that connects them, for the KV-cache TTL decision. See kvcache's own doc comment
// for the domain this sits inside — kvcache.Observation, kvcache.History and
// kvcache.Predictor already exist there; this package catalogs what a feature IS (where it
// comes from, when it is knowable, who may see it, what it costs to compute) and what a
// predictor IS (a fitted model plus the provenance that makes its number arguable), and it
// replays both against real history without ever letting either see the future.
//
// # Why a new package rather than kvcache/registry.go
//
// kvcache/registry.go is the STRATEGY registry: named arms (fixed-5m, keepalive-1h, ...)
// the dashboard and the offline evaluator both resolve by name. A predictor is not a
// strategy — kvcache.Custom{Predictor: p} is what turns one into an arm, and this package
// hands it exactly that seam (see ToStrategy). Naming this package `registry` too, or
// putting it inside kvcache/registry.go, would make "the registry" ambiguous between two
// different lists the moment someone says the word, which is the same drift problem that
// file's own doc comment warns a second table causes. `kvcache/predictor` because it is a
// sub-domain of kvcache (it imports kvcache; nothing in kvcache imports it) and because
// nothing called `keepalive` exists as a top-level package today — inventing one for this
// would be a second home for concepts kvcache already owns.
//
// # The trap this package exists to close
//
// A prior model keyed its statistics cells by (tenant, model, hour-bucket) and not by the
// request's own stop_reason cluster. Since 92.5% of decision points are "still working"
// (tool_use, stop_sequence, ...; see kvcache.ClusterStillWorking), that majority swamped
// the minority signal inside every cell, and the arm it fed came out actively harmful —
// worse than doing nothing. ClusteredHistory in this package pins the cluster into every
// rung of its fallback ladder for exactly this reason: cluster is a dimension this package
// never drops, unlike user/model/bucket, which it drops one at a time exactly as
// kvcache.History already does.
//
// A second trap: `reverts`, `expands` and `cache_miss_reason` are columns on the same row a
// decision is being made about, and they LOOK present-tense the way StopReason is — but
// their AVAILABLE-AT timestamp trails the request they describe, because dash's async
// analysis pipeline finishes classifying a row after it has already been billed. Reading
// the CURRENT row's value for any of the three is therefore a leak that would not be caught
// by kvcache's own future-only checks (StopReason and MissReason sit on the same struct,
// and only one of the two is safe to read at lag zero). Every feature in this package that
// reads them does so through a lag-1 extractor — see the *Lag1 features in builtin.go — and
// TestNoRegisteredFeatureReadsTheFuture in leakage_test.go is the loud check that nothing
// registered here, present or future, gets this wrong.
package predictor

import (
	"fmt"
	"sort"
	"sync"

	"github.com/rossoctl/context-guru/kvcache"
)

// Provenance is what makes a predictor's number arguable rather than asserted — the same
// standard kvcache/reusemodel_v1_gen.go's doc comment sets for ReuseModelV1 (Version and
// TrainedOn), generalised so a predictor that is NOT compiled into this binary — a Python
// fit, a hand-tuned threshold, an experiment still under review — can state the same facts
// about itself and be compared on equal terms.
type Provenance struct {
	// TrainingWindow is the UTC span the fit was trained on, free text in ReuseModelV1's own
	// style ("2026-08-17 11:48 -> 2026-09-04 21:45 UTC") rather than two timestamps, because
	// a fit is sometimes trained on a filtered subset of a span and the exact predicate
	// belongs in Description, not squeezed into a pair of fields that cannot say it.
	TrainingWindow string
	// CodeSHA is the commit the fitting code ran at. Empty is legitimate for a predictor with
	// no code behind it at all (a hand-set threshold) — see Registered.Validated for why an
	// empty CodeSHA is not, by itself, a reason to refuse registration.
	CodeSHA string
	// Hyperparameters is the fit's own knobs — a learning rate, a regularisation strength, a
	// tree depth — as a flat, JSON-able map so a report can print it without knowing the
	// model family in advance.
	Hyperparameters map[string]any
	// Seed is the RNG seed training ran with, and Seeded says whether one applies at all: 0
	// is a valid seed and must not read as "none".
	Seed   int64
	Seeded bool
	// CalibrationData names the holdout or calibration set the predictor's probabilities were
	// checked against — a path, a query, or a description a person can go re-run. Not a
	// score: a score belongs in Description, this is where the number underneath it came
	// from.
	CalibrationData string
}

// Registered is one predictor in the registry: identity, a fitted model, and the paper
// trail that says whether it may be trusted with real traffic.
type Registered struct {
	// ID is the predictor family's stable name ("reuse-model", "stop-reason-logistic") and
	// Version the specific fit ("v1", "v2-stop-reason-lag1"). Together they are the
	// registry's key: a family is re-fitted repeatedly, but a given fit is rarely renamed —
	// the same split ReuseModelV1's own Version/TrainedOn fields already make.
	ID, Version string
	Description string
	Provenance  Provenance
	// Validated gates ENFORCEMENT, not visibility. An unvalidated predictor registers fine —
	// it appears in List, it can be replayed and scored — but ToStrategy refuses to arm it
	// unless the caller explicitly overrides that refusal. "We have not checked this one
	// yet" and "we checked it and it is fine" are different claims, and a registry that
	// could not tell them apart would let an unreviewed fit reach production by the same
	// path as a reviewed one.
	Validated bool
	// Predictor is the fitted model itself, answering kvcache.Predictor — the same interface
	// ReuseModelV1 and BudgetPolicy already use, so nothing about Simulate or the dashboard
	// has to change to adopt one (see kvcache/strategy.go's own Predictor doc comment).
	Predictor kvcache.Predictor
}

func (r Registered) key() string { return r.ID + "@" + r.Version }

// Predictors is the predictor registry: a concurrency-safe map of Registered, keyed by
// (ID, Version).
type Predictors struct {
	mu      sync.RWMutex
	entries map[string]Registered
}

// NewPredictors builds an empty predictor registry.
func NewPredictors() *Predictors { return &Predictors{entries: map[string]Registered{}} }

// Register adds one predictor. It refuses a blank ID or Version, a nil Predictor, and a
// (ID, Version) pair already registered — re-registering under the same key would let a
// later call silently swap the model a replay, or the live path, is reading, which is
// exactly the "same name, a different thing on two surfaces" failure kvcache/registry.go's
// own doc comment names for strategies.
func (p *Predictors) Register(r Registered) error {
	if r.ID == "" {
		return fmt.Errorf("predictor: ID is required")
	}
	if r.Version == "" {
		return fmt.Errorf("predictor: %q: Version is required", r.ID)
	}
	if r.Predictor == nil {
		return fmt.Errorf("predictor: %s@%s: Predictor is nil", r.ID, r.Version)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := r.key()
	if _, dup := p.entries[k]; dup {
		return fmt.Errorf("predictor: %s already registered; register a new Version instead "+
			"of overwriting one that already ran", k)
	}
	p.entries[k] = r
	return nil
}

// List returns every registered predictor, sorted by (ID, Version) — the order Replay
// consults them in, so the same registry produces the same DecisionPoint.Predictions keys
// in the same order every time (see replay.go's determinism note).
func (p *Predictors) List() []Registered {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Registered, 0, len(p.entries))
	for _, r := range p.entries {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// Eligible is List filtered to Validated predictors — what ToStrategy, and anything
// choosing a policy for real traffic, may pick from.
func (p *Predictors) Eligible() []Registered {
	all := p.List()
	out := make([]Registered, 0, len(all))
	for _, r := range all {
		if r.Validated {
			out = append(out, r)
		}
	}
	return out
}

// Get looks up one predictor by its key.
func (p *Predictors) Get(id, version string) (Registered, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.entries[id+"@"+version]
	return r, ok
}

// ToStrategy arms a registered predictor as a kvcache.Strategy — the seam that turns a
// probability into an Action, priced by kvcache.Simulate exactly like every hand-written
// arm. It WRAPS kvcache.Custom rather than reimplementing it: the break-even arithmetic
// (r/(w−r) = 8.70% on the documented Anthropic multiples, never r/w = 8.0%) already lives
// in Custom/HistoricalProbability, and restating it here would be the second table
// kvcache/registry.go's own doc comment warns against.
//
// It refuses an unvalidated predictor unless allowUnvalidated is true — the one flag that
// makes an unvalidated fit reachable from anything that could act on real traffic. Pass it
// for a replay that is ITSELF the validation, never for enforcement. p5m and p1h of 0 take
// Custom's own defaults (kvcache.DefaultP5m / DefaultP1h, both 50%).
func ToStrategy(r Registered, allowUnvalidated bool, p5m, p1h float64) (kvcache.Strategy, error) {
	if !r.Validated && !allowUnvalidated {
		return nil, fmt.Errorf("predictor: %s@%s is not Validated; pass allowUnvalidated to "+
			"run it anyway (e.g. for a replay that is itself the validation), never for "+
			"enforcement", r.ID, r.Version)
	}
	return kvcache.Custom{
		Label:     r.ID + "@" + r.Version,
		Predictor: r.Predictor,
		P5m:       p5m,
		P1h:       p1h,
	}, nil
}
