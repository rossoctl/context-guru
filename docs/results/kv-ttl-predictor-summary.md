# KV-cache TTL predictors: the whole story, in one place

Everything the predictor workstream found, tried, shipped, and left open — pulled forward
from five detailed docs so nobody has to read all five to get the picture. Every number
below is cited from those docs, not re-derived; go to the linked doc for method, per-fold
tables, and the full derivation. All figures are from the live deployment, measured
**read-only**, aggregate-only, tenant ids pseudonymized (`t01`..`t17`), on 2026-08-26.

## Ship this

**Gate every keep-alive ping on the request just served's `stop_reason`: ping only when it
clusters as "actually done" (`end_turn`, `max_tokens`, `refusal`); never on "still working"
(`tool_use`, `stop_sequence`, `tool_calls`, `length`, `content_filter`) or "looks done, isn't"
(`stop`, unset).**

- **+1.54% vs `fixed-5m`, pooled, 95% CI [+0.60%, +2.79%]** — the CI does not cross zero,
  measured with a rolling-origin time split and a conversation-level bootstrap over 66,779
  requests / 17 tenants ([full method and per-fold tables](kv-ttl-predictor-arms.md)).
- It is **statistically indistinguishable from a trained logistic regression** (`logreg-v1`:
  +1.69%, CI [+0.79%, +2.94%]) fit on `stop_reason` cluster, tenant, hour/weekday, `Turn`, and
  rolling-gap features. Same result, no training step, no coefficients to keep in sync with a
  retrained model, no model artifact — the free rule wins on Occam's razor, not because the
  model failed.
- It is a **one-line change already made**: `kvcache.StopReasonGated` is registered
  (`kvcache/registry.go`) and wired as an **opt-in** live-traffic predictor gate in
  `proxy/keepalive.go`/`proxy/keepalivestrategy.go`. Nothing changes for an account until a
  manager creates a strategy that names it.
- **It is a net win, not a pooled fact**: it gains on `t01`/`t06`/`t12` (the tenants whose own
  `end_turn`-conditioned band rate is highest) and loses a little on `t04`/`t11` (whose band
  rate is lowest). See [Per-tenant results](#per-tenant-results-caveated) below before turning
  it on for every account without exception.

## Every feature found

Full derivations, the break-even formula, and the caveats are in
[kv-ttl-predictor-features.md](kv-ttl-predictor-features.md). Measured on 66,779 non-ping
requests, 17 tenants, 51,420 observed gaps, 2026-08-17 → 2026-08-26.

### `stop_reason` — the headline feature

Splits into three behavioural clusters, not two — a naive "did the turn end" split gets the
middle cluster backwards:

| cluster | stop_reason values | n | P(<5m) | **P(5m–1h band)** | P(>1h) |
|---|---|---:|---:|---:|---:|
| still working | `tool_use`, `stop_sequence`, `tool_calls`, `length`, `content_filter` | 41,199 | 99.4–100% | **0.0–0.6%** | ~0% |
| looks done, isn't | `stop`, unset | 3,196 | 93.4–96.2% | **2.9–6.1%** | 0.4–0.9% |
| actually done | `end_turn`, `max_tokens`, `refusal` | 7,025 | 43.3–84.6% | **11.7–43.3%** | 3.7–13.3% |

The one-ping break-even is ≈8.0% (`read_rate/write_rate`, a simplified but explicitly-flagged
cut). Only "actually done" clears it with real margin — "looks done, isn't" sits *below*
break-even despite reading like end-of-turn, which is the whole reason a two-way split
misfires.

### Per-tenant heterogeneity: extreme

Conditioned on `stop_reason = end_turn` alone, band rate ranges **0.76%–41.4%** across the 15
tenants with ≥10 `end_turn` events:

| tenant | n (end_turn) | P(band \| end_turn) | | tenant | n (end_turn) | P(band \| end_turn) |
|---|---:|---:|---|---|---:|---:|
| t12 | 29 | **41.4%** | | t04 | 276 | 11.6% |
| t01 | 982 | 37.8% | | t15 | 501 | 11.0% |
| t06 | 335 | 29.3% | | t16 | 20 | 10.0% |
| t13 | 33 | 27.3% | | t17 | 701 | 3.1% |
| t08 | 179 | 23.5% | | t02 | 1,048 | 0.86% |
| t09 | 19 | 21.1% | | t11 | 2,112 | **0.76%** |
| t07 | 242 | 19.0% | | | | |
| t10 | 70 | 14.3% | | | | |

The two highest-volume tenants (t11, t02) have the *lowest* band rates — a volume-weighted
pooled average is dragged toward "never ping" by exactly the accounts a ping matters least
for. **Any predictor shipped must be per-tenant, or per-tenant-adjusted via the existing
`kvcache.Stats`/`History` fallback chain** (`LevelUserBucket → LevelUserModel → LevelUser →
LevelModel → LevelGlobal`, `minCell = 6`) — reused, not reinvented.

### Time of day: real structure, UTC only

Two real peaks (04:00, 07:00 UTC) and a trough 18:00–23:00 UTC, on `end_turn` rows. The
store carries no per-tenant timezone, so every hour-of-day claim on this page is UTC, full
stop — see the features doc for the illustrative (not factual) Jerusalem-local comparison.

### 1h TTL requested vs honoured: a hard constraint on the whole 1h workstream

`cache_ttl = 'ephemeral_1h'` on 17.7% of requests (11,822 of 66,779); `cache_write_1h > 0` on
**zero** of them. On this gateway/model mix, `aws/claude-sonnet-5` silently downgrades every
requested hourly tier. Not a cost bug (a downgraded write still bills at 1.25x, not 2.0x), but
a coverage lie: any code assuming a `ttl:"1h"` request is protected for an hour is wrong here.
`claude-haiku-4-5` is the one model this gateway actually grants it on — see
[the sticky-session arm](#the-sticky-whole-session-1h-vs-5m-arm) below.

### Full feature inventory

| available now | source |
|---|---|
| `stop_reason` (3-cluster) | `requests.stop_reason` — the headline finding |
| tenant identity | `requests.tenant_id` — extreme heterogeneity, see above |
| hour of day (UTC), day of week | `requests.ts` |
| `Turn`, `SinceLastMs`, cached prefix size | already on `kvcache.Observation` |
| per-(tenant,model,bucket) historical reuse rate | `kvcache.History`/`Stats.ReuseWithin` |
| median/EWMA of past gaps | `deploy/harbor/kv_ttl_survival_predictor.py`'s feature engineering |
| `agent` (client dialect) | `requests.agent` — a distinct population, not just a label |
| `model`, `reasoning_effort`/`thinking_*`, tool/system-block counts, `cache_ttl`, `cache_miss_reason` | `requests.*` |

| requested, not instrumentable today | why |
|---|---|
| tenant's other open sessions (live count) | not tracked as a live gauge anywhere |
| per-request tool-call name/duration | `tool_uses` is session-level, not request-level |
| subagent/sidechain marker | no parent/child session link recorded |
| "was this a `Monitor` tool call" marker | same session-level-only limitation |

None of the missing features are needed for the two strongest signals found
(`stop_reason`, tenant identity) — they are a genuine gap, not a blocker for what shipped.

## Every predictor/arm tried

Full method (rolling-origin split, conversation-level bootstrap, the `History` port, the
logistic regression's features) and per-fold tables are in
[kv-ttl-predictor-arms.md](kv-ttl-predictor-arms.md). Measured on 66,779 requests, 17 tenants,
9-day window (2026-08-17 → 08-26), against `fixed-5m` (pooled baseline $7,239.02, 11,684
conversation-fold-segments).

### Pooled results

| arm | what it is | Δ vs fixed-5m | 95% CI | distinguishable from zero? |
|---|---|---:|---:|---|
| `logreg-v1` | logistic regression on stop-cluster, tenant, hour/weekday, Turn, rolling-gap features | +1.69% | [+0.79%, +2.94%] | yes |
| `stop-reason-x-hour` | `stop_reason` gate + a tuned good-hours window | +1.57% | [+0.63%, +2.77%] | yes |
| **`stop-reason-gated`** | gate on `stop_reason` cluster alone — **shipped** | **+1.54%** | **[+0.60%, +2.79%]** | yes |
| `historical-probability-tenant-tuned` | the shipped mechanism's own arm, per-tenant-tuned | −0.03% | [−0.08%, +0.02%] | **no** |

The three positive arms' CIs overlap almost completely — this data cannot tell "gate on
`stop_reason`" apart from "gate on `stop_reason` and hour" or "gate on the logistic
regression's probability." It *can* tell all three apart from doing nothing, and from
`historical-probability-tenant-tuned`, whose CI straddles zero: **not shown to help, not shown
to hurt.**

### Why `historical-probability` doesn't move

Its `Stats` cells are keyed on `(tenant, model, hour-bucket)` — not on `stop_reason`. A cell
blends the ~92.5% of turns that are `tool_use`/`stop_sequence` (near-0% band rate) with the
`end_turn` minority (11.7–43.3% band rate), so the blended `ReuseWithin(5m)` almost always
clears even a tuned threshold and the arm takes the "just write 5m" branch nearly every time —
indistinguishable from the baseline. Diagnosed, not a bug in the tuning: see
[What's not solved yet](#whats-not-solved-yet).

### Feature importance (both models agree on the top two)

| rank | logistic regression (\|standardized coef\|) | gradient-boosted trees (importance) |
|---:|---|---|
| 1 | `turn` (−2.99) | `turn` (0.198) |
| 2 | `stop_cluster=still_working` (−2.09) | `stop_cluster=still_working` (0.197) |
| 3 | `user_id=t02` (−1.16) | `request_hour_sin` (0.150) |
| 4 | `user_id=t04` (−1.08) | `user_id=t01` (0.093) |
| 5 | `user_id=t01` (+0.92) | `previous_gap_seconds` (0.064) |

Later turns are less likely to land in the rescuable band (consistent with the shipped
`pingable()`'s own "single-request sessions are 79% of pings and 0.9% of value"), and
excluding the "still working" cluster is the single strongest signal either model found —
ahead of tenant identity, ahead of time-of-day. This is the same conclusion the rule arms
reached by hand.

### Per-tenant results (caveated)

Point estimates, **no per-tenant confidence interval** (the pooled bootstrap resamples across
the whole test population; a per-tenant resample restricted to that tenant was not built).
Read directionally, especially `t02` (n=47) and `t12` (n=36) — both near the 30-event
reporting floor:

| tenant | n (actually_done, test) | stop-reason-gated | historical-probability | logreg-v1 |
|---|---:|---:|---:|---:|
| t12 | 36 | **+18.5%** | +0.17% | **+18.9%** |
| t06 | 270 | +8.1% | −2.2% | +8.2% |
| t01 | 274 | +5.9% | +0.3% | +5.8% |
| t14 | 141 | +4.1% | 0.0% | +4.1% |
| t07 | 127 | +3.0% | 0.0% | +4.4% |
| t02 | 47 | −1.8% | 0.0% | +1.4% |
| t15 | 390 | +1.1% | +0.2% | +1.1% |
| t17 | 459 | −0.4% | 0.0% | −0.4% |
| t11 | 2,126 | −0.13% | 0.0% | −0.02% |
| t04 | 285 | **−1.5%** | −0.15% | −1.5% |

Seven tenants (`t03, t05, t08, t09, t10, t13, t16`) don't clear the 30-event reporting floor
in the test window and are skipped, not zeroed. `t01`, `t06`, `t12` carry almost all of the
pooled gain — the same three the feature catalog flags at the high end of
`P(band | end_turn)`. `historical-probability` avoids the small loss on `t04`/`t11` but also
misses the gain on `t01`/`t06`/`t12` — **complementary failure modes**, not competitors (see
[What's not solved yet](#whats-not-solved-yet)).

### What's currently shipped, measured fresh (before this workstream's gate)

The two manager strategies live before `stop-reason-gated` existed, unconditional on
`stop_reason`, on the full corpus — the "helps some, hurts others" problem that motivated this
whole workstream, now with numbers ([full table](kv-ttl-predictor-comparison.md)):

| | value |
|---|---:|
| total pings | 143 |
| conversion rate | 23.78% (76.22% of pings rescue nothing) |
| net | +$6.48 |

**8 of 12 touched tenants were net-negative** under the un-gated schedule; one tenant (t10)
carried essentially all of the positive net.

### The sticky whole-session 1h-vs-5m arm

`kvcache.StickySession1h` (`kvcache/sticky.go`) decides once, at a conversation's first
request, whether to commit that whole session to the 1-hour or 5-minute tier — modeling the
real constraint that a created cache entry can't be renegotiated in place. Built, registered,
and tested against the same "never beats `optimal`," "never sees the future" invariants every
other arm here is held to. Full detail: [kv-cache-ttl.md](../how-to/kv-cache-ttl.md#the-sticky-whole-session-arm-and-a-real-haiku-4-5-measurement).

**Its simulated performance on this deployment's actual (sonnet-5-heavy) traffic is not
reported** — that would need the same aggregate-only DB access pattern the rest of this page
uses, and that step was deliberately not improvised for this arm.

**The one real, directly-measured number**: a genuine 3-turn session run through a local
proxy against `claude-haiku-4-5` (the one model this gateway actually grants a 1h TTL on),
with a real 392-second gap between turns 2 and 3:

| | 1-hour TTL (real, billed) | 5-minute TTL (hand-computed, same real tokens) |
|---|---:|---:|
| total, 3 turns, 1 gap | **$0.019594** | $0.0229506 |

**1h was 14.6% cheaper on this one real trajectory.** `cache_write_1h` matched `cache_write`
exactly on all three turns, confirming the tier was genuinely granted — unlike every one of
the 11,822 live `ttl:"1h"` requests on `aws/claude-sonnet-5` in the main corpus. A real, small,
single-session sanity check that the arm's underlying trade is real on a model this gateway
grants it on — not a corpus-wide claim, and not evidence for what the arm would do on this
deployment's actual sonnet-5-heavy traffic.

## What's live on the dashboard now

Everything above is one query away — no need to trust this page's numbers on faith.

- **KV-cache tab** (top nav, next to Keep-alive) → **"What a different TTL strategy would
  have cost"** panel: the arm comparison table (`fixed-5m`, `keepalive-5m`,
  `historical-probability`, `stop-reason-gated`, `optimal`, and every other registered arm),
  replayed live against whatever window/tenant filter is selected.
- Same panel, **"What actually predicts a rescue"** subsection, directly below the arm table:
  the feature-importance panel — the logistic-regression and gradient-boosted-trees top-5
  tables above, served as static data from the offline study (`dash/kvcachesim.go`'s
  `kvCacheFeatureImportance`) and rendered by `dash/ui/kvcache.js`'s
  `renderKVFeatureImportance`. Static because it's a model *fit*, not something this window's
  own rows recompute per request — refreshed by re-running the offline study, not by
  reloading the page.
- **Strategies tab** (manager-only) → create or edit a strategy → **"Predictor gate
  (optional, in addition to the windows below)"** fieldset: a **Predictor** dropdown (today,
  `stop-reason-gated` is the only registered option) plus a **Minimum probability** threshold.
  Naming a predictor here is what turns the live opt-in hook on for that strategy's matched
  tenants — `proxy/keepalive.go`'s `pingable()` then also requires
  `predictorFor(id)(stopReason) >= threshold` before it pings. No strategy names a predictor
  today, so behaviour is unchanged for every account until a manager opts one in.

## Related docs (full methodology)

- [kv-ttl-predictor-features.md](kv-ttl-predictor-features.md) — the feature catalog: the
  `stop_reason` cluster split, per-tenant heterogeneity, UTC hour structure, the 1h-TTL
  finding, and every feature considered.
- [kv-ttl-predictor-arms.md](kv-ttl-predictor-arms.md) — the rule-based and learned arms,
  scored with rolling-origin splits and bootstrap CIs; per-fold tables; the
  `historical-probability` diagnosis in full.
- [kv-cache-ttl.md](../how-to/kv-cache-ttl.md) — the prior exact-ceiling study (the domain model, the
  cost formulas, the arm registry) plus the sticky-session-1h arm and the real haiku-4-5
  measurement.
- [kv-ttl-predictor-comparison.md](kv-ttl-predictor-comparison.md) — the terminal comparison
  tying feature catalog, arms, and sticky arm together, with the recommendation this page's
  headline restates.
- [kv-ttl-savings-reconciliation.md](kv-ttl-savings-reconciliation.md) — why the Strategies
  tab and the Overview page can show different keep-alive savings numbers (two real,
  unlabeled gaps — cross-strategy double counting and a time-window mismatch — not a math
  bug).

## What's not solved yet

- **`HistoricalProbability`'s cell keying is diagnosed, not fixed.** Its `Stats` cells key on
  `(tenant, model, hour-bucket)`, not `stop_reason` cluster, which is why it can't see the
  signal `stop-reason-gated` exploits (see above). The fix — key `kvcache.History`'s cells on
  `stop_reason` cluster too — is a Go change to a structure `HistoricalProbability`,
  `StickySession1h`, and `Custom` all share. **Assessed for this round and deliberately not
  built**: it changes `History.Observe`'s signature and every cell's fallback ladder, which
  ripples through every strategy that reads `Stats` and every existing historical-probability
  number on this page, and any real claim about the result would need a fresh live-data
  scoring pass (the same read-only aggregate access the arms doc used). That's a bigger,
  riskier change than fits inside "wire two already-built pieces together" — it's a
  structural change to a shared accumulator, not a new arm. **This is the recommended next
  step**, and it's well-scoped: a `kvcache.Strategy` that gates on `stop_reason` (present
  tense, from `Observation.StopReason`, no history needed) AND *then* checks a per-tenant
  `HistoricalProbability`-style threshold keyed with the cluster included, registered exactly
  like `StopReasonGated`/`StickySession1h` were, scored against `fixed-5m` with the same
  rolling-origin/bootstrap method the arms doc used.
- **No per-tenant confidence intervals.** Every per-tenant number above is a point estimate;
  the pooled bootstrap resamples across the whole population, not within one tenant. Building
  one is mechanical (the same conversation-level bootstrap, restricted to one tenant's
  conversations) but wasn't done here.
- **`sticky-session-1h` is built, tested, and registered — unscored on this deployment's live
  (sonnet-5-heavy) traffic.** The one real number (14.6% cheaper) is from a single 3-turn
  haiku-4-5 session, not this corpus.
- **The Strategies-tab-vs-Overview labeling fix has been applied** (this line in the earlier
  draft of this doc was stale). Both notes the reconciliation doc proposed are live in
  `dash/ui/app.js`: one line above the Strategies list table stating pings/cost are additive
  across strategies but Saved is not, and an "(all time, ignores whatever date range the
  dashboard is set to)" label in the ledger drawer.
- **1h-tier findings are simulated for the traffic that carries this deployment's spend**
  (`aws/claude-sonnet-5`) and can only be live-verified on `claude-haiku-4-5`, which the
  gateway actually honours a 1-hour TTL on.
- **No per-tenant timezone exists.** Every hour-of-day feature and finding on this page is
  UTC; a genuinely "local morning" feature needs a tenant-timezone field the store doesn't
  have.
- **`agent`/`model` were left out of the learned arm's (`logreg-v1`) feature set**, even
  though `agent` is flagged as a genuinely distinct population (different client dialects, not
  necessarily the same human-behaviour population). A candidate for a v2 model, not a defect
  in this one.
