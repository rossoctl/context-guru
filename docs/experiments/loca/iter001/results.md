# LOCA-bench — iteration 001 (first replay) · ⚠️ RETRACTED

!!! danger "Retracted — the setup made every request a cold first turn"
    This run sent **one request per conversation** (the deepest). Each was therefore turn 1 with
    `MaxCachedIdx = -1`, so **the tail gate never engaged**. That inflated `mask` to essentially
    its non-tail figure and produced a "`mask` removes 8.3× more than `coref`" comparison that is
    an artifact of the harness, not a property of the components.

    It also reported **zero `closed`** outputs on LOCA, which is false — `closed` requires a
    reference that has since gone stale, which cannot exist when every request is turn 1.
    [iteration 002](../iter002/results.md) surfaces 25 of them.

    Kept for the record. The cause is more instructive than the numbers were.

**Date:** 2026-08-20 · **Goal:** get `coref` to act on real traffic for the first time.
**Cost: $0** (deterministic arms). Binary: `/tmp/cg-coref/cg-proxy-fix`.

Substrate: the 9 deepest real request bodies from the LOCA capture, 3.34 MB, tool-output mass
4,940–232,505 tokens each, replayed cache-aware.

## Result (as measured — see the retraction)

| arm | acted on | tokens saved | shrink |
|---|---|---|---|
| `mask` | 9/9 | 402,135 | **52.3%** ← inflated |
| `coref` | 2/9 | 48,532 | 6.5% |
| `coref` + `cut_closed` | 2/9 | 48,532 | 6.5% — identical |

Classification over the 148 outputs above the 300-token floor: **142 (96%) `open`**, 6 `opaque`,
**0 `closed`**, ~17 cut as `unreferenced`.

## What survived the retraction

- **`coref` does act on real traffic** — 48,532 tokens. That much is real and was the point.
- **96% of eligible outputs are `open`.** LOCA's agents reference tool results immediately and
  repeatedly, so the detector correctly reports little is safe to remove. Reproduced in iter002.
- **A live observation of the `skipReduce` first-refusal interaction:** an earlier probe ran
  `[mask, coref, extract]` together and reported `coref` doing nothing. `mask` ran first and
  replaced every output with a short marker, so `coref` saw only sub-floor content. **Arms must be
  run one component at a time**, which is how iter002 was built.

## What this does NOT prove

Everything comparative. The `mask`-vs-`coref` ratio is a harness artifact; the `closed` count is
wrong; no reward was measured.

## Next levers

1. **Sequential replay** so the cache boundary advances → [iteration 002](../iter002/results.md).
2. Reward, which replay cannot give.

## Artifacts

`/tmp/cg-coref/loca-replay.jsonl` (9 bodies), `/tmp/cg-coref/arm-*.log` on the eval box.
