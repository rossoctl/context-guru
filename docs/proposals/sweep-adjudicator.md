# The cold sweep should adjudicate, not compact

**Status:** proposed, not implemented. This document is the specification; the implementation is
the work.

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

**One call per output.** Not a batch. `4ca1f13` established that the merged mode "was never bulk —
it adjudicated one output per call", so per-output adjudication is a shape that has already run.
Batching is what remains experimental, and it is what forces the failure modes this design avoids:
a shared reply that can be truncated mid-array, quote fidelity degrading with batch size (4 of 37
quotes non-verbatim at batch 16 against 0 of 16 at batch 10), and a batch-truncation counter to
compensate. A per-output call has none of those.

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

- **Is the obligation quote worth its tokens per-output?** Requiring evidence halved false drops
  in the merged probes (4/4 → 2/4), but that was measured at batch size, where one reply covered
  many outputs. Per-output the quote is a larger share of each reply. Worth measuring before
  assuming it carries over.
- **Does a spent-ness judgement need `context: full`?** Deciding "needed by nothing" plausibly
  requires seeing the whole transcript, which is the expensive context mode. If so, that pairing
  should be the sweep's default rather than an operator's discovery.
- **What is the economic gate's break-even for a drop?** A drop removes the entire output rather
  than a fraction of it, so the saving per call is much larger than for a compaction, and the gate's
  current arithmetic is calibrated on the latter.

## Execution order

1. The contract and its parser, in `internal/extract`, with invariants 1–4 as unit tests. No
   component wiring — this slice is verifiable alone.
2. The drop path: descriptor generation, marker, stash, invariants 5–6.
3. The component and its config, plus the `extract_llm` surface removals.
4. Counters and their `/stats` golden-contract entries.
