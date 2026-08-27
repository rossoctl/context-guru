# The cold sweep should adjudicate, not compact

**Status:** implemented as `extract_llm_sweep`.

**CORRECTION, 2026-08-27.** The version of this document that was implemented from argued for ONE
CALL PER OUTPUT, and its argument was inverted: it cited `4ca1f13` as evidence that per-output
adjudication "is a shape that has already run", when that commit diagnoses the per-output shape as a
DEFECT — an arm that had degraded to 1.02 verdicts per call, which it names as "the per-output design
refuted at 6% live-kept, not the bulk shape that measured 58%". The secondary argument (that batching
invites truncated replies and degraded quote fidelity) was also wrong: both were already solved on
`feat/coref-compaction` before this document was written. The section below has been rewritten. The
safety invariants, the counters and the config surface were unaffected — they are per-verdict
properties and hold at any batch size.

## The problem

`extract_llm` does one thing in two situations that want different things.

On a **warm turn** it works the uncached tail: a cheap model writes a Starlark program that trims
one recent tool output down to what the agent needs next. Rewriting is right there — the output is
recent, the agent may still want most of it, and a smaller version of it is more useful than none
of it.

On a **cold sweep** (`cold_cache`, prompt cache expired, whole transcript re-billing at the write
rate) it does the same thing to outputs *deep in history*. That is the wrong operation. Deep
history is either still load-bearing — in which case rewriting it corrupts content the model has
already reasoned about — or it is spent, in which case the right answer is to remove it, not to
produce a smaller version of something nobody will read.

## The contract to use instead

`feat/coref-compaction` reached this conclusion for the merged design and removed the `trim`
verdict (`cc1aa9f`). The evidence:

> trim was chosen **zero times in 21 probe opportunities**, metrics were **identical without it**,
> and in production it was accepted **once against eight rejected as invented**. It was the only
> verdict that asked the model to **transport text**, which is what it is worst at.

What remained (`internal/extract/bulk.go` on that branch) is binary:

> `"needed_by"` — which of (a)/(b)/(c) still needs this output, or `"none"` if it is spent.
> `"quote"` — when needed_by is a/b/c, the transcript text that creates that obligation, copied
> VERBATIM. Leave empty only when needed_by is `"none"`.
> `"verdict"` — keep (still needed, **or you are unsure — this is the default**) or drop (its
> information is spent; a short descriptor of its shape will remain in its place). A verdict of
> `"drop"` **REQUIRES** needed_by `"none"`.

Where (a)/(b)/(c) are: the current step, an unfinished user instruction, or a next step the agent
itself stated.

The model returns a verdict and a quote. It never returns content. That removes the transporting
*operation*, not merely the transporting *strategies* — which is the strongest available form of
"never ask a model to transport text".

## What to build

`extract_llm_sweep`: a registered component that, on a cold sweep, adjudicates each candidate
output and either keeps it **verbatim** or drops it, leaving a short shape descriptor plus the
existing `<<cg:HASH>>` marker so `expand` still recovers the original.

**One call per BATCH.** `4ca1f13` is the commit this rests on, and it points the other way from how
it was first read. It found a live arm reporting 2,030 bulk calls and 2,074 verdicts — 1.02 verdicts
per call, so every "bulk" adjudication judged a single output — and filed that as the bug: *"That is
the per-output design refuted at 6% live-kept, not the bulk shape that measured 58%, so iteration 014
measured something other than what it claimed."* It also added the assertion whose absence let it
through: the prompt must offer more than one output, because *"asserting a single call was not enough,
since one call carrying one item is exactly the refuted design"*.

The measurement behind that, from `docs/results/coref-selection-experiment.md` over 8,105 recorded
decisions: shown ONE output, a model scored 6% live-kept on haiku and 14% on sonnet, both inside the
drop-everything null model's error bar — shown a single output, a model simply drops it. Shown ~15
together it reached 58% at the LOWEST cost per output, because the overhead amortises and, more
importantly, because comparative judgement beats absolute judgement: ranking a dozen candidates
against each other is a question a model can answer, "is this one output expendable" is not.

`cc1aa9f` gives the direction of the failure, which is the one a SWEEP specifically cannot tolerate:
*"at batch 3-6 the model dropped a genuinely-spent output only 2 times in 4, at batch 10 it dropped it
4 in 4 and cleared 100% of genuinely-spent candidates. Small batches do not make it wrong, they make it
UNWILLING TO ACT, which is what a 94.6% keep rate looks like from inside."* A sweep exists because the
entire transcript is re-billing at the write rate; an adjudicator too timid to remove anything is an
expensive no-op exactly where the money is.

The batch is capped at **12 items**, a measured ceiling rather than a round number: quote fidelity
degraded with batch size, 4 of 37 quotes non-verbatim at batch 16 against 0 of 16 at batch 10, so the
transport limit sits between them and 12 takes the conservative end (`cc1aa9f`).

**Neither objection to batching survives.** Both were solved on `feat/coref-compaction` before this
document was written, so they never distinguished the two shapes:

- *A shared reply truncated mid-array.* `659e7a6` traced 24 of 34 unparseable replies to a 2048-token
  output budget and raised it to 16,000. A verdict array over 12 items, each carrying an obligation
  label and a verbatim quote, is simply long, and a model running adaptive thinking spends part of the
  budget before emitting any text. Output bills as generated rather than as budgeted, so the ceiling
  costs nothing until used. Truncation is now counted separately from a format failure, because the
  two need opposite fixes — raise the budget versus fix the prompt.
- *Quote fidelity decaying with batch size.* Measured, and the cap is set below the observed ceiling
  (above).

And the **transport principle never distinguished them at all**. Once `trim` is removed, no verdict
carries content in either shape: there is no reply field a model could return output text through.
That argument rules out rewriting; it says nothing about how many verdicts travel in one reply.

**Do not thin the batch upstream.** `4ca1f13`'s other finding is that the arm's real defect was an
upstream per-candidate filter — `prefix_still_referenced` removed 149,681 candidates, leaving about
one per request. Any per-candidate gate ahead of batch assembly reproduces this: it thins the batch one
output at a time until comparative judgement has nothing to compare, and returns the component to
batch-of-one silently. That is why the economic gate is evaluated ONCE for the batch rather than per
candidate — which is also the correct arithmetic, since one call now covers up to twelve candidates and
charging each of them a whole call priced the batch at ~12x its real cost.

**Reuse, do not fork.** The adjudication contract text should move to a shared location rather
than being copied out of `bulk.go` — the *contract* is general, the *batching* is not. The model
client, pricing, result cache, keep-list harvesting, marker/stash machinery and the report/gate
plumbing are all shared with `extract_llm` and must stay shared.

## Config surface

The split that makes this expressible. `extract_llm` keeps the warm/tail path; `extract_llm_sweep`
is the cold one. Existing configs break deliberately — there is one deployment, and it is migrated
by hand.

| `extract_llm_sweep` | `extract_llm` only | Shared by both |
|---|---|---|
| `min_tokens` (its own floor, replaces `cold_cache.min_tokens`), `min_idle_seconds`, `max_calls` | `strategy`, `rewrite`, `aggressiveness`, `max_chars`, `fire_on`, `trigger`, `llm_every_n_requests`, `llm_max_per_request`, `allow_on_caching_backend` | `model`, `marker_mode`, `context`, `context_messages`, `model_max_input_tokens`, `economic_gate` |

Note what leaves the sweep's surface entirely: **`strategy`, `rewrite`, `aggressiveness` and
`max_chars` stop applying**, because an adjudicator selects no compaction strategy and produces no
rewritten text. `rewrite` in particular becomes moot rather than merely defaulted differently — it
governs how a *rewritten* result is validated, and nothing is rewritten. Any of these appearing
under `extract_llm_sweep` should be a config error naming the reason, not a silently ignored key.

`max_calls` bounds BATCH calls, not per-output calls. The item cap above and this are independent
brakes: one is a measured quote-fidelity ceiling, the other a spend/latency bound. It defaults to one
concurrency round rather than to a single call, because with one call a transcript carrying 40
candidates would have 12 adjudicated and 28 left verbatim — on the one turn whose whole point is that
everything is re-billing at the write rate, that is leaving most of the money. Nothing measured
compares four batches of 12 against one batch of 12; batch SIZE is the variable the experiments moved,
and the per-batch shape is identical either way. It must never truncate the CANDIDATE list instead of
the batch list: `max_calls: 4` leaving four candidates is a batch of four, which is the size at which
the model was measured unwilling to act.

`per_output` and the `cold_cache` block disappear from `extract_llm`, along with the
`per_output: false with cold_cache disabled leaves the component with nothing to do` error — that
error is the seam this split removes.

## Safety invariants

Each is a test, and each must be verified to FAIL when its subject is reverted.

1. **A drop that names an outstanding obligation is refused, not performed.** This is the one
   verification pointing the dangerous way, and the one to write first.
2. **Unsure defaults to keep.** A missing, malformed or unparseable verdict leaves the output
   verbatim.
3. **A fabricated obligation quote is counted.** It argues for *keeping*, so it is not dangerous —
   but it is the signal that the model is inventing, and on this design it is the only such signal
   left, since nothing else it returns is content.
4. **An unanswered criterion field is tolerated and counted.** Requiring it would collapse yield
   against a model that omits it; ignoring it would hide that the forcing function never ran.
5. **A dropped output stays recoverable.** Marker written, original stashed, `expand` resolves it.
   A drop advertised as reversible that is not is a worse defect than no drop.
6. **The descriptor left in place transports nothing.** It is generated from the output's shape
   (kind, size, line/record count) by *our* code, never by the model.

## Counters

`sweep_adjudicated`, `sweep_dropped`, `sweep_kept`, `sweep_drop_refused_obligation`,
`sweep_quote_fabricated`, `sweep_criterion_missing`. The refusal and fabrication counters are the
two that must be alertable: the first means the model tried to drop something still needed, the
second means it is inventing evidence.

## Open questions

- **Is the obligation quote worth its tokens?** Requiring evidence halved false drops in the merged
  probes (4/4 → 2/4), measured at batch size — which is the shape now shipped, so the measurement
  applies directly. It is kept. (The earlier framing of this question assumed per-output calls, where
  the quote would have been a much larger share of each reply; that concern is gone with the shape.)
- **Does a spent-ness judgement need `context: full`?** Deciding "needed by nothing" plausibly
  requires seeing the whole transcript, which is the expensive context mode. If so, that pairing
  should be the sweep's default rather than an operator's discovery.
- **What is the economic gate's break-even for a drop?** A drop removes the entire output rather
  than a fraction of it, so the saving per call is much larger than for a compaction, and the gate's
  current arithmetic is calibrated on the latter.

## Execution order (as landed)

1. The contract and its parser, in `internal/extract`, with invariants 1–4 as unit tests. No
   component wiring — this slice is verifiable alone.
2. The drop path: descriptor generation, marker, stash, invariants 5–6.
3. The component and its config, plus the `extract_llm` surface removals.
4. Counters and their `/stats` golden-contract entries.
