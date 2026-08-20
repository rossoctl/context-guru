# LOCA-bench — iteration 004 (reward under real pressure)

**Date:** 2026-08-21 · **Goal:** the reward gate, at a band that can actually answer it.
**Status:** running — this page states the design and the pre-registered reading before the
numbers land, so the interpretation cannot be fitted to them afterwards.

## Why the 64k band, and why not the others

[iteration 003](../iter003/results.md) established the instrument's shape the hard way. Neither
of the obvious bands works:

| band | runs? | baseline accuracy | verdict |
|---|:--:|:--:|---|
| 8K (`debug`) | yes | **1.0** (saturated) | good regression control, but only `format` fires — says nothing about `coref` |
| 128k | yes (post-shim) | **0.0** | genuine context-rot collapse; a zero floor at n=1 measures nothing |
| **64k** | **yes** | **1/3 partial** | **the only band with pressure *and* headroom** |

64k baseline probe (3 tasks): `ExcelMarketResearch` 1.0, `CanvasListTest` 0.0,
`CourseAssistant` 0.0 — all completing cleanly, 0 pairing repairs needed.

## Design

**12 tasks**, one per distinct environment (variety, so one flaky environment cannot dominate),
excluding `NhlB2bAnalysis` whose 232k-token tool output makes runs slow without adding signal.

| arm | pipeline |
|---|---|
| `a64-det` | `codesmart` − `extract_llm`, + `summarize` at a context max |
| `a64-full` | + `coref` + `extract_llm` with `allow_cached_prefix` |
| `a64-off` | passthrough baseline |

Chain: `LOCA → repair shim → cg-proxy → litellm gateway`. Deterministic GEM scorer, no LLM judge.

**Arm order puts the baseline LAST, deliberately.** iteration 003 found that back-to-back arms
share the provider's prompt cache — `cache_read` identical to the byte, `cache_write` falling to 0
after the first arm. Whichever arm runs first pays the prefix write and the rest ride free. Running
`off` last means the compaction arms cannot get a free ride from it. This does not remove the
confound; it stops it flattering the arms under advocacy. **Cost is therefore reported with that
caveat and never as a clean saving.**

## Pre-registered reading

Decided before the numbers, so the conclusion is not fitted to them:

- **Arms ≥ baseline** → compaction is reward-neutral-or-better under real pressure. Combined with
  [iteration 002](../iter002/results.md)'s 72% fewer summarizations, that is the first genuinely
  positive case for the component.
- **Arms < baseline** → the cuts cost tasks. `coref` fails its own gate, and the 11% false-drop
  measured in [the selection experiment](../../../results/coref-selection-experiment.md) is doing
  visible damage.
- **All three identical** → the pipeline is not engaging even at 64k, LOCA cannot test this at any
  feasible band, and the reward question moves to UltraHorizon.

**Power:** n=12 detects a gross effect only. A 1–2 task difference is noise at this size and will be
reported as noise, not read as a result. The corpus-level guard from
[§8](../../../proposals/coref-compaction.md#8-consequences-for-benchmark-selection) applies: do not
stop at first significance.

## Result

_Pending._

## Artifacts

`/tmp/cg-loca/out-arms64.log`, `loca-a64-*.log`, `st-a64-*.json`, `shim-a64-*.log`, and LOCA's run
dirs `/tmp/cg-loca/outputs/inf_claude_api_cg_64k_12_*` on the eval box.
Task set: `task-configs/cg_64k_12.json`.
