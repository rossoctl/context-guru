# LOCA-bench — iteration 002 (deferring summarization)

**Date:** 2026-08-20 · **Goal:** the proposal's largest claimed win, measured end to end — does
compacting the full request body **defer the summarization** an agent runs when its context fills?
**Cost: ~$0.16** across five arms. Binaries: `cg-proxy-fix` (S1–S4), `cg-proxy-fold` (S4b).

**Setup.** LOCA traffic is append-only, so turn *k* is reconstructed as `messages[0:k]` of the
final request, cut at assistant boundaries: **197 turns across 9 conversations**, replayed in order
under a stable per-conversation session id so `MaxCachedIdx` advances turn by turn. This is the fix
for [iteration 001](../iter001/results.md)'s cold-start artifact.

**`summarize` is wired at a 60,000-token context max and runs LAST**, mimicking an agent compacting
when full. It fires only when compaction failed to keep the turn under the max, so **firings are the
deferral measurement**.

Run: `/tmp/cg-coref/seqrun.sh <arm> <cfg> <port>`, arms strictly sequential.

## Result

| arm | pipeline | **summarizer firings** | vs baseline | shrink |
|---|---|---|---|---|
| **S1** | `summarize` alone | **71** | — | 64.6% |
| **S2** | `codesmart` − `extract_llm` | **46** | **−35%** | 58.2% |
| **S3** | + **tail** `extract_llm` | **46** | **+0** | 58.2% |
| **S4** | + `coref` | **20** | **−72%** | 45.6% |
| **S4b** | + `extract_llm` prefix reach | **20** | −72% | 45.6% |

Shrink *falls* as firings fall — correct, since `summarize` is the largest single shrinker, so
deferring it removes its contribution. Firings are the objective, not shrink.

Per-component in S4b:

| component | firings | tokens saved |
|---|---|---|
| **`format`** (lossless JSON repack) | 119 | **1,266,088** |
| `coref` | 28 | 1,008,646 |
| `dedup` | 54 | 15,546 |
| `extract_llm` | **0** | **0** |

`extract_llm` gates, S4 vs S4b — the proof that prefix reach engaged:

| gate | S4 | S4b |
|---|---|---|
| `cached_prefix` | 6,597 | **gone** |
| `cached_prefix_above_floor` | 279 | **gone** |
| `prefix_still_referenced` | — | **6,519** |
| `economic_gate` | 25 | **103** |

## What this proves

- **Deferral works and is large: 72% fewer summarizations**, and **`coref` delivers it** (28
  firings, 1,008,646 tokens, taking firings 46 → 20). First clearly positive end-to-end result for
  this line of work.
- **The deterministic pipeline gets a third of the way for free** (71 → 46).
- **The tail `extract_llm` lever adds exactly nothing** — S2 and S3 byte-identical.
- **`allow_cached_prefix` engages correctly and contributes nothing.** 98.8% of prefix candidates
  are still referenced; what survives cannot clear break-even.
- **The pre-filter was pointed at the wrong class.** For `unreferenced` content, dropping dominates
  trimming — a call can at best preserve part of what is already spent, where `coref` removes it
  free. Fixed by making the class selection configurable (`prefix_classes`).
- **`format` is the largest single lever** and is lossless. Every lossy component competes for the
  remainder.

## What this does NOT prove

- **No reward.** Nothing says the 20-vs-71 trade preserved task success. This is the gate and replay
  cannot answer it.
- **9 conversations, 197 turns.** Direction and mechanism, not calibration.
- **Turns are reconstructed prefixes.** Valid because LOCA is append-only, but each `/compact` call
  is independent — a real session carries the *compacted* transcript forward, so effects that
  compound across turns are invisible here.
- **The 60,000 max is a choice**, made because it fires often enough to measure (66 of 197 turns
  exceed it). Claude Code's real threshold is ~167,000, which only 2 of 197 turns reach.
- **The summarizer is cheap here because of freeze/replay** — 71 firings cost 17 model calls. An
  agent whose transcript actually changes after each summary pays more.
- **A stale binary produced a null result.** S4 ran on a binary built before `allow_cached_prefix`
  existed, so its "fold" arm was really `coref` alone. Caught only because the gate counters were
  byte-identical to S3. Every arm now records its binary.

## Next levers

1. **Reward on LOCA** — LOCA-bench's own ReAct agent with the deterministic GEM scorer, pointed at
   cg-proxy via `LOCA_ANTHROPIC_BASE_URL`. This is the gate. → iteration 003.
2. `prefix_classes: [closed]` / `[unreferenced, closed]` variants, now that the knob exists.
3. A second benchmark on the long-horizon axis.

## Artifacts

`/tmp/cg-coref/loca-seq.jsonl` (197 turns, 37 MB), `out-S1.log`, `out-rest.log`, `out-S4b.log`,
`st-*.json`, `s{1,2,3,4}-*.yaml` on the eval box. Analysis:
[deferring summarization on LOCA](../../../results/coref-loca.md).
