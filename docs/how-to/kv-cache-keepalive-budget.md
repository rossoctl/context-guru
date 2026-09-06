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
| unrestricted (adds `stop_reason`, `agent`, `tools`, …) | 21 | 0.7051 | 6.11% | — |
| **`Observation` + `Stats`** | **13** | **0.7004** | **6.07%** | **−0.04 pp** |
| `Observation` alone | 11 | 0.6916 | 5.71% | −0.40 pp |

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

## How to use it

`BudgetPolicy` is deliberately **not** in `Registry()`. Like `Custom`, it cannot be built from
a name alone — it needs a `Predictor` — and the registry's promise is that every name in it
resolves. Construct it and hand it to `Simulate`:

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
python3 kv_ttl_keepalive_policy.py --fixture f.json
```

Everything above the `── the fit ──` divider is **stdlib-only and pure**: `break_even`,
`windows`, `ping_budget`, `Rates`, `budget_for`. The scientific stack is imported inside the
fitting functions only, so the drift guard runs in a plain CI container. A guard that cannot
run is an absent guard.

`kv_ttl_keepalive_drift_test.go` drives that `--fixture` entry point and compares the budget
**and** both intermediate vectors across 11 cases chosen to sit on branches the two
implementations could plausibly diverge on — either side of the break-even to within 2 basis
points, a lean-then-rich pair, a worthless tail, a far-but-reachable window, a distribution
where most have already returned, a small prefix where the ping's fixed overhead matters, and
a rate scale 5× different to prove the threshold does not move. Deleting the survivor divide
from the Python makes it fail with `port 0.05` against `Go 0.50`, which is the check working.

## Where to look next

The measured blocker is specific: **the model cannot rank dollars.** Row AUC 0.93, falling to
0.57 on the spans holding 87% of the stake — the whole-population dollar AUC is the table cell
flagged unverified above, and is NOT 0.70, a figure the same table's own arithmetic rules out
by a wider margin than the 0.635 it disputes. Everything downstream of that — the threshold
tuning, the four feature families — was an attempt to work around it and none succeeded, which
is what makes it the blocker rather than one finding among several.

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
