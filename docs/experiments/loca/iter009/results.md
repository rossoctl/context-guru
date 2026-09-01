# Iteration 009 — re-scoring the selection experiment for $0: the merged design stays refuted

**Date:** 2026-08-21 · **Cost: $0** (no model calls — all 8,105 decisions were already recorded)
**Script:** `deploy/harbor/selection_rescore.py` · **Corpus:** Claude Code, n=885 candidates

## Why this ran

You approved building the **merged** design — folding the co-reference criterion into the
`extract_llm` call. Reading the code to build it turned up that
[the selection experiment](../../../results/coref-selection-experiment.md) had already measured the
per-output version of exactly that design and **refuted** it (6% live-kept, haiku; 14%, sonnet — both
inside the null model's error bar), and had found the deterministic index beating every model arm with
"no combination of index and model beat it".

But that comparison carried two documented weaknesses, and both were free to test:

1. **A floor asymmetry the doc itself flags as unfixed** — `min_later_turns` is a hard guard in the
   deterministic arms (`score.py:11`) and merely prompt text for the model arms (`runner.py:47,53`).
   The index got a guarantee, the models got a suggestion. Listed under *not settled*.
2. **Ground truth blind to the capability under test** — `future_referenced` is Tier-1 exact
   matching, while the entire argument for a model call is catching Tier-2/3 reuse an exact matcher
   cannot see. A model that correctly keeps a transformed-reuse output is credited nothing.

## Result: both corrections applied, neither rescues the model arms

| ground truth | index (unref only) | haiku bulk | sonnet bulk | sonnet strict |
|---|---|---|---|---|
| **exact** (as published) | 11% fd / **95%** lk | 36% / 58% | 29% / 57% | 30% / 58% |
| **Tier-2 strict** | 21% fd / **92%** lk | 44% / 56% | 39% / 53% | 39% / 53% |
| **Tier-2 loose** (upper bound) | 24% fd / **91%** lk | 46% / 55% | 41% / 52% | 41% / 52% |

*fd = false-drop, lk = live-kept.*

**1. Floor symmetry changes essentially nothing.** The floor overrides only **6–23 of 885** decisions
per arm and moves live-kept by 0–2 points (haiku bulk 58% → 59%; sonnet bulk 57% → 57%). Removing it
from the deterministic arms costs them one point (95% → 94%). **The documented asymmetry is not the
explanation for the gap** — it was a reasonable suspicion and it is now closed.

**2. Widening the ground truth hurts every arm and does not close the gap.** Referenced candidates go
408 → **473** (strict) → 484 (loose), i.e. +16%. Every arm's false-drop rises. The live-kept gap
between the index and the best model arm does not narrow — it slightly *widens* (95 vs 58 → 92 vs 56).

**3. A genuine update to a shipped default, in the unhelpful direction.** `cut_unreferenced`'s
false-drop floor is **21% (strict) to 24% (loose)**, not the published 11%. The "free safe cut" is
roughly twice as lossy as recorded. That revises `coref-selection-experiment.md` finding 4 and matters
independently of the merged question.

**4. Tier-2 blindness cannot be fixed deterministically — which is itself the answer to your
suggestion.** You chose the deterministic option specifically to avoid judge noise, and that was the
right call on noise. But the attempt shows the deterministic slice of Tier-2 is nearly empty: +65 of
885 candidates, from substring (50), case (15) and basename (11). Real Tier-2/3 reuse is semantic, so
**closing this bias needs a judge, and a judge reintroduces the noise that ruled out UltraHorizon**
([measurement-limits §6](../../../results/measurement-limits.md)). The residual blindness means the
gap is a *bounded* claim rather than a clean one — but a 36-point live-kept gap is not one a modest
correction closes.

## Pre-registered reading → Phase 2B

The reading was fixed before the numbers existed: *"bulk unchanged, or corrections touch few decisions
→ merged is settled negatively."* Both happened. **Do not implement bulk adjudication.** Spend the
budget on the 32k reward arm instead.

This does not make the merged idea silly — the mechanism you described (Tier-2/3 and anchor-vs-payload
are judgement calls an exact matcher structurally cannot make) is real, and it is written into the
code's own comments. What the evidence says is that a model asked to make those calls performs *worse*
overall than the blind matcher, and that this survives correcting both known biases against it.

## A fifth rig artifact, caught by audit rather than by luck

My first Tier-2 implementation also matched a path's **stem** (`src/auth.py` → `auth`). It reported
the index's false-drop rising from 11% to **53%** — a dramatic, publishable-looking result, and
entirely an artifact. An audit of which keys the rule fired on:

| collapsed key | distinct identifiers it matched | examples |
|---|---|---|
| `yaml` | **42** | `config.yaml`, `capture.yaml`, `base_model_config.yaml` |
| `json` | 25 | three unrelated output paths |
| `only`, `based`, `agent`, `memory`, `context`, `session`, `task` | 10–22 each | leaked from `Append-only`, `Cost-based` |

A later mention of *any* YAML file was scoring as reuse of a *different* one. That single rule supplied
**237 of 250** gains. The fix reuses `coref.py`'s own `distinctive()` filter rather than inventing a
length threshold — `yaml`, `only` and `auth` all fail it for carrying no interior structure, digit, or
CamelCase — and basename matching is now reported as a separate **upper bound** rather than folded in.

**The lesson is the day's recurring one, now five for five:** the error was in my instrument, not in
the thing measured, and it pointed in the direction that would have made the story more interesting.
A false *match* indicts the deterministic arms for reuse that never happened — the expensive
direction — which is why the conservative default belongs there.

Two smaller instrument corrections, both caught by the reproduce-first gate: `removed%` must count
trims as partial removal and charge arms for candidates they failed to parse, and the live-kept
*denominator* must include parse failures. Approximating those disagreed with the published table by
up to 17 points while looking entirely plausible — sonnet/bulk's 149 parse failures alone moved its
live-kept from 57% to 68%.

## Verification

- **Gate 1** — the exact column reproduces the published table row for row (index 11.8%/11%/95%,
  haiku bulk 40.2%/36%/58%, sonnet bulk 33.6%/29%/57%, haiku digest 90.9%/40%/6%). No re-scored number
  was read until this passed.
- **Gate 2** — candidate re-derivation from the raw captures reproduces `candidates.json` exactly:
  885/885 joined, **0** exact-match mismatches. The Tier-2 columns are only reported when this holds.

## Next

Phase 2B: two arms at 32k, `format` vs `format`+`coref`, two-sided, **margin declared before the run**
(iteration 007's failure). ~$340 for n=30 (harm ≤10%) or ~$510 for n=45 (≤6%). Reward is the axis
nothing has measured, and [iteration 008](../iter008/results.md) established 32k as the only band with
the headroom to measure it.
