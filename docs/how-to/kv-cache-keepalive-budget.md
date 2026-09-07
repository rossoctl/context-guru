# Decide how many keep-alives a conversation is worth

[Choose a cache TTL](kv-cache-ttl.md) prices the *tier* — five minutes or an hour. This page
is about the other lever: once an entry exists, **how many keep-alive pings to spend holding
it**, decided per conversation instead of once for the whole fleet.

The short version, and the last bullet is the one to read first:

- **The break-even is 8.70%**, and it comes out of the rates rather than a config file. It is
  the same number for every model, and for every prefix large enough that a ping's fixed
  overhead rounds away — 8.70% at 125k tokens, 9.74% at 500.
- **The decision is not "ping or not", it is "how many"**, and the pings are not independent:
  each one keeps the entry alive so the next is a `0.1x` read instead of a `1.25x`
  re-creation. That makes it a backward induction, not a threshold.
- **`kvcache.BudgetPolicy` is the arm**; `PingBudgeter` is the one-method seam that lets a
  per-conversation budget reach the ping loop. Neither changes any existing arm.
- **On money, a constant beats it.** A flat `MaxPings=5` takes 6.91% off the bill where this
  policy takes 6.07%. It wins only on **net per ping**, and against that same `MaxPings=5` by
  **7.6×** — the 9× below is against `MaxPings=6`, which is what the table is indexed to.
  If your pings are effectively free, raise `MaxPings` and do not deploy a model. That is the
  honest recommendation and the rest of this page is why.

## The economics, which is the part to argue with first

Per cached token, as multiples of base input:

| | multiple | what it is |
|---|---|---|
| cache read | `0.10x` | what a keep-alive ping costs |
| cache write (5m) | `1.25x` | what the successor pays if the entry lapsed |
| **rescue** | **`1.15x`** | `1.25 − 0.10`, what a successful ping avoids |

A ping pays for itself when the chance it rescues exceeds `0.10 / 1.15 = 8.70%`.

The model's per-token rate **cancels**, and so does the *per-token part* of the prefix — it
multiplies cost and benefit alike. That is why `BudgetPolicy` reads a probability and never a
token count, and why the threshold is computed from `Observation.Pricing` rather than written
down: a deployment on different rates has a different threshold, and one with no rates has none
(`PingBudget` returns `ok=false` and the configured `MaxPings` governs).

What does **not** cancel is the ping's fixed overhead. `Pricing.KeepAliveCost` carries
`ping_input` and `ping_output` terms that do not scale with the prefix, so

```
gate(prefix) = 0.0870 + (ping_input x input + ping_output x output)
                        / (prefix x (write_5m - cache_read))
```

| prefix | gate |
|---:|---:|
| 500 | 9.74% |
| 2,000 | 8.96% |
| 20,000 | 8.72% |
| 125,000 | 8.70% |

8.70% is the limit, not the number at every size, and the spread is the same order as the
margins this arm is chosen by. `MinPrefix` is the gate for exactly that reason, and
`TestBudgetGateRisesOnASmallPrefixByTheFixedOverhead` pins the four figures above.

Two consequences worth stating because they are easy to get backwards:

- **The payoff is 11.5:1.** A wrong skip costs 11.5 times a wasted ping. A policy that skips
  has to be right almost every time to come out ahead, which is most of why the measured
  result below goes the way it does.
- **A late ping is not a cheap ping.** One that lands after the entry lapsed is priced as a
  re-creation — `12.5x` a read — by `Pricing.RecreateCost`, and counted in
  `Result.PingsThatRewrote`. On the live deployment 1.72% of real pings did this. A schedule
  whose interval exceeds the lifetime it protects does it every time.

### The gate is the myopic bar, and it is too high

8.70% is what a single ping considered alone must clear. But a ping buys two things: this
window's rescue, *and* the right to make a cheap decision at the next sweep. So the budget is
an induction over the windows a ping would protect:

```
V_j = max(0, rescue×h_j − ping + s_j×V_(j+1)),   V_maxk = 0
budget = the FIRST j at which V_j is not positive
```

The **first**, not the last. With a non-monotone hazard — which real traffic has — taking the
last positive `V` double-counts value the induction has already propagated backwards, and buys
a run of pings across a dead stretch. Counting the option value properly lowers the effective
bar at the first sweep to about **7.4%** and is worth ~6% of the arm's net.

## What is being predicted, and why it is not the survival predictor's question

[`kv_ttl_survival_predictor.py`](kv-cache-ttl.md)
answers *"will this conversation return within five minutes?"* from the instant a request is
served. That is the right question for a tier. It is the wrong question for a keep-alive, for
two reasons.

**The population is different.** A keep-alive fires only after the conversation has *already*
been idle for one interval, so the decision is never faced by the ~92.5% of spans that close
inside five minutes. It is faced only by the survivors. On this corpus:

| population | 5-minute return rate |
|---|---|
| all requests (unconditional) | ~92% |
| spans that reached 270s idle, stake-weighted | **23.17%** |

A model fitted on the first number and then consulted at the ping instant is answering a
question nobody asks.

The practical form of this: **elapsed silence is not a feature, it is the sample definition.**
A row exists in the training frame *because* it reached that sweep. `person_periods()` builds
exactly that — one row per `(span, sweep it actually reached)` — and it is the layout the whole
method rests on.

**The target is different too.** The policy needs two conditional quantities per sweep, both
derived from the cumulative answer `F` that `kvcache.Predictor` already returns:

```
h_j = (F(t_j + life) − F(d_j)) / (1 − F(t_j))    this ping rescues
s_j = (1 − F(t_(j+1)))        / (1 − F(t_j))     reaches the next sweep
```

The divide by the survivor share is the conditioning, and it is not cosmetic: on a
distribution where 90% have already returned, 5 percentage points of remaining mass is a **50%**
conditional hazard, not 5%. `BudgetPolicy.Windows` is exported so that
`kv_ttl_keepalive_drift_test.go` can pin both vectors against the Python, because a budget
that agrees for the wrong reason is a guard that has already stopped working.

## The 13 features, and why the list is exactly that long

Every feature is carried by `kvcache.Observation`, or derived from `Observation.Stats` — the
leak-free accumulator the simulator already advances as gaps close. **That is a hard
constraint, not a preference:** a model fitted on anything else cannot be wired in behind
`kvcache.Predictor`, which is the entire point of the seam.

| block | features | source |
|---|---|---|
| categorical | `user_id`, `model`, `cache_ttl` | `Observation.User` / `.Model` / `.TTL` |
| span numeric | `log_prefix`, `log_prev_gap`, `turn` | `.CachedTokens`, `.SinceLastMs`, `.Turn` |
| state numeric | `sweep_k`, `log_elapsed`, `log_secs_to_deadline`, `hour_utc`, `dow_utc` | recomputed per sweep from `.Now` and `.ExpiresAt` |
| historical | `stat_p`, `stat_n` | `Observation.Stats.ReuseWithin`, with `History`'s own fallback ladder |

The state block is what makes the policy per-state: `hour_utc` is the hour **at the decision
instant**, not at the last message.

`sweep_k` is a legitimate feature here *only* because the frame is pooled across sweeps — it
absorbs the baseline hazard so the metadata explains deviation from it. Ranking features on
that same pooled frame is not legitimate, because `sweep_k` then swamps every covariate. That
mistake was made and corrected during this work.

### What the restriction costs: almost nothing

3-seed means, 5-fold rolling origin:

| feature set | n | AUC (dollar-weighted) | off the bill | vs unrestricted |
|---|---:|---:|---:|---:|
| unrestricted (adds `agent`, `tools`, …) | 21 | 0.7051 | 6.11% | — |
| **`Observation` + `Stats`** | **13** | **0.7004** | **6.07%** | **−0.04 pp** |
| `Observation` alone | 11 | 0.6916 | 5.71% | −0.40 pp |

`stop_reason` was one of the unrestricted set's features when this ran, and has since
joined the seam as `Observation.StopReason` — on the same footing as `CachedTokens`, and read
by the registry's `stop-reason-gated` arm. So the 13-feature column predates it and the
comparison above was not re-run against the wider seam; the numbers are the ones measured,
not the ones a rerun would produce.

The restriction is free (−0.04 pp is inside the noise floor). Dropping the two `Stats` features
is *not* free. The historical accumulator is doing most of the work the excluded
request-shape features would have done, which is a good outcome: it means the seam was
designed with the right things on it.

## What it measured

The hosted deployment's own capture: **34,577 idle spans** over 16.9 days, 5-fold
rolling origin, at `/etc/context-guru/prices.yaml` — which matters for the same reason it does
on the TTL page, since this gateway's rates are not anthropic.com's and the public list price
would overstate every figure below.

A span is one idle stretch of a trajectory, and a trajectory is `(account, session, MODEL)` — a
cache entry does not transfer between models, so a session that switches model is two
trajectories. Pings are at 280 s throughout, which is a parameter and not a law: it is inside
the 300 s lifetime it protects, so every ping is a read rather than a re-creation.

Savings are given as a share of the window's **total bill**, of which 87.1% is the prefix
portion a keep-alive can move, and as a share of what `optimal` reaches:

| arm | pings | off the bill | of the ceiling | net per ping |
|---|---:|---:|---:|---:|
| `optimal` *(unreachable — reads the future)* | 5,826 | **11.72%** | 100.0% | 27.7× |
| flat `MaxPings=6` | 95,640 | **6.96%** | 59.4% | 1.0× |
| flat `MaxPings=5` | 80,404 | 6.91% | 58.9% | 1.2× |
| **`BudgetPolicy`** | **9,248** | **6.07%** | 51.8% | **9.0×** |
| flat `MaxPings=2` *(`DefaultMaxPings`)* | 33,591 | 5.19% | 44.3% | 2.1× |

"net per ping" is each arm's saving divided by its ping count, indexed to flat `MaxPings=6`.

### Read the last two columns together, because they disagree

On **money** a constant wins by 0.89 pp of the bill — 7.6 points of the ceiling. The measured reason:

| population | share of stake | AUC by row | AUC by dollar |
|---|---|---|---|
| all spans | 100% | 0.93 | 0.635¹ |
| top 20% by prefix | 99.5% | 0.76 | 0.615 |
| **top 5% by prefix** | **86.7%** | **0.58** | **0.566** |

¹ **Unverified — do not quote this cell.** A stake-weighted AUC decomposes as
`p²·AUC_in + (1−p)²·AUC_out + 2p(1−p)·AUC_cross` for a subgroup holding stake share `p`. The
top-20% row above (`p = 0.995`, `AUC = 0.615`) puts a ceiling on this cell of
`0.995²·0.615 + 0.005²·1 + 2·0.995·0.005·1 ≈ 0.619` even granting the complement and every
cross-pair a perfect 1.0 — below the 0.635 claimed here, so the two rows cannot both be right.
Which one is mis-transcribed isn't recoverable from the published figures alone — that needs
the underlying per-span dollar values, which is outside this page's data boundary. Read this
cell as "high 0.6x, exact value unconfirmed," not as 0.635.

The model ranks *conversations* almost perfectly and *dollars* barely above chance — and it
degrades toward the tail where the money is. Its mistakes concentrate on the large-prefix
spans where a mistake costs most, and at 11.5:1 that is decisive.

On **pings** it is 9× better, and that is the case for deploying it: where a tenant rate limit
rather than money is what caps the budget, `BudgetPolicy` gets 88% of `MaxPings=5`'s saving for
11% of its pings. On the live deployment ping coverage sits at 3.7% of candidates, so the
budget plausibly *is* the binding constraint — but that is a rate-limit question, not a
modelling one, and it is the question to settle before choosing.

### Two things that were measured and did not work

Recorded so they are not retried blind.

**Threshold tuning has no headroom.** The best threshold chosen *in hindsight* — which no
production policy can reach — is worth **+0.38 pp** over ping-everything across all spans,
**+0.02 pp** on the top 20% of prefixes, and **−0.05 pp** on the top 5%. Isotonic recalibration collapsed to
pinging nothing.

**Four families of extra feature came in under the noise floor.** The same model varies
**±0.42 pp** of the bill on the RNG seed alone, and every feature effect measured is smaller:

| family | 3-seed mean | verdict |
|---|---:|---|
| day-of-week (as number, as category, as `is_weekend`, crossed with hour) | +0.04 pp | sign flips across seeds |
| last long pause via `gap > 100s` | +0.05 pp | sign flips; best dollar-AUC of any variant |
| trajectory gap history (max / mean / count) | −0.15 pp | sign flips |
| last human turn via `stop_reason == end_turn` | **−0.45 pp** | consistently *worse* on all 3 seeds |

The last one is instructive: those features are absent on 86.7% of rows but present on 87.8%
of the *stake*. Sparse in rows and concentrated in dollars is the worst shape for a
stake-weighted fit — few examples, all high-weight — so the model overfits exactly where
mistakes are expensive.

Day-of-week shows real descriptive structure (stake-weighted hazard 8.56% on Saturdays to
16.85% on Thursdays, and Saturday afternoon UTC falls to 3.99%, below the gate) that does not
survive out of sample. With 18 calendar days there are only 2–3 instances of each weekday and
a 5× volume ramp across the window, so the weekday label is partly a label for a specific
date. **Separating a weekly cycle from a date effect needs 8–12 weeks**, and until then no
feature question on this corpus is answerable.

## Turning it on

Pick it on the **KV-cache** page. It is the `keepalive-budget` checkbox, it is in the default
arm set, and it is scored against the same baseline, the same rates and the same savings
arithmetic as every other arm.

That is new. `BudgetPolicy` used to be unreachable from any page, and the reason was structural
rather than an oversight: it is built from a `Predictor`, no `Predictor` existed outside a test,
and the registry's promise is that every name in it resolves. A name resolving to an arm with a
nil `Predictor` would have been worse than no name at all — `Decide` returns `write_5m` when it
has no opinion, so that arm never pings, and the page would have shown a flat write policy under
a label promising a learned one.

[`kvcache.ReuseModelV1`](#the-shipped-model) removed the premise instead of the promise. The
model is a table of coefficients compiled into the binary, so `NewKeepAliveBudget` builds the arm
from nothing and the name resolves on every process. Nothing else about the seam changed: a
caller with its own predictor still constructs `BudgetPolicy` directly and gets its own label.

!!! warning "`Config.MaxPings` still caps it, and the page defaults to 2"
    A budget can ask for fewer pings than the cap, never more. The arm reasons over 8 sweeps, so
    at the page's default `k=2` it is bounded to a quarter of its own horizon. It still wins
    there — see the table below — but if you want to see the schedule it would actually choose,
    set the keep-alive cap to 8.

### The shipped model

`ReuseModelV1` is a **fixed** discrete-time survival model: one coefficient per shipped feature,
one per one-hot level, one intercept, compiled in and never re-fitted at runtime. The
coefficients and their full provenance are in `kvcache/reusemodel_v1_gen.go`, generated; the
arithmetic that reads them is in `kvcache/reusemodel.go`, hand-written.

| | |
|---|---|
| corpus | 18.4 days of the hosted capture, 2026-08-17 → 2026-09-04 UTC |
| 219,650 idle spans | of which **37,418 reach the first sweep** — the population the decision is faced on |
| 276,321 person-periods | one row per (span, sweep it actually reached) |
| holdout AUC | **0.9065** by row, **0.7837** by dollar, fitted on the early 70% and scored on the late 30% |
| reference GBM, in-sample | 0.9572 by row, 0.8705 by dollar — the ceiling a shippable model gives up |

Three things about it are worth arguing with.

**It is a logistic regression, not the gradient-boosted pair `fit()` uses.** Not because the
ensemble is worse — it is better, and the generated file prints by how much on every re-fit —
but because it cannot ship. Thirty-seven trees of thresholds cannot be reviewed in a diff, so
nobody could tell a re-fit that improved the model from one that broke it; and the live pinger
consults the same model on the request path, where `proxy/keepalivestrategy.go`'s own rule is "a
rule, or … a portable logistic-regression dot product — never an embedded model or a call out of
the hot path."

**It is fixed, and that is a feature.** A model re-fitted at startup would make yesterday's
dashboard figure unreproducible, and this page's whole claim is that replaying a historical
window gives the same answer every time.

**The sweep index is a one-hot, not a linear term.** This is the one place the shipped model's
specification departs from the reference's, and it was not a preference. `sweep_k`,
`log_elapsed` and `log_secs_to_deadline` are all exact functions of the sweep index — the third
is `life - interval` at *every* sweep, a constant. A tree ensemble treats that as redundancy; a
linear model cannot, and the first fit showed what it costs: coefficients of −4.77 and +4.09 on
two features that move together, summing to a hazard of ~14% at every sweep where the corpus
measures 4.86% at the first and 0.24% by the seventh. The arm bought its maximum budget on 97%
of decisions and saved *less than a flat `MaxPings=2`*. A free coefficient per sweep recovers the
real shape, which rises from −1.25 at the first sweep to +1.83 at the seventh and then falls —
not monotone, which is exactly why assuming monotone was wrong.

`user_id` and `cache_ttl` are both in the reference's 13 features and neither is shipped.
`user_id` is a per-tenant dummy: it does not generalise to a tenant the fit never saw and it
would put tenant identifiers in the binary, while `stat_p`/`log_stat_n` already carry that
tenant's own history in a non-identifying form. `cache_ttl` is subtler and only becomes wrong
once the model is actually wired in behind `Predictor`: offline it is the tier the corpus
*recorded*, but at serve time the seam hands over `Observation.TTL`, which `simulate.go` sets
from the tier **the arm under test chose on the previous turn**. Training on one and serving the
other is a train/serve skew, and the second meaning is the model's own prior output fed back as
an input. Dropping it cost 0.0001 of holdout dollar-AUC.

### What it is worth, measured

Same corpus, replayed through the real arm on the KV-cache page's own scorer, against the
`fixed-5m` baseline the page uses. `optimal` reads the future and is a ceiling, never a result.

| arm | pings | off the bill | of the ceiling | net per ping |
|---|---:|---:|---:|---:|
| `optimal` *(unreachable)* | 1,041 | **8.054%** | 100.0% | 3.069 |
| **`keepalive-budget`, cap 5** | **8,790** | **2.065%** | 25.6% | **0.0932** |
| **`keepalive-budget`, cap 8** | **9,003** | **2.059%** | 25.6% | **0.0907** |
| `keepalive-5m` flat `k=5` | 59,139 | 1.860% | 23.1% | 0.0125 |
| **`keepalive-budget`, cap 2** | **7,573** | **1.850%** | 23.0% | **0.0969** |
| `keepalive-5m` flat `k=6` | 69,864 | 1.817% | 22.6% | 0.0103 |
| `keepalive-5m` flat `k=2` | 25,586 | 1.808% | 22.4% | 0.0280 |
| `keepalive-5m-once` | 13,538 | 1.516% | 18.8% | 0.0444 |
| `keepalive-5m` flat `k=8` | 90,779 | 1.381% | 17.1% | 0.0060 |
| `stop-reason-gated` | 14,969 | 0.710% | 8.8% | 0.0188 |
| `fixed-5m` *(baseline)* | 0 | 0.000% | — | — |
| `historical-probability` | 0 | −0.128% | — | — |

**On this window the arm beats every reachable alternative on money, which is not what the
earlier measurement found.** The version of this page written against the previous corpus
reported a flat constant beating the arm by 0.89 pp; here `keepalive-budget` takes 2.07% against
the best flat arm's 1.86%, and does it with **8,790 pings against 59,139** — 7.5× the value per
ping. Two things changed and only one of them is the corpus: the window is longer (18.4 days
against 16.9), and the specification defects above were fixed. Read the earlier figure as
measuring a differently-specified model.

The budget genuinely varies per conversation, which is the entire point of the arm and the one
thing a flat `MaxPings` cannot do:

| budget chosen | share of decisions |
|---:|---:|
| 0 | 41.4% |
| 1 | 31.9% |
| 2 | 22.5% |
| 5 | 1.2% |
| 6 | 3.0% |
| 8 | 0.0% |

Note the shape: **41.4% of conversations are worth no ping at all**, and the arm declines them.
That is where the 6.6× reduction in ping volume comes from, and it is why the per-ping column
moves so much more than the money column.

### The fit weight: an argument that lost

The fit is weighted by **dollars at risk**, and the reasoning said it should not be. A
stake-weighted fit is deliberately miscalibrated — it reports the hazard of a dollar, not of a
conversation, and the two differ fourfold here (21.10% stake-weighted at the first sweep against
4.86% by row). `BudgetPolicy` compares that probability against a break-even of 8.70%, so the
whole decision turns on which side of 8.70% the number lands, and `log_prefix` is a covariate
precisely so the size effect survives without the weighting.

Both were fitted and both were replayed through the arm:

| `--weight` | pings | off the bill | net per ping | budget 0 on |
|---|---:|---:|---:|---:|
| `stake` | 9,003 | **2.059%** | 0.0907 | 41.4% |
| `none` | 7,250 | 1.836% | **0.1004** | 56.4% |

`stake` ships, because money is the question the page is asked. What the calibration argument
predicted did not happen: being better calibrated against the break-even did not produce better
decisions, it produced **fewer** of them. `--weight none` is still selectable, and the argument
is still a good one — worth re-testing on a longer window rather than re-deriving.

## How to use it from Go

The arm is in the registry, so a name is enough:

```go
s, _ := kvcache.NewStrategy(kvcache.StrategyKeepAliveBudget, nil, cfg)
result := kvcache.Simulate(reqs, s, cfg)
```

With your own predictor instead of the shipped one, construct it directly and hand it to
`Simulate`:

```go
pol := kvcache.BudgetPolicy{
    Predictor: yourModel,              // anything implementing kvcache.Predictor
    Interval:  cfg.PingIdle,           // MUST match what the simulator will ping at
    MaxK:      kvcache.DefaultBudgetMaxK,
    MinPrefix: 10_000,                 // optional: a ping's fixed overhead is not worth it below this
    Semantics: cfg.Semantics,
}
result := kvcache.Simulate(reqs, pol, cfg)
```

`Interval` must match `Config.PingIdle`. If it does not, the windows this policy prices are
not the windows the simulator buys, and every number it produces is about a schedule nobody
ran.

### The schedules it will not price

Every window above is priced as one `KeepAliveCost` — a `0.1x` read that carries the entry
another lifetime. Three configurations make that false, and the simulator bills all three as
`Pricing.RecreateCost`, a `1.25x` write:

| configuration | why the read model breaks | reachable via |
|---|---|---|
| `Semantics.PingRefreshesTTL` off | a ping extends nothing, so every ping after the first lands on a dead entry | `?ping_refresh=0` |
| `Semantics.HitRefreshesTTL` off | the entry may already be nearer expiry than one lifetime, and `Observation` carries no hit flag by design, so the arm cannot tell | `?hit_refresh=0` |
| `Interval >= 300 s` | every ping arrives after the entry it was meant to hold | `?x=300` and up |

Measured on `kvcache`'s replay fixture at `MaxPings=6`, one predictor throughout, against a
plain 5-minute write with no pings: **−0.9%** under the documented semantics, **+17.6%** with
`HitRefreshesTTL` off, **+86.0%** with `PingRefreshesTTL` off, **+146.1%** at a 400 s interval.

So `PingBudget` returns `ok=false` on all three rather than quote a break-even taken from a
rate it is not paying, and `Decide` then writes 5m and fires no pings —
`TestBudgetDeclinesASchedulePricedOnTheWrongRate`.

`Config.MaxPings` remains a **ceiling**. A budget above it is clamped, so a model can never
raise an operator's cap — asserted by `TestPingBudgeterBoundsTheSimulatedPings`.

### Implementing `PingBudgeter` on your own arm

```go
type PingBudgeter interface {
    PingBudget(o Observation) (budget int, ok bool)
}
```

`ok=false` means *"no opinion, use `Config.MaxPings`"* — not *"never ping"*. The distinction
matters: reading a genuine zero as an absence would silently restore the configured cap on
exactly the spans a model asked to leave alone, which is why `convState` carries an explicit
`budgeted` flag rather than a zero sentinel. Any arm that does not implement the interface is
budgeted by `Config.MaxPings` exactly as before.

## The offline side

`deploy/harbor/kv_ttl_keepalive_policy.py` holds the fit and the evaluation.

```sh
cd deploy/harbor
python3 kv_ttl_keepalive_policy.py --self-test    # asserts the module's claims, stdlib only
python3 kv_ttl_keepalive_policy.py --fixture f.json   # the decision arithmetic's guard
python3 kv_ttl_keepalive_policy.py --score f.json     # the shipped MODEL's guard

# re-fit the shipped model and regenerate both checked-in artifacts
python3 kv_ttl_keepalive_policy.py \
    --db /var/lib/context-guru/cg.db --prices /etc/context-guru/prices.yaml \
    --model-out reusemodel_v1.json --go-out ../../kvcache/reusemodel_v1_gen.go
```

`--db` is what this page used to say was missing. The fit reads the corpus read-only, builds the
person-period frame itself, and writes two artifacts: `reusemodel_v1.json`, which is the fit's own
output, and `kvcache/reusemodel_v1_gen.go`, generated from it. Both are checked in, and the pair
being checked against each other is the point — see `--score` below. The fit needs pandas and
scikit-learn; everything else here does not.

Everything above the `── the fit ──` divider is **stdlib-only and pure**: `break_even`,
`windows`, `ping_budget`, `Rates`, `budget_for`. The scientific stack is imported inside the
fitting functions only, so the drift guard runs in a plain CI container. A guard that cannot
run is an absent guard.

There are **two** guards, because they cover different things.

`kv_ttl_keepalive_drift_test.go` drives that `--fixture` entry point and compares the budget
**and** both intermediate vectors across 11 cases chosen to sit on branches the two
implementations could plausibly diverge on — either side of the break-even to within 2 basis
points, a lean-then-rich pair, a worthless tail, a far-but-reachable window, a distribution
where most have already returned, a small prefix where the ping's fixed overhead matters, and
a rate scale 5× different to prove the threshold does not move. Deleting the survivor divide
from the Python makes it fail with `port 0.05` against `Go 0.50`, which is the check working.

What that guard cannot see is the **model**: `--fixture` supplies the distribution as a
hand-written step CDF, so the coefficients, the feature derivation and the standardisation are all
outside it. `kv_ttl_reusemodel_drift_test.go` closes that hole. It drives `--score` over the
checked-in `reusemodel_v1.json` — the artifact the Go table was generated from, so a hand-edit of
the generated file or a regeneration that was never committed both fail — and its fixture carries
**raw `Observation` fields, never derived features**, so a `log1p` applied to milliseconds on one
side and seconds on the other is caught rather than agreed on. It compares four layers: the
per-sweep survival vector, the CDF at horizons deliberately placed on and off the knot grid, the
per-window `h`/`s` that `Windows` derives, and the budget. Tolerance is 1e-9 absolute, because
both sides do the same arithmetic in the same order and anything above float noise is a real
divergence rather than a tolerance question.

## Where to look next

The blocker this page named — **the model cannot rank dollars** — is still the blocker, and it is
now a number rather than an inference: holdout dollar-AUC **0.7837** against row-AUC 0.9065. The
gap is the finding. Row AUC says the model knows which conversations come back; dollar AUC says it
knows much less about which *dollars* do, and the dollars are concentrated hard enough that this is
the difference that matters. The reference ensemble reaches 0.8705 by dollar, so roughly half the
remaining gap is the estimator and half is the data.

What has changed is that this no longer blocks *shipping the arm*. The measurement above says the
arm beats every reachable alternative on money on this window even with a dollar-AUC of 0.78 —
because the induction only needs the probability to land on the right side of 8.70%, which is a
weaker requirement than ranking. The blocker is now about how much MORE there is, not about
whether there is any: the arm reaches 25.6% of the `optimal` ceiling, and the other 74% is what
better dollar discrimination would buy.

Two things follow, in order:

1. **A longer window.** At ±0.42 pp of seed noise on 18 days, no feature-level question is
   answerable. 8–12 weeks makes them answerable and separates weekday from date.
2. **Tenant-local time instead of UTC.** The night/weekend boundary is where the calendar
   signal lives, and UTC smears it for every tenant not near Greenwich. There is no timezone
   column, but each tenant's own activity histogram gives an offset. This is a *transform* of
   two existing features rather than a new one, so it can plausibly clear the noise floor
   where an added column cannot — and it targets the same physical signal ("nobody is at the
   desk") more directly than a weekday label does.

What is **not** worth doing is adding more columns and measuring them on 18 days. That
returns sign-flipping answers indefinitely, and this work produced four of them.

Two more, specific to the shipped model:

3. **Re-test `--weight none` on a longer window.** The calibration argument above is sound and
   lost by 0.22 pp on 18 days, which is within shouting distance of this corpus's ±0.42 pp seed
   noise. It is not settled, it is merely decided for now.
4. **The cap interacts with the budget and nobody has looked at why.** `keepalive-budget` scores
   *better* at `MaxPings=5` (2.065%) than at 8 (2.059%). The difference is small and may be noise,
   but the arm asking for more pings than it should on a thin tail is exactly the shape the
   backward induction is supposed to prevent, so a cap that improves on it is worth a look rather
   than a shrug.
