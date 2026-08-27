# `extract_llm_sweep` — the cold-sweep adjudicator

**Kind:** Offload. **Reversible:** yes (`marker_mode: full`, the default). **In presets:** `housellm`.

On a turn whose prompt cache has expired, this component asks a cheap model — a batch of tool
outputs at a time — which of them the agent still needs, and **removes** the ones it does not,
leaving a short shape descriptor plus a resolvable `<<cg:HASH>>` marker. It never rewrites anything.

## Why it is separate from `extract_llm`

`extract_llm` does the right thing on a **warm** turn: a cheap model trims one recent tool output
down to what the agent needs next. The output is recent, the agent may still want most of it, and a
smaller version of it is more useful than none of it.

On a **cold sweep** the same operation ran against outputs *deep in history*, and that is the wrong
operation on either branch of the only question that matters. Deep history is either still
load-bearing — in which case rewriting it corrupts content the model has already reasoned about — or
it is spent, in which case the answer is to remove it, not to produce a smaller version of something
nobody will read.

Until the split these were one component behind a `per_output` / `cold_cache.enabled` pair of
switches. They are two components now, and "is the sweep on" is its presence in the pipeline, like
every other component. The old keys are refused with an error naming their replacement.

## Why the cold turn is worth its own component

Measured on this deployment over 1.4 days: turns whose prompt cache had expired were **4% of
requests and 31% of spend** ($360 of $1,173, ~$1.64 each against $0.144 warm), because all 56.7M of
their tokens billed as `cache_creation_input_tokens`. The shipped pipeline saved 0.015% of it.

Two things are true only there, and both are load-bearing:

- removing a token is worth **12.5x** what it is worth on a warm turn (cache-write at 1.25x fresh
  against cache-read at 0.1x);
- acting at depth is **free**, because there is no live cached prefix left to invalidate.

## The contract

The model returns a **verdict**, never content:

- `needed_by` — which of **(a)** the step the agent is on now, **(b)** an unfinished user
  instruction, **(c)** a next step the agent itself stated, still needs this output; or `none`.
- `quote` — when `needed_by` is a/b/c, the transcript text creating that obligation, **verbatim**.
- `verdict` — `keep` (still needed, *or you are unsure — this is the default*) or `drop`. A `drop`
  **requires** `needed_by: none`.

There is no `trim`. It was measured and removed (`cc1aa9f`): chosen **zero times in 21 probe
opportunities**, identical metrics without it, and in production accepted **once against eight
rejected as invented**. It was the only verdict that asked the model to transport text. Removing it
removes the transporting *operation*, not merely the transporting *strategies* — after it, no reply
field can carry output content at all.

The criterion is a **required output field** rather than an instruction because arms carrying an
identical criterion differed only in whether the model had to name and quote the obligation, and the
arm that had to emit it **halved the false-drop rate** (4/4 → 2/4). Stating the criterion alone
measured inert.

## Batched, not one call per output

One call is shown up to **12** outputs together. This is the shape that was measured good, and the
per-output shape is the one that was **refuted**:

| shape | live-kept |
|---|---|
| one call, one output | 6% (haiku) / 14% (sonnet) — inside the drop-everything null model's error bar |
| one call, ~15 outputs | **58%**, at the lowest cost per output |

Comparative judgement beats absolute judgement: ranking a dozen candidates against each other is a
question a model can answer, "is this one output expendable" is not. And the failure direction of a
small batch is the one a sweep cannot tolerate — `cc1aa9f`: at batch 3–6 the model dropped a
genuinely-spent output only 2 times in 4; at batch 10, 4 in 4. *Small batches do not make it wrong,
they make it unwilling to act.*

**12 is a measured ceiling**, not a round number: quote fidelity degraded with batch size, 4 of 37
quotes non-verbatim at batch 16 against 0 of 16 at batch 10.

### Sibling batches and the shared prefix

Every batch of one sweep sends the same contract and the same conversation context. The prompt is
**split** so that invariant half can carry a `cache_control` breakpoint and the batches after the
first can read it instead of each paying fresh: the contract is a system block, the goal is a second
system block when there is more than one batch to share it, and **the candidates are never in the
prefix** — they differ per batch, so a cache entry containing them could never be read, which is
strictly worse than no breakpoint.

It also needs the calls *ordered*. `cheapmodel.claimCacheWrite` deliberately withholds the breakpoint
from concurrent siblings, because an entry that is only ever written is worse than none — so with
every batch in flight at once, sharing cannot happen even in principle. The first batch therefore runs
alone to earn the write, then the rest run concurrently to read it.

**But only where the write is earnable, and on the shipped model it is not.** A `cache_control` below
the provider's minimum cacheable prefix is silently ignored — no error, `cache_creation_input_tokens:
0` — and the minimum is 4,096 provider tokens on haiku-class against 1,024 on sonnet-class. Measured
with `internal/tokens`:

| | |
|---|---|
| adjudication contract | **504** o200k tokens |
| contract + a two-message `recent` context | **~537** o200k tokens |
| haiku-class floor | 3,413 o200k — needs ~2,900 tokens of conversation on top of the contract |
| sonnet-class floor | 853 o200k — needs ~349, which a real two-message context often supplies |
| unnameable gateway alias | treated as haiku-class, `minCacheablePrefix`'s conservative default |

So **sharing is reachable on sonnet-class and provably cannot work on haiku-class at `context:
recent`** — and `housellm` pins `claude-haiku-4-5`. The component reads its own prefix size, serializes
the first batch **only** when the model would honour the breakpoint (otherwise the serialized round
would buy a gateway queue wait, ~2–4 s p50, for nothing), and counts the outcome either way:
`sweep_prefix_uncacheable` when the floor cannot be cleared, `sweep_prefix_cache_read_ZERO` when a
sibling read nothing despite the ordering.

Raising `context` would clear haiku's floor. **That is not done here** — it is open question 2 and
unmeasured, and chasing a cache is the wrong reason to change what the model is shown.

What the duplication actually costs is small, and that is by construction rather than luck: three
extra batches re-send ~1,600 input tokens, a fraction of a cent on haiku and about 4% of one batch's
own body. The transcript is deliberately not in the prefix. This is the difference from
`extract_llm`'s `context: full` sweep, where the prefix *was* the transcript at ~138,000 tokens and
duplicating it across five calls was the whole defect. It is why `max_calls` defaulting to a
concurrency round stands even where sharing fails.

Two more things make the batch safe that were fixed on the way here:

- the reply budget is raised to **16,000 output tokens**. `659e7a6` traced 24 of 34 unparseable
  replies to a 2,048-token default: a verdict array over 12 items each carrying a verbatim quote is
  long, and a model running adaptive thinking spends part of the budget before emitting any text.
  Output bills as generated, not as budgeted, so the ceiling costs nothing until used.
- the economic gate is evaluated **once for the batch**. Per-candidate gating both misprices it
  (~12x, since one call covers twelve candidates) and *starves* it — `4ca1f13` found an upstream
  per-candidate filter had left a "bulk" arm judging 1.02 outputs per call, i.e. silently running the
  refuted design. `sweep_batch_of_one` counts that shape if anything reintroduces it.

## Safety

Every failure path resolves toward **keep**. A wrong keep costs tokens on one turn; a wrong drop is a
silent, permanent loss the agent does not notice and cannot ask about.

1. **A drop that names an outstanding obligation is refused**, not performed.
2. **Unsure defaults to keep** — a missing, malformed or unparseable verdict leaves the output
   verbatim.
3. **A fabricated obligation quote is counted.** It argues for keeping, so it is not dangerous, but
   it is the only remaining signal that the model is inventing, since nothing else it returns is
   content. Checked whitespace-insensitively on a miss, so a re-wrapped line does not cry wolf.
4. **An unanswered criterion field is tolerated and counted.** Requiring it would collapse yield
   against a model that omits it; ignoring it would hide that the forcing function never ran.
5. **A dropped output stays recoverable** — marker written, original stashed, `expand` resolves it
   byte-for-byte. The decision is frozen and replayed on later warm turns, so the saving survives and
   the cached prefix stays byte-stable.
6. **The descriptor transports nothing.** It is computed from the output's shape — content class,
   line count, record count, token count — by our code. No head peek: for a record set the first rows
   say nothing about whether the field you want is in there.

The prompt **never mentions recoverability**, even though the drop is recoverable. Measured:
reassuring the model that removals "stay recoverable on request" produced 91% removal at 6%
live-kept. Telling a model its mistakes are cheap makes it careless. The operator gets the safety
net; the model does not get to hear about it.

## Configuration

| key | default | what it does |
|---|---|---|
| `min_tokens` | 1000 | Per-output floor. Lower than `extract_llm`'s, because on this turn every candidate re-bills at the write rate anyway. At 3000 this produced **zero** extractions across 3,437 production requests. |
| `min_idle_seconds` | 0 | Demand MORE idle time than the provider TTL implies. Raises the bar, never lowers it — the TTL check is the correctness condition. |
| `max_calls` | one concurrency round | Cap **batch** calls (`-1` = unlimited). Each call covers up to 12 candidates. Unbounded was measured at 27 calls, $0.229, and 76.6 s added to a turn whose upstream took 33.5 s. |
| `context` | `recent` | How much conversation the prompt carries. `full` is plausibly what a spent-ness judgement needs and is also what made the predecessor lose money (99% of the prompt was a copy of the transcript being compacted). Unmeasured either way. |
| `context_messages` | 2 | The N for `context: recent`. The biggest lever on what a call costs. |
| `economic_gate` | `true` | Only call when the expected saving beats the priced call cost. **Note:** the gate's arithmetic is calibrated on compaction, which removes a fraction of an output; a drop removes all of it, so the break-even here is a different and unmeasured number. |
| `model.*` | — | Same block as every other model-using component. |
| `marker_mode` | `full` | `full` is the only mode that keeps a drop recoverable. |
| `model_max_input_tokens` | — | Pin the adjudication model's input budget for an id the static table cannot name. |

**`strategy`, `rewrite`, `aggressiveness` and `max_chars` are config errors here**, not ignored keys.
An adjudicator selects no compaction strategy and produces no rewritten text, so a silently accepted
`rewrite: false` would read as "verified deletion-only is on" when nothing is being rewritten.

## Counters

`sweep_offered`, `sweep_adjudicated`, `sweep_dropped`, `sweep_kept`,
`sweep_drop_refused_obligation`, `sweep_quote_fabricated`, `sweep_criterion_missing`.

The two to **alert** on:

- **`sweep_drop_refused_obligation`** — the model tried to remove an output it had, in the same
  reply, just said was still needed. The removal did not happen; that is the invariant. But a
  non-zero rate means the contract is not holding.
- **`sweep_quote_fabricated`** — the model cited transcript text that is not in the transcript. It is
  inventing evidence, and this is the only signal that says so.

Diagnostics, all visible at `/stats` under the component and as
`cg_component_gate_declines_total{component="extract_llm_sweep",gate="…"}`:
`sweep_batch_truncated`, `sweep_batch_of_one`, `sweep_kept_whole_batch` (a deliberate keep-all, which
is **not** a failure), `sweep_unparseable`, `sweep_reply_truncated` (a different fix from
unparseable: raise the budget, not the prompt), `sweep_verdict_unusable`,
`sweep_verdict_unknown_label`, `sweep_verdict_duplicate_label`, `sweep_verdict_missing`,
`sweep_reply_budget_not_raised`, `sweep_escalated_to_agent_model`, `sweep_call_failed`,
`sweep_drop_would_not_shrink`, `sweep_prefix_uncacheable`, `sweep_prefix_cache_read_ZERO`.

## What is not measured

Three questions the design records rather than answers, from
[the proposal](../proposals/sweep-adjudicator.md):

1. Whether the obligation quote pays for its tokens. Requiring evidence halved false drops at batch
   size, which is the shipped shape, so the measurement applies — but it has not been re-measured
   here.
2. Whether a spent-ness judgement needs `context: full`. The default is `recent` because `full` is
   what the predecessor lost money on, not because `recent` was shown sufficient.
3. The economic gate's break-even for a *drop*. The gate is calibrated on compaction. The sweep
   carries its own ratio tracker and the existing exploration budget learns the real drop rate, but
   the first calls of a session are priced on the compaction prior.
