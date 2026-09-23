# Advanced & experimental Offload components

Three Offload (LLM) components that are off-by-default, experimental, or reproduce a baseline
rather than recommending a preset: **extract_llm_sweep**, **agentdiet**, and
**cache_aware_summarizer**. Read the relevant section before turning one of these on — each has
sharp edges (cache-write economics, message-count restructuring, model-availability
requirements) that the deterministic components in
[Offload: deterministic reducers](offload-reducers.md) don't have.

---

## extract_llm_sweep

**Kind:** Offload. **Reversible:** yes (`marker_mode: full`, the default). **In presets:** `housellm`.

On a turn whose prompt cache has expired, this component asks a cheap model — a batch of tool
outputs at a time — which of them the agent still needs, and **removes** the ones it does not,
leaving a short shape descriptor plus a resolvable `<<cg:HASH>>` marker. It never rewrites anything.

### Why it is separate from `extract_llm`

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

### Why the cold turn is worth its own component

Measured on this deployment over 1.4 days: turns whose prompt cache had expired were **4% of
requests and 31% of spend** ($360 of $1,173, ~$1.64 each against $0.144 warm), because all 56.7M of
their tokens billed as `cache_creation_input_tokens`. The shipped pipeline saved 0.015% of it.

Two things are true only there, and both are load-bearing:

- removing a token is worth **12.5x** what it is worth on a warm turn (cache-write at 1.25x fresh
  against cache-read at 0.1x);
- acting at depth is **free**, because there is no live cached prefix left to invalidate.

### The contract

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

### One call, to the request's own model, over its cached transcript

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

#### `tool_choice`, and why the answer is a declared tool rather than a suppressed one

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

#### The verdict tool is advertised only where it can be used

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

### The trigger: pre-expiry, not cold

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

#### The second trigger, and what it has to pay for

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

#### The terms, and what the component knows about itself

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
| **premium** | how much a removed token is believed to be worth relative to the cache read it saves (`reward_premium`, default 1). The only term in the test that is a *belief* rather than a measurement — see [What the break-even cannot price](#what-the-break-even-cannot-price) |

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

#### When the trigger declines — and why it is usually *not* "no turns left"

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

#### Why there is no context-pressure floor

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

- **A pressure floor is also a HORIZON CAP.** The horizon is `turns x (1/p - 1)`, so a floor at pressure
  `p` puts a ceiling on the very term that authorises firing: 9x`turns` at 10%, 1x at 50%, **0.43x at
  70%**, 0.11x at 90%. At a 70% floor a transcript needs twelve assistant turns just to show a horizon
  of five. And the floor does not merely cap the multiplier, it *selects* for low turn counts — the
  requests that reach 70% fastest are the ones with few enormous tool outputs, which is precisely the
  shape this component exists to act on. Both factors fall together.
- **A pressure gate on a component that relieves pressure cannot fire once the component works.** The
  request stays at 20% *because* outputs are being removed. Requiring high pressure first is requiring
  the fever before the medicine that prevents it.

**Measured on iteration 024, whose firings are the only large sample that exists.** Counting only the
firings that still repay once priced against a correct window (74 of its 203):

| floor | firings kept | removal value lost |
|---|---|---|
| 10% | 57 of 74 | 8% |
| 20% | 51 of 74 | 12% |
| 30% | 48 of 74 | 21% |
| 50% | 39 of 74 | 37% |
| **70%** | **4 of 74** | **67%** |

So a floor is defensible at **10–20%** and destructive above about 30%. `min_pressure` exists for the
narrower job its own entry describes — keeping the ask ledger's warm-up samples off transcripts where
nothing has been superseded yet — and 0.70, tried in iteration 026, blocked 127 of iteration 024's 203
firings on its own and left the component firing zero times in a $65 run.

A free part of the same economy is `min_inventory`, which declines before any model call and raised
`sweep_inventory_below_min` **38 times** against 20 econ decisions in one run. It is not a substitute:
iteration 024's firings commonly carried **three** candidates, so a floor of 7 blocks a further quarter
of them.

#### What the break-even cannot price

`S x T > 11.5 x W` values a removal at **the cache reads it saves**. On the one iteration where this
component demonstrably helped, that is not what it was paid in.

| iteration 024, arm B | |
|---|---|
| sweep spend | **$20.26** |
| cache savings it banked | **$0.72** |
| ratio | **28:1 against** |
| task accuracy | 0.486 → **0.608**, 8 tasks better, 0 worse, clustered p = 0.0078 |
| cost per task | $2.808 → $3.419 (**+21.8%**) |
| steps per task | 24.3 → **29.0** (+19.4%) |
| cost per **step** | $0.1156 → $0.1179 (**+2%**) |

Read the last two rows together: the sweep's own overhead was 2%; the entire cost increase was **more
steps**. And the accuracy gains land exactly where the steps do — the 8 tasks that improved took +10.2
steps on average, the 7 that did not took −1.2. **The mechanism is trajectory headroom, not token
savings**, and headroom appears nowhere in the break-even.

`reward_premium` is where that gap is stated, in config, falsifiably: it multiplies the benefit, so a
premium of 20 says a removed token delivers twenty times the read it avoids. Priced at face value the
break-even authorises **19%** of the removal value that produced iteration 024's result; at a premium of
20 it reaches about **53%**. Two independent routes — sizing the premium to reproduce those firings, and
dividing that run's spend by its banked savings — both land near **28**.

**It cannot rescue a zero horizon.** `ceil(need/premium) >= 1 > 0`, so a request with no turns left
refuses at any premium. That is what the next section is about, and the two changes only work together.

#### The horizon is measured on the request the removal will leave behind

`T` used to be computed from the request **as it arrived** — which asks "how many turns remain if we do
nothing" and then charges the removal against that answer. At high pressure the two differ by everything:
a 90k request against a 64k window has no turns remaining and returns 0, so no benefit can ever repay it,
while the same request with 52k removed sits at 60% of the window with real turns ahead.

On iteration 024's own decisions the horizon was **exactly zero on 51 of 203 firings (25%)**, and a zero
refuses unconditionally. The growth *rate* still comes from the pre-removal request, because the rate is a
fact about history that already happened; only the *room left* is a fact about the future.

Note the asymmetry with `coref`, which shares this break-even but keeps the uncredited form: its
drop selection was calibrated against that expression, and moving the objective a measured component
optimises would invalidate those measurements rather than improve them.

### When the cache read does not happen

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

### The model is not a free choice here

Unlike `extract_llm`, this component cannot compact with any model you name — and the asymmetry is
structural rather than an oversight. `extract_llm`'s prompt **carries** the output it is compacting, so
any model can read it. This component's prompt carries an inventory, and the outputs are read from the
**prompt cache of the model being asked**. Only the request's model has that cache.

So `model.source: config` is not a cheaper configuration of this component, it is a broken one: the ask
would read nothing and degrade to paying fresh for the entire transcript. A `model` block is therefore
**refused** with an error naming that reason, rather than accepted and silently corrected.

### Safety

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

### Configuration

| key | default | what it does |
|---|---|---|
| `min_tokens` | 1000 | Per-output floor for naming a candidate in the inventory. Every line is paid fresh, and a small output's removal cannot repay the marker it leaves behind. At 3000 this produced **zero** extractions across 3,437 production requests. |
| `reward_premium` | 1 | How much a removed token is worth relative to the cache read it saves — the only term in the break-even that is a belief rather than a measurement. 1 is the unadjusted arithmetic. Values below 1 are refused (they assert a removal is worth less than the read it saves, tightening a gate that is already the restrictive term); above 100 is refused as a typo. See [What the break-even cannot price](#what-the-break-even-cannot-price) for the 28:1 measurement behind any value above 1, and read `premium` on `cg.sweep.econ` to re-price a run's declines without re-running it. |
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

### Counters

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
`sweep_prefix_cache_read_ok`, `sweep_prefix_cache_read_ZERO`, `sweep_prefix_cache_write`, `sweep_fallback_used`,
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

### Enabling it on real traffic

This component has never been measured on a workload independent of the one its thresholds were tuned
on. Every corpus behind `min_inventory`, `min_tokens`, `min_later_turns` and the 6%-vs-58% live-kept
curve is LOCA-derived (see LOCA iteration 028),
and on LOCA the cache arithmetic came out at 11.0 against a break-even of 11.5 — with `S` and `T` both
structurally thin there. So the first real deployment is a **measurement**, not a rollout, and it should
be configured to be readable rather than aggressive.

#### The two keys that turn it on

```yaml
extract_llm_sweep:
  econ_trigger: true    # without this it will almost never fire under load
  evidence: true        # co-reference index into the inventory line
```

`econ_trigger` is not optional in practice. The pre-expiry trigger fires on an idle clock, and any
concurrently-served deployment keeps refreshing the cached prefix — `not_in_pre_expiry_window` was
286/286 requests on LOCA, 378/378 on iteration 024 and 18/18 on iteration 022. Ship without
`econ_trigger` and you will measure a component that never ran.

Leave `reward_premium` at its default of 1 for a first deployment. Above 1 it asserts a removed token is
worth more than the cache read it saves, which is a belief about headroom value — and the one instrument
that could have priced it (the harness's own context clearing) has never fired, so nothing has tested it.

#### Whether it *can* pay, before asking whether it did

The gate is `S·T > 11.5·W`: mass removed, times turns remaining, against the cache-write it forces. Three
properties of the traffic decide it, and all three are readable before enabling anything:

| property | why it matters | what to look for |
|---|---|---|
| **candidates per request** | the model's judgement is a function of how many it compares; below `min_inventory` the component declines without asking | ≥10 settled, undecided tool outputs over `min_tokens`. LOCA supplied **1.5** and asked on 5% of requests |
| **session length** | `T` is turns *remaining*; a short session has nothing for a removal to pay back over | long-running sessions. LOCA's 10–30 step tasks are close to the worst case |
| **output size** | the rewrite cost is set by the **shallowest** removal, so mass per invalidation is what clears the break-even | large tool results, ideally several at similar depth. `min_tokens: 100` excluded 1,008 candidate-instances on LOCA |

A deployment that cannot supply ~10 candidates per request will produce a component that declines
correctly and teaches you nothing. Check that first.

#### What to read, in order

**1. Did it fire at all?** `acted`, `sweep_prefix_cache_read_ok`, `sweep_adjudicated`. If `acted` is 0,
read the gates before touching a threshold — `not_in_pre_expiry_window` (needs `econ_trigger`),
`sweep_below_min_pressure`, `sweep_inventory_below_min`. Note that `sweep_inventory_below_min` is raised
with `GateN(len(cands))`, so it counts **candidates, not requests**; dividing it by request count is a
misreading.

**2. Did it lose anything?** This is the question that decides whether to continue.

- `expand_unresolved_missing` **must stay 0.** It is the direct measure of a removal the agent then asked
  for and could not get back. It was 0 → 0 across iteration 028's arms with 105 drops performed.
- `sweep_drop_refused_obligation` — the model tried to remove an output it had just said was still
  needed. The removal did not happen, but a non-zero rate means the contract is not holding.
- `sweep_quote_fabricated` — the model cited transcript text that is not in the transcript. On this
  design it is the only signal that says the model is inventing.

**3. Did it pay?** Not from the dollar total — on LOCA the pass-level dollar difference was +4% and
paired per environment it was `+$0.077 ± $0.80`, i.e. noise. Compute the ratio instead, which is exact:

```
saved_tokens                     (component counter)
incremental cache creation       (arm with the sweep − arm without, or before/after enabling)
ratio = saved_tokens / incremental_cache_creation
```

**A ratio above 11.5 means the removals paid for the cache writes they forced; below it they did not.**
11.5 is `(2.50 − 0.20) / 0.20` — the incremental price of writing a token to cache rather than reading
it. LOCA realised **11.0**. Because the rewrite span is fixed by the shallowest removal, the lever is
**drops per invalidation** (`sweep_dropped / acted`; LOCA managed 8.75), not the number of asks.

**4. Is the ask worth its own price?** `econ_ask_not_repaid` and `prefix_rewrite_not_repaid` are raised
**exclusively** and must not be summed: the first means the batch cannot repay the price of *asking*, the
second that the cache-write does not earn itself back.

#### A first deployment that answers the question

- **Enable for a subset of sessions**, ideally long-running ones with large tool results — that is where
  `S·T` is largest and where a null result would be informative rather than structural.
- **Record the ratio, not the dollar delta.** The ratio is arithmetic on counters and needs no control
  arm; the dollar delta needs one and is dominated by step-count variance.
- **Treat any non-zero `expand_unresolved_missing` as a stop**, not as a rate to tune.
- **Expect it to decline often.** On LOCA it asked 15 times in 286 requests and that was correct
  behaviour, not a fault. `sweep_kept_everything` is likewise a deliberate keep-all.

### What is not measured

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

---

## agentdiet

!!! info "Offload (LLM) — lossy, reversible"
    A baseline reproduction of the published **AgentDiet** trajectory-reduction method: one
    cheap-model reflection per turn, on the step that has just aged past a fixed delay.

### Why it exists

`agentdiet` is a **comparable baseline**, not a recommendation. It reproduces the method from
*"Reducing Cost of LLM Agents with Trajectory Reduction"* (Xiao, Gao, Peng, Xiong — FSE 2026,
[arXiv:2509.23586](https://arxiv.org/abs/2509.23586)), which the authors call AgentDiet, so the
published approach can be A/B'd against context-guru's own reducers on the same traffic, agent and
benchmark. The paper reports **−39.9%…−59.7% input tokens** and **−21.1%…−35.9% total cost** at
unchanged task success on SWE-bench Verified and Multi-SWE-bench Flash.

### How it works

Its unit is the **step** — one assistant message plus the tool results that answered it — not one
tool output. Four things follow from that, and together they are what distinguishes it from
[`extract_llm`](extract_llm.md):

1. **A fixed age chooses the target, not size or economics.** When the agent has completed step
   `s`, only step `s − a` is eligible (`delay_steps`, a=2). The most recent `a` steps are never
   touched, so a bad reduction cannot corrupt what the agent is working on right now — the paper's
   protection against a malfunctioning reflection model.
2. **The model gets a sliding window of neighbouring steps**, serialized as XML
   (`context_steps`, b=1 ⇒ steps `[s−a−b … s]`). This is the part `extract_llm` structurally cannot
   do: seeing the steps around the target is what lets the model call content *redundant* (already
   stated nearby) or *expired* (mattered only to a finished sub-goal) rather than merely verbose.
3. **Two thresholds bound the spend.** A step below `min_step_tokens` (θ=500) never earns a call;
   and a reduction that comes back is applied only if it clears `min_saved_tokens` **or**
   `max_keep_ratio`, so a marginal rewrite does not pay a cache-write for a handful of tokens.
4. **A reduction is made once, then frozen.** Later turns replay the same bytes, so the request
   prefix stays stable and reductions accumulate over the session — which is what the paper gets
   for free by editing the agent's own trajectory in place.

The window is serialized in the shape the reflection prompt describes:

```xml
<step id="7">
<think>The fix works. Now run the existing suite to check nothing broke.</think>
<call tool="bash">{"command":"python -m pytest testing/test_collection.py -v"}</call>
<result id="0">… 74 collected, per-test PASSED lines, summary …</result>
</step>
```

### Before → After

```
before:  <result id="0">  … 74 individual "PASSED" lines … 73 passed, 1 xfailed in 4.48s
after:   ... (individual test lines omitted; mostly PASSED)
         ======= 73 passed, 1 xfailed in 4.48s  <<cg:…>> [full output: call context_guru_expand]
```

### Lossiness

Lossy but reversible — each reduced tool result is stashed under its own `<<cg:…>>` marker and
recovered via `context_guru_expand` / `GET /expand`.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `delay_steps` | 2 | *a* — steps of protection; only the step this far back is eligible. `0` is accepted for an ablation, but it targets the step the agent has just completed and so gives up the protection described above. |
| `context_steps` | 1 | *b* — steps of leading context in the window (`[s−a−b … s]`). |
| `min_step_tokens` | 500 | *θ* — a step below this is not worth a reflection call. |
| `min_saved_tokens` | 400 | Apply the reduction if it saves at least this many tokens… |
| `max_keep_ratio` | 0.8 | …or if it keeps less than this fraction of the step. |
| `model.source` | `incoming` | LLM source: `incoming` or `config` (the cheap model). The preset uses `config`. |
| `model.model` | *the source's own model* | The reflection model, on that source's endpoint and credential. The method's economics depend on it being much cheaper than the agent's. |
| `model.provider` | `anthropic` | Wire dialect for a config-pinned endpoint: `anthropic` \| `openai`. |
| `model.base_url` | *the provider's public API* | Pin a dedicated endpoint as a full URL. |
| `model.api_key` | *the process env key* | **Credential** for the pinned endpoint; empty falls back to the provider env key, which a hosted deployment refuses. Write-only on the settings page. |
| `model.auth` | `x-api-key` | Anthropic only: `x-api-key` \| `bearer`. |
| `marker_mode` | `full` | `full` (reversible) \| `summary` \| `off`. |
| `cache_tail_only` | `false` | Restrict new reductions to the uncached tail. See the warning below. |

`CONTEXT_GURU_AGENTDIET_TIMEOUT` (default `90s`) bounds one reflection call; `/stats` reports
`agentdiet_timeouts`, `agentdiet_errors` and `agentdiet_call_timeout_ms` beside it. A non-zero
timeout count means the budget is too small for the server's load, and that arm's savings are an
**undercount rather than a measurement**.

!!! warning "`cache_tail_only` defaults to `false`, unlike every other age-based offloader"
    The target step is chosen by age, so with `a ≥ 1` it is **always** inside the provider's cached
    prefix. A tail restriction would therefore make this component a silent no-op on every caching
    backend. The paper accepts one cache-write of the suffix per reduced step and counts it in its
    cost figures; because the decision is then frozen and replayed byte-identically, that write
    happens **once per step, not once per turn**. Set `true` only if you would rather keep the cache
    pristine and reduce nothing.

### Faithfulness to the paper

Defaults are the paper's tuned values (`a=2`, `b=1`, `θ=500`). `min_saved_tokens` (400) and
`max_keep_ratio` (0.8) come from the authors' artifact, whose apply-gate is
`saved >= 400 || keep < 0.8`; Algorithm 1 in the paper states this more simply as
`l_orig − l_reduced > θ`. The artifact's form is the default because it is what produced the
published numbers — set `min_saved_tokens: 500` and `max_keep_ratio: 0` to reproduce the paper's
stated gate exactly.

Three deviations, all forced by where context-guru sits:

- **The reduction is written in place**, into the step's tool-result messages. The paper's reflection
  module replaces the whole step with one assistant message; here only
  [`summarize`](summarize.md) changes the message count, and it must run alone for that reason.
  Tool results are where **63%** of trajectory tokens live by the paper's own accounting
  (30.4K of 48.4K).
- **Tool-call arguments are not reduced.** On Anthropic traffic bifrost's schema does not model
  `tool_use` blocks, so their name and input are not visible to any component here and the assistant
  message is not even rewritable. That forgoes the paper's `str_replace_editor` redundancy case
  (~25% of trajectory tokens) and is the main reason to expect a smaller reduction here than the
  published 39.9%–59.7%.
- **The prompt is written from the paper's description** of its four parts (job, format, the three
  waste categories with the examples the paper names, and the anti-loss guidelines) rather than
  copied from the authors' artifact. `components.Model` is one prompt in / text out, so the paper's
  system + user + assistant-prefill + `</step>` stop sequence is folded into a single prompt and the
  reply is parsed defensively instead (prose, code fences and a missing close tag are all tolerated;
  an unparseable reply reduces nothing). One case is declined rather than guessed: a `<result>` block
  whose `id` is missing or unparseable is used only when the step has a single result. On a step with
  parallel tool calls it is dropped, because placing it at `id 0` would put one tool's compressed
  output into another tool's message — smaller, so the never-worse check would pass it, and then
  frozen and replayed for the rest of the session.

### When it shines

Long agentic coding sessions with verbose tool output — test runs, build logs, directory listings —
where the same step is re-sent on every subsequent turn. Its `expired` category is the one signal no
other component here computes.

### When it's inert

Fewer than `b + a` completed steps; a target step below `min_step_tokens`; a reduction that fails the
apply-gate; no model available (it then replays what it already froze and reduces nothing new); or a
trajectory whose tool outputs are all small — GAIA-shaped traffic, where the median text tool output
is well under θ.

### Run it alone

The preset is `[format, agentdiet, cachesplit]` on purpose. The method's claim is what **one**
age-targeted reflection achieves; stacking context-guru's own offloaders beside it would reduce the
same tool outputs first, and there would be nothing left to attribute.

---

## cache_aware_summarizer

Offload (LLM). Replaces the middle of a long transcript with one model-written summary — the same
output shape as [`summarize`](summarize.md) — but builds the **summarization call** differently, so
that call can reuse a prefix the backend already holds instead of paying fresh prefill for tokens
it has already seen.

Run it **alone**: it changes the message count, and `apply`'s count-change rebuild re-emits each
retained message's original raw bytes by matching them exactly, so a message another component
edited in place fails to match and is re-marshalled instead.

### Why the call shape matters

`summarize` builds a fresh prompt — a preamble plus the rendered transcript as one user message.
That prompt shares no prefix with the conversation it describes, so the call is a cache miss on
every token.

This component sends **`[the conversation, verbatim and in order] + [one appended instruction]`**.
The rendered prefix is the one the agent's own request produced, so a prefix-caching backend can
charge prefill only on the appended suffix.

### How far the reuse actually goes

The prefix that gets hit is one the **backend** has seen, and the backend only ever receives what
this proxy forwards. The agent keeps sending its full uncompacted history, but from the first
triggering turn onward the backend has received **compacted** requests — so there is no
full-history prefix upstream to match, and the shared prefix is roughly the pinned head.

Two consequences, both worth measuring rather than assuming:

- the saving is largest on the **first** compaction and smaller afterwards;
- sending the full growing history as a side call can **evict** the compacted prefix the forwarded
  request needs.

Judge this component on the backend's own telemetry — `vllm:prefix_cache_hits_total`, or
`usage.cache_read_input_tokens`, which the OpenAI client records — never on a savings percentage.

### Where the instruction goes, and why it is per model

The appended instruction is an **operator** instruction, so `role: system` is the correct channel
where it exists: it is non-spoofable, and a trailing user turn on a long trajectory reads to the
model as more trajectory and gets summarized instead of followed.

But a system message at the **end** of the messages array is not universally accepted, and the
constraint lives in two different places — on Anthropic the **server** decides, on vLLM the model's
**chat template** decides (vLLM validates only that `role` is a string).

The two failure modes are not equally visible:

| | |
|---|---|
| **loud** | the provider returns `400 role 'system' is not supported on this model` |
| **silent** | a template **drops** or **hoists** the message. The model continues the task, and its next turn is recorded as the summary. |

`instruction_role: auto` resolves per model from
[`summarizer_model_profiles.yaml`](https://github.com/rossoctl/context-guru/blob/main/components/offload/summarizer_model_profiles.yaml).
That file has two parts: `system_models`, an allow-list of match strings that take the instruction as
`role: system`, and `profiles`, a provenance record that grants nothing and only says what was
established about each model. A pinned `instruction_role: system` for a model the allow-list does not
name **declines** at request time and increments `cache_aware_summarizer_unverified_system`, rather
than risking the silent case.

**`system_models` is empty, so every model resolves to `user` today.** The reason is that a profile
described a *model*, while what has to accept a trailing system message is the whole *path* to it.
The component requires a `components.MessagesModel` and only the OpenAI-shaped client implements one,
so every reachable deployment sends OpenAI-shaped requests — and a gateway translating those to a
native provider API may lift a mid-array system message into that API's top-level `system` field.
Measured against a LiteLLM gateway fronting Bedrock: `aws/claude-opus-5` with a trailing `role: system`
returned `400 … the conversation must end with a user message` on every call (the instruction had been
removed from `messages[]`), while the same request with `role: user` returned 200. End to end, the
`claude-opus-5` arm compacted nothing and reported only `cache_aware_summarizer_errors`; the
`claude-sonnet-5` arm, which resolved to `user`, cut 2228 tokens to 876.

To promote a model, verify the **path** — probe through the same endpoint and credential production
uses and confirm the instruction arrives last — then add its match string to `system_models`, or ship
a `profiles_path` override so the promotion needs no rebuild.

### The call is detached, so compaction takes two turns

A summary here covers most of the transcript, so the call is large by construction and its budget is
300 s. Running that inline would stall the triggering turn by minutes, billed against the agent's own
timeout — `summarize_async.go` exists because that was already found too expensive on the request
path.

So the control flow is the same two-turn shape `summarize` uses: **the turn that triggers commissions
a summary and forwards untouched; the next eligible turn finds the checkpoint and splices.** The
detached goroutine reuses the shared flight registry and the global concurrency bound, so a saturated
proxy sheds compaction (`cache_aware_summarizer_async_refused`) rather than queueing a call nobody is
waiting for.

Read the `async_started` / `async_committed` **pair**: started without committed is a summary that was
paid for and lost, which no other counter would reveal.

### Two gates, two different quantities

`min_tokens` asks *is the span worth a call*. `max_request_tokens` asks *can we afford the call* — and
they differ because the request carries the **whole conversation**, not the span.

`max_request_tokens` is a **refusal, never a truncation**. Truncating the conversation to fit is not
available: the appended-suffix shape is the entire mechanism, and a truncated conversation is a
different prefix that matches nothing. An over-large session declines and says so
(`cache_aware_summarizer_too_large`).

### Reversibility and reuse

`marker_mode: full` (default) stashes the replaced span through `commitMark`, so a store that
**refuses** the payload causes the compaction to be skipped rather than leaving a `<<cg:HASH>>`
marker pointing at nothing. `StashRoom` is checked before the model call, so a summary is not paid
for and then thrown away.

`resummarize_tokens` (default 6000) keeps a checkpoint: the spliced summary is re-emitted
**byte-identically** on later turns until the untouched tail passes that threshold. Without it the
forwarded prefix would change at the head on every turn and the component would invalidate the
cache it exists to protect. The checkpoint namespace is shared with `summarize` — safe because both
must run alone, and `CoveredHash` rejects a checkpoint whose covered prefix has changed.

The model's reply is treated as **untrusted**: `sanitizeSummary` strips forged expand markers (both
spellings), the summary sentinel and a premature `</summary>` before the text is framed as
trustworthy earlier context. The whole trajectory reaches the summarizer, so planted text in any
tool output gets a long run at it.

### Counters at `/stats`

| field | means |
|---|---|
| `cache_aware_summarizer_calls` | summarization passes paid for; this method's cost is per compacted turn |
| `cache_aware_summarizer_declined` | ⭐ no `MessagesModel` was available. **A declining arm compacts nothing and is byte-identical to `off` on every other metric** — read this before believing any delta |
| `cache_aware_summarizer_empty` | a call was paid for and returned nothing usable — the signature of a dropped or hoisted instruction |
| `cache_aware_summarizer_unverified_system` | declined a pinned `system` role for a model no profile verifies |
| `cache_aware_summarizer_refused_stash` | the store would not accept the span, so the compaction was skipped |
| `cache_aware_summarizer_profile_fallbacks` | `profiles_path` was unreadable, so the embedded registry was used |
| `cache_aware_summarizer_too_large` | declined because the outbound request would exceed `max_request_tokens` |
| `cache_aware_summarizer_async_started` / `_committed` | the **pair** is the signal — started without committed is a summary paid for and lost |
| `cache_aware_summarizer_timeouts` / `_errors` | fail-open paths; a timeout means the budget is too small for this load, an error means the route is wrong |

### Not yet measured

The prefix-cache-hit improvement this design predicts is **unverified** — there is no live run
behind it yet. The counters and the backend telemetry above are how to establish it.

See also: [Components overview](../components.md) · [extract_llm](extract_llm.md) ·
[summarize](summarize.md) · [Choose a preset](../reference/presets.md)
