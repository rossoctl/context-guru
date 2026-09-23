# The keep-alive reuse model: features, fit, and what it is worth

This is the research behind `kvcache.ReuseModelV1`, the fixed predictor behind the
`keepalive-budget` arm described in
[Budget keep-alive pings](../how-to/kv-cache-keepalive-budget.md#the-shipped-model). It is a
different model from the [TTL predictor](kv-ttl-predictor-summary.md): that one answers "will
this conversation return within five minutes?" at the instant a request is served; this one
answers "should we spend another ping?" only after a conversation has already gone idle for one
interval, and is faced only by the survivors of that first question.

## What is being predicted

The policy needs two conditional quantities per sweep, both derived from the cumulative answer
`F` a survival model returns:

```
h_j = (F(t_j + life) − F(d_j)) / (1 − F(t_j))    this ping rescues
s_j = (1 − F(t_(j+1)))        / (1 − F(t_j))     reaches the next sweep
```

The divide by the survivor share is the conditioning, and it is not cosmetic: on a distribution
where 90% have already returned, 5 percentage points of remaining mass is a **50%** conditional
hazard, not 5%.

## The 13 features

Every feature is carried by `kvcache.Observation`, or derived from `Observation.Stats` — the
leak-free accumulator the simulator already advances as gaps close. That is a hard constraint:
a model fitted on anything else cannot be wired in behind `kvcache.Predictor`.

| block | features | source |
|---|---|---|
| categorical | `user_id`, `model`, `cache_ttl` | `Observation.User` / `.Model` / `.TTL` |
| span numeric | `log_prefix`, `log_prev_gap`, `turn` | `.CachedTokens`, `.SinceLastMs`, `.Turn` |
| state numeric | `sweep_k`, `log_elapsed`, `log_secs_to_deadline`, `hour_utc`, `dow_utc` | recomputed per sweep |
| historical | `stat_p`, `stat_n` | `Observation.Stats.ReuseWithin`, with `History`'s own fallback ladder |

## The shipped model

Fitted on 18.4 days of the hosted capture (2026-08-17 → 2026-09-04 UTC): **219,650 idle spans**,
**37,418 reaching the first sweep** (the population the decision is actually faced on),
**276,321 person-periods**. Holdout AUC **0.9065** by row, **0.7837** by dollar, fitted on the
early 70% and scored on the late 30%, split by time rather than at random (one span contributes
up to `max_k` person-periods, so a random split scores a row against its own siblings). The
reference gradient-boosted fit reaches 0.9572 / 0.8705 — the ceiling a shippable linear model
gives up.

It is a logistic regression rather than that ensemble, traded for two things a tree ensemble
cannot give: thirty-odd coefficients are reviewable in a diff, and the live pinger (a stacked
change) consults the same model on the request path, where the standing rule is "a rule, or a
portable logistic-regression dot product — never an embedded model or a call out of the hot
path." It is also fixed rather than re-fitted at startup, so replaying a historical window gives
the same answer every time.

### Three specification defects, each a trap worth not re-entering

**Collinear sweep features.** `sweep_k`, `log_elapsed` and `log_secs_to_deadline` are exact
functions of the sweep index — the third is `life - interval` at every sweep, a constant. A tree
ensemble tolerates that as redundancy; a linear model cannot. The first fit produced −4.77 and
+4.09 on the two collinear terms, summing to a flat ~14% hazard at every sweep against a measured
4.86% falling to 0.24% by the seventh, and bought the maximum budget on 97% of decisions. The
sweep index is a one-hot now — a free baseline hazard per sweep, whose recovered shape rises from
−1.25 to +1.83 and then falls, i.e. not monotone.

**The last sweep's label.** The person-period frame labels survival at `k == max_k` against
`inf`, so it is identically 0. Harmless as a truncation, fatal once composed into a CDF: `F`
jumps to 1.0 at the tail and the backward induction reads `h_max_k = 100%`, propagating that
certainty back through every earlier sweep.

**`cache_ttl` is a train/serve skew once wired in.** Offline it is the tier the corpus recorded,
which is self-consistent for fitting. At serve time the seam hands over `Observation.TTL`, which
the simulator sets from the tier the arm under test chose on the *previous* turn — the model's
own prior output fed back as an input. Dropped, for 0.0001 of holdout dollar-AUC. `user_id` is
dropped too: a per-tenant dummy generalises to no tenant the fit never saw and would put tenant
identifiers in the binary, while `stat_p`/`log_stat_n` already carry that history without
identifying it.

### The fit weight: an argument that lost to a measurement

The fit is weighted by dollars at risk. The argument for fitting unweighted is a good one — the
arm compares a probability against an 8.70% break-even, so it needs calibration rather than
ranking, and a stake-weighted fit reports the hazard of a dollar rather than of a conversation
(21.10% stake-weighted at the first sweep against 4.86% by row). Both were fitted and replayed
through the real arm:

| `--weight` | pings | off the bill | net per ping | budget 0 on |
|---|---:|---:|---:|---:|
| `stake` | 9,003 | **2.059%** | 0.0907 | 41.4% |
| `none` | 7,250 | 1.836% | **0.1004** | 56.4% |

`stake` ships, because money is the question the page is asked. What the calibration argument
predicted did not happen — being better calibrated did not produce better decisions, it produced
fewer of them. `none` stays selectable in the fitting tool: the 0.22 pp gap is within shouting
distance of this corpus's ±0.42 pp seed noise, so this is decided, not settled.

## What it is worth, measured

Replayed through the KV-cache page's own scorer, against the `fixed-5m` baseline:

| arm | pings | off the bill | of the ceiling | net per ping |
|---|---:|---:|---:|---:|
| `optimal` *(unreachable — reads the future)* | 1,041 | **8.054%** | 100.0% | 3.069 |
| `keepalive-budget`, cap 5 | 8,790 | **2.065%** | 25.6% | 0.0932 |
| `keepalive-budget`, cap 8 | 9,003 | 2.059% | 25.6% | 0.0907 |
| `keepalive-5m` flat `k=5` | 59,139 | 1.860% | 23.1% | 0.0125 |
| `keepalive-budget`, cap 2 *(page default)* | 7,573 | 1.850% | 23.0% | 0.0969 |
| `keepalive-5m` flat `k=6` | 69,864 | 1.817% | 22.6% | 0.0103 |
| `keepalive-5m` flat `k=2` *(`DefaultMaxPings`)* | 25,586 | 1.808% | 22.4% | 0.0280 |
| `keepalive-5m-once` | 13,538 | 1.516% | 18.8% | 0.0444 |
| `keepalive-5m` flat `k=8` | 90,779 | 1.381% | 17.1% | 0.0060 |
| `stop-reason-gated` | 14,969 | 0.710% | 8.8% | 0.0188 |
| `historical-probability` | 0 | −0.128% | — | — |

**On this window the arm beats every reachable alternative on money — which is not what an
earlier measurement on a shorter, 16.9-day window found**, when a flat constant won by 0.89 pp.
Two things changed and only one is the corpus: the window is longer (18.4 vs 16.9 days), and the
three specification defects above were fixed. Read the earlier figure as measuring a
differently-specified model, not as a contradicted result.

The budget varies per conversation, which a flat cap cannot do — **41.4% of conversations are
worth no ping at all**, and the arm declines them:

| budget | 0 | 1 | 2 | 5 | 6 | 8 |
|---|---:|---:|---:|---:|---:|---:|
| share of decisions | 41.4% | 31.9% | 22.5% | 1.2% | 3.0% | 0.0% |

## Guarding it

Two guards cover different things. `kv_ttl_keepalive_drift_test.go` (existing) feeds a
hand-written step CDF, so the coefficients, feature derivation and standardisation are all
outside it — by its own docstring, "the drift guard covers NEITHER". `kv_ttl_reusemodel_drift_test.go`
closes that hole: it drives the checked-in `reusemodel_v1.json`, the artifact the Go table was
generated from, so a hand-edit of the generated file *or* a regeneration that was never committed
both fail. Its fixture carries raw `Observation` fields, never derived features, so a `log1p`
applied to milliseconds on one side and seconds on the other is caught rather than agreed on.
Four layers are compared — per-sweep survival, the CDF on and off the knot grid, the per-window
`h`/`s`, and the budget — at 1e-9 absolute.

## Where to look next

1. **A longer window.** At ±0.42 pp of seed noise on 18 days, no feature-level question is
   answerable; 8–12 weeks makes them answerable.
2. **Tenant-local time instead of UTC.** The night/weekend boundary is where calendar signal
   lives, and UTC smears it for every tenant not near Greenwich.
3. **Re-test `--weight none` on a longer window** — the calibration argument is sound and lost by
   a margin close to seed noise.
4. **The cap/budget interaction.** `keepalive-budget` scores slightly better at `MaxPings=5`
   (2.065%) than at 8 (2.059%) — small enough to be noise, but worth checking rather than
   shrugging off.
