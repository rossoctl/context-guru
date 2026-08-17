# Co-reference-aware compaction (`coref`)

**Status:** mechanism implemented and tested; the §7 measurement pass has **run on three corpora**
(Claude Code, UltraHorizon, LOCA-bench) — [results](../results/coref-density.md) — but not yet on the
eval-box captures the acceptance criteria are written against. The `coref` component ships **opt-in, in no preset**,
with the calibrated (`closed`) cut **off by default**. See [implementation status](coref-implementation.md).
**Headline numbers:** unreferenced tool-output mass is **21% on interactive traffic and ~70% on
benchmark traffic** (a 3.3x workload difference, not a constant), a reference consumes a median
18.7% of what its output introduced, and — the result that most affects the design — **recency is
nearly inert while reference count does all the discrimination**.
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

One more asymmetry worth naming: a wrong cut is not a wrong answer, it is a
`context_guru_expand` round-trip — one extra step plus a cache-write. Given fact (3), that
means **expand-call rate is the dominant cost term and the primary precision metric** for this
component. It is also observable on any traffic with no benchmark scoring, no seeds, and no
n=30 — which makes it the inner loop.

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

## 9. Open questions

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
