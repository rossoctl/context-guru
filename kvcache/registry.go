package kvcache

import (
	"fmt"
	"sort"
	"strings"
)

// The strategy REGISTRY, the explicit-action seam, and the exact cost ceiling.
//
// # Why a registry lives here and not in the API layer
//
// A dashboard route that maps "fixed-5m" to a Strategy is a second list of the arms that
// exist, and it drifts: an arm added here would be invisible there, and an arm renamed here
// would 404 there. Worse, the offline ML evaluator (deploy/harbor/kv_ttl_cost_model.py) has
// its own arm table, so a name that means one thing on the page and another in the
// evaluation is a comparison nobody can trust. One registry, in the domain package, is what
// the page, the API and the port all read.

// StrategySpec is one arm, as the API and the page should present it.
type StrategySpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Unreachable marks an arm that reads the FUTURE. It is a bound on what any policy could
	// achieve, never a result, and every surface that shows it must say so — a ceiling
	// rendered beside real arms in the same style is a promise the product cannot keep.
	Unreachable bool `json:"unreachable"`
	// NeedsDataset marks an arm that cannot be built from a name alone because it is
	// constructed from the whole trajectory set (it precomputes a per-request answer).
	NeedsDataset bool `json:"needs_dataset"`
	// Baseline marks the arm that is the honest default denominator for a savings figure.
	Baseline bool `json:"baseline,omitempty"`
}

// The canonical arm names. Strings, because they are a wire contract: they appear in a query
// parameter, in a JSON key, in the offline evaluator's own table and on the page.
const (
	StrategyNoCache     = "no-cache"
	StrategyFixed5m     = "fixed-5m"
	StrategyFixed1h     = "fixed-1h"
	StrategyKeepAlive5m = "keepalive-5m"
	StrategyKeepAlive1h = "keepalive-1h"
	// StrategyExtend1h writes at the cheap tier and extends to an hour only when a
	// conversation actually goes quiet.
	StrategyExtend1h   = "keepalive-5m-to-1h"
	StrategyObserved   = "observed-policy"
	StrategyHistorical = "historical-probability"
	StrategyOptimal    = "optimal"
	StrategyReplay     = "replay"
)

// registry is the ordered arm list. The ORDER is the presentation order and it is
// deliberate: the two baselines first, then the fixed tiers, then the keep-alive arms, then
// the ones that decide, and the ceiling LAST — so a reader meets the reachable arms before
// the one that cannot be reached.
var registry = []StrategySpec{
	{Name: StrategyNoCache, Description: NoCache{}.Describe()},
	{Name: StrategyFixed5m, Description: Fixed5m().Describe(), Baseline: true},
	{Name: StrategyFixed1h, Description: Fixed1h().Describe()},
	{Name: StrategyKeepAlive5m, Description: KeepAlive5m().Describe()},
	{Name: StrategyKeepAlive1h, Description: KeepAlive1h().Describe()},
	{Name: StrategyExtend1h, Description: Extend1h{}.Describe()},
	{Name: StrategyObserved, Description: "Replay the tier each request actually asked for; " +
		"where no tier was recorded, assume the provider's default and count the row as " +
		"uncovered.", NeedsDataset: true},
	{Name: StrategyHistorical, Description: HistoricalProbability{}.Describe()},
	{Name: StrategyReplay, Description: "Replay an explicit action supplied per request. The " +
		"seam a policy decided elsewhere — an offline predictor, a hand-written experiment — " +
		"is scored through.", NeedsDataset: true},
	{Name: StrategyOptimal, Description: "The cheapest action sequence that exists, computed " +
		"exactly. Reads the true next-request time.", Unreachable: true, NeedsDataset: true},
}

// Registry lists every arm, in presentation order. A copy, so a caller cannot reorder it.
func Registry() []StrategySpec { return append([]StrategySpec(nil), registry...) }

// StrategyNames is the arm names in the same order.
func StrategyNames() []string {
	out := make([]string, 0, len(registry))
	for _, s := range registry {
		out = append(out, s.Name)
	}
	return out
}

// NewStrategy builds one arm by name.
//
// reqs is required by the arms whose spec says NeedsDataset (they precompute a per-request
// answer over the whole set) and ignored by the rest. `replay` cannot be built here — it
// needs the action list itself — so it is an error, with the constructor named.
func NewStrategy(name string, reqs []*Request, cfg Config) (Strategy, error) {
	switch name {
	case StrategyNoCache:
		return NoCache{}, nil
	case StrategyFixed5m:
		return Fixed5m(), nil
	case StrategyFixed1h:
		return Fixed1h(), nil
	case StrategyKeepAlive5m:
		return KeepAlive5m(), nil
	case StrategyKeepAlive1h:
		return KeepAlive1h(), nil
	case StrategyExtend1h:
		return Extend1h{}, nil
	case StrategyObserved:
		return NewObserved(reqs, TTL5m), nil
	case StrategyHistorical:
		return HistoricalProbability{Semantics: cfg.Semantics, PingIdle: cfg.PingIdle,
			MaxPings: cfg.MaxPings}, nil
	case StrategyOptimal:
		return NewOptimal(reqs, cfg), nil
	case StrategyReplay:
		return nil, fmt.Errorf("kvcache: %q carries its own action list; build it with "+
			"NewReplay", name)
	}
	return nil, fmt.Errorf("kvcache: no strategy %q; have %s", name,
		strings.Join(StrategyNames(), ", "))
}

// ── the keep-alive arms ────────────────────────────────────────────────────

// KeepAlive is a fixed arm that holds every prefix at one tier AND refreshes it with
// keep-alives while the conversation is idle.
//
// Separate from Fixed because the difference is not the tier: it is whether anything is
// spent to hold the entry past its own lifetime. On the production corpus this is the only
// hand-written arm that beats the shipped 5-minute policy, and it does so without consulting
// any statistics at all — which is the measurement any learned arm has to be compared with.
type KeepAlive struct{ TTL TTL }

// KeepAlive5m and KeepAlive1h are the two shipped keep-alive arms.
func KeepAlive5m() KeepAlive { return KeepAlive{TTL: TTL5m} }
func KeepAlive1h() KeepAlive { return KeepAlive{TTL: TTL1h} }

func (k KeepAlive) Name() string { return "keepalive-" + k.TTL.Label() }
func (k KeepAlive) Decide(Observation) Action {
	if k.TTL == TTL1h {
		return ActionPing1h
	}
	return ActionPing5m
}
func (k KeepAlive) Describe() string {
	if k.TTL == TTL1h {
		return "Hold every prefix at the 1-hour tier and refresh it with keep-alives while " +
			"idle. Costs 2.0x input to create, and needs one twelfth as many refreshes as the " +
			"5-minute arm to hold the same span."
	}
	return "Hold every prefix at the 5-minute tier and refresh it with keep-alives while " +
		"idle. The cheapest tier to create, refreshed at 0.1x."
}

// Extend1h writes every prefix at the CHEAP five-minute tier and buys the long hold only if
// the conversation actually goes quiet.
//
// It is the arm the other two keep-alive arms bracket, and on traffic like this deployment's
// it is the one worth having: 92.5% of gaps close inside five minutes, so KeepAlive1h pays the
// 2.0x creation premium on nearly every request for a hold nearly none of them needs, while
// this pays 1.25x on every request and 2.0x only on the rare span that outlives five minutes.
// See ActionWrite5mPing1h for what the upgrading keep-alive costs and why it is priced as a
// write.
type Extend1h struct{}

func (Extend1h) Name() string              { return StrategyExtend1h }
func (Extend1h) Decide(Observation) Action { return ActionWrite5mPing1h }
func (Extend1h) Describe() string {
	return "Write every prefix at the 5-minute tier (1.25x input), and if a keep-alive comes " +
		"due before it lapses, extend the context by an hour with a 1-hour write (2.0x) — so " +
		"the long-hold premium is paid only on the conversations that actually go quiet."
}

// ── the explicit-action seam ───────────────────────────────────────────────

// Replay scores an action list decided somewhere else.
//
// This is how a policy that this package cannot run gets compared on equal terms: an offline
// learned predictor (deploy/harbor/kv_ttl_survival_predictor.py drives one), a hand-written
// experiment, or a recorded production decision. It receives the same replay, the same
// rates, the same baseline and the same savings arithmetic as every built-in arm, which is
// the whole point — an evaluation whose scorer differs from the product's is not an
// evaluation of the product.
type Replay struct {
	// Label is the name results are grouped under. Defaults to "replay".
	Label string
	// Fallback is used for a request the list does not mention. ActionExpire by default,
	// because it is the arm that cannot flatter anything.
	Fallback Action
	actions  map[int64]Action
	missing  int64
}

// NewReplay builds the arm from a per-request-id action list.
func NewReplay(label string, actions map[int64]Action, fallback Action) *Replay {
	r := &Replay{Label: label, Fallback: fallback, actions: map[int64]Action{}}
	if r.Label == "" {
		r.Label = StrategyReplay
	}
	if r.Fallback == "" {
		r.Fallback = ActionExpire
	}
	for id, a := range actions {
		r.actions[id] = a
	}
	return r
}

func (r *Replay) Name() string { return r.Label }
func (r *Replay) Describe() string {
	return fmt.Sprintf("An action supplied per request (%d of them), scored against the same "+
		"baseline and the same rates as every built-in arm.", len(r.actions))
}

// Decide reads the supplied action for the request being served.
func (r *Replay) Decide(o Observation) Action {
	if a, ok := r.actions[o.RequestID]; ok {
		return a
	}
	r.missing++
	return r.Fallback
}

// Unanswered is how many requests the list did not mention. Reported rather than silently
// defaulted: an action list that covers half the window is not a policy, and a total that
// hid the fact would be the flattering half of one.
func (r *Replay) Unanswered() int64 { return r.missing }

// ── the exact ceiling ──────────────────────────────────────────────────────

// Optimal is the cheapest action sequence that exists, and it is a BOUND, not a policy.
//
// # Why a dynamic program and not a greedy rule
//
// The action chosen at turn t decides two things at once: whether turn t itself may READ
// from cache (ActionExpire writes no cache_control, so the whole prefix is billed as fresh
// input however warm the entry was) and whether turn t+1 hits. A rule that looks only at the
// gap ahead therefore gets the current turn wrong. The first version of this in the offline
// evaluator was exactly that greedy rule, and it scored BELOW a plain keep-alive — which is
// impossible for a ceiling, and is how the error was caught.
//
// The exact optimum is cheap because the state is small. Everything about the entry entering
// turn t — its size, its tier, its deadline, the keep-alives that fired during the span — is
// a function of the PREVIOUS action alone, given the two rows' timestamps. So the cost
// decomposes as a sum of f(action[t-1], action[t]) along the chain and one Viterbi pass per
// trajectory settles it: len(Actions) states, len(Actions) transitions each, so
// O(len(Actions)^2 * n) — six actions today, and the bookkeeping is linear (see back[] below).
//
// Every surface that shows this must label it unreachable (see StrategySpec.Unreachable).
type Optimal struct{ chosen map[int64]Action }

// NewOptimal solves each trajectory. Deterministic: the action order is the `Actions` slice,
// never a map range, so the same dataset yields the same plan twice.
func NewOptimal(reqs []*Request, cfg Config) *Optimal {
	cfg = cfg.withDefaults(reqs)
	byConv := map[Conversation][]*Request{}
	for _, r := range reqs {
		byConv[r.Key()] = append(byConv[r.Key()], r)
	}
	keys := make([]Conversation, 0, len(byConv))
	for k := range byConv {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].User != keys[j].User {
			return keys[i].User < keys[j].User
		}
		if keys[i].Conversation != keys[j].Conversation {
			return keys[i].Conversation < keys[j].Conversation
		}
		return keys[i].Model < keys[j].Model
	})

	out := &Optimal{chosen: make(map[int64]Action, len(reqs))}
	for _, k := range keys {
		group := byConv[k]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].TS != group[j].TS {
				return group[i].TS < group[j].TS
			}
			return group[i].ID < group[j].ID
		})
		// cost[a] is the best total for a prefix of the trajectory whose LAST action is
		// Actions[a]. The plan itself is NOT carried forward beside it: back[i][a] records which
		// previous action that best came from, and the plan is recovered by walking those
		// pointers once at the end.
		//
		// That is the difference between the O(len(Actions)^2 * n) this claims and the O(n^2) an
		// earlier version actually did. It kept a []Action per state and copied the whole winning
		// prefix into each of them every turn, so the recurrence was linear while the bookkeeping
		// around it was quadratic in both time and allocation — measured at 8.4 ms for a
		// 500-request trajectory rising to 94 SECONDS at 64k, a clean 4x per doubling. Trajectory
		// length is not bounded by anything we control: a conversation is keyed on a
		// CLIENT-SUPPLIED session id, so one account reusing one id makes one group arbitrarily
		// long. A comment claiming linear bookkeeping over quadratic code is how that becomes a
		// production incident rather than a benchmark.
		cost := make([]float64, len(Actions))
		back := make([][]int, len(group))
		for i, r := range group {
			nextCost := make([]float64, len(Actions))
			back[i] = make([]int, len(Actions))
			for bi, b := range Actions {
				if i == 0 {
					nextCost[bi] = stepCost(nil, ActionExpire, r, b, cfg)
					back[i][bi] = -1
					continue
				}
				bestCost, bestAt := 0.0, -1
				for ai, a := range Actions {
					c := cost[ai] + stepCost(group[i-1], a, r, b, cfg)
					if bestAt < 0 || c < bestCost {
						bestCost, bestAt = c, ai
					}
				}
				nextCost[bi] = bestCost
				back[i][bi] = bestAt
			}
			cost = nextCost
		}
		// The OPEN span after the last turn: an action that pings keeps spending, bounded by
		// the window's end. Without charging for it the optimum would happily choose a
		// keep-alive it never settles up on.
		last := group[len(group)-1]
		bestCost, bestAt := 0.0, -1
		for ai, a := range Actions {
			c := cost[ai] + openSpanCost(last, a, cfg)
			if bestAt < 0 || c < bestCost {
				bestCost, bestAt = c, ai
			}
		}
		// Walk the back-pointers from the last turn to the first, then assign. One pass, one
		// int per (turn, state), no prefix ever copied.
		at := bestAt
		for i := len(group) - 1; i >= 0; i-- {
			out.chosen[group[i].ID] = Actions[at]
			if prev := back[i][at]; prev >= 0 {
				at = prev
			}
		}
	}
	return out
}

func (o *Optimal) Name() string { return StrategyOptimal }
func (o *Optimal) Describe() string {
	return "The cheapest action sequence that exists, solved exactly per trajectory. It reads " +
		"the true next-request time, so it is the CEILING no policy can reach — never a result."
}
func (o *Optimal) Decide(ob Observation) Action { return o.chosen[ob.RequestID] }

// entryAfter is the (tokens, tier, expires) a turn leaves behind. A hit refreshes and a
// write starts, and both land on the same expression, which is why this needs no hit flag.
func entryAfter(row *Request, action Action) (int64, TTL, int64) {
	tier := action.Tier()
	if tier == TTLNone {
		return 0, TTLNone, 0
	}
	return row.CachedContext, tier, row.TS + int64(tier.Lifetime()/timeMillisecond)
}

// stepCost is the DP's transition weight: what the pair (prevAction, action) adds — the
// span's keep-alives plus this request's own bill. It reads the SAME cost functions and the
// same ping core Simulate does, so the ceiling cannot be computed against different rules
// than the arms it bounds.
func stepCost(prev *Request, prevAction Action, row *Request, action Action, cfg Config) float64 {
	var tokens, expires int64
	tier := TTLNone
	var pingCost float64
	if prev != nil {
		tokens, tier, expires = entryAfter(prev, prevAction)
		span := pingSpan(tokens, tier, expires, prev.TS, prevAction, row.TS,
			cfg.Prices.For(prev.Model), cfg.Semantics, cfg)
		pingCost, expires = span.cost, span.expires
	}
	alive := tokens > 0 && row.TS < expires
	hit := alive && row.MissReason != "prefix_change" && row.MissReason != "cold_start"
	var reusable int64
	if hit {
		reusable = tokens
		if reusable > row.CachedContext {
			reusable = row.CachedContext
		}
	}
	t := action.Tier()
	fresh, read, write := row.InputTokens, reusable, int64(0)
	if t == TTLNone {
		fresh += row.CachedContext
		read = 0
	} else {
		write = row.CachedContext - reusable
		if write < 0 {
			write = 0
		}
	}
	price := cfg.Prices.For(row.Model)
	if !price.Known {
		return pingCost
	}
	return pingCost + price.RequestCost(fresh, read, write, row.OutputTokens, t)
}

// openSpanCost is what a pinging action keeps spending after a trajectory's last request.
func openSpanCost(last *Request, action Action, cfg Config) float64 {
	tokens, tier, expires := entryAfter(last, action)
	return pingSpan(tokens, tier, expires, last.TS, action, cfg.WindowEnd,
		cfg.Prices.For(last.Model), cfg.Semantics, cfg).cost
}

// timeMillisecond is time.Millisecond as an int64, so the conversions above read as
// arithmetic rather than as a cast chain.
const timeMillisecond = 1e6 // nanoseconds per millisecond
