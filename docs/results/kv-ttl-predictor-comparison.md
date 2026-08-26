# Comparing TTL predictors, pooled and per tenant — real numbers

This is the terminal comparison for the KV-cache TTL predictor work. It draws on
[kv-ttl-predictor-features.md](kv-ttl-predictor-features.md) (the feature catalog),
[kv-cache-ttl.md](../how-to/kv-cache-ttl.md) (the prior exact-ceiling study), and
[kv-ttl-savings-reconciliation.md](kv-ttl-savings-reconciliation.md) (why the Strategies
tab and Overview disagree — resolved, not a bug). All figures below are from the live
deployment, measured read-only, aggregate-only (see the feature catalog doc for the exact
access pattern), 2026-08-26.

## What's currently shipped, measured fresh

Two manager strategies (both 2×5-minute pings, working-hours windows) plus legacy/manual
pings, unconditional on `stop_reason`, on the full 66,779-request corpus:

| | value |
|---|---:|
| total pings | 143 |
| ping cost | $21.64 |
| requests rescued | 34 |
| conversion rate | **23.78%** (76.22% of pings rescue nothing) |
| saved | $28.13 |
| **net** | **+$6.48** |
| pings that arrived late (rewrote, not refreshed) | 4 (2.8%) |

Per tenant, net USD (ascending — this is the "helps some, not others" the manager
reported, now with numbers):

| tenant | pings | ping $ | rescued | saved $ | **net $** |
|---|---:|---:|---:|---:|---:|
| t09 | 23 | 4.73 | 5 | 2.34 | **−2.39** |
| t05 | 3 | 0.93 | 1 | 0.00 | **−0.93** |
| t07 | 6 | 0.77 | 0 | 0.00 | **−0.77** |
| t03 | 18 | 0.57 | 1 | 0.00 | **−0.57** |
| t12 | 8 | 0.42 | 3 | 0.33 | −0.09 |
| t11 | 2 | 0.07 | 0 | 0.00 | −0.07 |
| t06 | 8 | 0.34 | 1 | 0.30 | −0.04 |
| t01 | 15 | 4.46 | 5 | 4.45 | −0.01 |
| t02 | 1 | 0.02 | 1 | 0.28 | +0.26 |
| t08 | 17 | 0.85 | 6 | 2.30 | +1.45 |
| t04 | 32 | 6.23 | 7 | 8.77 | +2.53 |
| t10 | 10 | 2.25 | 4 | 9.37 | **+7.12** |

**8 of 12 touched tenants are net-negative.** t10 alone carries all of the positive net;
t04 and t08 are the only other contributors. This is the quantified version of "helped
some users and not others" that motivated this whole project — a single global policy is
structurally the wrong shape for this traffic.

## Feature findings driving the predictor work (see the feature catalog doc for the full analysis)

- `stop_reason` splits into three clusters: still-working (0.0–0.6% land in the
  addressable 5m–1h band, well under the ~8% break-even), looks-done-isn't (2.9–6.1%,
  still under break-even), actually-done (11.7–43.3%, clears it with margin). The
  currently-shipped mechanism pings on every cluster equally.
- Per-tenant band rate (conditioned on `end_turn`) ranges 0.76%–41.4% — a single
  threshold is wrong for almost everyone.
- 1h TTL: requested on 17.7% of requests, honoured on 0% (silently downgraded on
  `aws/claude-sonnet-5`; only granted on `claude-haiku-4-5`).

## Rule-based and learned arms

Full method, per-fold tables, and feature importances are in
[kv-ttl-predictor-arms.md](kv-ttl-predictor-arms.md). Pooled, against `fixed-5m`
($7,239.02 baseline, rolling-origin split, conversation-level bootstrap, 66,779 requests):

| arm | % vs fixed-5m | 95% CI | distinguishable from zero? |
|---|---:|---:|---|
| `logreg-v1` | +1.69% | [+0.79%, +2.94%] | yes |
| `stop-reason-x-hour` | +1.57% | [+0.63%, +2.77%] | yes |
| **`stop-reason-gated`** | **+1.54%** | **[+0.60%, +2.79%]** | yes |
| `historical-probability-tenant-tuned` | −0.03% | [−0.08%, +0.02%] | **no** |

**The three positive arms are not distinguishable from EACH OTHER** — their CIs overlap
almost completely. `stop-reason-gated` needs no training, no threshold tuning per model
version, and no coefficients to keep in sync with a retrained model, for the same result
the logistic regression gets. **Recommendation: ship `stop-reason-gated` as the default,
not the learned arm** — this is a case where the free rule wins on Occam's razor, not
because the model failed.

`historical-probability-tenant-tuned` (the existing shipped arm's own mechanism, tuned) is
statistically indistinguishable from doing nothing, and the reason is diagnosable: its
`Stats` cells are keyed on `(tenant, model, hour-bucket)`, not on `stop_reason` cluster, so
the ~92.5% "still working" majority swamps the "actually done" minority's signal in every
cell it looks at — the identical blind spot `kv-cache-ttl.md`'s original two ML arms had.
**Concrete next step this measurement points at**: key `kvcache.History`'s cells on
`stop_reason` cluster too, not just tenant/model/hour — a Go change, out of scope for the
Python workstream that found it, in scope for this repo's next iteration on
`kvcache.HistoricalProbability`.

Per tenant, the pooled gain is carried almost entirely by three tenants — and they are the
same three the feature catalog already flagged as sitting at the high end of P(band |
end_turn):

| tenant | P(band\|end_turn) (features doc) | `stop-reason-gated` savings |
|---|---:|---:|
| t12 | 41.4% | **+18.5%** |
| t06 | 29.3% | +8.1% |
| t01 | 37.8% | +5.9% |
| t04 | 11.6% | **−1.5%** |
| t11 | 0.76% | −0.13% |

`historical-probability-tenant-tuned` correctly avoids the small loss on t04/t11 but also
fails to capture the gain on t01/t06/t12 (per the diagnosis above). **The two are
complementary failure modes**: combining `stop_reason` gating with a per-tenant
historical-probability gate — ping only when both the turn looks done AND this tenant's
own history clears its own break-even — needs no new mechanism, only wiring the two
already-built pieces together, and is the next thing worth building before the Go-side
`History` fix above.

Top features (both models agree): `Turn` (later turns are less likely to land in the
rescuable band — consistent with `pingable()`'s own "single-request sessions are 79% of
pings, 0.9% of value"), then excluding the "still working" `stop_reason` cluster — ahead of
tenant identity, ahead of time-of-day, ahead of every rolling-gap feature.

## The sticky whole-session 1h-vs-5m arm

Full detail in [kv-cache-ttl.md](../how-to/kv-cache-ttl.md#the-sticky-whole-session-arm-and-a-real-haiku-4-5-measurement).
`kvcache.StickySession1h` decides once, at a conversation's first request, whether to
commit that whole session to the 1-hour or 5-minute tier, and never revisits the choice —
modeling the real constraint that a created cache entry cannot be renegotiated in place.
It reuses the same break-even `cacheinject.go`'s own TTL doc and this dashboard's own
`Raise5mTo1h`/`SavedPerMiss` already derive, against the account's own `P(return within
1h)`. Built, registered, and tested against the same "never beats `optimal`" / "never sees
the future" invariants every arm here is held to.

**Its simulated performance on this deployment's dataset is not reported** — scoring it
would need the same aggregate-only DB access this doc's other figures use, and that step
was deliberately not improvised for this arm. It stands built and tested, unscored.

**The one real, directly-measured number**, from an actual 3-turn session run through a
local proxy against `claude-haiku-4-5` (not sonnet-5 — the model this gateway actually
grants a 1-hour TTL on) with a genuine 392-second gap between turns 2 and 3:

| | 1-hour TTL (real, billed) | 5-minute TTL (hand-computed from the same real tokens) |
|---|---:|---:|
| total, 3 turns, 1 gap | **$0.019594** | $0.0229506 |

**1h was 14.6% cheaper on this one real trajectory.** `cache_write_1h` matched
`cache_write` exactly on all three turns — confirming the tier was genuinely granted,
unlike every one of the 11,822 live `ttl:"1h"` requests on `aws/claude-sonnet-5` in the
main corpus, all of which show `cache_write_1h = 0`. This is a real, small,
single-session number offered as a sanity check that the sticky arm's underlying trade is
real on a model this gateway grants it on — not a corpus-wide claim, and not evidence for
what `sticky-session-1h` would do on this deployment's actual (sonnet-5-heavy) traffic,
which remains unscored.

## Recommendation

**Pooled default: gate every keep-alive ping on `stop_reason` — ping only when the last
turn's `stop_reason` is `end_turn`, `max_tokens`, or `refusal`; never on `tool_use`,
`stop_sequence`, `tool_calls`, `length`, `content_filter`, `stop`, or unset.** This is a
one-line change to `proxy/keepalive.go`'s `pingable()`, needs no training, no model
artifact to keep in sync, and is statistically indistinguishable from the fanciest arm
tried. Estimated pooled effect: +1.54% vs `fixed-5m`, CI95 [0.60%, 2.79%] — real, not
overclaimed given the CI.

**Per-tenant refinement, not yet shipped**: the naive global gate loses a little money on
`t04`/`t11` while winning a lot on `t01`/`t06`/`t12`. Before rolling the gate out
unconditionally, either (a) accept the net-positive pooled trade (the loss on t04/t11 is
small — −1.5%/−0.13% — against the gain elsewhere), or (b) build the combined
stop_reason-AND-per-tenant-history gate the arms doc's own diagnosis points at, which this
work did not build. (b) is the better long-term answer and is now a well-scoped follow-up:
wire `stop-reason-gated` and a per-tenant `HistoricalProbability` threshold together, no
new mechanism required.

**1h-tier session decision**: see the sticky-session section above/below (pending the live
haiku measurement) — its savings are simulated for the sonnet-5 traffic that carries this
deployment's spend, and only get a real live number on haiku-4-5.

**Not recommended**: the per-tenant-tuned `historical-probability` arm as currently keyed
(tenant/model/hour, no `stop_reason`) — statistically indistinguishable from doing
nothing, for a diagnosed and fixable reason (see above), not because tuning failed.

## Honest limits

- 17 tenants, and the addressable signal is concentrated: t11/t02 (the two highest-volume
  tenants) have the lowest band rates, so a volume-weighted pooled figure understates the
  opportunity for the tenants it actually matters for.
- No per-tenant timezone exists; every hour-of-day feature is UTC.
- 1h-tier savings are simulated for the traffic that carries the spend (sonnet-5) and can
  only be live-verified on haiku-4-5.
- Time-based rolling-origin splits only — a random split would leak future information the
  whole `kvcache.History` design exists to prevent.
