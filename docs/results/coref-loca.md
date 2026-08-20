# `coref` on LOCA-bench — the first time it has acted on real traffic

[Component gating](component-gating.md) established that SWE-bench and Terminal-Bench have tool
outputs an order of magnitude below the thresholds these components need, leaving **LOCA-bench as
the only benchmark in the set where anything can fire**. This is that run.

It is the first measurement in which `coref` actually acts on real captured traffic rather than
being scored offline. It cost **$0** (deterministic arms only) and the result is **not favourable**.

Substrate: the 9 deepest real request bodies from the LOCA capture (one per conversation), 3.34 MB,
tool-output mass 4,940–232,505 tokens per request. Replayed through the proxy's `/compact` endpoint,
cache-aware (`CACHE_MODE=on`).

## Result

| arm | requests it acted on | tokens saved | request shrink |
|---|---|---|---|
| **`mask`** (age-based, ignores references) | **9/9** | **402,135** | **52.3%** |
| **`coref`** (shipped defaults, `min_tokens: 300`) | **2/9** | **48,532** | **6.5%** |
| `coref` + `cut_closed: true` | 2/9 | 48,532 | 6.5% — **identical** |

Per-request, `coref` acted on two: 46.6% and 11.2%. The other seven were left byte-identical.

**`mask` removes 8.3× more than `coref` does.**

## Why: on LOCA almost everything is still live

Classification across the 148 outputs that cleared the 300-token floor (292 fell below it):

| verdict | outputs | |
|---|---|---|
| **`open`** — referenced recently or ≥3 times | **142 (96%)** | keep |
| `opaque` — introduced nothing trackable | 6 | never cut |
| `closed` | **0** | — |
| `unreferenced` — cut | ~17 | the 48,532 tokens |

**96% of eligible outputs are `open`.** LOCA's agents reference their tool results immediately and
repeatedly, so the reference detector — working correctly — reports that there is almost nothing
safe to remove.

This is the same signature [the density pass](coref-density.md#and-loca-behaves-exactly-as-8-predicted)
found on the LOCA trajectories, reproduced here through a completely different path (live pipeline
rather than offline scoring): references are *immediate-and-repeated* or invisible, never
"taken once and abandoned".

### `cut_closed` is inert on LOCA, not merely low-yield

The `cut_closed` arm is byte-for-byte identical to the default because **there are zero `closed`
outputs**. The knob that was held back for a corpus that could justify it turns out to have no
effect on the corpus where the component can otherwise act at all. That settles a question the
density pass could only bound: `cut_closed`'s 0% on LOCA is structural, not a sampling artifact.

## The experiment this sharpens

`mask` removes **353,603 tokens that `coref` classifies as still live**. Exactly one question
decides which component is right, and it is the question that has never been asked:

> **Does `mask`'s extra cutting cost reward on LOCA?**

- If `mask` is reward-neutral there, `coref`'s caution is buying nothing and the component's whole
  premise fails on this workload — the only one where it can act.
- If `mask` loses reward, the 353,603-token gap is precisely the damage `coref` exists to prevent,
  and *that* is the product.

This is a far cheaper and sharper question than the reward-parity arm originally planned for
SWE-bench, and it is well-posed because both arms are deterministic: same corpus, same requests,
no sampling.

## Caveats

- **n = 9 requests, one per conversation.** Enough to show the direction and the mechanism; not a
  calibration.
- **No reward.** Nothing here says whether either arm's cuts are safe. That is the whole point of
  the question above.
- **Deepest-request-only.** The capture emits size-only records for non-final turns, so only the
  final request per conversation carries a full body. A real session would fire `coref` once at a
  threshold crossing, not on the deepest request in isolation.
- **LOCA is the adverse corpus for this detector** by design — Tier-2/3 heavy, which the proposal
  predicted. A poor result here is not evidence about Tier-1-rich long-horizon traffic, and no
  benchmark in the set provides that (SWE-bench is Tier-1-rich but its outputs are tiny).
- One measurement mistake worth recording: an earlier probe ran `[mask, coref, extract]` together and
  reported `coref` doing nothing. `mask` ran first and replaced every output with a short marker, so
  `coref` then saw only sub-floor content. That is the `skipReduce` first-refusal interaction
  [documented in the proposal](../proposals/coref-compaction.md#5-hard-constraints-the-codebase-imposes),
  observed live — and a reminder that these arms must be run **one component at a time**.

See also: [component gating](component-gating.md) · [density](coref-density.md) ·
[eval box](coref-evalbox.md) · [the proposal](../proposals/coref-compaction.md)
