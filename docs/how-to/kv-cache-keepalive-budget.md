# Budget keep-alive pings

[Choose a cache TTL](kv-cache-ttl.md) covers the *tier* — five minutes or an hour.
[Keep an idle prompt cache warm](cache-keepalive.md) covers turning keep-alive pings on at all.
This page is about the knob in between: **how many pings** a conversation is worth spending
while it's idle, rather than a single fleet-wide number.

## The short version

- A ping pays for itself once the chance it rescues the entry from expiring clears roughly
  **8.7%** — that number comes straight out of the provider's cache-read vs. cache-write rates,
  not a tunable constant.
- A wrong skip (letting the entry expire when a ping would have saved it) costs about **11.5x**
  what a wasted ping costs. That asymmetry is why the default is conservative.
- A late ping — one that arrives after the entry has already lapsed — isn't a cheap refresh, it's
  a full re-creation at the write rate. Keep your ping interval safely inside the tier's lifetime
  (under 300s for a 5-minute entry).
- **A per-conversation adaptive budget is now shippable and on by default on the KV-cache page**
  (`keepalive-budget`) — see [Turning it on](#turning-it-on) — and on the measured window it beats
  every reachable flat cap on both money and ping volume. See
  [the research](#the-research-behind-this) for the full comparison.

## How to configure it

The practical knobs live under the `cache` keep-alive config (see
[Keep an idle prompt cache warm](cache-keepalive.md#turning-it-on) for the full picture):

```yaml
cache:
  keepalive: true
  keepalive_idle_seconds: 280          # ping interval — keep this under the tier's TTL
  keepalive_max_pings: 2               # flat cap per idle span
  keepalive_min_prefix_tokens: 20000   # skip small prefixes — a ping's fixed cost isn't worth it below this
  keepalive_max_usd_per_ping: 0.25     # per-ping cost ceiling
```

`keepalive_max_pings` is a **ceiling** other policies (including the predictor-driven budget
below) can only tighten, never raise.

## Turning it on

Pick it on the **KV-cache** page: it is the `keepalive-budget` checkbox, in the default arm
set, scored against the same baseline, rates and savings arithmetic as every other arm.

That is new. `BudgetPolicy` used to be unreachable from any page — it needed a `Predictor`, and
no `Predictor` existed outside a test. [`kvcache.ReuseModelV1`](#the-shipped-model) is that
predictor: a fixed table of coefficients compiled into the binary, so `NewKeepAliveBudget`
builds the arm from nothing and the name resolves on every process.

!!! warning "`Config.MaxPings` still caps it, and the page defaults to 2"
    A budget can ask for fewer pings than the cap, never more. The arm reasons over 8 sweeps, so
    at the page's default `k=2` it is bounded to a quarter of its own horizon. It still wins
    there (see below), but if you want to see the schedule it would actually choose, raise the
    keep-alive cap to 8.

### The shipped model

`ReuseModelV1` is a **fixed** discrete-time survival model: one coefficient per shipped feature,
compiled in and never re-fitted at runtime. It is a logistic regression rather than the
gradient-boosted model the offline tooling can also fit, traded for reviewability (thirty-odd
coefficients fit in a diff) and for a rule cheap enough to run on the live request path.

Fitted on 18.4 days of the hosted capture (219,650 idle spans, 37,418 reaching the first sweep):
holdout AUC **0.9065** by row, **0.7837** by dollar. Full feature list, provenance and the three
specification pitfalls found while fitting it (collinear sweep features, a mislabeled last sweep,
and a `cache_ttl` train/serve skew) are in
[the reuse model research](../results/kv-cache-keepalive-reusemodel.md).

### What it is worth, measured

Replayed through the KV-cache page's own scorer, against the `fixed-5m` baseline:

| arm | pings | off the bill | net per ping |
|---|---:|---:|---:|
| `optimal` *(unreachable — reads the future)* | 1,041 | 8.054% | 3.069 |
| **`keepalive-budget`, cap 5** | 8,790 | **2.065%** | **0.0932** |
| **`keepalive-budget`, cap 2** *(page default)* | 7,573 | 1.850% | 0.0969 |
| `keepalive-5m` flat `k=5` | 59,139 | 1.860% | 0.0125 |
| `keepalive-5m` flat `k=2` *(`DefaultMaxPings`)* | 25,586 | 1.808% | 0.0280 |
| `stop-reason-gated` | 14,969 | 0.710% | 0.0188 |

**On this window the arm beats every reachable alternative on money, which an earlier
measurement on a shorter window did not find** — that measurement reported a flat constant
winning by 0.89 pp. Read it as a differently-specified model on a shorter corpus, not as a
contradiction; see [the reuse model research](../results/kv-cache-keepalive-reusemodel.md) for
the full accounting.

The budget genuinely varies per conversation — **41.4% of conversations are worth no ping at
all**, and the arm declines them, which is where most of the reduction in ping volume over a
flat cap comes from.

## Tuning guidance

- **The adaptive budget (`keepalive-budget`) is now the better default on both money and ping
  volume**, on the measured window above — prefer it over a flat cap unless you need the
  predictability of a fixed number.
- **Raise `keepalive_max_pings` if pings are effectively free to you** (no rate-limit pressure)
  and you'd rather use a flat cap — a flat cap of 5-6 still saves more in absolute dollars than a
  tightly-capped budget, at far more pings per dollar saved.
- **Lower it, or add a per-tenant strategy, if you're bumping into rate limits** — the adaptive
  budget spends far fewer pings for a similar or better fraction of the reachable savings.
- **Don't set `keepalive_idle_seconds` at or above the tier's lifetime.** A ping that lands after
  expiry pays the re-creation rate, not the read rate — it stops being a keep-alive and starts
  being an expensive accident.
- **Skip small prefixes.** `keepalive_min_prefix_tokens` exists because a ping has a small fixed
  token overhead of its own; below a few thousand tokens of cached prefix that overhead can erase
  the saving.

## How to use it from Go

The arm is in the registry, so a name is enough:

```go
s, _ := kvcache.NewStrategy(kvcache.StrategyKeepAliveBudget, nil, cfg)
result := kvcache.Simulate(reqs, s, cfg)
```

With your own predictor instead of the shipped one, construct it directly:

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

`Interval` must match `Config.PingIdle` — otherwise the windows this policy prices are not the
windows the simulator buys.

## The offline side

`deploy/harbor/kv_ttl_keepalive_policy.py` holds the fit and the evaluation:

```sh
cd deploy/harbor
python3 kv_ttl_keepalive_policy.py --self-test        # asserts the module's claims, stdlib only
python3 kv_ttl_keepalive_policy.py --fixture f.json   # the decision arithmetic's guard
python3 kv_ttl_keepalive_policy.py --score f.json     # the shipped MODEL's guard

# re-fit the shipped model and regenerate both checked-in artifacts
python3 kv_ttl_keepalive_policy.py \
    --db /var/lib/context-guru/cg.db --prices /etc/context-guru/prices.yaml \
    --model-out reusemodel_v1.json --go-out ../../kvcache/reusemodel_v1_gen.go
```

`--db`/`--model-out`/`--go-out` re-derive the shipped coefficients from the corpus rather than
requiring them to be hand-edited. Both output artifacts are checked in, and the pair being
checked against each other is what `kv_ttl_reusemodel_drift_test.go` guards.

## The research behind this

The break-even derivation, the backward-induction budget (why a ping's value depends on the pings
after it, not just the next window), the shipped model's full feature list and provenance, and
the measured comparison between a flat cap and the learned per-conversation budget all live in
Results:

- [The keep-alive reuse model: features, fit, and what it is worth](../results/kv-cache-keepalive-reusemodel.md)
- [KV-cache TTL predictor: summary](../results/kv-ttl-predictor-summary.md)
- [KV-cache TTL predictor: features](../results/kv-ttl-predictor-features.md)
- [KV-cache TTL predictor: arms](../results/kv-ttl-predictor-arms.md)
- [KV-cache TTL predictor: comparison](../results/kv-ttl-predictor-comparison.md)

## Related

- [Choose a cache TTL](kv-cache-ttl.md)
- [Keep an idle prompt cache warm](cache-keepalive.md)
- [Measuring savings](measure-savings.md)
