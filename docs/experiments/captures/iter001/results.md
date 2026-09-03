# Captures — iteration 001 (what actually fires)

**Date:** 2026-08-20 · **Goal:** before spending on any scored benchmark, find out which
components actually act on captured traffic and which are silently declining. Replay only — no
agent, no task, no reward. **Cost: ~$3.**

Run: `deploy/harbor/replay2.py /tmp/cg-runs/capture-swebench.jsonl --configs ...`, once with
`CACHE_MODE=off` and once with `CACHE_MODE=on`. 200 requests. Binary: `/tmp/cg-runs/cg-proxy-rp`.

## Result

The delta between the two cache modes **is** the tail gate's cost:

| component | non-tail (`off`) | tail-gated (`on`) | effect |
|---|---|---|---|
| `mask` | **50.67%** | **3.33%** | loses **93%** |
| `failed_run` | 1.29% | **0%** | **fully inert** |
| `extract` | 0.84% | 0.84% | unaffected (not tail-gated) |
| `cmdfilter` | 0.23% | 0.23% | unaffected |
| `dedup` | 0% | 0% | never fires on this corpus |

`extract_llm`, separately: **0% saved in every configuration** — gate on and off, cache-aware and
not. Recorded reason `below_output_floor`.

Tool-output sizes, the quantity that explains it:

| corpus | p50 | max | ≥3,000 (its floor) | ≥30,500 (its break-even as then published) |
|---|---|---|---|---|
| SWE-bench, a fresh run made for this pass | 71 | **2,760** | **0** | **0** |
| `capture-swebench` | 106 | 5,674 | a handful | **0** |
| `capture-tb` | ~0 | 1,906 | **0** | **0** |
| LOCA-bench | 185 | **59,857** | **54** | **7** |

## What this proves

- **The tail gate is expensive and undocumented.** `mask` keeps only 7% of its effect on caching
  traffic and `failed_run` none — and both ship in presets.
- **`extract_llm`'s output floor exceeds the largest tool output SWE-bench produces.** It is
  structurally incapable of acting there, and the same holds for Terminal-Bench.
- **`codesmart` therefore saves ~1%** on caching traffic — `extract` 0.84% + `cmdfilter` 0.23%.
- **The binding constraint is tool-output SIZE, not context length**, which is where the argument
  had been focused. That makes LOCA the only benchmark in the set where these components can act.
- A mispricing found here was fixed: the gate charged every saved token at the cache-READ rate,
  including the turn the cut is applied on, which is a cache-WRITE. See
  [`extract_llm`'s doc](../../../components/extract_llm.md).

## What this does NOT prove

- **Replay is not reward.** Nothing here says any cut is safe.
- **`capture-tb` and `capture-swe` are smoke captures** (6 and 2 outputs above 300 tokens). Only
  `capture-swebench` supports a claim, and it is 50 shallow sessions.
- **The ~27% figure could not be reproduced** in any configuration tried. Consistent with
  `config.go`'s own note that the published numbers describe an ancestor of the preset, but not an
  explanation of where it came from.
- **One observation is unexplained and stays that way:** `extract_llm` burned 12.8 s across 20
  requests while `acted=0`, `discarded_changes=0`, nothing above INFO in the log.

## Next levers

1. LOCA, as the only corpus where anything can fire → [loca/iter001](../../loca/iter001/results.md).
2. Re-measure what `mask` actually saves on caching traffic — currently unknown, and the docs no
   longer claim a number.
3. Diagnose the 12.8 s.

## Artifacts

`/tmp/cg-coref/phase0-{on,off}.json`, `/tmp/cg-coref/final-{on,off}.json` on the eval box;
analysis in [component gating](../../../results/component-gating.md).
