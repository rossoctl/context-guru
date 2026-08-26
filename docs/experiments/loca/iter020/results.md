# Iteration 020 — the merged design, finally run as specified

**Six launches, five aborted.** Each abort was triggered by the pre-registered criterion and each
exposed a defect the previous configuration had hidden. The sixth ran to completion and produced this
iteration's only instrument-independent numbers.

**Headline: 21/75 solves at $240 total, against iteration 018's 8/75 at $243.** That is *not* an effect
estimate for the merged design — see §5 before quoting it.

> **CORRECTION.** This iteration first reported **22/75**. The correct figure is **21/75**. The original
> method multiplied each task's `avg_accuracy` by its 5 seeds, and `avg_accuracy` averages only the runs
> that COMPLETED — so any task with an errored seed was over-credited. Recomputed per seed from
> `tasks/<Task>/state<N>/eval.json`, which is what intent-to-treat requires and what iteration 021's
> amendment 1 now mandates for both arms.

## 1. Read this before quoting any number from the LOCA output

LOCA prints **`Overall Success: 70/75`**. That is the count of runs that completed **without erroring**,
not the count of tasks solved. The comparable metric is accuracy-weighted:

| metric | value |
|---|---|
| `Overall Success` (ran without error) | 70/75 |
| **accuracy-weighted solves** | **21/75** |

Per task: two at 1.0, two at 0.8, one at 0.6, one at 0.2, nine at 0.0. Anyone reading the raw output
will see 70/75 first, and it is off by a factor of three.

## 2. What the run did

| | iter018 | **iter020** |
|---|---|---|
| accuracy-weighted solves | 8/75 | **21/75** |
| runs completed without error | 67/75 | 70/75 |
| LOCA cost | $234.09 | **$207.02** |
| CG model spend | $9.21 | $33.07 |
| **total** | ~$243 | **~$240** |
| `summarize` fired on | 56.1% | **41.3%** |
| `extract_llm` acted on | — | 1,465 / 2,303 requests (63.6%) |
| unique tokens removed by `extract_llm` | 318,955 | **8,140,204** |
| drops / keeps | 151 / 2,718 | **791 / 1,386** |
| expand restored / unresolved | 176 / 0 | **866 / 5** |

The prefix-ask mechanism carried **37,336,778 tokens at cache-read price with zero cache writes**, over
778 asks with 4 failures (first turn of a session). `adjudicate_stray` was **0** — the model never
called the injected maintenance tool on its own.

**Do not quote `savings_pct` (94.3%).** Iteration 012 established it is inflated 3-8× by frozen replays
re-crediting the same removal; `saved_tokens_unique` is the honest figure.

## 3. Where the mechanism still falls short

* **Verdict coverage 65%** (2,178 answered of 3,360 offered). A third of offered candidates get no
  verdict and are silently kept, so the design is not running at full strength even here.
* **59 batches truncated** at the 12-item cap, so candidates were discarded on those calls. Visible only
  because `merged_offered` now exists.
* **`merged_unparseable` 133** — replies that were neither a tool call nor parseable JSON.
* **`merged_quote_not_verbatim` 149 of 2,178 verdicts (6.8%)** — fabricated obligation quotes. Harmless
  in direction (a false obligation argues for keeping) but it is the batch-size ceiling signal.
* **`merged_drop_contradicts_obligation` 1** — one drop that named an outstanding obligation, refused.
  Rare, and the guard is load-bearing.
* **`refusal_reached_model` 48 (1.6%)** — expand refusals that survived to the model.

## 4. Seven defects in one component's measurement path

Every one of these produced the *same* misleading signal — "the model declines to act" — and each was
invisible until the layer above it was removed.

| # | defect | effect | origin |
|---|---|---|---|
| 1 | coref prefix pre-filter | removed 149,681 candidates, leaving ~1 per call | pre-existing |
| 2 | `llm_max_per_request` | caps candidates in a design with one call | pre-existing |
| 3 | economic gate | prices each candidate against a whole call; suppressed 1,497 vs 224 through | pre-existing |
| 4 | any pinned floor disables the pressure trigger | fired on 72% of requests regardless of context | pre-existing |
| 5 | `verdicts ÷ calls` used as batch size | counts what was ANSWERED, not OFFERED | mine |
| 6 | 2048 output-token ceiling | reply cut mid-array on ~70% of calls | mine |
| 7 | `tool_choice: none` | drove the model into PROSE — 0 of 6 labels | mine |

Numbers 1-4 predate this branch and are documented in
[`docs/results/min-tokens-vs-economic-gate.md`](../../../results/min-tokens-vs-economic-gate.md).
5-7 are mine, introduced while trying to measure or improve the thing.

**The judgement machinery was never the problem.** A sampled reply shows the model reasoning correctly
under the criterion — *"the task is not yet complete, and no summary of this raw data has been recorded
elsewhere"* — and simply saying so in prose, which the parser scored as a failure. The contract already
calls "keep everything" a valid and often correct answer.

### An eighth, in the rig rather than the product

`capture_hop.py` tested for markers with `"<<cg:" in body`, the **unescaped** spelling. Go's
`encoding/json` HTML-escapes `<`, so a marker the model reads as `<<cg:HASH>>` exists on the wire only
as `<<cg:HASH...`. Consequence: **`has_marker` read 0.0% on every arm ever run here**, which
reads as "removals are not reversible" while expand was restoring 866 of them. `expand/expand.go`'s
`rawMarkerRe` documents this exact trap and would have prevented it. Fixed in the rig; every previous
"marker present" line in this iteration series should be treated as unmeasured.

## 5. What this run does and does not establish

**Does:** the mechanism works end to end. Pressure-gated trigger, 100% prefix-cache reads, schema-shaped
verdicts through an injected tool, no truncated replies, no stray tool calls, reversibility intact,
recovery working at 866 restores with 5 unresolved.

**Does not: attribute 21 vs 8 to the merged design.** There was **no concurrent baseline**. The binary,
the configuration and seven defects all differ from iteration 018. The pre-registration scoped this as a
mechanism run and said solves were context only, with comparisons to earlier arms indicative at best —
that limit binds here.

**Deferral is mass, not judgement.** 56.1% → 41.3% says tokens were removed. A badly-chosen removal
defers `summarize` exactly as well as a well-chosen one.

## 6. What follows

1. **The reward pair.** Baseline and merged on this binary, same seeds, run together. Nothing before
   that can price the design.
2. **Coverage.** 65% offered-to-answered is the largest remaining gap. Note the probe answered **6 of 6**
   on a 6-item batch while production answers ~65% of 12 — so a **smaller** batch may act on more items
   in absolute terms, which would invert this iteration's premise. Cheap to test.
3. **Make the economic gate batch-aware** — amortise one call across the batch instead of charging each
   candidate a full call. Only now worth building, because bulk batches demonstrably change behaviour.
4. **Report offered-vs-answered by default.** One line of arithmetic that would have caught defects 1-5
   at iteration 014 and saved roughly $700.
