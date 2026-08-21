# LOCA iteration 008 — the 32k band headroom probe

**Date:** 2026-08-21
**Question:** is the ~20% base solve rate at 64k a property of LOCA, or an artifact of the band?
**Config:** upstream `final_32k_set_config.json` — the **same 15 tasks × 5 seeds** as the 64k set, so
this is a matched comparison: identical tasks, smaller data volume.
**Path:** LOCA → `repair_shim.py` (**fixed**) → benchmark gateway. **No context-guru in the path**,
deliberately — this measures the band, not a component.
**Cost:** $85.10 ($5.67/task) · **Result: the band was the problem.**

## Result

| | 32k | 64k (matched tasks) |
|---|---|---|
| **solve rate** | **53%** (8/15) | 25% (3/12) |
| errors | **0/15** | 3/15 |
| cost/task | **$5.67** | $7.59 |

Per task, 32k solved four that 64k did not (`CanvasArrangeExam`, `CourseAssistant`, `SetConfCrDdl`,
`UpdateMaterialInventory`), lost one (`ExcelMarketResearch`), and cleanly ran all three that 64k
errored on.

**Three findings:**

1. **The thin-headroom problem was an artifact of band choice.** 53% is not merely better than 25%,
   it is near the *optimum* for detecting change: sensitivity to both improvement and degradation is
   maximal at a 50% base rate. The 64k band was starving every reward arm of headroom in both
   directions.
2. **The shim fix is confirmed on live traffic.** 0 errors in 15 tasks, against 3 in 15 at 64k
   through the broken shim. This is independent of the isolated two-process test.
3. **It is also cheaper** — $5.67 vs $7.59 per task, because the smaller data volume means shorter
   trajectories.

## The band still has compaction pressure

The obvious worry: if 32k contexts never grow, coref has nothing to act on and the band is useless
for our purpose. Measured (approximate, ~4 chars/token, from cumulative step payloads):

| band | peak context, median | p75 | max | steps, median |
|---|---|---|---|---|
| 32k | **~45k tokens** | ~52k | ~56k | 12 |
| 64k | ~64k tokens | ~83k | ~213k | 10 |

So 32k still builds 45–56k-token contexts — ample material. **The tradeoff is real and must be
stated:** less context means less to remove, so absolute savings will be smaller at 32k than at 64k.

**That argues for splitting the endpoints across bands, which is legitimate because savings do not
need reward headroom:**

- **Savings** — measure at 64k, where it is already precisely measured (`coref` acted on 54/1271
  requests, ~981k tokens, in [iteration 007](../iter007/results.md)). Thousands of observations,
  no significance test required.
- **Reward** — measure at 32k, the only band with headroom in both directions.

## What each budget now buys

Two-sided (per the two-sided-effect argument in
[measurement-limits §1](../../../results/measurement-limits.md)), at $5.67/task, 2 arms:

| pairs | cost | harm bound if 0 harmed | requires |
|---|---|---|---|
| 15 | $170 | ≤ 18% | nothing — grouped as-is |
| 30 | $340 | ≤ 10% | `group_by_seed=False` |
| 45 | $510 | ≤ 6% | `group_by_seed=False` |
| 75 | $850 | ≤ 4% | full set, all 5 seeds |

Same money goes measurably further at 32k than at 64k:

| budget | 32k | 64k |
|---|---|---|
| $400 | n=35, harm ≤ 8% | n=26, harm ≤ 11% |
| $600 | n=52, harm ≤ 6% | n=39, harm ≤ 7% |

**Caveat on n from seeds:** reaching n>15 means patching `group_by_seed`, and the extra pairs are
5 seeds of the same 15 tasks — correlated, not independent. The effective n is therefore below the
nominal count, and any analysis should cluster by task rather than treat 75 as 75 free observations.

## Recommendation for the re-cut

Two arms at 32k — `format` (lossless baseline) vs `format`+`coref` — with the margin declared in
advance, which is what [iteration 007](../iter007/results.md) failed to do. Approximately $340–$510
buys a ≤10%–≤6% bound, against the ≤26% that stage 1 bought for $215.

Not yet addressed by this probe, and still open: the **merged** design (co-reference criterion inside
`extract_llm`'s prompt), `summarize` on provider-validated traffic, and `summarize` + `cachesplit`.

## Rig notes

- `cli-mcp-server` fails to start here as everywhere (`'Server' object has no attribute
  'list_tools'`), so the terminal MCP never registers. Checked and dismissed as a cause in
  [iteration 007](../iter007/results.md): these tasks issue zero terminal calls.
- The tracebacks in the run log are overwhelmingly the *agent's own* generated Python failing
  (`NameError: name 'students' is not defined`), which is normal agent behaviour and not a rig fault.
  A monitor filter matching `Traceback` is therefore useless here.
