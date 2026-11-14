# Keep-alive predictor arms, round 3: four new arms, two missing statistics, issue #326

Condensed pointer page. The full report -- config cards, every table, the plots, the
discrepancy this round found, and the explicit "not reached" list -- lives at
`reports3/experiments.md` (not committed; scratch per this project's convention for
long-form study writeups, same as `reports2/KA-predictor.md` and
`paper/keepalive-predictor-study.md`). This page states the headline and points there.

Extends the `stop_reason`-gated rule / oracle page (`kv-ttl-predictor-arms.md`) and a
second, not-yet-merged page ("A per-tenant cap beats every predictor we've built",
2026-09-28, `kv-ttl-keepalive-generalization-2026-09.md`, currently on `night0928/ka-work`)
whose own established numbers this round reproduced exactly before adding anything (see
`runs/ka_arms_baseline.json` under this round's scratch output).

## Headline

Four new predictor arms were built (`gbm-policy`, `ensemble-hg2-gbm`, `cost-aware-simple`,
`cost-aware-hazard`) on the same feature set and tenant-threshold-with-fallback machinery
the prior page's `hybrid2` uses. **None beats the established `flat-cap-2` rule
(+$619.50/+5.78%)**; the best new arm, a calibrated ensemble, reaches +$476.79/+4.45%,
below even the prior page's own `hybrid2` (+$525.10/+4.90%). The two cost-aware arms --
which feed a better-calibrated probability into the already-correct `policy_expected_cost`
dollar rule -- land at **exactly** the same near-zero result the prior page found with a
different, less calibrated probability source, extending that finding rather than repeating
it: the decision population, not model quality, is the bottleneck.

Both statistics flagged as never computed anywhere in this line of work are produced for the
first time: **learning curves** (AUC and net-$ vs. training-set size, scored on a genuinely
untouched final holdout fold) and the **full per-ping cost percentile curve**, including p90
($0.1717) and max ($0.3800) -- previously only p50/p99 were known, and this round's own p50
($0.0354) and p99 ($0.3403) do **not** match that previously-quoted pair; the discrepancy is
reported, not resolved (full detail and two candidate reconciliations that also failed to
match, in the full report).

Issue #326 (`ReuseModelV1` + `stop_cluster`) is answered at a bounded, explicitly stated
scope: adding `stop_cluster` to the reference 13-feature hazard model improves AUC by
+0.69pp (0.7746 -> 0.7815) and stake-weighted average precision by +0.62pp on a held-out
split -- real but modest, and not currently worth widening the `kvcache.Observation` seam
for, since `stop_reason` is not a field that interface carries. This is a feature-value
answer only, not a re-derivation of the $ budget policy.

A fleet-wide net-$-vs-ping-budget sweep (N=1..20; prior work only tried 1, 2, 6) finds the
flat optimum at **N=3** (+$620.87), a hair above the established N=2 (+$619.50, inside
noise) and turns net-negative by N=16.

## What's new vs. reproduced

See `reports3/experiments.md`'s own table; briefly, the reproduction (flat-cap-2, hybrid2,
per-tenant best-of, calibration, LOTO/LOMO) matched the prior page's published figures, and
everything past that point in this page's headline is new.

## Configuration

Snapshot `snap/cg.db` + `cg-control.db` (`mode=ro`), `#324`-patched local `prices.yaml`
copy, `train_frac=0.6`, 3 rolling-origin folds (fold 2 held untouched by any fitting or
tuning step), seed 0, 400-rep bootstrap, 18 tenants, 261,895 requests. Full card with exact
UTC boundaries in `reports3/experiments.md`.

## Reproduction

```
deploy/harbor/kv_ttl_arms_ext.py --db <cg.db> --prices <prices.yaml> \
  --train-frac 0.6 --folds 3 --bootstrap 400 --seed 0 --out arms_ext_result.json \
  --ping-cost-out ping_cost_distribution.json --funnel-out funnel.json \
  --gap-dist-out gap_distribution_sample.json --learning-curve-out learning_curves.json

deploy/harbor/kv_ttl_per_tenant_cap.py --db <cg.db> --prices <prices.yaml> \
  --candidates 1,2,3,4,5,6,8,10,12,16,20 --json ping_budget_sweep.json

deploy/harbor/kv_ttl_reusemodel_stopcluster.py --db <cg.db> --prices <prices.yaml> \
  --out reusemodel_stopcluster_result.json

deploy/harbor/kv_ttl_plots.py --ka-arms ka_arms_baseline.json --ext-arms arms_ext_result.json \
  --ping-cost ping_cost_distribution.json --funnel funnel.json --gap-dist gap_distribution_sample.json \
  --learning-curve learning_curves.json --ping-budget-sweep ping_budget_sweep.json \
  --out-dir plots/ --aggregates-out aggregates_plots.json
```

Every script reads `mode=ro` only; none writes to a live path. Tenant references are
`t01`..`t18` (sorted by raw tenant id) everywhere, checked by grep against every output file
before publishing -- zero raw-id matches.

## Not reached

A cost-aware arm that beats `flat-cap-2`; cheap text/regex features from `request_content`
(skipped, not attempted, over transcript-content-leak risk); a full budget-policy
re-derivation with `stop_cluster` wired through `ReuseModelV1`'s backward induction;
resolving the per-ping-cost discrepancy above; a live randomized keep-alive holdout; gateway
ping-rate/capacity implications of a higher `max_pings`; a true dollar-weighted AUC for the
two new calibrated arms (a stake-weighted average-precision proxy was used for the
`stop_cluster` side experiment only). Full detail in `reports3/experiments.md`.
