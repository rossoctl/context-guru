# Deferring summarization on LOCA-bench

Does paying a cache-write to compact the full request body **defer the summarization** an agent
otherwise has to run when its context fills? That is the largest claimed win in
[the proposal](../proposals/coref-compaction.md), and this is the first end-to-end measurement of it.

**Result: yes, and by a lot — 72% fewer summarizations.** But not for the reason the design
predicted, and the measurement found a design error worth more than the number.

Total spend: **~$0.16** across five arms.

## Setup

LOCA-bench, because [component gating](component-gating.md) established it is the only benchmark in
the set whose tool outputs clear these components' thresholds *and* whose requests reach real context
pressure (median 52,027 tokens, max 233,288 — SWE-bench never exceeds 46,405).

**Sequential replay.** LOCA traffic is append-only, so turn *k* of a conversation is reconstructed as
`messages[0:k]` of its final request, cut at assistant boundaries: **197 turns across 9
conversations**, replayed in order under a stable per-conversation session id so `MaxCachedIdx`
advances turn by turn as it does in production.

!!! warning "This supersedes an earlier version of this page, which was wrong"
    The first run sent **one request per conversation** (the deepest). Every request was therefore a
    cold first turn with `MaxCachedIdx = -1`, so the tail gate never engaged. That inflated `mask` to
    52.3% — essentially its non-tail figure — and produced a "`mask` removes 8.3× more than `coref`"
    comparison that was an artifact of the setup. It also reported **zero `closed`** outputs on LOCA,
    which the sequential replay shows is false (25 of them): `closed` requires a reference that has
    since gone stale, which cannot exist when every request is turn 1.

**`summarize` is wired at a 60,000-token context max and runs last**, mimicking an agent compacting
when full. It therefore fires only when compaction failed to keep the turn under the max, which makes
**firings the deferral measurement**.

## Result

| arm | pipeline | **summarizer firings** | vs baseline | request shrink |
|---|---|---|---|---|
| **S1** | `summarize` alone | **71** | — | 64.6% |
| **S2** | `codesmart` − `extract_llm` | **46** | **−35%** | 58.2% |
| **S3** | + **tail** `extract_llm` | **46** | **+0** | 58.2% |
| **S4** | + `coref` | **20** | **−72%** | 45.6% |
| **S4b** | + `extract_llm` **prefix reach** | **20** | −72% | 45.6% |

Shrink *falls* as firings fall, and that is the correct direction: `summarize` is the largest single
shrinker, so deferring it removes its contribution. Shrink is not the objective here — firings are.

Three findings, in order of how much they matter.

## 1. Deferral works, and it is `coref` that does it — **measured**

**72% fewer summarizations.** `coref` acted 28 times, removing 1,008,646 tokens, and that alone took
firings from 46 to 20. This is the first clearly positive end-to-end result for this line of work.

The deterministic pipeline gets a third of the way there (71 → 46) for free. **The tail
`extract_llm` lever adds exactly nothing** (S2 and S3 are byte-identical), consistent with every
other measurement of it on this corpus.

## 2. The fold engages correctly and contributes nothing — **measured**

`allow_cached_prefix` demonstrably worked. The gate counters are the proof:

| gate | S4 (prefix reach off) | S4b (on) |
|---|---|---|
| `cached_prefix` | 6,597 | **gone** |
| `cached_prefix_above_floor` | 279 | **gone** |
| `prefix_still_referenced` | — | **6,519** |
| `economic_gate` | 25 | **103** |

The blanket refusal is replaced by the index pre-filter, which rejected **6,519 prefix candidates for
free**; the economic gate then declined what survived. And the outcome is **byte-identical** to S4 —
same 20 firings, same 45.6%, `extract_llm acted=0`.

Two reasons. 98.8% of prefix candidates are still referenced (matching the 96% `open` share). And the
handful that pass cannot clear break-even.

## 3. The pre-filter selects the wrong class — **a design error, and the useful result**

The deeper problem is not yield. **For the `unreferenced` class, dropping strictly dominates
trimming.** The pre-filter hands `extract_llm` only content the index has shown nothing ever used —
and for that content a model call can at best preserve *part* of what is already spent, while paying
a call and a cache-write. `coref` removes it outright for free. There is no work for the model to do
in the class it is allowed to see.

The class where trimming belongs is **`closed`** — referenced once, long ago; the value was taken and
the remainder is chaff. That content is still partly live, so deciding what to keep genuinely needs
judgement:

| class | who should act | why |
|---|---|---|
| `unreferenced` | **`coref` — drop** | provably spent; a call could only preserve less |
| **`closed`** | **`extract_llm` — trim** | partly live; needs judgement |
| `open` / `opaque` | neither | still in use, or no evidence |

The sequential replay makes this testable for the first time: it surfaced **`class_closed: 25`**,
where the earlier one-request setup structurally could not produce any. Repointing the pre-filter
from `unreferenced` to `closed` is a one-line change and is the obvious next experiment.

## Also: the biggest lever is lossless, and it is none of these components

| component | firings | tokens saved |
|---|---|---|
| **`format`** (lossless JSON repack) | 119 | **1,266,088** |
| `coref` | 28 | 1,008,646 |
| `dedup` | 54 | 15,546 |
| `extract_llm` | **0** | **0** |

`format` recovers more than `coref` does, losslessly and for free. Every lossy component here is
competing for what is left after it.

## Caveats

- **No reward.** Nothing here says the 20-vs-71 tradeoff preserved task success. That remains the
  gate, and it is not answerable from replay.
- **9 conversations, 197 turns.** Direction and mechanism, not calibration.
- **Turns are reconstructed prefixes**, valid because LOCA is append-only, but each `/compact` call is
  independent — a real session would carry the *compacted* transcript forward, so effects that
  compound across turns are not captured.
- **The 60,000-token max is a choice**, made because it fires often enough to measure (66 of 197
  turns exceed it). Claude Code's real threshold is ~167,000, which only 2 of 197 turns reach.
- **The summarizer is cheap here because of freeze/replay** — 71 firings cost 17 model calls. An agent
  whose transcript actually changes after each summary would pay more.

See also: [component gating](component-gating.md) · [density](coref-density.md) ·
[eval box](coref-evalbox.md) · [reachability](coref-reachability.md) ·
[the proposal](../proposals/coref-compaction.md)
