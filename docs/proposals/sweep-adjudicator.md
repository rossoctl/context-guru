# The cold sweep should adjudicate, not compact

**Status:** implemented as `extract_llm_sweep`. This document has now been wrong twice; what follows
is a description of the code rather than a specification ahead of it.

**CORRECTION 2, 2026-08-28 — the delivery mechanism.** The sweep no longer copies candidate content
into any prompt. It asks the REQUEST's own model, over the transcript already in that model's prompt
cache, and ships an INVENTORY of candidates rather than their content (`a9d666f`). Three measurements
force it:

- appending a trailing user message to a byte-identical prefix read **19,595 tokens from cache and
  created 0**, on the live route;
- verbatim quoting — the only remaining signal that the model is inventing — degraded to **20.8%** on
  the cheap model at bulk batch sizes, against **0 of 59** on the request model;
- need is relevance *minus* what has already been captured elsewhere, and that second term lives in
  the later turns, which a prompt carrying only the candidate cannot show.

That makes it **one call for all candidates**. The batch assembler, the 12-item cap, per-batch
concurrency and `max_calls` are gone: they existed only because content was being copied.

**CORRECTION 1, 2026-08-27 — the batch, now also superseded.** The first implemented version asked one
call per output, and this document argued for it by citing `4ca1f13` as evidence that per-output "is a
shape that has already run". That was inverted: the commit diagnoses per-output as the shape **refuted
at 6% live-kept**. Batching fixed that; the prefix ask then removed the need for batching at all. The
secondary argument for per-output — that batching invites truncated replies and degraded quote fidelity
— was also wrong, and both were already solved before this document was written (`659e7a6`, `cc1aa9f`).

**What survived both corrections unchanged:** the contract, the six safety invariants, and the counters.
They are properties of a VERDICT, and a verdict has the same shape whatever the model was reading when
it wrote one.

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

**One call, to the request's own model, over its cached transcript.**

The judgement needs the whole transcript and it needs a model that quotes faithfully. Sending the
transcript fresh costs ~10x a cache read, and the cheap model does not quote faithfully at the sizes
required. Only one construction satisfies both: append the question to the bytes this session already
sent, so the provider reads the prefix it cached from them.

What travels is an inventory — one line per candidate, carrying a small integer label, a token count
and a bounded locating head. Not the outputs: *"Paying fresh to send truncated copies of content the
model is reading from cache would defeat the mechanism and show it an excerpt of something it could
read in full."* Labels are integers because asked for opaque `tool_use` ids the model regularised them;
with integers it was 0 bad labels in 40+ trials.

**The prefix is the PREVIOUS turn's SENT body.** The upstream cache was populated by what context-guru
emitted, i.e. the compacted form; the incoming body is uncompacted and diverges at the first thing any
component removed, making everything past that point a fresh charge. The consequence is that the ask
sees the transcript as of the previous turn, so the newest tool output is invisible to it — acceptable,
because tail content has had no turns in which to be superseded.

**The cache read is VERIFIED and always counted; what happens on a miss is a choice.** `PrefixUsage` is
returned rather than merely recorded so the caller can gate on it, and `sweep_prefix_cache_read_ZERO`
fires whenever the read did not happen — that part is not optional, because a silent miss looks
identical to a working call except on the bill, and silent misses are what hid this class of problem
before.

By **default** the sweep then falls back to a self-contained completion, which is what `a9d666f` chose:
treating "no prefix" as "no verdicts" would disable the component on every session's *first* turn and
read, in the counters, as a model that declined to act. The fallback carries a bounded sample of each
output, so it pays fresh for content the cached path reads for a tenth of the price — the cost the
prefix ask exists to avoid. It goes to the **request's own model** regardless, because the measurement
that chose that model is about faithful quoting rather than caching, and that reason survives the loss
of the cache read.

`block_fallback: true` declines instead. That is the right setting where the bill matters more than the
removal, and the honest one to reach for if `sweep_prefix_cache_read_ZERO` turns out to be common.

**Reuse, do not fork.** The adjudication contract text should move to a shared location rather
than being copied out of `bulk.go` — the *contract* is general, the *batching* is not. The model
client, pricing, result cache, keep-list harvesting, marker/stash machinery and the report/gate
plumbing are all shared with `extract_llm` and must stay shared.

## A caveat on the 58% figure

The measurement that justifies batch-style adjudication — 6% live-kept shown one output, **58%** shown
~15 together — was taken **with the co-reference index supplying evidence** on every candidate: `novel`,
`refs`, `ref_age`, `used_frac`, `later_turns`, and the index's own verdict, for the model to weigh or
veto.

`main` has no such index. The model reasons from the transcript alone, with no reference-tracking hints
at all. That is **plausibly better** — it is not limited to exact matches, and the index's documented
blind spot was precisely *transformed* reuse, a value summed or reworded before being restated, which
leaves `refs=0` on something still load-bearing — but it is **untested**. The 58% should be read as
measured on a differently-informed model.

PR #80 will rebase onto this branch and bring the index with it; `AdjudicationItem.Evidence` is the seam
it renders through, and it is deliberately empty until then. Whether the evidence helps, and whether it
must arrive as evidence rather than as a pre-filter (see the starvation note at the candidate-gathering
site), is the **second** thing a measurement should settle after the pre-expiry window's width.

## The trigger: pre-expiry, not cold

The two halves of this component want **opposite** cache states, and that contradiction is what the
trigger resolves:

- the **ask** needs a WARM cache — a prefix ask reads an entry that must still exist, or the call pays
  fresh for the whole transcript;
- the **removal** wants a COLD cache — rewriting deep history invalidates a live prefix and forces a
  cache-write of the whole suffix at 1.25x fresh.

Both are cheap in the window where the entry still exists but has little life left. So the sweep fires
when `0 < remaining <= pre_expiry_seconds`, where `remaining` is the cache's believed lifetime minus
this session's idle time.

**The TTL is derived, never assumed.** `Ctx.CacheTTLMs` is the same figure apply's cold decision uses,
read out of the request itself: a bare `ephemeral` mark is 5 minutes, an explicit `ttl: "1h"` is an
hour, widened to the longest lifetime this prefix has ever asked for. `0` means unknown, and unknown
does not fire — a window computed from a guessed TTL would invalidate live prefixes on exactly the
deployments whose TTL could not be read.

**The window's WIDTH is the one unmeasured number in the design.** It defaults to one minute, which is
`apply.coldMargin` — the single figure in this codebase with a stated purpose for clock uncertainty
around cache expiry. Wider fires more often and invalidates more remaining TTL; narrower fires rarely.
Nothing measures either side, so it is configurable and deliberately narrow rather than tuned.

## Config surface

The split that makes this expressible. `extract_llm` keeps the warm/tail path; `extract_llm_sweep`
is the cold one. Existing configs break deliberately — there is one deployment, and it is migrated
by hand.

| `extract_llm_sweep` | `extract_llm` only |
|---|---|
| `min_tokens`, `pre_expiry_seconds`, `block_fallback`, `marker_mode` | `strategy`, `rewrite`, `aggressiveness`, `max_chars`, `fire_on`, `trigger`, `llm_every_n_requests`, `llm_max_per_request`, `allow_on_caching_backend`, `model`, `context`, `context_messages`, `economic_gate` |

The sweep's surface is nearly empty, and every absence is structural rather than a default:

- no **`model`**: the ask goes to the request's own model because only that model's cache holds the
  transcript. Naming another would read a different namespace and pay fresh for everything.
- no **`context` / `context_messages`**: the conversation IS the cached prefix, so there is no amount
  of it to choose to re-send.
- no **`max_calls`**: one ask covers every candidate.
- no **`economic_gate`**: the gate prices a per-output cheap-model call against an expected saving.
  This is one cached read for the whole transcript, so its arithmetic does not describe the component.
  The brakes are the floor, the window, and `block_fallback` where an operator wants one.

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
