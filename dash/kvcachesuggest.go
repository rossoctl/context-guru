package dash

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// The KV-cache page's per-user, per-hour strategy suggester.
//
// It answers a narrower question than the simulator above: not "how did every arm do over the
// whole window", but "for THIS account, in THIS hour of the day, which arm would have cost the
// least" — one answer per (user, hour-of-day) cell, deterministic and reproducible from exactly
// the history that already drives the rest of this page.
//
// # Why Friday and Saturday are simply not read
//
// This deployment's work week is Sunday through Thursday. A cell built from a work-week hour
// must not be informed by weekend traffic, whose shape (who is active, how long a gap runs) is
// a different population. So the weekday check below is a FILTER on which rows even enter the
// candidate pool — not a down-weighting, not a separate "weekend" group reported alongside —
// because a policy chosen from a blend of the two describes neither.
//
// # Why a cell can be simulated with nothing more than the rows already at hand
//
// kvcache.Simulate derives each conversation's own gaps FROM THE SLICE IT IS GIVEN, walking it
// in (ts, id) order and tracking state per Conversation key — it does not read Request.HasNext,
// .IdleMs or .NextTS, which are dataset/display fields filled in elsewhere. So handing it a
// cell's own rows (one user, one hour-of-day, Sunday–Thursday only) is not an approximation
// bolted onto the real simulator; it IS the real simulator, replaying exactly the population the
// cell claims to describe. Two rows from the same conversation that land in the same hour on two
// different days are treated as consecutive turns with a multi-day gap, which is the honest
// reading of "this is what this account's traffic in this hour looks like" rather than a defect
// to be worked around.
//
// # Why the candidate list is not written down here
//
// It is KVCacheArms() filtered to what an unattended sweep can actually run — see
// kvSuggestCandidates. A strategy added to the registry becomes a candidate the day it lands,
// with nothing here to update; the alternative is a second, driftable list of arm names, which
// is the exact defect that made four shipped arms invisible to the simulator's own picker.

// kvSuggestMinRequests is how many requests a (user, hour) cell needs before its winner is
// reported as a recommendation rather than a guess.
//
// 5, one below kvcache's own minCell of 6: minCell governs a STRATEGY's internal fallback
// (whether HistoricalProbability trusts a cell's own history over its parent's), and this
// governs whether THIS page trusts the cell's REPLAY at all. A cell below the floor still
// simulates — every candidate's own numbers are on the wire — but InsufficientData is set so a
// caller does not act on three requests as though they were a pattern.
const kvSuggestMinRequests = 5

// kvSuggestWeekdays is the work week this suggester reads, in the order a reader expects.
var kvSuggestWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday"}

// kvInWorkWeek reports whether ts's UTC weekday is Sunday..Thursday. Friday and Saturday are
// excluded, not reclassified: see the file doc comment.
func kvInWorkWeek(ts int64) bool {
	switch time.UnixMilli(ts).UTC().Weekday() {
	case time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday:
		return true
	}
	return false
}

// kvSuggestCandidates is every arm an unattended sweep may pick as a cell's winner: every
// REACHABLE, name-buildable registry arm. `optimal` is excluded because it reads the true
// next-request time (Unreachable), `replay` because it carries no action list here
// (unbuildable), and `custom` because there is no operator present to configure its
// thresholds or hand it a predictor.
func kvSuggestCandidates() []string {
	out := []string{}
	for _, a := range KVCacheArms() {
		if a.Selectable && !a.Unreachable && a.Name != KVStrategyCustom {
			out = append(out, a.Name)
		}
	}
	return out
}

// KVCacheSuggestion is one (user, hour-of-day) cell's winner, decided from exactly that
// account's own Sunday–Thursday requests in that hour — nothing else.
type KVCacheSuggestion struct {
	User    string `json:"user"`
	HourUTC int    `json:"hour_utc"`
	// Requests is how many Sunday–Thursday requests fall in this cell. It is the population
	// every figure below was decided from — Friday and Saturday requests, if this account has
	// any in this hour, are not in it.
	Requests int64 `json:"requests"`
	// InsufficientData is true below kvSuggestMinRequests. Every candidate is still simulated
	// and reported — a floor on trusting the answer is not a reason to hide the arithmetic.
	InsufficientData bool `json:"insufficient_data"`

	// Baseline is the arm every saving below is measured against — the same honest denominator
	// the page-wide simulator uses, not a second definition of it.
	Baseline    string  `json:"baseline"`
	BaselineUSD float64 `json:"baseline_usd"`

	// BestStrategy is the argmax by absolute dollar saving against Baseline, over every valued
	// candidate. Baseline is itself always a candidate, so BestStrategy is never worse than
	// changing nothing: the floor of this field is "keep doing what you already do".
	BestStrategy    string  `json:"best_strategy"`
	BestDescription string  `json:"best_description,omitempty"`
	BestUSD         float64 `json:"best_usd"`
	// SavingUSD and SavingPct are Baseline's cost minus the winner's, absolute and as a share of
	// Baseline. Never negative — see BestStrategy — and never clamped either; a cell with no
	// arm beating the baseline simply reports 0.
	SavingUSD   float64 `json:"saving_usd"`
	SavingPct   float64 `json:"saving_pct"`
	SavingKnown bool    `json:"saving_known"`
	// Valued is false where neither the baseline nor the winner could be priced — every dollar
	// figure on this cell is then 0.00 for want of a rate, not because nothing was spent.
	//
	// It is a floor, not a coverage measure: kvcache.Result.Valued is `Unpriced < Requests`, so
	// ANY one priced request in the cell sets it. UnpricedRequests is the coverage — how many of
	// this cell's own Requests contributed nothing to SavingUSD because their model has no
	// rates. Nonzero means SavingUSD describes only part of this cell, which is why the
	// service-wide total refuses such a cell rather than adding it (see KVCacheSuggestions).
	Valued           bool  `json:"valued"`
	UnpricedRequests int64 `json:"unpriced_requests"`

	// OptimalUSD and OptimalSavingUSD are the exact ceiling's own cost on this cell's rows,
	// and its saving over Baseline — computed and frozen ALONGSIDE the winner, never as one
	// of the candidates a sweep could pick (see kvSuggestCandidates' own doc comment for why
	// `optimal` is excluded there: it reads the true next-request time). Reported so a reader
	// always sees how much headroom remains beyond what was actually recommended, not only
	// against the baseline.
	OptimalUSD       float64 `json:"optimal_usd"`
	OptimalSavingUSD float64 `json:"optimal_saving_usd"`
	OptimalKnown     bool    `json:"optimal_known"`

	// Candidates is every arm's own saving over this cell's own rows, baseline included, so the
	// winner is checkable rather than asserted.
	Candidates []kvcache.Savings `json:"candidates"`
}

// KVCacheSuggestions is the whole /api/kvcache/suggest payload.
type KVCacheSuggestions struct {
	Baseline string   `json:"baseline"`
	Weekdays []string `json:"weekdays_included"`
	TimeZone string   `json:"time_zone"`
	// MinRequests is kvSuggestMinRequests, echoed so a reader of the JSON does not have to find
	// it in the code that produced it.
	MinRequests int      `json:"min_requests"`
	Users       []string `json:"users"`
	// Cells is one entry per (user, hour) that had any Sunday–Thursday traffic at all, ordered
	// by user then hour so the same window renders the same list twice.
	Cells []KVCacheSuggestion `json:"cells"`
	// TotalSavingUSD sums SavingUSD over the cells that cleared MinRequests and were priced in
	// FULL — the aggregate this deployment would have kept had every cell run its own winner
	// instead of the baseline, over exactly the history read.
	//
	// In full, because a cell's own Valued flag is `Unpriced < Requests` (kvcache/simulate.go):
	// a cell with 99 of its 100 requests unpriced is Valued, and its SavingUSD is then measured
	// on the one priced request while being labelled with the whole cell.
	//
	// The dollars themselves are not inflated by this, and it matters that the comment says so:
	// an unpriced request contributes $0 to the baseline AND $0 to the winning arm, so a
	// partially-priced cell UNDERSTATES its own saving. What is actually damaged is the CHOICE.
	// BestStrategy is an argmax over the priced subset, and that ranking moves: pricing one
	// missing model was measured to flip keepalive-5m-once from -0.07% to +1.07% and
	// stop-reason-gated from -0.35% to +0.75%, reordering ranks 5 through 8 — and
	// stop-reason-gated buys 249 pings. So the defect is a total that claims coverage it does
	// not have, over cells whose winner was picked on a fraction of their own traffic.
	//
	// A cell therefore enters this total only when nothing in it went unpriced, which makes the
	// figure a floor over fully-covered cells rather than a blend of two populations.
	//
	// TotalSavingKnown is false when NO cell qualified. The figure is then n/a, not $0.00 — a
	// zero here would claim that switching every cell to its own winner saves nothing, which is
	// the opposite of "we could not price it". TotalSavingCells is how many cells are behind the
	// figure and TotalUnpricedRequests how many requests were left out of it for want of a rate,
	// so the coverage is on the payload rather than inferred from it. Before this field existed
	// `grep -n Unpriced dash/kvcachesuggest.go` returned nothing: no UI could disclose the
	// coverage because the payload did not carry it, while the sibling table on the same tab
	// does (kvcache.js:1136).
	TotalSavingUSD        float64 `json:"total_saving_usd"`
	TotalSavingKnown      bool    `json:"total_saving_known"`
	TotalSavingCells      int64   `json:"total_saving_cells"`
	TotalUnpricedRequests int64   `json:"total_unpriced_requests"`

	Scanned   int64 `json:"scanned"`
	Total     int64 `json:"total"`
	Truncated bool  `json:"truncated"`

	Notes []string `json:"notes"`
}

// KVCacheSuggest builds the per-user, per-hour strategy suggestion.
func (d *DB) KVCacheSuggest(f Filter, o KVCacheOptions, p modelinfo.Pricer,
	cfg KVCacheSimConfig) (*KVCacheSuggestions, error) {
	baseName := cfg.Baseline
	if baseName == "" {
		baseName = kvCacheDefaultBaseline()
	}
	// Validated up front, against an empty probe dataset — the same probe KVCacheArms() itself
	// uses to decide Selectable. A caller's typo must fail the WHOLE request, the same way it
	// fails /api/kvcache/simulate (see KVCacheSimulate's identical check), rather than reaching
	// every cell and rendering a silent, all-empty 200.
	if buildStrategy(baseName, nil, cfg, kvcache.Config{}) == nil {
		return nil, fmt.Errorf("dash: unknown baseline strategy %q", cfg.Baseline)
	}

	rows, total, err := d.KVCacheDataset(f, o)
	if err != nil {
		return nil, err
	}

	inWeek := make([]*kvcache.Request, 0, len(rows))
	for _, r := range rows {
		if kvInWorkWeek(r.TS) {
			inWeek = append(inWeek, r)
		}
	}

	// candidates ALWAYS includes the baseline, even one an operator picked that
	// kvSuggestCandidates() would not itself offer — `optimal`, say, to see the headroom
	// against the true ceiling. Without this a cell could report a "best" that costs MORE than
	// a baseline that was never in its own comparison, which is the one thing SavingUSD must
	// never do.
	candidates := kvSuggestCandidates()
	if !slices.Contains(candidates, baseName) {
		candidates = append(candidates, baseName)
	}
	prices := kvcache.NewPriceList(context.Background(), modelsOf(inWeek), p,
		cfg.Multipliers, cfg.Overrides)

	type cellKey struct {
		user string
		hour int
	}
	groups := map[cellKey][]*kvcache.Request{}
	userSet := map[string]bool{}
	for _, r := range inWeek {
		k := cellKey{r.User, r.HourUTC}
		groups[k] = append(groups[k], r)
		userSet[r.User] = true
	}

	out := &KVCacheSuggestions{Baseline: baseName, Weekdays: kvSuggestWeekdays, TimeZone: "UTC",
		MinRequests: kvSuggestMinRequests, Cells: []KVCacheSuggestion{},
		Scanned: int64(len(rows)), Total: total, Truncated: int64(len(rows)) < total}
	for u := range userSet {
		out.Users = append(out.Users, u)
	}
	sort.Strings(out.Users)

	for _, u := range out.Users {
		for h := 0; h < 24; h++ {
			grp := groups[cellKey{u, h}]
			if len(grp) == 0 {
				continue
			}
			cell := kvSuggestCell(u, h, grp, candidates, baseName, prices, cfg)
			out.Cells = append(out.Cells, cell)
			out.TotalUnpricedRequests += cell.UnpricedRequests
			// SavingKnown and full pricing coverage, not just Valued: see TotalSavingUSD.
			if cell.Valued && cell.SavingKnown && cell.UnpricedRequests == 0 && !cell.InsufficientData {
				out.TotalSavingUSD += cell.SavingUSD
				out.TotalSavingCells++
				out.TotalSavingKnown = true
			}
		}
	}

	out.Notes = []string{
		"Every timestamp and hour is UTC, matching the rest of this page. Friday and Saturday " +
			"requests are not read at all, not down-weighted and not shown in a separate group.",
		"Each cell is replayed on ONLY its own rows: this account's Sunday–Thursday requests in " +
			"this one hour of the day, nothing else. Two requests from the same conversation that " +
			"land in the same hour on different days are treated as consecutive turns with a " +
			"multi-day gap, which is what this account's traffic in this hour actually looks like.",
		"BestStrategy is never worse than Baseline: Baseline is itself always a candidate, so a " +
			"cell where nothing beats it simply recommends keeping it.",
		"A cell below min_requests still reports every candidate's own numbers; " +
			"insufficient_data says not to act on it as a pattern.",
	}
	return out, nil
}

// kvSuggestCell replays one (user, hour) cell under every candidate and picks the winner.
func kvSuggestCell(user string, hour int, rows []*kvcache.Request, candidates []string,
	baseName string, prices *kvcache.PriceList, cfg KVCacheSimConfig) KVCacheSuggestion {
	sim := kvcache.Config{Prices: prices, Semantics: cfg.Semantics, PingIdle: cfg.PingIdle,
		PingIdle1h: cfg.PingIdle1h, MaxPings: cfg.MaxPings}

	byName := map[string]*kvcache.Result{}
	for _, name := range candidates {
		s := buildStrategy(name, rows, cfg, sim)
		if s == nil {
			continue
		}
		byName[name] = kvcache.Simulate(rows, s, sim)
	}
	baseline := byName[baseName]
	if baseline == nil {
		if s := buildStrategy(baseName, rows, cfg, sim); s != nil {
			baseline = kvcache.Simulate(rows, s, sim)
			byName[baseName] = baseline
		}
	}

	cell := KVCacheSuggestion{User: user, HourUTC: hour, Requests: int64(len(rows)),
		InsufficientData: int64(len(rows)) < kvSuggestMinRequests,
		Baseline:         baseName, Candidates: []kvcache.Savings{}}
	if baseline == nil {
		return cell
	}
	cell.BaselineUSD = baseline.TotalUSD

	// The exact ceiling, built and simulated on exactly this cell's own rows — the same
	// population every candidate above was scored on — but never added to `candidates`:
	// kvSuggestCandidates() excludes `optimal` for the whole suggester on purpose (it reads
	// the true next-request time), and that must hold here too, not just for the arms an
	// unattended sweep could pick as a winner.
	if s := buildStrategy(KVStrategyOptimal, rows, cfg, sim); s != nil {
		optimal := kvcache.Simulate(rows, s, sim)
		cell.OptimalUSD = optimal.TotalUSD
		cell.OptimalSavingUSD = kvcache.Compare(baseline, optimal).AbsoluteUSD
		cell.OptimalKnown = baseline.Valued && optimal.Valued
	}

	var best kvcache.Savings
	var bestResult *kvcache.Result
	haveBest := false
	for _, name := range candidates {
		r := byName[name]
		if r == nil {
			continue
		}
		s := kvcache.Compare(baseline, r)
		cell.Candidates = append(cell.Candidates, s)
		if !r.Valued {
			continue
		}
		if !haveBest || s.AbsoluteUSD > best.AbsoluteUSD {
			best, bestResult, haveBest = s, r, true
		}
	}
	if !haveBest {
		return cell
	}
	cell.BestStrategy = best.Strategy
	cell.BestDescription = bestResult.Description
	cell.BestUSD = bestResult.TotalUSD
	cell.SavingUSD = best.AbsoluteUSD
	cell.SavingPct = best.PercentUSD
	cell.SavingKnown = best.Known
	cell.Valued = baseline.Valued && bestResult.Valued
	// Every arm replays the SAME rows through the same PriceList, so an unpriced request is
	// unpriced under all of them; the baseline's count is the cell's.
	cell.UnpricedRequests = baseline.Unpriced
	return cell
}
