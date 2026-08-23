package dash

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// The KV-cache page's simulation half: turn the page's inputs into strategies, replay the
// window under each of them, and score them against one baseline.
//
// Nothing here decides anything. Every policy, every price and every cache semantic is in
// package kvcache; this file resolves NAMES to those objects and reports what came back. That
// separation is what lets a future learned predictor appear on this page without either the
// simulator or the dashboard changing: it implements kvcache.Predictor, gets a name in
// kvCacheStrategies, and everything downstream is unchanged.

// KVCacheCustom is the configurable arm's inputs, as the page sends them.
type KVCacheCustom struct {
	// P5m and P1h are the reuse-probability thresholds, as fractions.
	P5m float64 `json:"p_5m"`
	P1h float64 `json:"p_1h"`
	// MinPrefix is the prefix below which nothing is cached at all, in tokens.
	MinPrefix int64 `json:"min_prefix"`
	// AlwaysPing forces the long hold onto the keep-alive path rather than the cheaper of the
	// two, for an operator measuring the ping path specifically.
	AlwaysPing bool `json:"always_ping"`
}

// KVCacheSimConfig is everything the page can change about a simulation.
type KVCacheSimConfig struct {
	// Multipliers and Overrides are the pricing experiment: the multiples used to fill in a
	// rate the price list does not state, and per-model rates typed in the page.
	Multipliers kvcache.Multipliers
	Overrides   map[string]kvcache.Override
	// Semantics is the provider cache behaviour.
	Semantics kvcache.Semantics
	// The keep-alive schedule.
	PingIdle   time.Duration
	PingIdle1h time.Duration
	MaxPings   int
	// Strategies names the arms to run, and Baseline the one the others are scored against.
	Strategies []string
	Baseline   string
	// Custom is the configurable arm's parameters.
	Custom KVCacheCustom
	// Predictor, when non-nil, is wired into the custom arm. There is no way to set this from
	// an HTTP request and that is deliberate: a predictor is code, not a query parameter. It
	// is here so an in-process caller — the proxy, a benchmark harness, a test — can score a
	// learned model against exactly the same baseline the page shows.
	Predictor kvcache.Predictor
}

// The strategy names the page may ask for.
//
// Every one of them is kvcache's own constant rather than a second spelling of it. The name
// arrives from a query string and is resolved through kvcache.NewStrategy, so a typo selects
// nothing rather than something — and there is exactly one list of arms in the process, which
// is the point: dash used to carry a closed six-name list of its own, and the four arms the
// domain had gained since (the two keep-alive arms, the extend-to-1h arm and the exact ceiling)
// were unreachable from the dashboard while looking perfectly present in the code.
const (
	KVStrategyNoCache     = kvcache.StrategyNoCache
	KVStrategyFixed5m     = kvcache.StrategyFixed5m
	KVStrategyFixed1h     = kvcache.StrategyFixed1h
	KVStrategyKeepAlive5m = kvcache.StrategyKeepAlive5m
	KVStrategyKeepAlive1h = kvcache.StrategyKeepAlive1h
	KVStrategyExtend1h    = kvcache.StrategyExtend1h
	KVStrategyObserved    = kvcache.StrategyObserved
	KVStrategyHistorical  = kvcache.StrategyHistorical
	KVStrategyOptimal     = kvcache.StrategyOptimal
	// KVStrategyCustom is the one arm that is NOT in kvcache's registry, because it cannot be
	// built from a name: it carries the page's own thresholds and the in-process Predictor
	// seam. It is offered beside the registry's arms and scored identically.
	KVStrategyCustom = "custom"
)

// KVCacheArm is one selectable arm as the page presents it: the domain's own spec, plus whether
// this API can actually construct it.
//
// Selectable is a real distinction rather than a filter. `replay` is a registry arm whose whole
// input is an action list supplied in-process, so no query string can ask for it — and dropping
// it from the payload would leave the page unable to explain why an arm the docs mention is not
// on the picker. It is listed, marked, and not offered as a checkbox.
type KVCacheArm struct {
	kvcache.StrategySpec
	Selectable bool `json:"selectable"`
}

// KVCacheArms is every arm the page may show, in the domain's presentation order, with the
// dash-only custom arm last.
//
// The order is kvcache.Registry()'s and is deliberately not re-sorted here: it runs
// cheapest-to-reason-about first, so a reader meets the baseline before the policies scored
// against it. Sorting by cost or by hit rate would be the page choosing a winner.
func KVCacheArms() []KVCacheArm {
	out := []KVCacheArm{}
	for _, spec := range kvcache.Registry() {
		// Probed rather than hardcoded: an arm the domain adds becomes selectable here on the
		// day it lands, and one that needs an in-process input is marked without this file
		// having to know which one that is. NeedsDataset arms are constructed with the real
		// rows at simulate time, so an empty probe set is enough to tell "needs data" from
		// "cannot be built from a name at all".
		_, err := kvcache.NewStrategy(spec.Name, nil, kvcache.Config{})
		out = append(out, KVCacheArm{StrategySpec: spec,
			Selectable: err == nil || spec.NeedsDataset && !isUnbuildable(spec.Name)})
	}
	return append(out, KVCacheArm{Selectable: true, StrategySpec: kvcache.StrategySpec{
		Name: KVStrategyCustom,
		Description: "Configured thresholds over your own closed gaps, or an in-process " +
			"predictor. The seam a learned next-use model plugs into without changing this page.",
	}})
}

// isUnbuildable names the arms no HTTP request can construct, whatever their spec says.
//
// One name, and it is a name rather than a probe because the probe cannot tell it apart from an
// arm that merely needs rows: `replay` is scored from an action list decided elsewhere, which is
// the seam an offline model's answers arrive through.
func isUnbuildable(name string) bool { return name == kvcache.StrategyReplay }

// KVCacheDefaultStrategies is what the page runs when the caller names none.
//
// Seven arms, and each earns its place: the two bounds (no-cache below, optimal above), the two
// fixed tiers, the cheapest keep-alive arm, the policy already in force, and the one arm that
// learns from the account's own history. `optimal` is in the DEFAULT set on purpose — it is the
// only figure that says how much headroom exists at all, and an arm nobody sees by default is
// one nobody compares against — and it is marked Unreachable so no surface can present it as a
// result.
var KVCacheDefaultStrategies = []string{KVStrategyNoCache, KVStrategyFixed5m, KVStrategyFixed1h,
	KVStrategyKeepAlive5m, KVStrategyObserved, KVStrategyHistorical, KVStrategyOptimal}

// kvCacheDefaultBaseline is the arm every saving is measured against when the caller names none,
// and it comes from the REGISTRY rather than from a constant here.
//
// It matters more than a default usually does. Measured on the production corpus, prompt caching
// is already saving 53%, so scoring against `no-cache` reports an enormous saving that no
// decision can act on — the cache is not going to be switched off. The honest denominator is the
// cheapest thing already in force, which is what the registry's Baseline flag names. A second
// answer here would be a second definition of the one number every percentage is divided by.
func kvCacheDefaultBaseline() string {
	for _, s := range kvcache.Registry() {
		if s.Baseline {
			return s.Name
		}
	}
	return KVStrategyFixed5m
}

// buildStrategy resolves one name against the dataset and the configuration.
//
// Every registry arm goes through kvcache.NewStrategy, so this file holds no policy of its own.
// The custom arm is the exception and the reason is structural: it carries inputs that only exist
// on this page, and the Predictor an in-process caller supplies.
//
// `sim` is THE SAME kvcache.Config the replay will run with, passed through rather than rebuilt.
// That is not tidiness. An arm that decides by comparing costs needs the PRICE LIST at
// construction time, and a locally rebuilt Config that omitted it made every action cost zero:
// the exact ceiling then chose "let it expire" for every request and reported the same total as
// the no-cache arm — 2.1x the cheapest real policy, presented as the cheapest plan that exists.
// TestThePageIsTheRightShapeOnProductionLikeTraffic is what caught it, by asserting that nothing
// which cannot see the future may cost less than the arm that can.
func buildStrategy(name string, rows []*kvcache.Request, cfg KVCacheSimConfig,
	sim kvcache.Config) kvcache.Strategy {
	if name == KVStrategyCustom {
		return kvcache.Custom{Label: KVStrategyCustom, Predictor: cfg.Predictor,
			P5m: cfg.Custom.P5m, P1h: cfg.Custom.P1h, MinPrefix: cfg.Custom.MinPrefix,
			Semantics: cfg.Semantics, PingIdle: cfg.PingIdle, MaxPings: cfg.MaxPings,
			AlwaysPing: cfg.Custom.AlwaysPing}
	}
	if isUnbuildable(name) {
		return nil
	}
	s, err := kvcache.NewStrategy(name, rows, sim)
	if err != nil {
		return nil
	}
	return s
}

// KVCacheSimulation is the whole /api/kvcache/simulate payload.
type KVCacheSimulation struct {
	// Baseline is the arm every saving below is measured against, by name.
	Baseline string `json:"baseline"`
	// Results is one entry per arm, in kvCacheStrategyOrder.
	Results []*kvcache.Result `json:"results"`
	// Savings is one entry per arm INCLUDING the baseline (whose own saving is exactly zero,
	// shown rather than omitted so the table has a row for every arm).
	Savings []kvcache.Savings `json:"savings"`

	Pricing     *kvcache.PriceList `json:"pricing"`
	Assumptions KVCacheAssumptions `json:"assumptions"`
	// Arms is every arm that exists, so the page builds its picker from what the server can
	// actually run instead of from a list of its own. A hardcoded picker is how four shipped
	// arms stayed invisible for a day.
	Arms []KVCacheArm `json:"arms"`

	// Unknown names any requested strategy that does not exist, rather than silently running
	// something else.
	Unknown []string `json:"unknown,omitempty"`

	Scanned   int64 `json:"scanned"`
	Total     int64 `json:"total"`
	Truncated bool  `json:"truncated"`
	// WindowEnd is the instant an open idle span is bounded at, and it is on the wire because
	// PingsOnOpenSpans cannot be read without it.
	WindowEnd int64 `json:"window_end"`
}

// KVCacheSimulate replays one window under every requested strategy.
func (d *DB) KVCacheSimulate(f Filter, o KVCacheOptions, p modelinfo.Pricer,
	cfg KVCacheSimConfig) (*KVCacheSimulation, error) {
	rows, total, err := d.KVCacheDataset(f, o)
	if err != nil {
		return nil, err
	}
	out := &KVCacheSimulation{Baseline: cfg.Baseline, Results: []*kvcache.Result{},
		Savings: []kvcache.Savings{}, Scanned: int64(len(rows)), Total: total,
		Truncated: int64(len(rows)) < total, Assumptions: kvCacheAssumptions(cfg),
		Arms: KVCacheArms()}
	out.Pricing = kvcache.NewPriceList(context.Background(), modelsOf(rows), p,
		cfg.Multipliers, cfg.Overrides)

	// The window's own end, used to bound an OPEN idle span. The filter's `until` where it has
	// one, otherwise the last request in the dataset — never time.Now(), which would make the
	// same historical window produce a different answer every time it was replayed.
	windowEnd := f.Until
	if windowEnd <= 0 {
		for _, r := range rows {
			if r.TS > windowEnd {
				windowEnd = r.TS
			}
		}
	}
	out.WindowEnd = windowEnd

	sim := kvcache.Config{Prices: out.Pricing, Semantics: cfg.Semantics,
		PingIdle: cfg.PingIdle, PingIdle1h: cfg.PingIdle1h, MaxPings: cfg.MaxPings,
		WindowEnd: windowEnd}

	names := cfg.Strategies
	if len(names) == 0 {
		names = KVCacheDefaultStrategies
	}
	names = orderStrategies(names)
	byName := map[string]*kvcache.Result{}
	for _, name := range names {
		s := buildStrategy(name, rows, cfg, sim)
		if s == nil {
			out.Unknown = append(out.Unknown, name)
			continue
		}
		r := kvcache.Simulate(rows, s, sim)
		out.Results = append(out.Results, r)
		byName[r.Strategy] = r
	}
	// The baseline. A named one that was not run is an error the caller must see, not a
	// silent substitution — every percentage on the page is divided by this arm's total.
	baseName := cfg.Baseline
	if baseName == "" {
		baseName = kvCacheDefaultBaseline()
	}
	baseline := byName[baseName]
	if baseline == nil {
		// The baseline must be replayed even when it was not asked for, or the savings column
		// has no denominator. Adding it to the results as well would change the arms on
		// screen, so it is computed and used but not listed.
		s := buildStrategy(baseName, rows, cfg, sim)
		if s == nil {
			return nil, fmt.Errorf("dash: unknown baseline strategy %q", cfg.Baseline)
		}
		baseline = kvcache.Simulate(rows, s, sim)
	}
	out.Baseline = baseline.Strategy
	for _, r := range out.Results {
		out.Savings = append(out.Savings, kvcache.Compare(baseline, r))
	}
	return out, nil
}

// orderStrategies puts the requested arms in the registry's presentation order and drops
// duplicates. An unrecognised name is KEPT, at the end, so KVCacheSimulate can report it rather
// than silently running something else.
func orderStrategies(names []string) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := make([]string, 0, len(want))
	for _, a := range KVCacheArms() {
		if want[a.Name] {
			out = append(out, a.Name)
			delete(want, a.Name)
		}
	}
	rest := make([]string, 0, len(want))
	for n := range want {
		rest = append(rest, n)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// kvCacheAssumptions is the server's own statement of what every figure on the page rests on.
//
// It is DATA rather than prose in the UI for one reason: a formula typed into a JavaScript
// template is a second definition of the arithmetic, and nothing tests it against the first.
// These strings are checked against the functions that implement them — see
// TestTheDocumentedFormulasMatchTheCode.
func kvCacheAssumptions(cfg KVCacheSimConfig) KVCacheAssumptions {
	sem := cfg.Semantics
	if sem == (kvcache.Semantics{}) {
		sem = kvcache.DefaultSemantics()
	}
	idle, idle1h, k := cfg.PingIdle, cfg.PingIdle1h, cfg.MaxPings
	if idle <= 0 {
		idle = kvcache.DefaultPingIdle
	}
	if idle1h <= 0 {
		idle1h = kvcache.DefaultPingIdle1h
	}
	if k <= 0 {
		k = kvcache.DefaultMaxPings
	}
	a := KVCacheAssumptions{
		TimeZone:         "UTC",
		TimeUnit:         "milliseconds",
		Horizon5mSeconds: kvcache.Horizon5m.Seconds(),
		Horizon1hSeconds: kvcache.Horizon1h.Seconds(),
		Multipliers:      cfg.Multipliers.WithDefaults(),
		Semantics:        sem,
		Schedule: KVCacheSchedule{IdleSeconds: idle.Seconds(),
			IdleSeconds1h: idle1h.Seconds(), MaxPings: k},
	}
	a.Formulas = []KVCacheFormula{
		{"Uncached input", "input_tokens × input_rate",
			"A prompt with no cache_control at all: every token billed once, at the fresh rate."},
		{"Cache read", "cache_read_tokens × cache_read_rate",
			"A hit. The read rate defaults to 0.1× input, and a read refreshes the entry's " +
				"lifetime for no extra charge."},
		{"Cache write, 5-minute TTL", "written_tokens × write_5m_rate",
			"Creating an entry that lives five minutes. Defaults to 1.25× input."},
		{"Cache write, 1-hour TTL", "written_tokens × write_1h_rate",
			"Creating an entry that lives an hour. Defaults to 2.0× input — no gateway " +
				"publishes this rate, so it is derived from the multiplier and editable above."},
		{"Keep-alive ping", "cached_tokens × cache_read_rate + ping_input × input_rate + " +
			"ping_output × output_rate",
			"One refresh. It is a cache READ, so a five-minute and a one-hour keep-alive cost " +
				"the same per ping; what differs is the creation tier that put the entry there " +
				"and how often a ping is needed."},
		{"Keep-alive that arrived late", "cached_tokens × write_rate(tier) + ping_input × " +
			"input_rate + ping_output × output_rate",
			"A ping that fires after the entry lapsed re-creates it, at 12.5× a read for the " +
				"5-minute tier and 20× for the 1-hour one. Counted as pings_that_rewrote."},
		{"Request cost", "uncached_input + cache_read + cache_write + output",
			"One request's whole bill under a simulated strategy."},
		{"Total cost", "Σ request_cost + Σ ping_cost", "One strategy's whole bill."},
		{"Incremental cache premium", "total_cost − uncached_cost",
			"What the caching machinery itself cost, separate from the bill it sits inside. " +
				"NEGATIVE means the cache paid for itself."},
		{"Absolute savings", "baseline_cost − strategy_cost",
			"Negative where the strategy costs more, and shown that way — never clamped to zero."},
		{"Percentage savings", "absolute_savings ÷ baseline_cost × 100",
			"Undefined, not 0%, when the baseline is zero."},
	}
	a.Notes = []string{
		"Every timestamp, hour and time-of-day band on this page is UTC. The store carries no " +
			"per-user timezone, so a local one would be invented.",
		"A conversation is (account, session, model). The account is in the key because a " +
			"session id is client-supplied and can collide between accounts; the MODEL is in it " +
			"because a cache entry does not transfer between models, so a request on one model " +
			"cannot read another model's entry and the two are not each other's successor.",
		"A request with no next request in the same conversation has NO idle time. It is " +
			"counted separately and excluded from every average, never treated as a zero gap.",
		"Keep-alive ping rows written by context-guru itself are excluded from this dataset: " +
			"counted, they would split one real idle gap into two short ones.",
		"A cache miss recorded as prefix_change or cold_start cannot be rescued by any TTL. " +
			"Those rows force a miss in every simulated arm and are reported as forced_misses.",
		"Requests whose token accounting was incomplete have an UNKNOWN cost and contribute " +
			"nothing to any dollar figure. They are counted, never valued at zero.",
		"The one-hour write rate is derived from a multiplier because no gateway publishes " +
			"one. Both the multiplier and the resulting rate are editable above.",
		"A ping cannot generate nothing on a provider that requires max_tokens ≥ 1, so its " +
			"minimum output is priced and stated rather than rounded away.",
		"Latency avoided is derived from THIS window's own means: the mean upstream time of " +
			"requests that really missed, minus that of requests that really hit. It is " +
			"absent where either population is under 20 rows.",
	}
	return a
}

// ── the pricing panel ──────────────────────────────────────────────────────

// KVCachePriceCost is one model's rates turned into DOLLARS on this window's own median
// prefix.
//
// It exists because a per-token rate is not a number anybody can act on. "$0.0000060 per
// token at the one-hour tier" and "$0.75 to hold your median 124,845-token prompt for an
// hour" are the same fact, and only the second one answers the question the reader has. Both
// are shown: the rate, because it is what they can edit, and the cost, because it is what
// they are deciding about.
type KVCachePriceCost struct {
	Model string `json:"model"`
	Known bool   `json:"known"`
	// Uncached is the prefix billed as fresh input — the no-cache alternative to all of it.
	Uncached float64 `json:"uncached_usd"`
	// Read is one cache hit on the prefix; Write5m and Write1h are one creation at each tier.
	Read    float64 `json:"read_usd"`
	Write5m float64 `json:"write_5m_usd"`
	Write1h float64 `json:"write_1h_usd"`
	// KeepAlive is ONE refresh, and Late is one refresh that arrived after the entry lapsed.
	KeepAlive float64 `json:"keep_alive_usd"`
	Late5m    float64 `json:"late_5m_usd"`
	Late1h    float64 `json:"late_1h_usd"`
	// Hold5m and Hold1h are the whole cost of holding the prefix at each tier through one idle
	// span: the creation plus MaxPings refreshes. The two numbers a strategy chooses between.
	Hold5m float64 `json:"hold_5m_usd"`
	Hold1h float64 `json:"hold_1h_usd"`
}

// KVCachePriceView is /api/kvcache/pricing: the editable rates, the assumptions, and what
// those rates come to on this window's own prefix.
type KVCachePriceView struct {
	Pricing     *kvcache.PriceList `json:"pricing"`
	Assumptions KVCacheAssumptions `json:"assumptions"`
	// Arms is every arm that exists, so the page builds its picker from what the server can
	// actually run instead of from a list of its own. A hardcoded picker is how four shipped
	// arms stayed invisible for a day.
	Arms []KVCacheArm `json:"arms"`
	// Prefix is the window's own MEDIAN billed prefix, in tokens, and the size every cost
	// above is computed on. The median rather than the mean: per-request cache cost is
	// bimodal on real traffic, so a mean would describe no request.
	Prefix int64              `json:"prefix_tokens"`
	Costs  []KVCachePriceCost `json:"costs"`
}

// KVCachePriceView builds the pricing panel's payload.
func kvCachePriceView(models []string, prefix int64, p modelinfo.Pricer,
	cfg KVCacheSimConfig) *KVCachePriceView {
	list := kvcache.NewPriceList(context.Background(), models, p, cfg.Multipliers, cfg.Overrides)
	a := kvCacheAssumptions(cfg)
	out := &KVCachePriceView{Pricing: list, Assumptions: a, Prefix: prefix,
		Costs: []KVCachePriceCost{}}
	sem := a.Semantics
	k := a.Schedule.MaxPings
	for _, m := range list.Models {
		c := KVCachePriceCost{Model: m.Model, Known: m.Known}
		if m.Known {
			c.Uncached = m.UncachedCost(0, prefix, 0)
			c.Read = float64(prefix) * m.CacheRead
			c.Write5m = m.HoldCost(prefix, kvcache.TTL5m, 0, sem)
			c.Write1h = m.HoldCost(prefix, kvcache.TTL1h, 0, sem)
			c.KeepAlive = m.KeepAliveCost(prefix, sem)
			c.Late5m = m.RecreateCost(prefix, kvcache.TTL5m, sem)
			c.Late1h = m.RecreateCost(prefix, kvcache.TTL1h, sem)
			c.Hold5m = m.HoldCost(prefix, kvcache.TTL5m, k, sem)
			c.Hold1h = m.HoldCost(prefix, kvcache.TTL1h, k, sem)
		}
		out.Costs = append(out.Costs, c)
	}
	return out
}

// KVCacheModels is the distinct models in one window, so the pricing panel has a row for every
// model the reader could be pricing — including the ones with no rates.
func (d *DB) KVCacheModels(f Filter) ([]string, error) {
	cond, args := f.where()
	rows, err := d.sql.Query(`SELECT DISTINCT r.model FROM requests r WHERE `+cond+
		` ORDER BY r.model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// KVCacheMedianPrefix is the window's own median billed prefix (cache_read + cache_write), in
// tokens.
//
// NOT tokens_before, which is message text only and runs a median 3.38x low — the same
// correction the keep-alive tab's calculator already applies. Exact rather than interpolated,
// for the reason pctlF is: sorting a few thousand values is free and an estimate here is a
// number nobody can reproduce.
func (d *DB) KVCacheMedianPrefix(f Filter) (int64, error) {
	cond, args := f.where()
	rows, err := d.sql.Query(`SELECT r.cache_read + r.cache_write FROM requests r WHERE `+cond,
		args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var xs []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		xs = append(xs, v)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return int64(pctlF(xs, 0.50)), nil
}
