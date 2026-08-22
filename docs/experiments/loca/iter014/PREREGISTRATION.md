# Iteration 014 — pre-registration: the MERGED design vs separate components

**Written before the run.** Queued behind [iteration 013](../iter013/PREREGISTRATION.md); nothing had
started when this was committed.

## Question

Does folding the co-reference criterion **into** `extract_llm`'s own call do better than running
`coref` and `extract_llm` as **separate sequential components**?

This is the design originally proposed — *"once you're already using an LLM, you can do all of that in
that call"* — and it has never been measured, in either sense of the word "fold" this log has used.
See the terminology note in [the index](../../README.md): iterations 002–004, 012 and 011's arm 3 all
mean *`extract_llm` added as its own component*, not this.

## Arms

| arm | pipeline | source |
|---|---|---|
| separate | `[format, coref, extract_llm, summarize]` | **reused** from iteration 013 arm 4 |
| **merged** | `[format, extract_llm(selection_mode: merged), summarize]` | new |

`coref` is **absent** from the merged arm deliberately — that is the whole claim. Its criterion is
carried inside `extract_llm`'s prompt as evidence (`novel`, `refs`, `ref_age`, `used_frac`,
`later_turns`, and the index's own verdict), so one call makes both judgements.

Everything else is held identical: 128k band, `MODEL_INFO_URL` declaring a 128k window,
`min_request_frac 0.78`, `resummarize_tokens 20000`, LOCA clearing at 128k, same task set and seeds.

## What is being tested, precisely

Not "is an LLM better than the index" — that is **already answered and negative** (95% live-kept at
11% false-drop for free, and no model arm beat it even after correcting both known biases,
[iteration 009](../iter009/results.md)). What is untested is whether the merged shape moves **reward**,
which every decision-quality experiment structurally cannot speak to.

**The implementation is bulk, not per-output**, because the per-output form of exactly this design was
refuted at 6% live-kept — inside the drop-everything null model's error bar. The prompt also uses
cost-honest framing (worth ~26 points measured) and never mentions recoverability.

## Endpoints, declared now

**Primary — reward**, paired on `(task, seed)`, two-sided, quoted on the **task-clustered** end.
LOCA has 15 independent `*_s2l` environments, verified against its env registry, so the harm bound
floors near ≤18% and cannot be improved on this benchmark at any n.

**Secondary — cost and calls.** The merged arm should make **far fewer model calls** (one per request
versus up to `llm_max_per_request` per request), so $/arm and `llm_calls` are the efficiency claim. A
merged arm that is dearer than separate components has no case at all.

**Tertiary — yield, in unique tokens.** Never `savings_pct`: it overcounts non-deterministic
components 2.2–8.4× by re-crediting replayed frozen rewrites, and it ordered the arms *backwards* in
[iteration 012](../iter012/results.md).

**Diagnostic — the merged gate counters** (`merged_drop` / `merged_trim` / `merged_keep` /
`merged_trim_not_contained` / `merged_unparseable` / `merged_call_failed`). These separate "the model
kept everything" from "the model was never asked" from "the model returned junk", which a single
acted-count cannot.

## How each outcome will be read

| outcome | reading |
|---|---|
| reward within the bound **and** fewer calls / lower cost | merged is the better *shape* — same decisions, cheaper. The strongest available result |
| reward within the bound, cost similar or higher | no case for merged; separate components already work and are simpler |
| `merged_keep` dominates | the model declines to act on evidence; consistent with iteration 009 and a negative answer |
| `merged_unparseable` non-trivial | a prompt/parse problem, not a design result; fix and re-run |
| reward harm beyond the bound | folding the criterion into the prompt loses what the index enforced structurally |

## Pre-declared threats

- **Reused baseline** (iteration 013 arm 4). The validity check must be *real* this time: iteration
  012's version compared the reused data to itself and could not fail. Here the check is that arm 4's
  own error count and solve rate are re-read from disk and reported alongside, and that both arms ran
  on the same band, window map and task set — stated, not assumed.
- **Different binaries.** Iteration 013 runs `cg-proxy-v7`, this needs `v8` (which adds the merged
  path). v8 differs only by the new code path plus a residue fallback used only in merged mode, so
  non-merged behaviour is unchanged — but it is a difference, and it is recorded rather than waved at.
- **A null yield is plausible.** `extract_llm` acted on 248 of 1,752 requests in the separate arm at
  32k; the merged shape may simply keep everything, which iteration 009's evidence predicts.
