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

## Result — INVALID as a reward test, and the reason is the finding

| arm | solved | errors | **solved/12** | cost |
|---|--:|--:|--:|--:|
| **`a64-off`** (baseline) | **4** | **0** | **0.333** | $21.34 |
| `a64-det` | 2 | 1 | 0.167 | $20.39 |
| `a64-full` | 3 | **3** | 0.250 | $16.81 |

Per task, aligned (`1` solved, `0` failed, `E` errored):

```
off:   0 1 0 0 0 0 1 0 1 0 1 0
det:   0 1 0 0 0 0 0 0 1 E 0 0
full:  0 1 0 0 0 1 E 0 1 E 0 E
```

Read naively this says compaction costs reward (4 → 2 and 4 → 3). **It does not, because the
losses are a configuration bug of mine.**

### Every error coincided with a `summarize` firing

| arm | `summarize` firings | task errors |
|---|--:|--:|
| `a64-det` | **1** | **1** |
| `a64-full` | **3** | **3** |
| `a64-off` | 0 | 0 |

All three errors are HTTP 400 **schema** violations, not model failures:

- `messages: Unexpected role "tool"` (×2) — a `tool`-role message reached the provider. Anthropic
  accepts tool results only as **user** messages carrying `tool_result` blocks; `role: tool` is the
  OpenAI shape.
- `messages.1: role 'system' must precede an 'assistant' message` — misplaced `system`.

[`components.md`](../../../components.md) states the cause plainly: `summarize` *"restructures the
whole transcript (changes the message count) — **run it alone** so no other component's in-place
edits race `apply`'s rebuild."* I ran it alongside nine other components. The count correlation is
exact, so the mechanism is not in doubt.

**So the arms measure my misconfiguration, not compaction.** Rerun without `summarize` is
iteration 004b; the baseline needs no rerun (no `summarize`, no errors).

!!! danger "This also undermines [iteration 002](../iter002/results.md)"
    iter002 used the same `summarize`-in-a-pipeline configs and reported **72% fewer
    summarizations**. But it replayed through `/compact`, which never forwards upstream — so the
    malformed bodies were **never validated by a provider**. The same pipeline 400s in production.

    The deferral number is therefore measured on requests that could not have been sent. What
    survives is narrower: the *mechanism* (compaction reduces how often a context max is reached)
    is still sound, but the specific pipeline that produced 71 → 20 is not a shippable
    configuration, and the figure must be re-earned with `summarize` isolated.

    A replay harness that does not forward upstream cannot catch schema violations. That is a
    structural blind spot in every `/compact`-based measurement in this project.

### What did work, and it is the fold

`extract_llm` **acted for the first time in this entire investigation**: 27 firings, **584,125
tokens**, in the `allow_cached_prefix` arm.

| component | `a64-det` | `a64-full` |
|---|--:|--:|
| total saved | 20.0% (1,202,964) | **31.8% (1,680,426)** |
| `format` (lossless) | 1,103,713 (**92%** of that arm's saving) | 722,190 |
| **`extract_llm`** | — | **584,125** (27) |
| `dedup` | 54,171 | 239,408 |
| `coref` | — | 45,454 (4) |
| model calls | 1 | 18 |

The fold adds **~12 points of saving**, and here it is `extract_llm` doing the work while `coref`
contributes little — the **reverse** of iter002, where `coref` did everything and the fold added
nothing. The difference is the band: at 64k there is prefix content large enough to clear both the
output floor and the break-even. That is the first evidence the fold does something no other
configuration achieves.

And once again `format` — a lossless JSON repack — is the single largest lever, at 92% of the
deterministic arm's total.

## Corrections to this page's own design

- The `mean` accuracy I first computed **excluded errored tasks from the denominator**, which
  flattered the compaction arms (0.182/0.333/0.333). An errored task is a failed task; the table
  above uses `/12` throughout.
- Cost came out *lower* for the arms ($16.81–20.39 vs $21.34) but this is **not** a saving: the arms
  errored out of tasks early, and the prompt-cache confound described in the design section applies.
  Cost is not interpretable here.

## Artifacts

`/tmp/cg-loca/out-arms64.log`, `loca-a64-*.log`, `st-a64-*.json`, `shim-a64-*.log`, and LOCA's run
dirs `/tmp/cg-loca/outputs/inf_claude_api_cg_64k_12_*` on the eval box.
Task set: `task-configs/cg_64k_12.json`.
