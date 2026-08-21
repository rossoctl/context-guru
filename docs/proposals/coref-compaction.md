# Co-reference-aware compaction (`coref`)

**Status:** mechanism implemented and tested; the §7 measurement pass has **run on three corpora**
(Claude Code, UltraHorizon, LOCA-bench) — [results](../results/coref-density.md) — but not yet on the
eval-box captures the acceptance criteria are written against. The `coref` component ships **opt-in, in no preset**,
with the calibrated (`closed`) cut **off by default**. See [implementation status](coref-implementation.md).
**Headline numbers:** unreferenced tool-output mass is **21% on interactive traffic and ~70% on
benchmark traffic** (a 3.3x workload difference, not a constant), a reference consumes a median
18.7% of what its output introduced, and — the result that most affects the design — **recency is
nearly inert while reference count does all the discrimination**.
A later [held-out experiment](../results/coref-selection-experiment.md) added the decision-quality
numbers the density pass could not: the deterministic index keeps **95%** of what the agent went on
to need while removing 11.8% of mass, no model-in-the-verdict arm beat it, `cut_unreferenced`
carries an irreducible **11% false-drop**, and selective removal **cannot replace a summarizer** at
any aggression setting — see [§8b](#8b-what-the-held-out-experiment-settled).
**Related:** [the component reference](../components/coref.md) · [cheat sheet](../reference/coref-glossary.md) ·
[dynamic, model-aware triggers](../components.md) ·
[improvement plan §0, §C](../results/improvement-plan.md) · [agent compaction](../how-to/agent-compaction.md)

The idea, as originally posed: once a session passes a context threshold, compact — but pick
*what* to drop by looking at **back-references**. If a later turn references an earlier tool
result, that reference means one of two things:

- **A.** The model already took what it needed from that output (it lifted one value out of a
  large response and does not need the rest).
- **B.** The model has effectively marked the output as important, and may need it again.

The original framing separates A from B by **distance from the current turn**: B for recent
references, A for early ones. And it notes the asymmetry that matters — A licenses a *large*
cut, a large cut rewrites the cache, so A must only fire when confidence is high.

This doc does three things: says what a reference actually is in our traffic, argues that
distance is the wrong discriminator and proposes a better one, and then prices the whole idea
against numbers already measured in this repo. The pricing is the part that changes the design.

!!! info "Scope: this proposal is now explicitly about the **caching** regime only"
    Earlier drafts priced both regimes. That was wasted breath: on a non-caching backend every
    turn re-sends the whole transcript at full input price, so the bill grows quadratically in
    session length and the workload is uneconomic before any component's decisions matter.
    **The non-caching regime is out of scope by decision, not by oversight.**

    This narrows the proposal in a useful way. `coref` no longer has to justify itself as a
    general token reducer; it has exactly one job — **be worth a cache-write** — and every
    section below is a test of that. It also removes the only argument for a `coref` variant that
    scans all messages instead of the tail: the tail restriction was never the point, the
    cache-write was.

    Two conventions are being **changed** as a consequence, and both are recorded where they
    live: `TailOnly` is no longer treated as inviolable for backward-looking offloaders (§5.3),
    and `allow_on_caching_backend` is no longer a blanket veto on model-calling offloaders — the
    gate becomes "does this pay for the rewrite", which is a measurement rather than a policy.

---

## 1. Why today's relevance signal cannot do this

Every LLM component gets its relevance signal from `conversationGoal`
([`components/offload/common.go`](https://github.com/rossoctl/context-guru/blob/main/components/offload/common.go)): the first user turn
(the task), plus the most recent assistant and user turns (current intent). Tool outputs are
deliberately excluded — they are the mass being reduced, not the goal.

That signal is **forward-looking and position-free**, and both halves matter:

- *Forward-looking* — it describes the destination ("fix the failing auth test"), not the history.
- *Position-free* — it is one blob of text. Nothing in it says which message an output was, or
  which earlier output a later turn leaned on.

Concretely. Suppose turn 4 read `src/auth.py`, turn 5 said "the bug is `TOKEN_GRACE_SECONDS`",
and thirty turns later the agent is editing tests. Asked "is the turn-4 output still needed?",
`conversationGoal` can only answer "well, the task is still about auth" — which is true of every
output in the session and therefore decides nothing. What actually settles it is that the one
value the agent ever took out of that output (`TOKEN_GRACE_SECONDS`) is sitting in turn 5, and
turn 5 is not going anywhere. That is a *positional, backward-looking* fact, and today's signal
cannot represent it at all.

Co-reference is therefore not a tuning change to an existing input; it is a new input, and it is
the only input that can justify dropping a *large*, *early* span rather than projecting a recent one.

The good news is that the repo already contains most of the machinery, pointed the other way.
Two existing pieces, and what each contributes:

- **`internal/extract/deterministic.go`** keeps a list of "important keys" (`id`, `status`,
  `state`, `name`, `error`, `reason`, `date`, `time`) used to decide which leaves of a JSON blob
  are worth keeping when shrinking it. That list is already an answer to *"which parts of an
  output is a model likely to carry forward?"* — the same question a reference detector asks.
- **`internal/extract/contain.go`** checks that a shrunken output is a *subset of* its original —
  a guard that the shrinker did not invent text.

The second is the reusable idea, run backwards. Today it asks *"is this compacted text contained
in the original output?"*. Invert the direction and ask *"is this span of the original output
contained in a **later message**?"* and the same containment test becomes a reference detector.
Same primitive, opposite direction: today it validates a rewrite, inverted it measures reuse.

That is the beginning of a reference index — beginning, because §2 shows raw containment is
badly wrong on its own.

## 2. What a reference is, in three tiers

Ordered by how deterministically it can be detected:

| Tier | Signal | Example | Detectable |
|---|---|---|---|
| **1** | `tool_use_id` ↔ `tool_result` pairing; and **literal carry-over** — a span introduced by tool result *i* reappearing verbatim in a later `tool_use` argument or assistant text (paths, symbols, line numbers, IDs, hashes, error strings) | output: `TOKEN_GRACE_SECONDS = 0` → later turn: `Edit(old="TOKEN_GRACE_SECONDS = 0")`. The string is *identical*, so a substring test finds it. | exact, zero LLM |
| **2** | **transformed** carry-over — the agent summed the rows, converted units, reworded the finding. No substring match exists | output: `[{"ms": 1200}, {"ms": 1800}]` → later turn: *"total latency is 3 seconds"*. `3` appears nowhere in the output; it was computed. Same for `1200ms` → `1.2s`, or `ETIMEDOUT` → *"the request timed out"*. | no; this is the deterministic ceiling |
| **3** | **semantic** — "as I noted earlier", "per the schema", a plan step that depends on an observation without naming it | output: a directory listing → later turn: *"as I saw earlier, the tests live beside the source"*. The reference is unmistakable to a reader and carries **no shared token at all**. | LLM only |

Tier 2 is the objection raised on the thread — values drift through paraphrase and unit
conversion, so exact matching will miss real references — and the "maybe it covers 90% of
cases" instinct is the right *shape* of estimate. It is also a measurable quantity rather than
an arguable one, which is what §7 is for.

### The confound that decides whether Tier 1 means anything

A naive implementation will report near-total reference density and be wrong. If the agent
calls `Read(src/auth.py)`, the path appears in the **`tool_use` argument**, is echoed in the
`tool_result`, and appears again in a later `Edit(src/auth.py)`. Exact matching sees a
reference from the output to a later turn. But the path was never *extracted from the output* —
it was already in context before the output existed.

The only sound signal is **novel** tokens: identifiers the tool output *introduced*, which do
not appear anywhere at or before the `tool_use` that produced it, and which are then reused
later. Everything else is echo. The measurement script implements this as a prior-vocabulary
exclusion, and it is not an optional refinement — without it the numbers are meaningless.

## 3. Distance is a proxy; open-vs-closed is the discriminator

Distance conflates two different things. A span referenced three times, forty turns ago, is not
case A — it is a hot span that happens to be old. What actually distinguishes A from B is
whether the reference is **closed**:

- **Closed (A).** The span yielded a value that now has a **surviving copy** — written to a
  file, carried into a plan or todo, or restated in a later message that will itself survive
  the cut. The original is redundant *with something still in context*. Cut hard.
- **Open (B).** Referenced **repeatedly**, or **most recently**, or referenced without any
  specific value having been lifted out (the agent is still surveying, still searching). Keep.

The practical payoff is that "certain enough" stops being a confidence score on a distance
curve and becomes a **verifiable predicate**: take the large cut only when you can point at the
surviving copy. Score on `(reference count, how long ago the last reference was, witness
present)` — with recency measured **from the head of the transcript**, not from the output's own
position, since "recent messages vs early messages" is a statement about now.

And the witness looks free. `coref` only ever cuts **tool outputs**; references live in
**assistant** turns, which are never cut. So a reference is always a *surviving copy of
something* — which is what makes the closed case cheap to establish rather than a second search.

**But "a surviving copy of something" is not "a surviving copy of what is needed", and the gap
is a real hole in the argument above.** Raised in review, with this counter-example:

```jsonc
// tool output
[{"name": "david", "id": 123, "address": "foobarbaz"},
 {"name": "osher", "id": 235, "address": "banana"}]

// the model's next turn
"I need to remember david 123 address."
```

The reference is real. `david` and `123` genuinely came from the output and genuinely reappear.
But the value the model actually needs — `foobarbaz` — was **never copied into a model turn**. It
referenced an **anchor** precisely *in order to* point at a payload it did not restate. Cut the
output and the address is gone.

So the witness argument holds only when the reference *carries* the value, and an exact matcher
cannot tell an anchor reference from a payload reference. Three consequences:

1. **`closed` cannot rest on "referenced once, long ago" alone.** That is exactly the anchor
   case's signature, so it is the *reason* `cut_closed` ships off rather than merely an
   abundance of caution. Distinguishing anchors needs a signal this index does not have — most
   plausibly requiring the reference to have consumed a large share of what the output
   introduced, which is `used_frac`, and which cuts against the reading in §7 that a *low*
   `used_frac` is evidence for case A. A low `used_frac` is in fact **ambiguous**: "took the
   value, rest is chaff" and "took an anchor, still needs the payload" look identical.
2. **It is not a contradiction of case B, it is a demonstration that the A/B labels are not
   observable from a reference alone** — which is the same conclusion §3 reaches about distance,
   arrived at from the other side.
3. **The record shape it uses is exactly the shape this index is blind to.** `david`, `123`,
   `foobarbaz` are short lowercase words and a 3-digit number — none survive the precision rules
   in §2, so the index sees *no* novel tokens at all. That produced a separate and worse defect,
   now fixed: see `opaque` below.

### `opaque`: absence of evidence is not evidence of deadness

The counter-example above exposed a defect in the first implementation. An output that
introduced **no trackable identifier** was scored `unreferenced` — because both states satisfy
`refs == 0` — and `unreferenced` is what the DEFAULT configuration cuts. But the two mean
opposite things:

| state | meaning |
|---|---|
| introduced 200 identifiers, no later turn touched one | evidence of deadness → the safe cut |
| introduced nothing the index can see | **no evidence at all** → no opinion |

Measured, this is not a corner case: **8% of tool-output mass on interactive traffic, 20% on
UltraHorizon and 40% on LOCA-bench** introduces nothing the index can track. On LOCA that is 11
outputs averaging 22k tokens — bulk record and spreadsheet dumps of human-readable values, the
exact shape of the counter-example. The default config was about to delete them on no evidence.

`opaque` is therefore its own class and is **never cut**. The asymmetry is deliberate: an opaque
output costs tokens, while a wrongly cut one costs an `expand` round-trip plus a cache-write, and
can cost the task.

### The opportunity floor

The same review raised the mirror-image error: an output near the *tail* has had no chance to be
referenced yet, so scoring it as unused would make a batched pass preferentially cut the most
**recent** context — the worst possible choice. `mask` avoids this with `keep_recent`; `coref`
now expresses it in turns (`min_later_turns`, default 8): an output with fewer model turns after
it than that is treated as `open` regardless of everything else. §7's measurement had *bounded*
this bias but nothing had *guarded* against it.

Framed this way, `coref` is `dedup` generalized: from "this tool output is byte-identical to
another" to "this tool output's useful content survives elsewhere in the request".

## 4. The economics, and why they reshape the design

This is where the proposal has to survive contact with what the repo already measured
([improvement plan §0 and §C](../results/improvement-plan.md)).

**Three measured facts:**

1. **The agent appends.** 1124/1124 consecutive turn pairs carry the entire previous turn as a
   byte-identical prefix; 232/232 re-sent large outputs live at exactly one stable message
   index for the whole session. Nothing is "re-sent as new bytes" — a re-read is a cache-read
   of a stable prefix.
2. **A cache-write costs 11.5 cache-reads** — `($2.50 − $0.20)/$0.20`. Mutating already-cached
   content rewrites the entire suffix.
3. **Steps dominate the bill.** `corr(Δsteps, Δcost) = +0.95` on every arm and both benchmarks.
   Unique token removal is a rounding error: 0.024% of billed input on SWE-bench, 0.127% on
   Terminal-Bench. The SWE-bench win was a −13.7% step reduction that multiplied 165k removed
   tokens into −18.3M cache-read tokens — **110× leverage**.

Now price a cut. Cut `S` tokens at message index `i`, with `W` tokens of transcript after `i`,
and `T` turns remaining in the session. The prefix hash breaks at `i`, so the suffix is
cache-**written** once, then read cheaply thereafter:

```
cost    = W × (2.50 − 0.20)  =  11.5 × W   (in cache-read-equivalents)
benefit = S × T × 0.20       =   S × T
break-even:  S × T  >  11.5 × W
```

Put a real shape to it. A 5k-token output sitting at 20% depth of a 150k-token transcript
gives `W ≈ 120k`, so it needs `5k × T > 1.38M` → **T > 276 turns**. That does not happen.

> **A single early cut can never pay for itself on token savings alone.** This is not an
> argument against the idea — it is the quantified version of the original concern about cache
> rewrites, and it dictates the design.

!!! note "`W` is not "everything after `i`" — it is set by the breakpoints, and that is favourable"
    The formula above prices `W` as the whole suffix, which is the conservative reading and the
    one the measurement used. The provider is more forgiving than that. Anthropic caching has
    **at most 4 `cache_control` breakpoints** and reads from the deepest one whose prefix still
    matches, with a 20-block lookback. So a cut at index `i` invalidates only from **the nearest
    live breakpoint at or before `i`** — the blocks before it are still served from cache. The
    honest cost term is:

    ```
    cost = 11.5 × (nearest live breakpoint at or before i  →  end)
    ```

    Two consequences worth acting on. **Cutting several outputs that share one breakpoint span is
    free relative to cutting one of them** — which strengthens the batching argument in a way §4
    understates. And **where the breakpoints sit is a lever `coref` could pull**, via
    [`cachesplit`](../components/cachesplit.md): placing a breakpoint just below the intended cut
    depth bounds the damage in advance.

    Unmeasured, and it needs to be before this is leaned on: whether adding a breakpoint over
    bytes the provider has *already* cached itself incurs a write charge. If it does, the lever
    costs what it saves. Cheap to answer with a two-request probe against real usage figures.

Three things *can* pay, and they are the design:

- **Batching.** One rewrite serves every cut taken at that boundary, so `S` is the **sum** of
  all cuts in the pass. Cutting 60k of a 150k transcript at 20% depth needs `T > 23` turns —
  plausible in a long session. So: a **rare, batched, threshold-triggered pass**, never a
  per-output per-turn decision. The original instinct — A fires only when we are certain, and
  it is a big cut — is exactly right, and this is why.
- **Step reduction.** At `corr = 0.95`, removing rot that costs the agent even a few turns
  dominates all token arithmetic. The objective function is **steps and reward, not bytes**.
- **Deferring the agent's own compaction.** Claude Code compacts at
  `min(window, configured) − min(maxOutput, 20000) − 13000` (167k on a 200k model), counted from
  the **API's** reported usage — so our reduction genuinely pushes that back
  ([agent compaction](../how-to/agent-compaction.md)). Staying under that threshold avoids a
  full-transcript summarization that is both a large cache event and a quality loss. Plausibly
  the largest prize here, and the reason a threshold-triggered regime is the right frame.

One more asymmetry, stated carefully because the obvious version of it is wrong. It is
tempting to say a wrong cut is not a wrong answer, only a `context_guru_expand` round-trip
plus a cache-write. **That holds only when the model notices.** Expansion is
model-initiated — the tool is advertised and the host loop merely answers a call — and
nothing in the system detects a bad cut. So a wrong cut has three outcomes:

| | Outcome | Cost |
|---|---|---|
| 1 | The model notices and expands the right marker | one round-trip + a cache-write |
| 2 | It notices something is missing but cannot tell which marker holds it | several expands, or it proceeds without |
| 3 | **It never notices** | it answers from less than it had — silently |

Only (1) is the cheap case. **Reversibility is a capability, not a guarantee:** the stash
guarantees the bytes are recoverable, never that they get recovered.

Row 3 is the one that matters, and Tier 3 is where it lives. A missing Tier-1 reference is a
token the model goes looking for and cannot find. A missing *semantic* reference is the model
reasoning from something it no longer has — there is nothing to look up, so nothing prompts
the expand call. The result is a plausible answer built on less evidence.

Two consequences the rest of this document depends on:

- **`expand` rate is a precision metric for noticed errors only.** It is still the right
  inner loop — cheap, available on any traffic, no scoring — but it is *blind to row 3* by
  construction. Anything that improves it by making the model expand less could be an
  improvement or could be row 3 getting worse, and the metric cannot tell you which.
- **Reward is therefore the only instrument that sees the worst failure**, which is why §7
  and §8 treat it as a gate rather than as one number among several.

What the design *can* influence is the gap between rows 1 and 2: whether the residue left in
place lets the model tell that this marker is where the thing it wants lives. That is why the
marker describes the SHAPE of structured content (`200 records, fields: address, id, name`)
rather than only peeking at its first line, and why it never asserts that the cut was safe —
an earlier version wrote "no later turn referred back to it", which is precisely the claim
that is false in the Tier-2/3 case, and which reads as reassurance not to bother expanding.

## 5. Hard constraints the codebase imposes

These are not preferences; each one is a property of existing machinery.

1. **Decisions must be latched, not re-derived.** `repairLostFreeze`
   (`components/offload/state.go`) is documented as safe *only* for offloaders whose
   replacement is a pure function of `(content, config)` — `mask` and `failed_run` qualify
   because their output is position-independent. A co-reference decision is **history-dependent
   by construction**: whether to cut span *i* depends on messages after *i*, so re-deriving it
   next turn may yield different bytes, which is precisely the flip that rewrites the suffix.
   `coref` must therefore **store its decision per session** and replay the latched bytes.
   `extract_llm` is already excluded from freeze-repair for the analogous reason (sampled
   output); `coref` inherits that exclusion.
2. **One-way: keep → cut only.** New evidence can never resurrect a span, because un-cutting is
   a second prefix rewrite. Monotonicity is a cache-cost requirement, not tidiness.
3. **`TailOnly` is being deliberately violated, so it must be budgeted.** Every other
   age-based offloader refuses to touch the cached prefix (`Ctx.TailOnly`,
   `components/component.go`). `coref`'s entire purpose is to mutate the prefix. That makes it
   the first component that must **spend** cache-writes on purpose, under an explicit
   per-session budget (a bounded number of rewrite events), with the spend reported next to the
   benefit — the dashboard already has the slots (cache-frozen tokens, restorations, reverts).
4. **`freeze` / `reapplyFrozen` are mandatory.** Improvement plan §C3 records 101
   compacted→full→compacted flip turns and 15 non-byte-stable replays because 5 of 7 offloaders
   never wired freeze. A component that mutates the prefix cannot ship without it.
5. **Reversibility and the expand-loop guard.** Standard `<<cg:HASH>>` + Store stash, and
   `MarkKeptVerbatim` on anything the agent expands, so a restored span is never re-cut.
6. **Fail open, never worse.** Unchanged: any error reverts this component only; a pass that
   would not shrink the request is reverted.
7. **`skipReduce` makes `coref` and `extract_llm` mutually exclusive per output, first-come.**
   Every offloader consults `skipReduce` (`components/offload/state.go:292`), which refuses any
   content already carrying an offload marker. So whichever of the two reaches an output first
   owns it: once `coref` has replaced it with `<<cg:HASH>>`, `extract_llm` sees a placeholder and
   declines, and vice versa. They do not compose or stack on the same output — pipeline order is
   the whole policy. That is a **design constraint that has never been stated**, and it decides
   the shape of the answer: a "coref-aware `extract_llm`" and a separate `coref` component are
   not additive alternatives, they are competing owners of the same candidate set. If the two
   ideas are to combine, they must combine **inside one component's decision**, not as two
   components in a pipeline.
8. **The kept-verbatim guard is cross-session and best-effort, and both halves are surprises.**
   `MarkKeptVerbatim` keys purely by content hash (`keptKey(ck) = "cg:keep:" + ck`,
   `state.go:275`) with **no session in the key** — the comment says "session-independent" and
   means it. So one expand of a given byte-identical output permanently exempts *that content in
   every future session*. For genuinely per-session content that is harmless; for content that
   recurs across sessions — a config file, a repeated banner, a standard schema dump, exactly the
   high-value repeated mass — **one agent asking for it back once opts it out of compaction
   globally, for every session thereafter.** Nothing reports this, so the effect is a slow,
   invisible erosion of yield that looks like the component getting worse over time.

   And it is best-effort in the other direction: the flag goes through `Store.Put`, so it carries
   the store's default TTL *and* competes for capacity in the same LRU as multi-kilobyte stash
   payloads. A one-byte guard flag can be evicted by payload pressure, after which the content is
   re-cut and the expand loop the guard exists to prevent can recur.

   Neither behaviour is `coref`-specific and neither is a `coref` bug — but `coref` is the first
   component whose cuts are **latched and never revisited**, so it is the first for which a lost
   guard flag is unrecoverable rather than self-healing next turn.

   **Both halves are now fixed.** `keptKey` is scoped by session — the minimal scope that still
   prevents every loop the guard was built for, since the loop is intra-session by construction —
   and `store.KeptPrefix` joined `DefaultPinPrefixes`, so a one-byte guard no longer competes for
   LRU capacity against the multi-kilobyte stashes it guards. The scoped session travels from
   `apply.Trace.Session` through to the proxy's expand loop rather than being recomputed there,
   so the mark is always written under the id the pipeline compacted under. An empty session is a
   no-op rather than a global mark. Two tests cover the half that used to be wrong: the exemption
   does not leak to another session, and it still holds for the session that earned it.

## 6. Trigger integration

`coref` is an expensive, prefix-mutating pass, so it gates on the existing `Trigger`
(`components/trigger.go`) rather than inventing thresholds — `MinRequestFrac` against the
dynamically resolved `Ctx.CtxWindow` is the natural dial, which keeps it model-general.

Two additions the current `Trigger` cannot express, both implied by §4:

- **A remaining-turn estimate `T`.** The break-even inequality needs it. Cheap proxies:
  elapsed turns against the threshold distance, or the observed step rate. Firing at 90% of the
  window is firing when `T` is nearly zero — i.e. paying a rewrite for nothing. The
  counter-intuitive consequence: **the profitable moment to compact is earlier than the
  moment of maximum pressure.**
- **A rewrite budget** (constraint 3), which is a policy field rather than a shape threshold.

### The deferral gate: designed, unquantified

`min_batch_frac` is a correct implementation of the **token** argument in §4 and a poor proxy for
the **deferral** argument — which is the one §7's measurement says actually pays. This subsection
records the gap, the corrected arithmetic, and the order in which it should be closed. None of it
is built.

**The prize is a step function, and clearing the threshold is not enough.** Cutting to exactly the
threshold buys one turn: the next turn grows the transcript, crosses again, and now you either eat
the compaction anyway or pay a **second** cache-write — at the point where `W` is largest and each
write is most expensive. So the requirement is not `deficit`, it is:

```
required cut  ≥  (usage − threshold)  +  growthPerTurn × headroomTurns
```

Measured on the 19 real sessions in [the corpus](../results/coref-density.md) that passed Claude
Code's 167k threshold, expressed as a share of the request:

| headroom bought | required cut | achievable with `unreferenced` + `closed` |
|---|---|---|
| H = 0 (bare clear) | 7.3% | 10/19 |
| H = 20 | 12.6% | 5/19 |
| H = 40 | 18.0% | **0/19** |
| H = 60 | 23.5% | **0/19** |

So a bar high enough to avoid paying twice (≈20–25% of the request, which is what 40–60 turns of
headroom costs) is a bar **Tier-1 matching cannot clear**. Mean available cut is 4.4% of the
request for `unreferenced` and 9.6% including `closed`. That is also how the old
`min_batch_frac: 0.15` default was found to admit **1 of 19** sessions, and 0 at the shipped cut
set — a gate no traffic can clear is an off switch that looks like a threshold.

!!! warning "One figure here is partly an artifact"
    Peak request ≈180k and deficit ≈13k are shaped by `cc_capture.py` segmenting transcripts at
    180k tokens, so peaks cluster there by construction. The durable number is the one that does
    not depend on it: **available cuttable mass is 4–10% of the request.**

**What the gate should ask instead.** `coref` never runs alone, and it is the only component that
pays a prefix rewrite — `mask`, `extract` and `cmdfilter` all work in the uncached tail and are
cache-safe (`extract_llm`'s tail pass measured 12.5% on SWE-bench, 27.5% on Terminal-Bench —
figures long miscredited to `mask`, which is inert behind the tail gate). So the deferral
prize is mostly earned by the components that pay nothing for it, and `coref` is a marginal
contributor paying the most. Its gate should therefore not ask "is my cut large?" but "**does my
cut change the outcome?**":

| Case | Condition, evaluated at `coref`'s entry | Action | Why |
|---|---|---|---|
| **Already safe** | under the threshold with headroom before `coref` runs | **do not cut** | The prize is already won; a rewrite buys nothing |
| **Decisive** | over the threshold before, under it after | **cut** | `coref` is what tips it — the only case that justifies a rewrite |
| **Unreachable** | still over the threshold even after `coref` | **do not cut** | The agent compacts regardless, so we would pay the rewrite *and* eat the compaction |

The counter-intuitive case is the first: `coref` should be **less** aggressive when the rest of the
pipeline is doing well. `min_batch_frac` cannot express any of this — a 6% cut is "too small"
whether it is the decisive 6% or an irrelevant one.

**It all reduces to one scalar: how many tokens until the agent compacts.** Which is the hard part,
because the threshold is compared against the **provider's own reported usage**, not against
anything we compute. Claude Code sums all four tiers of the most recent response's `usage`
(`input_tokens + cache_creation + cache_read + output_tokens`) plus a local estimate for the
trailing user turn ([agent compaction](../how-to/agent-compaction.md)). That figure includes the
`system` array, the tool definitions and last turn's *output* — none of which a component can see,
which is why `Ctx.ExistingBreakpoints` exists at all. `schema.MessagesTokens` is therefore a
systematic undercount by an unknown amount.

**Three routes, in increasing cost — and the order matters, because the first may make the others
unnecessary:**

1. **Measure whether the prize is even in play.** `modes.Tracker` already detects the agent's
   compaction resets, so on real traffic we can ask how often the agent compacts and whether a
   `coref` pass moves that at all. This needs *nothing new*, and it is ground truth rather than an
   estimate. If the answer is "rarely, or not measurably", the whole gate is solving a problem we
   do not have and `min_batch_frac` is adequate.
2. **Let the host supply the distance.** The proxy holds the raw body, including `system` and
   `tools`, so it can count the full request and pass one number down against a configured
   threshold. Self-contained, and it avoids the response path entirely; last turn's `output_tokens`
   is a correction (capped ~20k against 167k), not the substance.
3. **Calibrate and learn.** Only if (2) proves too coarse: record per-session the offset between our
   count and the previous turn's reported usage, and the observed marginal growth per turn, in the
   Store — session-scoped like `sumCheckpoint`, with a cross-session prior like `markSeenContent` so
   turn one is not cold. The offset is not constant within a session (tool definitions change,
   output varies), so it wants a recent estimate rather than a lifetime average.

**Start conservative, because the asymmetry is sharp.** Over-estimating growth means demanding more
headroom and cutting less often — safe. Under-estimating means cutting, crossing again, and paying a
second rewrite at maximum `W` — the disaster case. So a cold start should bias growth *high* and
headroom *large*, and relax only as evidence accumulates.

**And none of this touches reward.** Every route above can tell us whether a compaction was
deferred. None can tell us whether the content we removed was needed — that is the silent failure in
§4, and it remains visible only in reward.

**Status: the prize is argued, not measured.** This document has claimed throughout that deferring
the agent's own compaction is plausibly the largest win, without ever measuring how often it is
reachable. Route (1) is the resolution, and nothing else here should be built before it.

## 7. What we do not know — the measurement pass

Nothing above should be built before the substrate is measured, and it can be measured for
**zero API dollars**: the proxy already captures pristine inbound bodies as JSONL
(`CONTEXT_GURU_CAPTURE`, `proxy/proxy.go`), the same corpus that refuted `xdedup` (1,325
requests / 51 sessions across `capture-tb`, `capture-swe`, `capture-swebench`).

A second input path needs no proxy run at all:
[`deploy/harbor/cc_capture.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/cc_capture.py) converts a Claude Code
session transcript — the agent's own append-only log of what it sent — into the same capture shape. That
is what produced [the measured results](../results/coref-density.md) when the eval box was out of reach, and
it is the cheapest way to re-measure on any workstation.

[`deploy/harbor/coref.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/coref.py) computes, per capture file and
per session, over Tier-1 novel-token references only:

- **Referenced mass** — what fraction of tool-output tokens are ever referenced later at all.
- **Reference recency** — how many messages **ago** the last reference was, measured from the
  head of the transcript. This is the A/B axis, and getting it right matters: the tempting
  quantity is the output's own depth, or the gap from the output to its reference, and neither
  is what "recent messages vs early messages" means. Reported alongside it, separately, is
  **consume lag** (output → last use), which says how long an output stayed live.
- **Reference-count distribution** — 1× vs 2× vs 3+, the other half of the open/closed axis.
- **Reuse fraction** — of the novel tokens an output introduced, how many the model actually
  carried forward. If this is low, "took a value, doesn't need the rest" is confirmed as the
  dominant pattern rather than assumed.
- **Unreferenced mass** — outputs no later model turn touches. The safe, free, deterministic
  cut, and the honest ceiling for a zero-LLM implementation.
- **Break-even table** — for each candidate cut set, the rewritten suffix `W`, the `T` it would
  need, and the `T` the session actually had, so §4's inequality is answered with this traffic
  rather than with an illustrative example.

Note the pleasant simplification the open/closed predicate gets for free: because `coref` only
ever cuts **tool outputs**, and references live in **assistant** turns which are never cut, any
reference at all *is* a surviving copy. "Referenced by a model turn" ≡ "the value that was
taken still exists in the request". The witness needs no separate search.

### Validated against a known-answer fixture

[`deploy/harbor/coref_fixture.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/coref_fixture.py) builds four tool
outputs whose correct classification is fixed by construction — one closed, two unreferenced,
one open — including the echo confound from §2 (a `Read(src/config.py)` whose only later
overlap is the path that arrived as the `tool_use` argument). All four classify correctly.

The **negative control** is the part worth keeping. *Echo-exclusion guard* is the mechanism from
§2 above, named: only tokens the output **introduced** are eligible, so any token already present
at or before the producing tool call is discarded as an echo of context that predates the output.
Disabling it means counting every token match, echoes included.

With the guard disabled, the
`src/config.py` output flips from `unreferenced` to `open`, and measured cuttable mass drops
**49% → 23%**. The guard is not a refinement — it is the difference between a usable
measurement and one that reports everything as load-bearing.

```sh
python3 deploy/harbor/coref_fixture.py /tmp/cap.jsonl
python3 deploy/harbor/coref.py /tmp/cap.jsonl sweep=1
# then, on real traffic:
CONTEXT_GURU_CAPTURE=/tmp/capture-swe.jsonl ./bin/context-guru-proxy --preset off
python3 deploy/harbor/coref.py /tmp/capture-swe.jsonl window=200000 fire_frac=0.6 sweep=1
```

`sweep=1` prints closed-mass share across a `closed_dist` × `open_reps` grid, so the thresholds
come from the data instead of from this document.

It reports Tier-2/3 mass as **unknown**, never as unreferenced. An exact matcher cannot
distinguish "never used" from "used after transformation", and conflating them is how a
compactor cuts something load-bearing.

**Decision rules this pass feeds:**

- If unreferenced mass is large and concentrated, the first version is deterministic and needs
  no model call.
- If referenced mass is dominated by low reference counts with distant last-references, the
  closed-case predicate is worth building.
- If the break-even table says no realistic `T` clears `11.5 × W` even when batched, the
  component is a step-reduction and agent-compaction-deferral play only, and must be evaluated
  that way — or not built.

**Every one of those rules is about cost, and cost alone can never authorize shipping this.** A
compactor that saves tokens and loses a single task is a regression, because the value of the
context it removed was never denominated in tokens. So **reward is a gate, not a metric**: no arm
of any experiment here is interpretable without it, and a cost win alongside an unmeasured reward
is not a result. §8's acceptance criteria are deliberately ordered with reward parity first, and
the measurement in this section is explicitly *not* an experiment — it reads what a cut would
remove from traffic that already happened, and can say nothing about reward by construction. That
is a limitation of the measurement, not a reason to defer the question.

## 8. Consequences for benchmark selection

A benchmark only tests this if its traffic contains co-reference **at the tier the detector
targets**. That criterion cuts against the intuitive ordering:

| Vehicle | Role |
|---|---|
| **SWE-bench Verified** (already wired, `deploy/harbor/swebench.py`) | **Tier-1-rich.** `Read → Edit → Bash` flows reference earlier outputs by exact path, symbol, line, error string. The right substrate for the deterministic detector, and the incumbent cost/reward regression floor (cache-read-dominated: 64% of the bill) |
| **Terminal-Bench 2.0** (already wired) | A **different cost regime** — output tokens are 47% of the bill, larger than cache-read. Must be tuned separately, and it is where a step-reduction claim is won or lost |
| **LOCA-bench** (MIT, native `anthropic` SDK + `LOCA_ANTHROPIC_BASE_URL` → direct attach) | The **controlled instrument**: context length is a dial (8K→256K) with fixed task semantics, deterministic binary scoring, and built-in `memory_tool` / `ptc` / context-editing arms — the naive-compaction baselines to beat. But its BigQuery/Sheets/Snowflake domains *aggregate and compute over* tool results, so references arrive transformed: it is a **Tier-2/3 stress test**, not a showcase for exact matching. Note its native trimmer orphans `tool_use`/`tool_result` pairs at 64K and provokes provider 400s. **Port the existing fix rather than rediscovering it:** `repair_tool_pairing()` in forever's `forever/benchmarks/_anthropic_auth_hop.py` already solves this rig-side in two phases — drop `tool_result` blocks whose `tool_use` was trimmed away (and any message left empty), then synthesize placeholder results for `tool_use` blocks left unanswered — and it counts the repairs so the rate is visible instead of silent. Worth noting separately that `coref` cannot *cause* this bug: it rewrites a tool message's text in place and never removes a message, so pairing is structurally preserved (unlike `summarize`, which restructures the list) |
| **UltraHorizon** | The most extreme regime (200k+ tokens, 400+ tool calls, hard in-context wipe), but LLM-judged, capability-gated, expensive, no license. Not a driver |
| **SlopCodeBench** | Resets per checkpoint, so sessions never approach the threshold. Structurally cannot test this |

**Acceptance criteria** (in priority order, from §4): reward parity or better; **steps** down;
**cache-write** within a stated budget rather than "unchanged"; `expand` rate below a
pre-registered ceiling; billed cost down. And two methodology guards the corpus has already
paid for: do not stop at first significance (`p = 0.036` at `n ≈ 22` regressed to `p = 0.22` at
`n = 30`), and prevent-and-measure rather than filter-after-the-fact (dropping anomalous runs
introduced survivorship bias when the failure rate was arm-imbalanced).

!!! info "Implementation status lives in its own document"
    What is built, what is deliberately inert, and the ordered next steps are in
    **[`coref` implementation status](coref-implementation.md)**. It is kept separate because it
    goes stale on every commit while the argument above does not — and because a proposal that
    doubles as a changelog stops being reviewable as a proposal.

## 8b. What the held-out experiment settled

§7's measurement pass reads what a cut *would* remove. A separate, later experiment asked the
harder question — **do the decisions come out right** — by holding out the future of 885 real tool
outputs and scoring ten arms against it. Full method, arms and limitations:
[the selection experiment](../results/coref-selection-experiment.md). Four of its results change
this proposal.

**1. The deterministic index is the strongest discriminator measured, not a fallback.** It keeps
95% of what the agent went on to need while removing 11.8% of mass. Every arm that put a model in
the verdict path — including a merged prompt seeing both the content and the reference evidence —
scored *worse on both axes*, and no combination of index and model beat the index alone. This
**refutes** the intermediate design the discussion around this proposal had converged on
(demote the index to an evidence supplier, move the verdict into `extract_llm`'s prompt).

**2. `cut_unreferenced` is not the free safe cut §3 calls it.** Measured false-drop is **11%**,
it is not a boundary artifact (57% of the errors land 51+ turns past the firing point), and it is
irreducible with the features the index has. §3's language has been corrected in the
[cheat sheet](../reference/coref-glossary.md); it should be read as a *cheap* cut with a bounded
error rate, never a free one.

**3. Prompt framing is worth ~26 points of accuracy — and reassurance is the failure mode.** A
prompt telling the model its cuts "stay recoverable on request" produced 91% removal at 6%
live-kept. Replacing that with the real cost — *the agent usually does not notice the gap and
answers from worse information instead of asking for it back* — moved live-kept to 58%. This is
the same finding as the marker rule in [`corefstub.go`](../components/coref.md), arrived at from
the other end: **any surface that tells a decider a cut is cheap makes the decider careless**,
whether that decider is the compacting model or the agent reading the marker.

**4. Selection cannot replace a summarizer, and this is arithmetic rather than a measurement.**
Let `g` be the mass arriving per turn and `f` the fraction that ever becomes removable. Sustained
removal is `f × g`, and `f < 1` always — dead content is a subset of arriving content — so removal
is strictly less than growth. Selection cannot hold the line; it multiplies time-to-threshold by
`1/(1−f)`:

| removable share of request | session extension |
|---|---|
| 4.4% (`unreferenced`, measured) | 1.05× |
| 9.6% (`+closed`, measured) | 1.11× |
| 18% (most aggressive model arm) | 1.22× |
| 24% (most aggressive arm at 34% false-drop) | 1.32× |

A summarizer reaches ~96% reduction because it compresses **live** content; `coref` can only
remove **dead** content. So the ambition of replacing the agent's own compaction — running a
lighter pass at 60% and a heavier one at the threshold — is **not available to a selective
component at any aggression setting.** `coref` is a deferral play, permanently. The honest
framing is that it buys 5–30% more turns before the summarizer runs, and its case rests on
whether those turns are worth a cache-write.

### Two design notes from review, neither implemented

**1. The MERGED call — what "fold" actually means, and what was built instead.**

Review's proposal is that the co-reference *reasoning* belongs **inside `extract_llm`'s prompt**,
not beside it. The argument is economic and it is strong: Tier-2 (transformed) and Tier-3 (semantic)
references are invisible to exact matching by construction, Tier-1 carries a measured **11%
false-drop**, and *a model call is already being made* to decide what to trim. The marginal cost of
adding "…and consider whether later turns referred back to this, including in paraphrase" to a
prompt that is already being sent is approximately **zero**. One call, both jobs.

What is implemented (`allow_cached_prefix`) is **not that**. It uses the index as an *eligibility
gate* — the index decides which prefix outputs are candidates, then the model decides how much of
each to keep. Two sequential mechanisms; the deadness judgement stays deterministic and stays
Tier-1-blind.

The distinction matters because it is easy to think the merged design has already been refuted. It
has not. [The selection experiment](../results/coref-selection-experiment.md) refuted
**model-as-decider** — eight arms where the model judged deadness from content plus evidence, best
of them 58% live-kept against the index's 95%, and no intersection or union beating the index alone.
That is a different question. It never tested *adding a criterion to a call already being paid for*,
and the economic objection that ran through it — that calls cost money the saving cannot repay —
does not apply when the call happens regardless. The nearest data point is the intersection arm:
**96% live-kept at 8% removed**, marginally safer than the index alone at lower yield. Suggestive,
not damning.

So the merged design is **untested**, and testing it is a prompt change plus passing the reference
evidence alongside the content — not new machinery.

**2. A cache rewrite is sometimes already free, and §4 never accounts for it.**

`S × T > 11.5 × W` prices a cache-write that the mutation *causes*. But when the provider's cache has
already expired, the prefix is re-written on the next turn **regardless** — so at that moment a
prefix mutation costs nothing incremental, and no break-even test is needed at all. A component that
can never justify a rewrite could act for free whenever the gap between turns exceeds the cache TTL:
a slow tool, a queue, a human thinking between turns.

Measured on LOCA (`deploy/harbor/cache_opportunity.py`, direct from per-step
`cache_read_input_tokens`): **zero such moments**. That is a property of the benchmark rather than an
answer — LOCA drives local mock MCP servers, so turns land seconds apart and a 5-minute TTL never
lapses. The workloads where it would appear are the slow-tool ones: SWE-bench container builds and
`pytest` runs, Terminal-Bench, anything with a human in the loop. Note the irony that SWE-bench was
[ruled out](../results/component-gating.md) for having tool outputs too small for these components,
yet its slow tools may make it the right vehicle for *this* question.

**Deferred deliberately, to be examined on a different benchmark.** Acting on it needs TTL state at
*decision* time, and the provider reports cache usage in the **response** — after the decision — so
it would have to be inferred from the previous turn's usage plus elapsed wall-clock. That is a design
question with a real failure mode (guess wrong and you pay 11.5× believing it was free), not a patch.

### The hypothesis this proposal should be tested against

Everything above narrows the claim to one testable sentence, which is what §8's acceptance
criteria should be pointed at:

> **On a caching backend, a batched, latched, one-way `coref` pass firing once per session at
> 55–70% of the context window removes 10–25% of the request at ≤11% false-drop, defers the
> agent's own compaction by 20+ turns, and does so at reward parity — with the cache-write it
> spends repaid by the deferred summarization rather than by the tokens it removed.**

It is falsifiable in four independent places, and three of the four are cheap:

| Clause | How it fails | Cost to test |
|---|---|---|
| "removes 10–25%" | Measured 4.4–9.6% on interactive traffic at the shipped cut set | **done** — it currently fails |
| "defers by 20+ turns" | 0/19 sessions could reach 40 turns of headroom **when fired late**; fired at the crossing the deficit term vanishes and 20–60 turns is affordable, but only in the 17–29% of sessions that compact at all | **partly answered** — see [reachability](../results/coref-reachability.md) |
| "cache-write repaid by deferred summarization" | Needs the deferral prize to be reachable at all | cheap — `modes.Tracker` reset detection, no new machinery |
| "at reward parity" | Any task lost to a false drop | expensive — the eval box, and the only real gate |

One clause **fails on measured traffic** and one is **partly rescued** by firing earlier, which is
why the component ships opt-in and in no preset. Recording that plainly is more useful than restating the ambition: the
remaining case for `coref` is that the corpus it failed on is the wrong one (interactive research
traffic, mostly one author, `opaque`-heavy), and the corpus the acceptance criteria are written
against has never been measured.

## 9. Open questions

- ~~**How often is the deferral prize actually reachable?**~~ **Measured** — see
  [reachability](../results/coref-reachability.md). The prize exists in **17% of sessions (6/35)**
  and **29% of sessions past 200 model turns**, so every expected-value argument here must be
  multiplied by ~0.17–0.29 — a factor no version of this document carried. The same pass produced
  the first clause of the hypothesis that does *not* fail: fired **at** the threshold crossing the
  deficit term vanishes (it was an artifact of measuring after a late fire), leaving only
  `growth × headroom`, which the available cut can supply for 20–60 turns. That vindicates §6's
  counter-intuitive claim about the profitable moment from a second direction. It is sensitive to a
  growth estimator two measurements disagree about by 2×, and it still inherits the 11% false-drop.
- **Is `xdedup` back on the table?** §C left one caveat explicitly open: compaction is the one
  regime that could make cross-turn dedup viable, because it removes the first copy while later
  re-reads land in the mutable tail. `coref` *creates* that regime. C1 should be re-measured
  after, not assumed still refuted.
- **Where does the reference index live?** Recomputing it per turn on a 150k transcript is
  latency the sync path may not absorb (budget: ~117 ms added today, ~450 ms with the LLM
  trimmer). An incremental per-session index in `store`/`session` is the likely answer, but it
  is state the components layer does not currently keep.
- **Does `observe` mode suffice for the first read?** It measures what a pipeline would have
  done with zero request modification, which fits — but the cache-write cost of a prefix
  mutation is exactly the thing observe cannot observe, since nothing is forwarded.
- **Tier-2 escalation shape.** If the deterministic ceiling is low, the escalation is an
  `extract_llm`-style cheap-model pass restricted to far-field large spans. That pass is
  *sampled* output, so per constraint 1 its decision must be latched on first computation and
  never recomputed.
