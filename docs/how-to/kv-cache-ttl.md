# Choose a cache TTL, and know what it is worth

The provider sells two prompt-cache lifetimes. A five-minute entry costs `1.25x` base input
to create; a one-hour entry costs `2.0x`. Both are read back at `0.1x`. Choosing between
them — per request, per conversation, per account — is a policy, and until now this repo had
no way to price one.

This page is that way. It covers the cost model, the strategies, the offline predictor, and
what the whole thing measured on this service's own traffic. The short version:

- **Prompt caching itself is already earning 54%** of the bill (`no-cache` costs 115.62%
  more than `fixed-5m`), and that is banked.
- **The entire remaining TTL headroom is 8.74%**, and reaching it requires knowing the future.
- **A blind keep-alive takes 2.91% of it** with no model at all.
- **93% of the headroom is 285 decisions out of 14,407.** This is a rare-event problem, which
  is a very different thing from a prediction problem, and it is the finding that should
  shape any further work.

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

The hosted deployment's own capture: 14,407 requests, 12 accounts, 1,772 trajectories,
2026-08-17 11:48 → 08-19 20:38 UTC, at `/etc/context-guru/prices.yaml` — which matters,
because this gateway bills `aws/claude-sonnet-5` at `$1.52/MTok` where anthropic.com bills
`$3.00`, so the public list price would overstate every figure below by about `2x`.

Over the whole set, against `fixed-5m` at **$4,540.33**:

| arm | total | vs `fixed-5m` | hit % | pings | upgrades |
|---|---:|---:|---:|---:|---:|
| `optimal` *(ceiling)* | $4,143.46 | **−8.74%** | 78.2% | 92 | 0 |
| **`keepalive-5m`** | **$4,408.10** | **−2.91%** | 78.3% | 2,023 | 0 |
| `fixed-5m` | $4,540.33 | — | 77.1% | 0 | 0 |
| `keepalive-5m-to-1h` | $5,256.81 | +15.78% | 79.3% | 1,836 | 1,076 |
| `fixed-1h` | $6,115.49 | +34.69% | 79.0% | 0 | 0 |
| `no-cache` | $9,789.96 | +115.62% | 0.0% | 0 | 0 |

Where the money goes:

| arm | fresh in | read | write | pings | output |
|---|---:|---:|---:|---:|---:|
| `optimal` | 269.51 | 696.63 | 2,990.83 | 11.50 | 174.99 |
| `keepalive-5m` | 104.95 | 682.96 | 3,350.48 | 94.72 | 174.99 |
| `fixed-5m` | 104.95 | 663.23 | 3,597.16 | 0.00 | 174.99 |
| `keepalive-5m-to-1h` | 104.95 | 697.14 | 3,173.30 | **1,106.44** | 174.99 |
| `fixed-1h` | 104.95 | 693.92 | **5,141.63** | 0.00 | 174.99 |

### Hit rate is not the objective

`fixed-1h` has the second-best hit rate on the page and is the most expensive arm on it.
`optimal` has a *lower* hit rate than `fixed-1h` and is the cheapest. Against `fixed-5m`,
`fixed-1h` buys 281 extra hits for **$1,575.16** — **$5.61 per extra hit**, where an avoided
miss on the median 124,845-token prefix is worth **$0.55**. It is paying roughly `10x` what the
thing is worth, because to rescue those few requests it pays the hourly creation premium on
*all* of them.

**A page that sorts or colours by hit rate will recommend the worst option.**

### Why `keepalive-5m-to-1h` loses, and what would fix it

It should be strictly smarter than `fixed-1h`, and it is — by $859. It still loses to plain
`fixed-5m`, and the decomposition says why: **1,076 of its 1,836 pings were upgrades**, each a
one-hour write of a whole prefix at `2.0x`. That is $1,106 of ping cost to save $424 of
writes. It fires far too often, because 92.5% of gaps close inside five minutes and most of
those upgrades bought an hour of hold for a conversation that came back in fifteen seconds.

`optimal` does the same thing 92 times, not 1,836. The arm is not wrong; its trigger is
unconditional.

### The learned arms took nothing

On the **held-out half** — 5,722 requests, where the ceiling is 7.50% rather than the whole
set's 8.74%, because the two are different populations and their percentages are not
interchangeable — the threshold-ladder arm scored **−0.01%** and a cost-based rule over the
same model scored **+0.00%**. Not because the model is bad. It halves the Brier score
of the base rate:

| horizon | actual | predicted | Brier | Brier of the base rate |
|---|---:|---:|---:|---:|
| ≤ 5m | 0.7558 | 0.8157 | 0.0986 | 0.1846 |
| ≤ 1h | 0.8315 | 0.8587 | 0.0695 | 0.1401 |

They took nothing because **92.5% of gaps close inside five minutes**, so the five-minute tier
is already right for almost every request and there is no decision left for a probability to
improve. The model also runs about six points **optimistic** on this split — the training half
had a higher return rate than the test half — so re-fit and re-check calibration before
trusting a threshold on it.

### Where the headroom actually is

Replaying `optimal`'s own choices one kind at a time — **on the whole set**, so these compose
with the 8.74% above rather than with the held-out figures in the section before it:

| the optimum's choices, in isolation | saving vs `fixed-5m` |
|---|---:|
| only the **218 requests** it wrote at the 1-hour tier | **+6.37%** ($289.18) |
| only the **67 requests** it held with keep-alives | +1.77% ($80.39) |
| only the **2,862 requests** it declined to cache | +0.47% ($21.39) |

Those are counts of REQUESTS the optimum decided about, which is not the same as the columns
in the table above: 92 there is keep-alives that actually *fired* (67 requests can attract more
than one), and 230 is cache-creation *events* at the hourly tier, which a keep-alive can cause
as well as a write. Two similar-looking numbers counting different things is worth one sentence
rather than a reader's afternoon.

Those three sum to **8.61%** against a combined **8.74%**, and the 0.13-point residual is not
rounding: the choices interact. An hour-long hold on one turn changes whether the next turn
hits, so replaying one kind of choice in isolation is not the same as its share of the total.
The three rows are therefore *contributions*, not a partition, and they are quoted here because
the ORDERING is the finding — not because they add up.

**93% of the reachable headroom is 285 decisions out of 14,407 — 2.0% of requests.** That is
what to build for: not a model that is well calibrated on average, but one that is precise on
the tail and weighted by prefix size. A model scored on average calibration will look good and
buy nothing — which is exactly what the two learned arms above did.

## The honest downside

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
