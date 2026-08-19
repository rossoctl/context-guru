# Is the deferral prize reachable at all?

[The proposal](../proposals/coref-compaction.md) calls deferring the agent's own compaction its
largest claimed win. [Implementation status](../proposals/coref-implementation.md) calls how often
that prize is reachable **"the largest unexamined claim in the proposal"**, and notes it needs no
API budget and no eval box. This is that measurement. It cost **$0**.

It also found a defect in how every earlier measurement in this repo read its corpus, and one
result that is the **first positive finding** for the proposal.

Reproduce: `python3 deploy/harbor/coref_reachability.py`

## 1. The prize does not exist in most sessions — **measured**

Counted over real `isCompactSummary` events, i.e. the points at which Claude Code actually
compacted itself. Not reconstructed, not inferred.

| population | sessions that ever compacted | events |
|---|---|---|
| all main-conversation transcripts | **6/35 (17%)** | 9 |
| ≥ 50 model turns | 5/30 (16%) | 7 |
| ≥ 100 model turns | 5/22 (22%) | 7 |
| ≥ 200 model turns | **5/17 (29%)** | 7 |

So the prize is a **long-session feature that is still absent from two thirds of long sessions**.
Any expected-value argument for `coref` has to be multiplied by ~0.17–0.29, and that multiplier
has been missing from every version of the proposal.

Subagent transcripts (197 of them, under `<session>/subagents/`) are excluded: they are separate
conversations with their own context, so counting them would inflate the denominator with sessions
that structurally cannot reach a threshold.

## 2. The corpus cannot support absolute request sizes — **methodological defect, quantified**

The first attempt at this measurement produced a "pre-compaction request" of **777,339 tokens** on a
200k model. That is impossible, and chasing it found a real problem.

A Claude Code transcript is **not a linear conversation**. It is a tree: `--resume` and message
edits fork it, and the compacted transcripts here carry **25–51 forks and 338–632 leaf entries**
each. Reading the file in order concatenates every abandoned branch. Worse, the `parentUuid` graph
is too fragmented to fix by walking it — the longest reconstructible root→leaf chain collapses to
**5–78 entries out of 1,486–5,217**.

**So absolute request size is not recoverable from this corpus**, and any figure derived from a
linear read spans multiple context windows rather than describing one request.

!!! success "But share-based results are safe, and that was worth checking rather than assuming"
    If branches duplicated content heavily, every class share and reference statistic in
    [the density pass](coref-density.md) and [the selection experiment](coref-selection-experiment.md)
    would be contaminated. Measured directly: exact-duplicate tool outputs are **16% by count but
    only 3% of mass pooled, 2% median per transcript, 8% worst case**. The duplicates are small
    repeated reads, not the large outputs the measurements turn on.

    Those documents' **share**-based findings therefore stand. What does not stand is any absolute
    size claim — and the density doc's peak-request and deficit columns were already labelled
    artifacts of 180k segmentation, so the two caveats compound rather than conflict.

## 3. Firing at the crossing removes the deficit term — **the first positive finding**

The requirement is `required cut ≥ (usage − threshold) + growth × headroom`. The density pass
measured the first term at **7.3% of the request** and concluded H=40 was unreachable (0/19).

But that term is an artifact of measuring **after** the crossing — `cc_capture.py` segments at
180k, which places the measurement point past the threshold by construction. **At the moment the
agent compacts, usage *is* the threshold, by definition — that is what triggered it.** So a pass
that fires at the crossing faces only the growth term:

```
required cut  ≈  growth_per_turn × headroom_turns
```

which needs no absolute size measurement at all — only growth per turn, which is a ratio of two
quantities the branch problem inflates alike, and therefore robust.

Measured growth in the 6 sessions that actually compacted: **min 201, median 239, max 9,765
tok/turn** (the max is one short session and not representative).

| headroom bought | required cut | as share of a 167k request | `unreferenced` (4.4%) | `+closed` (9.6%) |
|---|---|---|---|---|
| H = 20 | 4,778 | 2.9% | **yes** | yes |
| H = 40 | 9,555 | 5.7% | no | **yes** |
| H = 60 | 14,333 | 8.6% | no | **yes** |
| H = 80 | 19,110 | 11.4% | no | no |

**This is the first clause of [the hypothesis](../proposals/coref-compaction.md#the-hypothesis-this-proposal-should-be-tested-against) that does not fail.** Fired at
the crossing rather than at maximum pressure, the available cut is enough for 20–60 turns of
headroom. It vindicates the proposal's own counter-intuitive claim that **the profitable moment to
compact is earlier than the moment of maximum pressure** — which had been argued from cache
economics and is now also true of the deferral prize.

### And it is sensitive to the growth estimator, which is not pinned down

The density doc reports **~514 tok/turn** from a different estimator (request-size deltas inside a
180k segment); this script measures **239** (total content ÷ model turns). The verdict flips:

| growth estimate | H = 20 | H = 40 | H = 60 |
|---|---|---|---|
| 239 tok/turn (this script) | `unreferenced` suffices | needs `cut_closed` | needs `cut_closed` |
| 514 tok/turn (density doc) | needs `cut_closed` | **neither** | **neither** |

`cut_closed` **ships off by default**, so even the optimistic column requires enabling a knob whose
yield ranges 0–15% by workload. Pinning the estimator down needs request-level data this corpus
cannot provide.

## What this settles

| claim | status |
|---|---|
| How often the deferral prize is reachable | **Answered: 17% of sessions, 29% of long ones.** Was unmeasured. |
| Can `coref` supply the required cut? | **Yes for 20–60 turns if it fires at the crossing** — reversing the density pass's 0/19, which measured a late fire. |
| Is that robust? | **No.** It flips on a growth estimator two measurements disagree about by 2×. |
| Do absolute request sizes from Claude Code transcripts mean anything? | **No** — tree-structured transcripts, unreconstructable branches. Share-based results unaffected (3% duplicate mass). |
| Reward | **Still unmeasured**, still the gate. Nothing here touches it. |

Every "yes" above inherits the **11% false-drop** measured in
[the selection experiment](coref-selection-experiment.md), and applies only inside the 17–29% of
sessions where the prize exists at all.

See also: [the proposal](../proposals/coref-compaction.md) ·
[density](coref-density.md) · [selection experiment](coref-selection-experiment.md) ·
[implementation status](../proposals/coref-implementation.md)
