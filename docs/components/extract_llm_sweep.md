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

Two construction facts, each measured and each a test: `tools` **are** in the cache key, so stripping
them reads a different, smaller entry; and the route rejects assistant prefill, which an appended user
message satisfies by construction.

### `tool_choice`, and why the answer is a declared tool rather than a suppressed one

This page previously stated the inverse of what is now measured — that forcing `tool_choice: none` was
"free, and **necessary**, or the prefix's tools make the model answer with a `tool_use`". The second
half of that sentence was **right about the mechanism and wrong about the remedy**, and the fix is not
to suppress the tool call but to declare a tool worth calling.

The ask carries the agent's own tools, because `tools` are in the cache key and stripping them costs
the read. Three arms, same transcript, same ask model (`aws/claude-sonnet-5` over LOCA S2L, three tasks,
three passes each, run sequentially):

| arm | `tool_choice` | verdict tool declared | asks replied | unusable | answered via `tool_use` | verdict coverage |
|---|---|---|---|---|---|---|
| A | `{"type":"none"}` | no | 20 | **6 (30.0%)** | 0 | 71.5% |
| B | omitted | **no** | 24 | **14 (58.3%)** | 0 | 41.2% |
| C | omitted | **yes** | 77 | **7 (9.1%)** | **43 (55.8%)** | 90.9% |

Fisher two-tailed: A vs C `p = 0.0245`, B vs C `p = 0.0000`, A vs B `p = 0.0755`.

Arm **B is the one that had never been run**, and it is the reason the tool is declared rather than the
`tool_choice` simply being dropped. Removing the suppression without offering an answer tool is
**worse than either other arm**: the model, now free to call something and offered only the agent's
tools and `context_guru_expand`, calls one of those. Directly observed on the wire by logging every
reply's content blocks — `thinking,tool_use:context_guru_expand` with **no text block at all**, which
the text-only extraction reads as the empty string and files as unusable. Arm B also lost 5 asks to the
90 s `llmCallTimeout` against 0 in arm A and 1 in arm C.

So the three shapes are: suppress the call and the model argues its verdicts in prose (arm A's failures);
allow the call with nothing worth calling and the answer is lost into somebody else's tool (arm B);
allow it and declare the right tool, and 55.8% of replies come back schema-shaped, at cache-read price,
with the rest still read by the unchanged prose parser (arm C).

`tool_choice` is not free in every form either. Naming a tool wrote a **second** cache entry (8,378
against the 8,268 already cached), so `tool_choice` does participate in the key when it names a tool,
even though `none` does not. Omitting it entirely is what reads the prefix for free.

The residual 9.1% in arm C is dominated by a reply with a `thinking` block and no answer at all. That
is a separate defect, present in every arm, and it is tracked apart from this component's contract.

### The verdict tool is advertised only where it can be used

`context_guru_adjudicate` is appended to the `tools` array of every request on the **Anthropic** route
whose pipeline **contains `extract_llm_sweep`** — and nowhere else. It costs a measured 946 bytes at the
head of the cacheable prefix, so both conditions matter:

- **Pipeline membership.** A preset with no sweep can never adjudicate. `off` is the control arm of every
  published comparison in this repo, and injecting there perturbed the baseline of all of them.
- **Provider.** `prefixAskerFor` returns nil for anything but Anthropic and `cheapmodel/openai.go` has no
  `CompletePrefixed` at all, so on the OpenAI route the definition is unreachable by construction.

Neither condition varies per turn — pipeline membership is fixed per **config document**, the provider
by the route — so the prefix stays byte-stable for every request under a given config and never flaps.
That is the distinction the cache argument actually requires: it forbids gating on something that
changes turn to turn, not on something fixed for the config. (Not "fixed at config load": `tenancy.go`
rebuilds a tenant's `*Pipeline` when the config document changes, mid-session. A config change that adds
or drops this component has already invalidated the prefix for much bigger reasons than 946 bytes.)

Because the model is told not to call it and sometimes still would, a call the **agent** makes is
answered on the response path before the client is written to — **except** when the model calls it
alongside a *client* tool in the same assistant turn. The response loop cannot continue a turn whose
other `tool_use` only the client can execute, so it hands that round over whole and
`adjudicate.AnswerStrayCalls` repairs it on the next request: the agent pays one turn, the session is
fine. `/stats` publishes `adjudicate_stray` either way — measured 0 across all three passes of arm C.

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

### The second trigger, and what it has to pay for

`econ_trigger` fires on **mass** rather than the clock, which is how the sweep reaches a session whose
cache keeps being refreshed — the long agent run with the most to save, and the one the pre-expiry
window can never reach. It pays a real cache-write to do it, so it has to clear a break-even first.

That break-even has three terms, and the third one is easy to forget because it is not a property of the
transcript:

| | |
|---|---|
| **benefit** | the mass removed, collected on every remaining turn — and discounted by how much of the inventory the adjudicator actually takes |
| **cost** | the cache-write the mutation forces, charged once, from the earliest dropped index to the cached boundary |
| **cost** | **the adjudication itself**, charged whether or not the answer turns out to be "drop something" |

Leaving that last term out is not a rounding error. Measured on iteration 025's pre-flight, it was the
*whole* cost: 9 asks authorised out of 9, six of which removed nothing, $0.4339 spent for $0.0017 of
value. The damage concentrated on the case where every candidate already sits past the cached boundary —
there the cache-write is genuinely free, which used to authorise unconditionally, and which is exactly
the case where the ask is the only thing being paid for.

Both the ask's price and the approval rate are **measured from this component's own asks** rather than
configured, for the same reason the rate card is preferred to a constant: a literal approval rate is one
workload's average wearing a threshold's authority. Two guards keep a self-referential gate from
strangling itself — a short warm-up, because a single ask can only ever report 0% or 100% of its
inventory, and a floor under the approval rate, without which one unlucky run of empty asks would
decline every future one and destroy the evidence that could revise the estimate.

Read `prefix_rewrite_repaid` against `econ_ask_not_repaid` and `prefix_rewrite_not_repaid`: the two
declines name **different** costs and are raised exclusively, so they sum rather than overlap.

### The terms, and what the component knows about itself

Every turn the sweep may ask the model *"which of these tool outputs are spent?"*. That question costs
money, so a test decides whether to ask at all. Its vocabulary:

| term | meaning |
|---|---|
| **candidate** | one tool output being considered for removal |
| **inventory** / **batch size** | how many candidates go into a single ask (`offered` in the logs) |
| **approval** | the fraction of offered tokens the model actually agrees to remove. Offer 10,000, get 3,000 removed, approval is 0.30 |
| **the ledger** | a running record of what this component's own asks have cost and how much they removed — how approval gets *measured* rather than assumed |
| **warm-up** | the first three asks, before the ledger can average. Assumed values are used instead: approval 1.0, and a cost estimated from the request's shape |
| **floor** | the lowest approval the ledger will report, 0.05. A guard so a measured zero cannot drive the expected saving to zero and disable the component outright |

The ledger holds four running totals — asks, dollars spent, tokens offered, tokens actually removed — and
derives two predictions for the next ask: cost as `dollars / asks`, and approval as `removed / offered`.

**So approval answers "is the question worth asking?"** — when this component pays to ask, how much does
it get back? A poor track record predicts a poor next ask, and the price stops being justified. It is the
component observing its own history.

Two things that vocabulary hides and that matter:

- **The thing being asked is the ADJUDICATOR, not the agent.** It is a second call to the same model,
  judging which outputs are spent. The agent doing the task never sees it. A low approval means the
  *judge* kept the outputs, not that the agent did anything.
- **A low approval is not automatically a failure.** It means the judge found those outputs still
  load-bearing, and that may be correct. If they genuinely are not spent, declining to pay for the
  question is the right answer.

**Which is exactly what makes the estimator dangerous: it is self-referential.** It predicts its own
future from its own past, and its predictions determine what evidence it receives. Predict low, do not
ask, learn nothing, keep predicting low — defensible at every individual step and permanently wrong if the
early samples were unrepresentative.

That is not hypothetical. On the iteration 026 probe the first three asks all carried **three** candidates,
a batch size this repo had already measured as one where the model does not act (about 94% kept when shown
a single output, against 58% dropped at ~15). One drop out of nine candidates, approval measured near
zero, clamped to the 0.05 floor — and because approval sits in the DENOMINATOR of the break-even, 0.05
multiplies the turns-to-repay by twenty. A real decision at `need=36, have=7` was declined that would have
read `need≈2` at approval 1.0. After that no ask cleared the bar, so no new sample arrived, and the
estimate stayed floored.

**The floor prevents approval reaching zero. It does not prevent the estimator getting STUCK**, which is
the failure that actually occurred, and the deeper defect is one of shape rather than value: approval is a
CURVE in batch size and the code stores a single point on it. One scalar cannot express "a batch of three
will not yield but a batch of eight will", and that sentence is both true and necessary. Tracked in the
issue on the approval estimate.

### When the trigger declines — and why it is usually *not* "no turns left"

The condition is `need > have`:

```
have = estimated turns before the request fills the window
need = turns of saving required to repay the one-time costs
     = ceil( (11.5 × rewritten  +  askUSD / cache_read_rate)
             ─────────────────────────────────────────────── )
                        offered × approval
```

Which gives **three** routes to a decline, not one:

| route | meaning |
|---|---|
| `have` falls | near the window ceiling, few turns left to collect on — this is the "no turns left" case |
| numerator rises | the question got expensive (more uncached prefix to read), or the rewrite reaches deeper |
| denominator falls | less mass on offer, or a measured approval rate saying the model will not take much of it |

**In the iteration 025 pre-flight only the last two fired.** `have` actually *rose* over the run, 27 → 120,
and the trigger declined anyway:

| | first ask | at the first decline |
|---|---|---|
| `offered` | 11,011 | 6,014 (÷1.8) |
| `approval` | 1.00 | 0.328 (÷3.1) |
| `askUSD` | $0.0161 | $0.0498 (×3.1) |
| **`need`** | **8** | **127** |
| `have` | 27 | 120 |

The numerator tripled as the ledger learned what an ask really costs; the denominator fell 5.6x as the
inventory thinned and the approval rate came in. **17x on `need`, while the turns available got better.**
So the decline meant *"the question now costs what it really costs, and what is left to ask about cannot
repay it"* — not *"we are running out of runway"*.

That is the shape to expect, because **the sweep eats its own lunch.** The first asks remove the large,
obviously-spent outputs (13,122 and 1,676 tokens here); what remains is smaller, while the ask's price
holds or climbs as the uncached prefix grows. The profitable sweeps happen and then the trigger shuts
itself off, which is why there is no call cap — it is self-limiting.

There is a second-order effect worth noticing in that table: `have` jumped from ~25 to ~120 immediately
after those first two removals. Taking 15k tokens out shrank the request, so more turns fit before the
window fills — **the sweep's own success bought it more runway**, which then part-funded the later asks.

### Why there is no context-pressure floor

A natural-looking economy is "do not even evaluate the trigger until the context is, say, 70% full — that
saves paying for asks early in a conversation". **This component deliberately has no such floor, and on
the measured workload it would disable it completely.** Every ask in the pre-flight fired between
**13.9% and 29.3%** of the 64k window:

| ask | pressure | tokens removed |
|---|---|---|
| 1 | 29.3% | **13,122** |
| 2 | 13.9% | 1,676 |
| 3–9 | 18.3% – 23.5% | 0 – 1,194 each |
| 10 | 25.4% | **5,978** |

A 70% floor blocks all ten, and the two asks doing most of the work — 19,100 of 26,528 tokens between
them — are the *first* and the *last*, both under 30%.

Two reasons this is structural rather than a quirk of one run:

- **`T` is the whole benefit.** The saving is collected once per remaining turn, so waiting for pressure
  waits for the moment `T` is smallest. Firing at 90% of the window means paying a rewrite to collect a
  saving roughly once. The profitable moment to compact is **earlier** than the moment of maximum
  pressure, which is the same finding stated at the top of this section.
- **A pressure gate on a component that relieves pressure cannot fire once the component works.** The
  request stays at 20% *because* outputs are being removed. Requiring high pressure first is requiring
  the fever before the medicine that prevents it.

The early-conversation economy people are reaching for already exists, and it is **free**:
`min_inventory` declines before any model call, and it raised `sweep_inventory_below_min` **38 times**
against 20 econ decisions in the same run. That is the filter doing the work a pressure floor was meant
to do, without a rate card, a window fraction, or a paid call.

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
| `min_inventory` | 10 | Fewest candidates worth asking about; below it the sweep declines without asking. The model's judgement is a function of how many candidates it **compares**: shown one output it scored 6% live-kept, ~15 together reached 58% at the lowest cost per output. Below the floor a removal is a guess, and a wrong removal costs content the agent still needs while a wrong keep costs one turn's tokens. |
| `pre_expiry_seconds` | 60 | Width of the pre-expiry window. The component's one unmeasured number. |
| `evidence` | `false` | Add the co-reference index's record to each inventory line. It is **evidence the model weighs, never a filter** over the candidates — a pre-filter left about one candidate per request, collapsing a bulk arm into the per-output shape refuted at 6% live-kept. Also adds a paragraph teaching how to read the counters; counters with no explanation invite an invented reading. |
| `econ_trigger` | `false` | Add the **economic** trigger alongside the pre-expiry window, so a live cached prefix can be swept when the saving outruns the cache-write it forces. The two are OR'd and neither contains the other: pre-expiry fires on the clock and cannot reach a session whose cache keeps being refreshed — the long run with the most to save — while econ fires on mass and cannot know how much time is left. |
| `econ_ignore_ask_cost` | `false` | Restore the econ trigger's original break-even, which charged the cache-write and **not** the adjudication that reads it. Left out, that authorised 9 asks in 9 on iteration 025's pre-flight, six of which removed nothing: $0.4339 spent against $0.0017 of value. Set true only to attribute a run's difference to the change. |
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
`not_in_pre_expiry_window`, `sweep_inventory_below_min`, `drop_unaffordable_pruned`,
`prefix_rewrite_not_repaid`, `econ_ask_not_repaid`.

The last two are raised **exclusively**, and reading them as one number loses the finding:
`prefix_rewrite_not_repaid` means the cache-write does not earn itself back, `econ_ask_not_repaid` means
the batch cannot repay the price of *asking* about it. `prefix_rewrite_repaid` is the matching event when
the trigger does fire — and it says the batch was worth asking about, never that a saving was banked.

## What is not measured

Three questions the design records rather than answers. The full argument, including which
measurements refuted earlier versions of this design, is in `docs/proposals/sweep-adjudicator.md` in
the repository — deliberately not published here, because it is addressed to whoever changes the code
next rather than to whoever runs it.

1. Whether the obligation quote pays for its tokens. Requiring evidence halved false drops at batch
   size, which is the shipped shape, so the measurement applies — but it has not been re-measured
   here.
2. Whether a spent-ness judgement needs `context: full`. The default is `recent` because `full` is
   what the predecessor lost money on, not because `recent` was shown sufficient.
3. The **width of the pre-expiry window**. It defaults to the codebase's own clock-uncertainty margin,
   which makes it safe rather than optimal. Widening it fires on more turns and invalidates prefixes
   with more remaining TTL; nothing measures either side of that trade.
4. Whether the **co-reference evidence** helps. The 58%-live-kept figure that justifies this shape was
   measured with an index supplying `refs` / `ref_age` / `used_frac` per candidate; on `main` the model
   gets none and reasons from the transcript alone. Plausibly better — it is not limited to exact
   matches, and the index's blind spot was transformed reuse — but untested. `AdjudicationItem.Evidence`
   is the seam it will arrive through.
5. How often `sweep_prefix_cache_read_ZERO` fires in practice. That number decides whether the
   fallback default is right, and it is the first thing to look at after this ships.
