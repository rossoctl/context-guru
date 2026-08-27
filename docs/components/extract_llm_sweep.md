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

## One call, to the request's own model, over its cached transcript

The question goes to the **request's own model**, appended as a trailing user message to the exact body
this session forwarded on the previous turn, so the provider reads the prompt cache those bytes
populated. What travels is an **inventory** — one line per candidate, carrying a small integer label, a
token count and a bounded locating head — never the outputs themselves.

Three measurements force that shape:

| | |
|---|---|
| appending to a byte-identical prefix | **19,595 tokens read from cache, 0 created** |
| verbatim quoting, cheap model at bulk batch sizes | **20.8%** |
| verbatim quoting, request model | **0 of 59 non-verbatim** |

The second and third settle *which* model is asked: a fabricated quote is the only remaining signal
that the model is inventing, so a judge that cannot quote faithfully cannot be checked. The first
settles how the transcript is afforded. And the judgement needs the whole transcript because need is
relevance *minus* what has already been captured elsewhere, and that second term lives in the later
turns — which a prompt carrying only the candidate cannot show.

Because nothing is copied per candidate, **one call covers all of them**. The batch assembler, the
12-item cap, per-batch concurrency and `max_calls` are gone; they existed only to bound copied content
against a cheap model's window.

Labels are integers because asked for opaque `tool_use` ids the model regularised them — `toolu_01..07`
for `toolu_probe_00..07` — since reproducing a random identifier from thousands of tokens back is a
copying task, not a judgement. With integers it was 0 bad labels in 40+ trials. The ids stay on our
side, for logs.

**The prefix is the previous turn's SENT body.** The upstream cache was populated by what context-guru
emitted, i.e. the compacted form; the incoming body is uncompacted and diverges at the first thing any
component removed, making everything past that point a fresh charge. The consequence: the ask sees the
transcript as of the previous turn, so the newest tool output is invisible to it. Acceptable — tail
content has had no turns in which to be superseded — and it keeps a large model call off the agent's
critical path.

Three construction facts, each measured and each a test: `tool_choice: none` is **not** in the cache key
so forcing it is free, and **necessary**, or the prefix's tools make the model answer with a `tool_use`;
`tools` **are** in the key, so stripping them reads a different, smaller entry; and the route rejects
assistant prefill, which an appended user message satisfies by construction.

## The trigger: pre-expiry, not cold

The two halves of this component want **opposite** cache states:

- the **ask** needs a WARM cache — it reads an entry that must still exist, or the call pays fresh for
  the whole transcript;
- the **removal** wants a COLD cache — rewriting deep history invalidates a live prefix and forces a
  cache-write of the whole suffix at 1.25x fresh.

Both are cheap in the window where the entry still exists but has little life left. The sweep therefore
fires when `0 < remaining <= pre_expiry_seconds`, where `remaining` is the cache's believed lifetime
minus this session's idle time.

**The TTL is derived, never assumed.** It is the same figure the cold decision uses, read out of the
request itself: a bare `ephemeral` mark is 5 minutes, an explicit `ttl: "1h"` is an hour, widened to the
longest lifetime this prefix has ever asked for. Unknown does not fire — a window computed from a
guessed TTL would invalidate live prefixes on exactly the deployments whose TTL could not be read.

**The window's WIDTH is the one unmeasured number in the design.** It defaults to one minute, which is
the codebase's own clock-uncertainty margin for cache expiry. Wider fires more often and invalidates
more remaining TTL; narrower fires rarely. Nothing measures either side.

## When the cache read does not happen

`PrefixUsage` is returned rather than merely recorded, so the component gates on it. A read of zero is
**always counted** (`sweep_prefix_cache_read_ZERO`) — a silent miss looks identical to a working call
except on the bill.

By default it then **falls back** to a self-contained completion carrying a bounded sample of each
output. That keeps the component working on a session's first turn and whenever an entry has gone;
treating "no prefix" as "no verdicts" would disable it there and read as a model that declined to act.
The fallback is the expensive path by construction — it pays fresh for content the cached path reads for
a tenth of the price, and shows a truncated view of each output — so it is counted every time
(`sweep_fallback_used`). It still asks the **request's** model: the reason that model was chosen is
faithful quoting, not caching.

`block_fallback: true` declines instead, forgoing the yield rather than paying for it.

Note what neither mode can undo: the fresh read that already happened on the call that missed. The
counter is what tells an operator the window is mistimed.

## The model is not a free choice here

Unlike `extract_llm`, this component cannot compact with any model you name — and the asymmetry is
structural rather than an oversight. `extract_llm`'s prompt **carries** the output it is compacting, so
any model can read it. This component's prompt carries an inventory, and the outputs are read from the
**prompt cache of the model being asked**. Only the request's model has that cache.

So `model.source: config` is not a cheaper configuration of this component, it is a broken one: the ask
would read nothing and degrade to paying fresh for the entire transcript. A `model` block is therefore
**refused** with an error naming that reason, rather than accepted and silently corrected.

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
| `min_tokens` | 1000 | Per-output floor for naming a candidate in the inventory. Every line is paid fresh, and a small output's removal cannot repay the marker it leaves behind. At 3000 this produced **zero** extractions across 3,437 production requests. |
| `pre_expiry_seconds` | 60 | Width of the pre-expiry window. The component's one unmeasured number. |
| `block_fallback` | `false` | Decline instead of falling back to a content-carrying completion when the cache read did not happen. |
| `marker_mode` | `full` | `full` is the only mode that keeps a removal recoverable. |

Every other key is a **config error naming its reason**, not an ignored one. `strategy`, `rewrite`,
`aggressiveness` and `max_chars` because an adjudicator selects no strategy and produces no rewritten
text — a silently accepted `rewrite: false` would read as "verified deletion-only is on" when nothing is
being rewritten. `model` because only the request's model has the cache (above). `context` and
`context_messages` because the conversation *is* the cached prefix. `max_calls` because one ask covers
every candidate. `economic_gate` because it prices a per-output cheap-model call, which this is not.

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
`sweep_prefix_cache_read_ok`, `sweep_prefix_cache_read_ZERO`, `sweep_fallback_used`,
`sweep_fallback_blocked`, `sweep_fallback_failed`, `sweep_fallback_no_model`, `sweep_no_asker`,
`sweep_no_prefix`, `sweep_ask_failed`, `sweep_inventory_of_one`, `sweep_kept_everything`,
`sweep_unparseable`, `sweep_reply_truncated`, `sweep_verdict_unusable`, `sweep_verdict_unknown_label`,
`sweep_verdict_duplicate_label`, `sweep_verdict_missing`, `sweep_drop_would_not_shrink`,
`not_in_pre_expiry_window`.

## What is not measured

Three questions the design records rather than answers, from
[the proposal](../proposals/sweep-adjudicator.md):

1. Whether the obligation quote pays for its tokens. Requiring evidence halved false drops at batch
   size, which is the shipped shape, so the measurement applies — but it has not been re-measured
   here.
2. Whether a spent-ness judgement needs `context: full`. The default is `recent` because `full` is
   what the predecessor lost money on, not because `recent` was shown sufficient.
3. The **width of the pre-expiry window**. It defaults to the codebase's own clock-uncertainty margin,
   which makes it safe rather than optimal. Widening it fires on more turns and invalidates prefixes
   with more remaining TTL; nothing measures either side of that trade.
4. How often `sweep_prefix_cache_read_ZERO` fires in practice. That number decides whether the
   fallback default is right, and it is the first thing to look at after this ships.
