package kvcache

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Action is what a strategy decides to do about a conversation's cached context for the
// idle span that starts now.
//
// Five actions and no sixth: they are the whole set of things a caller can actually buy
// from a prompt-caching provider. "Do nothing" is ActionExpire — an entry nobody pays to
// keep lapses on its own — and saying so explicitly is what makes a no-cache baseline a
// strategy rather than an absence.
type Action string

const (
	// ActionExpire writes no cache_control at all: this request's prompt is billed as fresh
	// input and nothing is held afterwards.
	ActionExpire Action = "expire"
	// ActionWrite5m writes/retains the prefix at the 5-minute tier.
	ActionWrite5m Action = "write_5m"
	// ActionWrite1h writes/retains the prefix at the 1-hour tier.
	ActionWrite1h Action = "write_1h"
	// ActionPing5m writes/retains at 5 minutes AND keeps refreshing it with keep-alive
	// reads while the conversation is idle.
	ActionPing5m Action = "ping_5m"
	// ActionPing1h holds the entry at the 1-hour tier and refreshes it with keep-alive
	// reads while the conversation is idle.
	ActionPing1h Action = "ping_1h"
	// ActionWrite5mPing1h writes at the CHEAP tier and buys the long hold only if the
	// conversation actually goes quiet: the request itself creates a 5-minute entry at 1.25x
	// input, and if a keep-alive comes due before that lapses, THAT keep-alive extends the
	// context by an hour — a 1-hour write of the prefix at 2.0x, after which the entry is
	// hourly and needs refreshing twelve times less often.
	//
	// It is a distinct arm from ActionPing1h and not a variation of it. ActionPing1h pays the
	// 2.0x creation premium on EVERY request; this pays 1.25x on every request and 2.0x only
	// on the rare span that outlives five minutes. On traffic where 92.5% of gaps close inside
	// five minutes that is a different bill entirely, which is the whole reason it exists.
	//
	// The upgrade is priced as a 1-hour WRITE rather than a read, and that is a measurement
	// rather than a guess: the deployment's own capture shows a request asking for ttl:"1h"
	// over an already-warm prefix returning 36,251 of 36,574 written tokens on the 1h
	// creation tier (see the cache_write_1h column in dash/schema.go). If some provider bills
	// it as a read instead, this arm is CHEAPER than reported here, never dearer.
	ActionWrite5mPing1h Action = "write_5m_ping_1h"
)

// Actions is every action, in escalating cost order.
var Actions = []Action{ActionExpire, ActionWrite5m, ActionWrite1h, ActionPing5m, ActionPing1h,
	ActionWrite5mPing1h}

// Tier is the TTL the REQUEST ITSELF writes at. Not necessarily the tier the entry ends up
// at: ActionWrite5mPing1h writes 5 minutes and its keep-alive upgrades that to an hour, which
// is exactly the distinction that makes it cheap in the common case.
func (a Action) Tier() TTL {
	switch a {
	case ActionWrite5m, ActionPing5m, ActionWrite5mPing1h:
		return TTL5m
	case ActionWrite1h, ActionPing1h:
		return TTL1h
	}
	return TTLNone
}

// PingTier is the tier this action's keep-alives hold the entry at, which is what a ping has
// to pay to reach. TTLNone for an action that never pings.
func (a Action) PingTier() TTL {
	switch a {
	case ActionPing5m:
		return TTL5m
	case ActionPing1h, ActionWrite5mPing1h:
		return TTL1h
	}
	return TTLNone
}

// Pings reports whether this action sends keep-alives during the idle span.
func (a Action) Pings() bool {
	return a == ActionPing5m || a == ActionPing1h || a == ActionWrite5mPing1h
}

// Caches reports whether this action writes cache_control at all.
func (a Action) Caches() bool { return a.Tier() != TTLNone }

// Load is optional system-load information, for a strategy that wants to back off when the
// deployment is busy. A POINTER on the Observation, and nil on every deployment that does
// not measure it: a strategy that reads a zero as "idle" would behave differently on a
// deployment that simply has no gauge, which is the failure mode a bool-plus-value pair
// cannot express.
type Load struct {
	// InFlight is concurrent upstream requests at the decision instant.
	InFlight int `json:"in_flight"`
	// UpstreamMs is the recent mean upstream latency, in milliseconds.
	UpstreamMs float64 `json:"upstream_ms"`
}

// Observation is everything a strategy is allowed to see, and it is defined by what is
// ABSENT from it.
//
// There is no next-request timestamp on it, no idle duration, and no field derived from
// either. That is not an oversight to be fixed by a future field — it is the invariant the
// whole simulator exists to hold, because a predictor that can see how long the gap turned
// out to be will "predict" it perfectly and every saving it reports will be unreachable in
// production. TestStrategiesCannotSeeTheFuture asserts it by handing every strategy an
// Observation whose future is a trap.
//
// Everything here was knowable at Now: the request that has just been served, the state of
// the entry it leaves behind, the account's rates, and statistics accumulated from gaps
// that had already CLOSED.
type Observation struct {
	// Who and what. User is the tenant, Conversation the trajectory.
	User         string
	Conversation string
	Model        string

	// RequestID identifies the request just served. Present-tense identity, not a
	// prediction: it is here so an arm that replays a recorded per-request decision — and a
	// future predictor that keys features per request — needs no side channel.
	RequestID int64

	// Now is the decision instant: epoch ms, UTC. It is the timestamp of the request just
	// served, which is when the cache_control header has to be chosen.
	Now     int64
	HourUTC int
	Bucket  Bucket

	// CachedTokens is the prefix that exists after this request — the thing a TTL protects
	// and a ping refreshes, and the number every cost below is proportional to.
	CachedTokens int64

	// SinceLastMs is how long this conversation had been idle BEFORE the request just
	// served, i.e. the gap that has already closed. 0 on a conversation's first request.
	// Past information, and the single most useful past fact a strategy has.
	SinceLastMs int64

	// TTL is the tier the entry is currently held at and ExpiresAt when it would lapse if
	// nothing touched it. Both describe the state entering the decision, not its outcome.
	TTL       TTL
	ExpiresAt int64

	// Turn is how many requests this conversation has served so far, including this one.
	Turn int

	// Stats is the historical view, or nil where a caller has none. Every number it serves
	// is accumulated from gaps that closed strictly before Now.
	Stats Stats

	// Pricing is the model's rates, so a strategy can compare what an action costs with
	// what it might avoid rather than acting on a token count alone.
	Pricing Pricing

	// Load is system-load information where the deployment has it, nil where it does not.
	Load *Load
}

// Strategy is a KV-cache management policy: given only what was knowable at the decision
// instant, choose what to do with the cached context for the span that starts now.
//
// Deliberately one method. A strategy that needs to learn from outcomes does so through
// Stats, which the simulator advances as gaps close — not through a callback the simulator
// would have to invoke with the future in hand.
type Strategy interface {
	// Name is the label the dashboard groups results by. Stable, because it is a key.
	Name() string
	// Decide chooses the action for the idle span starting at o.Now.
	Decide(o Observation) Action
}

// Describer is an optional interface: a strategy that can explain itself in one sentence
// gets that sentence rendered beside its results instead of just its name.
type Describer interface {
	Describe() string
}

// Predictor is the seam a LEARNED model plugs into.
//
// It answers exactly the question a TTL decision needs — will this conversation come back
// within `horizon`? — and it receives the same Observation every strategy does, so a model
// trained on features the Observation does not carry cannot be wired in by accident. A
// predictor that has no opinion returns ok=false and Custom falls back to its thresholds.
//
// Nothing in the simulator or the dashboard needs to change to adopt one: build a
// Custom{Predictor: yourModel} and it appears beside the hand-written strategies with the
// same costs, the same baseline and the same savings arithmetic.
type Predictor interface {
	ReuseProbability(o Observation, horizon time.Duration) (p float64, ok bool)
}

// Stats is the historical view a strategy may lean on, with an explicit FALLBACK LEVEL on
// every answer.
//
// The level matters as much as the number: "62% of this user's afternoon gaps closed within
// five minutes, over 340 gaps" and "62%, over 3 gaps, actually the service-wide average
// because this user is new" are different facts, and a strategy that cannot tell them apart
// will act confidently on nothing. Every method therefore returns (value, n, level).
type Stats interface {
	// ReuseWithin is the share of this cell's already-closed gaps that were no longer than
	// d. level is one of the Level* constants.
	ReuseWithin(user, model string, b Bucket, d time.Duration) (p float64, n int, level string)
	// MedianIdle is this cell's median already-closed gap.
	MedianIdle(user, model string, b Bucket) (idle time.Duration, n int, level string)
}

// The fallback levels, most specific first. LevelNone means nothing at all was known — not
// "zero probability".
const (
	LevelUserBucket = "user+model+bucket"
	LevelUserModel  = "user+model"
	LevelUser       = "user"
	LevelModel      = "model"
	LevelGlobal     = "global"
	LevelNone       = "none"
)

// minCell is how many closed gaps a cell needs before it is trusted over its parent.
//
// Six because a five-minute reuse probability estimated from fewer is not a probability: at
// n=3 the resolution is 33 percentage points, and a strategy switching tier on that is
// choosing by noise. Below it the lookup falls through to the next level, and the level it
// actually used is returned so the caller can say which.
const minCell = 6

// ── the accumulating statistics the simulator advances as gaps close ────────

// History is the leak-free Stats implementation the simulator uses.
//
// It is EMPTY at the start of a replay and gains one observation per gap AS THAT GAP
// CLOSES — that is, when the successor request arrives, not when the predecessor is
// decided. So a decision made at time T can only ever see gaps that ended at or before T.
// That is the whole mechanism by which the replay is honest, and it is why History is a
// mutable accumulator rather than a precomputed table: a precomputed table over the window
// would carry the future into every early decision.
type History struct {
	cells map[statKey]*cell
}

type statKey struct {
	user, model string
	bucket      Bucket
}

type cell struct {
	gaps []time.Duration
	// sorted tracks whether gaps is ordered, so the median is not re-sorted per lookup.
	sorted bool
	within map[time.Duration]int
}

// NewHistory builds an empty accumulator.
func NewHistory() *History { return &History{cells: map[statKey]*cell{}} }

// keys are the cells one observation lands in, from most specific to least. A gap is
// counted in EVERY level, so a fallback is a real aggregate rather than an empty parent.
func keysFor(user, model string, b Bucket) []statKey {
	return []statKey{
		{user, model, b},
		{user, model, ""},
		{user, "", ""},
		{"", model, ""},
		{"", "", ""},
	}
}

// levels are the labels of keysFor, index for index.
var levels = []string{LevelUserBucket, LevelUserModel, LevelUser, LevelModel, LevelGlobal}

// Observe records one CLOSED gap. Callers must not call it for a gap that has not happened
// yet — that is the leak this type exists to prevent, and the simulator calls it in exactly
// one place, on the successor's arrival.
func (h *History) Observe(user, model string, b Bucket, gap time.Duration) {
	if h == nil {
		return
	}
	if gap < 0 {
		gap = 0
	}
	for _, k := range keysFor(user, model, b) {
		c := h.cells[k]
		if c == nil {
			c = &cell{within: map[time.Duration]int{}}
			h.cells[k] = c
		}
		c.gaps = append(c.gaps, gap)
		c.sorted = false
		if gap <= Horizon5m {
			c.within[Horizon5m]++
		}
		if gap <= Horizon1h {
			c.within[Horizon1h]++
		}
	}
}

// lookup finds the most specific cell with enough observations, and says which it was.
func (h *History) lookup(user, model string, b Bucket) (*cell, string) {
	if h == nil {
		return nil, LevelNone
	}
	ks := keysFor(user, model, b)
	for i, k := range ks {
		if c := h.cells[k]; c != nil && len(c.gaps) >= minCell {
			return c, levels[i]
		}
	}
	// Nothing cleared the floor. The GLOBAL cell is still a better answer than none, and
	// returning it with its own n is what lets a caller decide; it is only reported at
	// LevelGlobal so nobody reads it as this user's own figure.
	if c := h.cells[ks[len(ks)-1]]; c != nil && len(c.gaps) > 0 {
		return c, LevelGlobal
	}
	return nil, LevelNone
}

// ReuseWithin implements Stats.
func (h *History) ReuseWithin(user, model string, b Bucket, d time.Duration) (float64, int, string) {
	c, level := h.lookup(user, model, b)
	if c == nil || len(c.gaps) == 0 {
		return 0, 0, LevelNone
	}
	n := len(c.gaps)
	// The two horizons are counted incrementally; anything else is an O(n) scan, which is
	// fine because a custom horizon is rare and correctness beats a cached wrong answer.
	if hits, ok := c.within[d]; ok {
		return float64(hits) / float64(n), n, level
	}
	hits := 0
	for _, g := range c.gaps {
		if g <= d {
			hits++
		}
	}
	return float64(hits) / float64(n), n, level
}

// MedianIdle implements Stats.
func (h *History) MedianIdle(user, model string, b Bucket) (time.Duration, int, string) {
	c, level := h.lookup(user, model, b)
	if c == nil || len(c.gaps) == 0 {
		return 0, 0, LevelNone
	}
	if !c.sorted {
		sort.Slice(c.gaps, func(i, j int) bool { return c.gaps[i] < c.gaps[j] })
		c.sorted = true
	}
	return c.gaps[(len(c.gaps)-1)/2], len(c.gaps), level
}

// ── the shipped strategies ─────────────────────────────────────────────────

// NoCache is the baseline: never write cache_control, so every prompt is billed as fresh
// input and nothing is ever held.
//
// It is the honest denominator for "what does caching earn", and it is also the arm that
// makes a bad strategy visible: a policy whose total exceeds this one is charging the
// caller for a cache that never pays for itself, and that has to be reportable rather than
// clamped away.
type NoCache struct{}

func (NoCache) Name() string              { return "no-cache" }
func (NoCache) Decide(Observation) Action { return ActionExpire }
func (NoCache) Describe() string {
	return "Never write cache_control. Every prompt is billed as fresh input; nothing is held."
}

// Fixed always chooses one tier, and never pings. The two rungs everybody compares against.
type Fixed struct{ TTL TTL }

// Fixed5m and Fixed1h are the two shipped fixed strategies.
func Fixed5m() Fixed { return Fixed{TTL: TTL5m} }
func Fixed1h() Fixed { return Fixed{TTL: TTL1h} }

func (f Fixed) Name() string { return "fixed-" + f.TTL.Label() }
func (f Fixed) Decide(Observation) Action {
	if f.TTL == TTL1h {
		return ActionWrite1h
	}
	return ActionWrite5m
}
func (f Fixed) Describe() string {
	if f.TTL == TTL1h {
		return "Always write the prefix at the 1-hour tier (2.0x input to create, 0.1x to read)."
	}
	return "Always write the prefix at the 5-minute tier (1.25x input to create, 0.1x to read)."
}

// Observed replays THE POLICY THAT ACTUALLY RAN, reconstructed from each row's own recorded
// tier.
//
// It is the arm that answers "is any of this better than what we already do", and it is the
// only strategy that reads a field of the request rather than deciding from the
// Observation — which is legitimate precisely because it is not predicting anything: the
// tier is a header the client already sent, i.e. a fact about the present request.
//
// Where the tier could not be reconstructed (TTLSourceUnknown, which is every row written
// before the cache_ttl column existed) it falls back to Fallback and the simulator counts
// those decisions, so the arm's coverage is reportable rather than assumed. The default
// fallback is the provider's own default tier, because a request that reached a
// prompt-caching provider with breakpoints in it got 5 minutes whether or not we recorded
// the header.
type Observed struct {
	// tiers is the reconstructed tier per request id, supplied by the caller from the rows.
	tiers map[int64]TTL
	// known is the ids whose tier was actually recorded rather than assumed.
	known map[int64]bool
	// Fallback is the tier used where nothing was recorded.
	Fallback TTL
}

// NewObserved builds the arm from the dataset. Reading the tiers from the rows here, once,
// keeps Observation free of a field only one strategy could use.
func NewObserved(reqs []*Request, fallback TTL) *Observed {
	o := &Observed{tiers: make(map[int64]TTL, len(reqs)), known: make(map[int64]bool, len(reqs)),
		Fallback: fallback}
	if !fallback.Valid() {
		o.Fallback = TTL5m
	}
	for _, r := range reqs {
		if r.TTL.Valid() && r.TTLSource != TTLSourceUnknown {
			o.tiers[r.ID] = r.TTL
			o.known[r.ID] = true
			continue
		}
		o.tiers[r.ID] = o.Fallback
	}
	return o
}

func (o *Observed) Name() string { return "observed-policy" }
func (o *Observed) Describe() string {
	return "Replay the tier each request actually asked for; where no tier was recorded, assume " +
		o.Fallback.Label() + " and count the row as uncovered."
}

// Decide reads the tier recorded on the request being served, by its Observation.RequestID.
func (o *Observed) Decide(ob Observation) Action {
	t, ok := o.tiers[ob.RequestID]
	if !ok {
		t = o.Fallback
	}
	if t == TTL1h {
		return ActionWrite1h
	}
	if t == TTL5m {
		return ActionWrite5m
	}
	return ActionExpire
}

// Covered reports how many of the arm's decisions rested on a recorded tier.
func (o *Observed) Covered(id int64) bool { return o != nil && o.known[id] }

// HistoricalProbability is the simple learned-from-your-own-history arm: choose the cheapest
// action whose horizon the account's own gaps say the conversation will probably come back
// inside.
//
// The rule, in the order it is evaluated:
//
//  1. If P(next request within 5 minutes) >= P5m, a plain 5-minute write is enough: the
//     entry survives on its own and a ping would be pure waste.
//  2. Otherwise, if P(within 1 hour) >= P1h, hold it for an hour. Whether that is a 1h WRITE
//     or a 5m write plus keep-alive pings is decided by which is cheaper on THIS prefix —
//     the arithmetic is in cheaperLongHold, and it is the whole reason a strategy needs the
//     rates rather than a token count.
//  3. Otherwise let it expire.
//
// Both thresholds are fields, and the fallback chain is Stats's: a user with too little
// history of their own is decided on their model's statistics, then on the service's, and
// the level that was used is recorded per decision so the results can say how much of the
// arm was actually personalised.
type HistoricalProbability struct {
	// P5m and P1h are the probability thresholds. Zero means the shipped default.
	P5m float64
	P1h float64
	// MinPrefix is the prefix below which nothing is cached at all: a small prefix cannot
	// repay a write, and on the production corpus the 10th percentile prefix is 0 tokens.
	MinPrefix int64
	// Semantics is needed for the cost comparison in step 2.
	Semantics Semantics
	// PingIdle and MaxPings define the keep-alive schedule the long hold would use.
	PingIdle time.Duration
	MaxPings int
}

// The shipped defaults for HistoricalProbability. 0.5 on both is the honest reading of a
// probability threshold: act when the outcome is more likely than not. MinPrefix follows the
// keep-alive tab's own shipped gate.
const (
	DefaultP5m       = 0.5
	DefaultP1h       = 0.5
	DefaultMinPrefix = 20000
	// DefaultPingIdle and DefaultMaxPings are the shipped keep-alive schedule for a
	// FIVE-MINUTE entry: a ping every 280 s, at most 2 per idle span. 280 rather than 300 so
	// the refresh lands before the lifetime, not on it.
	DefaultPingIdle = 280 * time.Second
	DefaultMaxPings = 2
	// DefaultPingIdle1h is the same margin against the one-hour lifetime: 56 minutes rather
	// than 60. A one-hour entry therefore needs one twelfth as many refreshes to be held for
	// the same wall-clock span, which is the ping-count half of the 5m-vs-1h trade.
	DefaultPingIdle1h = 3360 * time.Second
)

func (h HistoricalProbability) withDefaults() HistoricalProbability {
	if h.P5m <= 0 {
		h.P5m = DefaultP5m
	}
	if h.P1h <= 0 {
		h.P1h = DefaultP1h
	}
	if h.MinPrefix < 0 {
		h.MinPrefix = 0
	}
	if h.MinPrefix == 0 {
		h.MinPrefix = DefaultMinPrefix
	}
	if h.PingIdle <= 0 {
		h.PingIdle = DefaultPingIdle
	}
	if h.MaxPings <= 0 {
		h.MaxPings = DefaultMaxPings
	}
	if h.Semantics == (Semantics{}) {
		h.Semantics = DefaultSemantics()
	}
	return h
}

func (h HistoricalProbability) Name() string { return "historical-probability" }
func (h HistoricalProbability) Describe() string {
	d := h.withDefaults()
	return fmt.Sprintf("Use your own closed gaps: 5m write when P(return within 5m) >= %.0f%%, "+
		"a 1-hour hold when P(return within 1h) >= %.0f%% (write or pings, whichever is cheaper "+
		"on that prefix), otherwise let it expire. Prefixes under %s tokens are never cached.",
		d.P5m*100, d.P1h*100, humanTokens(d.MinPrefix))
}

func (h HistoricalProbability) Decide(o Observation) Action {
	d := h.withDefaults()
	if o.CachedTokens < d.MinPrefix {
		return ActionExpire
	}
	if o.Stats == nil {
		// No history at all: the provider's own default is the least-surprising action, and
		// it is what the traffic would have got with no strategy in play.
		return ActionWrite5m
	}
	if p, n, _ := o.Stats.ReuseWithin(o.User, o.Model, o.Bucket, Horizon5m); n > 0 && p >= d.P5m {
		return ActionWrite5m
	}
	if p, n, _ := o.Stats.ReuseWithin(o.User, o.Model, o.Bucket, Horizon1h); n > 0 && p >= d.P1h {
		return d.longHold(o)
	}
	return ActionExpire
}

// longHold picks the cheaper of the two ways to hold an entry past five minutes.
//
//	write_1h = prefix × write_1h_rate                      (2.0x input, paid once, no pings)
//	ping_5m  = prefix × write_5m_rate + K × keep_alive_cost (1.25x input plus K refreshes at 0.1x)
//
// Which wins is a property of the RATES and of K, not a constant: at the production corpus's
// median prefix of 124,845 tokens the two are within a few tenths of a cent of each other,
// and the answer flips with the ping budget. That is exactly why it is computed here rather
// than decided in advance, and why a strategy needs the price list and not just a token count.
func (h HistoricalProbability) longHold(o Observation) Action {
	if !o.Pricing.Known {
		// Unpriced: the comparison cannot be made, so choose the action that cannot be a
		// surprise bill. A 1h write is a known one-off multiple of input; an unbounded ping
		// schedule at an unknown rate is not.
		return ActionWrite1h
	}
	write1h := float64(o.CachedTokens) * o.Pricing.Write1h
	pingHold := float64(o.CachedTokens)*o.Pricing.Write5m +
		float64(h.MaxPings)*o.Pricing.KeepAliveCost(o.CachedTokens, h.Semantics)
	if pingHold < write1h {
		return ActionPing5m
	}
	return ActionWrite1h
}

// Custom is the configurable arm: thresholds, or a Predictor, or both.
//
// It exists so a new idea can be evaluated against the same baseline without a new type and
// without touching the simulator. With a Predictor set it asks that; with none it falls back
// to the same historical thresholds HistoricalProbability uses, so a partially-configured
// Custom is still a working strategy rather than a no-op.
type Custom struct {
	// Label is the name the dashboard groups by. Defaults to "custom".
	Label string
	// Predictor, when set, answers the two horizon questions instead of Stats.
	Predictor Predictor
	// Decider, when set, replaces the whole rule. The last resort seam: a policy that is not
	// expressible as two thresholds plugs in here and still gets scored identically.
	Decider func(Observation) Action
	// Thresholds and gates, as HistoricalProbability's.
	P5m       float64
	P1h       float64
	MinPrefix int64
	Semantics Semantics
	PingIdle  time.Duration
	MaxPings  int
	// AlwaysPing forces the long hold to use pings rather than the cheaper of the two, for
	// an operator who wants to measure the ping path specifically.
	AlwaysPing bool
}

func (c Custom) Name() string {
	if c.Label != "" {
		return c.Label
	}
	return "custom"
}

func (c Custom) Describe() string {
	switch {
	case c.Decider != nil:
		return "A caller-supplied decision function, scored against the same baseline as every " +
			"other arm."
	case c.Predictor != nil:
		p5, p1 := c.thresholds()
		return fmt.Sprintf("A supplied predictor's reuse probability against thresholds "+
			"%.0f%% (5m) and %.0f%% (1h).", p5, p1)
	}
	p5, p1 := c.thresholds()
	return fmt.Sprintf("Configured thresholds over your own closed gaps: %.0f%% (5m), %.0f%% (1h).",
		p5, p1)
}

func (c Custom) thresholds() (float64, float64) {
	p5, p1 := c.P5m, c.P1h
	if p5 <= 0 {
		p5 = DefaultP5m
	}
	if p1 <= 0 {
		p1 = DefaultP1h
	}
	return p5 * 100, p1 * 100
}

func (c Custom) Decide(o Observation) Action {
	if c.Decider != nil {
		return c.Decider(o)
	}
	base := HistoricalProbability{P5m: c.P5m, P1h: c.P1h, MinPrefix: c.MinPrefix,
		Semantics: c.Semantics, PingIdle: c.PingIdle, MaxPings: c.MaxPings}.withDefaults()
	if o.CachedTokens < base.MinPrefix {
		return ActionExpire
	}
	if c.Predictor == nil {
		if c.AlwaysPing {
			// Same rule, but the long hold is forced onto the ping path.
			if p, n, _ := o.Stats.ReuseWithin(o.User, o.Model, o.Bucket, Horizon5m); n > 0 && p >= base.P5m {
				return ActionWrite5m
			}
			if o.Stats != nil {
				if p, n, _ := o.Stats.ReuseWithin(o.User, o.Model, o.Bucket, Horizon1h); n > 0 && p >= base.P1h {
					return ActionPing5m
				}
			}
			return ActionExpire
		}
		return base.Decide(o)
	}
	if p, ok := c.Predictor.ReuseProbability(o, Horizon5m); ok && p >= base.P5m {
		return ActionWrite5m
	}
	if p, ok := c.Predictor.ReuseProbability(o, Horizon1h); ok && p >= base.P1h {
		if c.AlwaysPing {
			return ActionPing5m
		}
		return base.longHold(o)
	}
	return ActionExpire
}

// humanTokens renders a token count the way the dashboard does, for a description string.
func humanTokens(n int64) string {
	if n >= 1000 {
		v := float64(n) / 1000
		if v == math.Trunc(v) {
			return fmt.Sprintf("%.0fk", v)
		}
		return fmt.Sprintf("%.1fk", v)
	}
	return fmt.Sprintf("%d", n)
}
