# Choose a cache TTL, and know what it is worth

The provider sells two prompt-cache lifetimes. A five-minute entry costs `1.25x` base input
to create; a one-hour entry costs `2.0x`. Both are read back at `0.1x`. Choosing between
them — per request, per conversation, per account — is a policy, and until now this repo had
no way to price one.

This page is that way. It covers the cost model, the strategies, the offline predictor, and
what the whole thing measured on this service's own traffic. The short version:

- **Prompt caching itself is already earning 77.4%** of the uncached bill, and that is banked.
- **The reachable TTL headroom on top of it is 7.44%**, taken by a blind keep-alive with no model
  at all. The exact ceiling is 21.95%, and reaching it requires knowing the future.
- **93% of that ceiling is 526 decisions out of 14,407** — 3.7% of requests. This is a rare-event
  problem, which is a very different thing from a prediction problem, and it is the finding that
  should shape any further work.
- **Hit rate is not the objective.** The arm with the second-best hit rate on the page is the most
  expensive reachable arm, by 41%.

## What the domain package is

`kvcache` is a plain Go package: no SQL, no HTTP, no DOM. The dashboard's read layer hands it
rows and it does the arithmetic.

| type | what it is |
|---|---|
| `Request` | one historical request, plus its derived successor and idle time |
| `Derive()` | fills `NextTS` / `IdleMs` per conversation, chronologically |
| `Pricing`, `PriceList`, `Override` | per-model rates, and the hand edits an operator can lay over them |
| `Semantics` | the provider's cache behaviour, made explicit |
| `Action`, `Strategy`, `Observation` | a TTL decision, and everything it is allowed to see |
| `Predictor` | the seam a learned model plugs into |
| `Stats`, `History` | the leak-free statistics accumulator |
| `Simulate()`, `Result`, `Compare()` | the replay, its bill, and the savings |
| `Registry()`, `NewStrategy()` | the one list of arms the page, the API and the offline evaluator all read |

### Units, stated once

Every timestamp is **epoch milliseconds, UTC**. Every price is **USD per token** (the
operator's price list is per million and is converted on load). There is no per-user timezone
anywhere in the store, so "time of day" is UTC and every label that carries it says so —
inventing a local timezone from a tenant id would be a fabricated measurement.

### A conversation is a pair

`Conversation` is `(tenant_id, session_id)`, never the session alone. A session id is
client-supplied, so two accounts can present the same one; keyed on it alone the dataset
splices their requests into one interleaved trajectory and derives idle gaps across the join.
That is both a wrong measurement and a cross-account read.

### The last request of a conversation has no idle time

`Request.IdleMs` is a **pointer**, and it is `nil` there. Not `0` — zero reads as "it came
back instantly", which is the opposite of what is known. On the production window 1,772 of
14,407 requests are in that state, 12.3%, and all of them are at the long end. Any model
trained on this data that silently treats them as instant returns is biased toward short gaps.

## The cost formulas

Per request, under the tier the action chose:

```
uncached_input = input_tokens    x input_rate
cache_read     = read_tokens     x cache_read_rate
cache_write    = written_tokens  x write_rate(tier)   # 5m: 1.25x in, 1h: 2.0x in
output         = output_tokens   x output_rate
request_cost   = uncached_input + cache_read + cache_write + output
```

`read_tokens` is `min(entry held, this request's prefix)` when the entry is still alive, else
0; `written_tokens` is the rest of the prefix. With no `cache_control` at all the whole prefix
is billed as **fresh input** instead — which is what makes "let it expire" a real arm rather
than an absence, and it is the reason expiring is *not* free: a request that would have read
from cache pays `1.0x` on its whole prefix instead of `0.1x`.

Per keep-alive:

```
keep_alive = cached x cache_read_rate + ping_input x input_rate + ping_output x output_rate
recreate   = cached x write_rate(tier) + ping_input x input_rate + ping_output x output_rate
```

A keep-alive is a cache **read**, and it costs the read rate whether the entry is held at five
minutes or at an hour. **The difference between a 5m and a 1h keep-alive is not the price of
one ping.** It is the creation tier that put the entry there, and how *often* a ping is needed
— twelve times as often at five minutes. Charging two different per-ping rates would be
inventing a price no provider publishes.

A keep-alive that arrives *after* the entry lapsed is not a refresh at all: it **re-creates**
the prefix at the write rate, `12.5x` a read at the five-minute tier and `20x` at the hourly
one. `Result.PingsThatRewrote` counts those, because a schedule whose interval exceeds the
lifetime it is protecting is paying to fix a problem it caused.

`ping_output` is 1 token because Anthropic's Messages API requires `max_tokens >= 1`. A
provider that accepts a zero-generation request sets `Semantics.ZeroGeneration` and the
assumption disappears from the bill instead of being rounded away.

### The one rate nobody publishes

No gateway publishes a **one-hour** cache-creation rate. It is derived from the documented
multiplier against base input (`2.0x`, against `1.25x` for five minutes), the multiplier is a
field rather than a literal, and it is never allowed below the five-minute rate — a price list
that implied otherwise would be a typo, and honouring it would make every 1h arm look free.

### Total, premium, and savings

```
total_usd         = sum(request_cost) + sum(ping_cost)
uncached_usd      = the same traffic with no prompt cache at all
cache_premium_usd = total_usd - uncached_usd     # NEGATIVE means the cache paid for itself

absolute_savings   = baseline_cost - strategy_cost
percentage_savings = absolute_savings / baseline_cost x 100
```

`total_usd` and `cache_premium_usd` are different numbers and must never be shown as if they
were the same one. **Savings are not clamped**: an arm that costs more than its baseline
reports a negative saving, because that is the only way a comparison stays one.

## Cache semantics, made explicit

Anthropic's documented behaviour is the default, and each part is a field so a provider that
differs can say so rather than being silently mispriced:

> By default, the cache has a 5-minute lifetime. **The cache is refreshed for no additional
> cost each time the cached content is used.**

- a cache **hit** refreshes the entry for its tier's full lifetime, free
  (`Semantics.HitRefreshesTTL`)
- a keep-alive read does the same (`Semantics.PingRefreshesTTL`)
- a refresh does **not upgrade** a tier: a five-minute entry refreshed is good for another
  five minutes, not an hour
- a recorded `cache_miss_reason` of `prefix_change` or `cold_start` **forces a miss** whatever
  the policy chose. No TTL rescues content that moved, and none conjures an entry that never
  existed. Those rows are the ceiling on every arm, and `Result.ForcedMisses` reports them —
  a simulator that let a 1-hour TTL "rescue" a prefix change would promise savings no
  configuration can reach.

## The arms

Render them from `kvcache.Registry()` rather than hardcoding a list; an unknown name is an
error from `NewStrategy`, never a silent default.

| arm | what it does |
|---|---|
| `no-cache` | never writes `cache_control`; the honest denominator |
| `fixed-5m` | always the five-minute tier |
| `fixed-1h` | always the hourly tier |
| `keepalive-5m` | five-minute tier, refreshed with keep-alives while idle |
| `keepalive-1h` | hourly tier, refreshed with keep-alives while idle |
| `keepalive-5m-to-1h` | writes cheap, and **extends to an hour only if a keep-alive comes due** |
| `observed-policy` | replays the tier each request actually asked for |
| `historical-probability` | the account's own closed gaps against two thresholds |
| `replay` | an action list decided elsewhere — the seam an offline model is scored through |
| `optimal` | the cheapest sequence that exists. **Unreachable: it reads the future** |

`keepalive-5m-to-1h` is the arm worth understanding. `keepalive-1h` pays the `2.0x` creation
premium on *every* request; this pays `1.25x` on every request and `2.0x` only on the rare
span that outlives five minutes. `Result.PingsThatUpgraded` counts those deliberate upgrades,
and it is deliberately **not** the same counter as `PingsThatRewrote`: one is a policy buying
a hold it chose, the other is a schedule repairing damage it caused, and one number for both
would make a working arm and a broken one look identical.

### `optimal` is a bound, not a result

It is solved exactly, per conversation, by a Viterbi pass over the six actions — not by a
greedy per-row rule, and the distinction is not academic. The action at turn *t* decides two
things at once: whether *t* itself may read from cache, and whether *t+1* hits. A rule that
looks only at the gap ahead gets the current turn wrong. The first implementation here *was*
that greedy rule, and it scored **below a plain keep-alive** — impossible for a ceiling, and
how the error was caught. `TestOptimalIsALowerBoundOnEveryOtherArm` replays every arm in the
registry and fails if any comes out cheaper.

Every surface that shows `optimal` must label it unreachable (`StrategySpec.Unreachable`).

## No future leakage

A `Strategy` sees an `Observation`, and an `Observation` is defined by what is **absent** from
it: no next-request timestamp, no idle duration, no field derived from either. The historical
statistics attached to it come from `History`, which is **empty** at the start of a replay and
gains one observation per gap *as that gap closes* — when the successor arrives, not when the
predecessor is decided. So a decision at time *T* can only see gaps that ended at or before
*T*.

The real next-request time is used exactly once: to **score** the decision after it is made.

`TestStrategiesCannotSeeTheFuture` walks the `Observation`'s numeric fields by reflection and
fails if any carries a fact about a request that had not happened yet. That check exists
because the leak is invisible on screen — a predictor that can see the gap it is predicting
"predicts" it perfectly, and every saving it reports is unreachable in production while the
page looks completely fine.

## The offline tools

Two Python files in `deploy/harbor/`, because the predictor needs the scientific stack and an
evaluation loop that shelled into Go for every candidate threshold would not get run.

### `kv_ttl_cost_model.py` — the scorer

Give it a trajectory and one action per turn and it returns the decomposed bill.
`PriceBook.from_operator_file()` reads the real price list and resolves rates **per row from
the trajectory's own `model` field**.

It is a **faithful port** of `kvcache.Simulate`, not a second opinion. Two implementations of
one money question is exactly the drift this project has been bitten by, so
`kv_ttl_cost_drift_test.go` beside it replays the same trajectory through both and compares
**22 fields**. It runs on the system `python3` — the scientific stack is imported lazily by
the predictor bridge and the price-list reader, neither of which that path touches — so it is
an always-on guard rather than a skipped one. **If the two disagree, Go is right and the port
is the bug.**

Four guards:

| guard | what it pins |
|---|---|
| `TestPythonCostModelAgreesWithTheShippedSimulator` | 22 fields on a fixture reaching every shared branch |
| `TestTheUpgradingKeepAliveAgrees` | the one keep-alive that is a write *on purpose* |
| `TestTheTwoExactCeilingsAgree` | the two independently written dynamic programs |
| `TestPingScheduleMatchesThePort` | the ping cadence over a 160-case table |

### `kv_ttl_survival_predictor.py` — the predictor

A discrete-time logistic-hazard (person-period) survival model over the time until the next
cache-compatible request. It returns a **distribution** and deliberately does not choose a
TTL: the probabilities are consumed by a separate cost policy, which is the only place the
rates live. A model that picked a tier would bury the pricing assumption inside the fit.

With the default bucketing the two columns a TTL decision needs are read straight off:

```
proba = predictor.predict_proba(rows)
p_5m  = proba[:, 0]        # P(return inside the five-minute lifetime)
p_1h  = 1.0 - proba[:, -1] # P(return inside the hourly lifetime)
```

Those are exactly the two questions `kvcache.Predictor.ReuseProbability` is asked, so a
service that wants to use this wires it in behind that interface and nothing in the simulator
or the dashboard changes.

Its own docstring carries the full column contract, the extraction SQL, and seven numbered
traps. The two that bite hardest:

1. **`observation_end` is not optional.** Without it the last request of every compatibility
   group gets a `NaN` duration and `fit` silently drops it — 12.3% of rows, all long-idle.
2. **`compatibility_columns` must include the model.** A cache entry does not transfer between
   models, and 101 of this deployment's 1,772 trajectories use more than one.

### Running them

```sh
python3 -m venv .venv-kvpred
.venv-kvpred/bin/pip install "numpy<2" pandas scikit-learn joblib pyyaml

.venv-kvpred/bin/python deploy/harbor/kv_ttl_cost_model.py \
    --db /var/lib/context-guru/cg.db \
    --prices /etc/context-guru/prices.yaml
```

Read the store **read-only**. The predictor is fitted on the first 70% of the window and its
thresholds tuned there; every arm is then scored on the held-out remainder, so no arm is
reported on rows its parameters were chosen from.

## What it measured

The hosted deployment's own capture: 14,407 requests, 12 accounts, **1,896 trajectories**,
2026-08-17 11:48 → 08-19 20:38 UTC, at `/etc/context-guru/prices.yaml` — which matters, because
this gateway bills `aws/claude-sonnet-5` at `$1.52/MTok` where anthropic.com bills `$3.00`, so
the public list price would overstate every figure below.

A trajectory is `(account, session, MODEL)`, which is why there are 1,896 of them and not 1,772:
a cache entry does not transfer between models, so a session that switches model is two
trajectories. Everything in this section is the whole window, one population throughout, at the
default keep-alive schedule (a refresh every 280 s for a 5-minute entry, every 3,360 s for an
hourly one, **at most 2 per idle span**). That schedule is a parameter, not a law, and a
different ping budget is a lever these figures hold fixed.

Against `fixed-5m` at **$2,215.28**:

| arm | total | vs `fixed-5m` | hit % | pings |
|---|---:|---:|---:|---:|
| `optimal` *(unreachable — reads the future)* | $1,728.95 | **−21.95%** | 76.8% | 119 |
| **`keepalive-5m`** | **$2,050.56** | **−7.44%** | 76.8% | 2,726 |
| `keepalive-1h` | $2,086.91 | −5.79% | **78.6%** | 1,789 |
| `fixed-1h` | $2,150.19 | −2.94% | 78.0% | 0 |
| `fixed-5m` | $2,215.28 | — | 74.7% | 0 |
| `observed-policy` | $2,215.28 | — | 74.7% | 0 |
| `keepalive-5m-to-1h` | $3,124.58 | **+41.05%** | 78.5% | 2,406 |
| `no-cache` | $9,789.96 | +341.93% | 0.0% | 0 |

Where the money goes:

| arm | fresh in | read | write | pings | output |
|---|---:|---:|---:|---:|---:|
| `optimal` *(unreachable)* | 161.74 | 907.83 | 475.16 | 9.22 | 174.99 |
| `keepalive-5m` | 104.95 | 889.91 | 763.66 | 117.05 | 174.99 |
| `fixed-1h` | 104.95 | 902.62 | 967.63 | 0.00 | 174.99 |
| `fixed-5m` | 104.95 | 865.41 | 1,069.93 | 0.00 | 174.99 |
| `keepalive-5m-to-1h` | 104.95 | 906.57 | 555.37 | **1,382.70** | 174.99 |

### Prompt caching itself is the win, and it is already banked

`no-cache` costs 4.4x the shipped policy: caching is taking **77.4%** off the uncached bill
today. Everything else on this page is a fight over the remaining quarter.

### Hit rate is not the objective

This is the most useful thing on the page, and the corrected data makes it sharper rather than
weaker:

- `keepalive-1h` has the **best hit rate on the page** (78.6%) and is not the cheapest arm.
- `keepalive-5m-to-1h` has the **second-best hit rate** (78.5%) and is **the most expensive
  reachable arm by a wide margin** (+41.05%).
- `optimal` has a **lower** hit rate than three arms it beats on cost, and is the cheapest thing
  that exists.
- The cheapest reachable arm, `keepalive-5m`, hits **less often** than `keepalive-1h`,
  `keepalive-5m-to-1h` and `fixed-1h`.

**A view that sorts or colours by hit rate will recommend the worst option.** `keepalive-5m-to-1h`
is the demonstration: 1,486 of its 2,406 keep-alives were tier upgrades, each a one-hour write of
a whole prefix at 2.0x input, and that is $1,382.70 of ping cost buying $515 of avoided writes. It
fires on almost every idle span because 92.5% of gaps close inside five minutes, so most upgrades
bought an hour of hold for a conversation that returned in seconds. The arm is not wrong; its
trigger is unconditional.

### Where the headroom is

Replaying `optimal`'s own choices one kind at a time — each against the same `fixed-5m` baseline,
with every other request falling back to `fixed-5m`. These are **ablations, not a partition**: an
hour-long hold on one turn changes whether the next turn hits, so they interact and do not sum to
21.95%.

| the optimum's choices, in isolation | saving vs `fixed-5m` |
|---|---:|
| only the **435 requests** it wrote at the 1-hour tier | **+17.93%** ($397.22) |
| only the **91 requests** it held with keep-alives | +3.09% ($68.37) |
| only the **2,801 requests** it declined to cache | +0.62% ($13.68) |

So **526 decisions out of 14,407 — 3.7% of requests — carry almost all of the headroom.** That is
a rare-event problem, which is a different thing from a prediction problem, and it is the finding
that should shape any further work: not a model that is well calibrated on average, but one that
is precise on the tail and weighted by prefix size.

### The learned arms

On a held-out remainder (the predictor is fitted on the first 70% of the window by time and its
thresholds tuned there), both machine-learned arms scored within a rounding error of `fixed-5m`.
The cost-based rule is not merely unprofitable but **degenerate**: it chose `write_5m` on every
row, so it never deviated from the baseline at all.

That is not because the model is bad. It halves the Brier score of the base rate — see
`kv_ttl_survival_predictor.py`, which reports that on its own chronological split, a **different**
split from the one the arms were scored on. It buys nothing because 92.5% of gaps close inside
five minutes, so the five-minute tier is already right for almost every request and there is no
decision left for a probability to improve. The 3.7% above is where a model would have to earn
its keep.

### These figures were recomputed after three correctness fixes

An earlier version of this page published numbers from code with three defects, all of which moved
figures in the flattering direction, and one of which **inverted a recommendation**: `fixed-1h`
was reported at +34.69% (the worst reachable arm) when it is in fact −2.94% (an improvement). The
defects were a cache entry being handed between models, the ceiling not being a bound on any
trajectory that switched model, and an `expire` turn being counted as a cache hit. They are fixed,
pinned by tests, and verified by exhaustive search over every action sequence on multi-model
fixtures plus 450 randomised trajectories. The figures above are from the fixed code.

## The honest downside

- **The 1-hour creation rate is DERIVED, and it is the single biggest lever on these results.**
  No gateway publishes one, so it is the documented 2.0x multiple of base input. `fixed-1h`'s
  −2.94%, `keepalive-1h`'s −5.79% and the whole of `keepalive-5m-to-1h`'s +41.05% move directly
  with that multiplier. It is a configurable field, which is a design virtue and *not* a reason to
  treat the number as measured. Worse, the code knows the sharper risk: a requested `ttl:"1h"` is
  not always honoured — on this gateway it depends on the model — so a 1h arm may be priced for a
  tier the provider silently declined to give.
- **The keep-alive schedule is held fixed at 2 refreshes per idle span.** Every ping figure and
  the ceiling itself are optimal *under that budget*. A 5-minute entry can be held at most
  2x280+300 = 860 s, so the band between 14 minutes and an hour is unexplored, and a larger budget
  buys it at the 0.1x read rate against a rescue worth ~1.15x. The ceiling is therefore a ceiling
  over six actions at one schedule, not over all policies.
- **An `expire` turn is modelled as LAPSING the cache entry, and the provider appears not to do
  that.** Anthropic's cache is content-addressed with no delete verb, so a request omitting
  `cache_control` should neither read, refresh nor remove the entry. Tested against this window:
  in 132 cases where a caching request followed a non-caching one inside the earlier entry's TTL,
  **128 (97%) still read from cache**. The simulator charges those turns for a re-creation they
  would not pay, so every arm that uses `expire` — including the ceiling — is charged **too much**,
  and the ceiling is understated rather than overstated. Not corrected here because fixing it makes
  the dynamic program's state no longer a function of the previous action alone, which is the same
  structural break as `HitRefreshesTTL=false`; the two are one change and it needs its own PR.
- **`NewOptimal` is unsound under `Semantics{HitRefreshesTTL: false}`** — measured 18% overstated —
  because the entry's deadline then carries across an unbounded run of hits. That setting is a
  configuration flip, so the arm must refuse it rather than answer.
- **Token counts and miss reasons are observed under ONE policy and held fixed across every
  counterfactual arm.** `cached_context` is what the 5-minute policy that actually ran was billed,
  and a recorded `cold_start` forces a miss on every arm. Defensible, and the foundation of the
  whole replay.

- The measured window is **57 hours** and one deployment. Every figure here is that window's,
  not a law.
- Only **221 of 1,772 conversations** have two or more requests, so the gap distribution rests
  on a minority of trajectories.
- The production snapshot used for these numbers **predates the `cache_ttl` column**, so
  `observed-policy` falls back to five minutes for every row and equals `fixed-5m` exactly
  there. That is not a coincidence, and `Result.ObservedCoverage` (recorded vs assumed) is
  what says so. On a current database it is a genuinely distinct arm.
- The upgrading keep-alive's cost is priced as a **1-hour write**, which is the pessimistic
  reading. It rests on this deployment's own capture (a request asking for `ttl:"1h"` over an
  already-warm prefix returned 36,251 of 36,574 written tokens on the hourly creation tier).
  If some provider bills it as a read instead, that arm is cheaper than reported here, never
  dearer.
- An open idle span — a conversation whose last request is inside the window — has a length
  nobody knows. Its keep-alives are billed only to the window's end and counted apart in
  `Result.PingsOnOpenSpans`, because they rest on an assumption the closed spans do not need.
- A model with no rates contributes to **no** dollar figure and is counted in
  `Result.Unpriced`. An unpriced model is not a free one.

## Related

- [Keep an idle prompt cache warm](cache-keepalive.md) — the shipped keep-alive mechanism
- [Measuring savings](measure-savings.md) — how cost is attributed elsewhere in this repo
- [Dashboard](../dashboard.md)
