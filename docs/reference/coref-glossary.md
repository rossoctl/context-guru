# `coref` cheat sheet

Every term the co-reference work uses, in one page, in the order you meet them. Full argument in
[the proposal](../proposals/coref-compaction.md); measured numbers in
[the results](../results/coref-density.md); config in [the component reference](../components/coref.md).

## The one-sentence version

`coref` cuts tool outputs that **nothing later in the conversation ever used** — and because cutting deep
in a transcript forces the provider to re-write its cache, it does all its cutting **at once, rarely, and
never changes its mind**.

---

## 1. The core idea

| Term | Means |
|---|---|
| **co-reference** | A later turn pointing back at an earlier tool output. The whole component is built on detecting these. |
| **reference** | Concretely: an identifier that a tool output *introduced* shows up again in a later **model** turn (its prose, or a tool-call argument). |
| **novel token** | An identifier the output **introduced** — one that appears nowhere at or before the tool call that produced it. Only these can constitute a reference. |
| **echo** | The opposite, and the trap. `Read(src/auth.py)` puts the path in the tool-call *argument*; the result echoes it; a later `Edit(src/auth.py)` uses it again. Exact matching sees a reference, but nothing was ever taken *from the output*. |
| **echo guard** (prior-vocabulary exclusion) | Excluding tokens already in context before the output existed. Not optional: without it the measurement reports nearly everything as load-bearing, and the component then declines to cut anything. |

## 2. The two cases from the original idea

The idea started with: if a later turn references an earlier output, that means one of two things.

| Term | Means |
|---|---|
| **case A** | The model already **took what it needed** — lifted one value out of a big response and doesn't need the rest. Licenses a *large* cut. |
| **case B** | The model has effectively **marked it important** and may need it again. Keep. |

The original framing separated A from B by **distance** (B = recent, A = early). The measurement says
distance barely works — see `closed_dist` below.

## 3. The three verdicts (what the classifier outputs)

For each tool output, exactly one of:

| Verdict | Means | Cut it? |
|---|---|---|
| **`unreferenced`** | No later turn ever used anything this output introduced. | **Yes — the free cut.** No threshold needed, no model call. This is the shipped default (`cut_unreferenced`). |
| **`closed`** | Referenced **once or twice, and not for a long time**. Whatever the model took survives in the turn that took it, so the original is redundant *with content still in the request*. This is **case A** made checkable. | Optional (`cut_closed`, **off by default**). |
| **`open`** | Referenced **recently, or repeatedly**. Still load-bearing. This is **case B**. | **No.** |

!!! warning "`unreferenced` never means 'unused'"
    It means "no later **exact** use". A value the model summed, converted or reworded leaves no substring
    to match, so it lands here too. Always an **upper bound** on what is safe to cut.

**Why "closed" is cheap to establish:** `coref` only ever cuts *tool outputs*, and references live in
*model turns*, which it never cuts. So "a later turn referred back to this" and "the value it took still
exists in the request" are the same fact — the surviving copy (the **witness**) needs no separate search.

## 4. The two thresholds that decide `closed` vs `open`

| Knob | Default | Means | Verdict from the data |
|---|---|---|---|
| **`closed_dist`** | 12 | How many messages **ago** the last reference must be before the output counts as `closed`. Newer than this ⇒ `open`. | **Nearly inert.** A 10× sweep (4→40) moves the answer 2–3 points. Don't tune it. |
| **`open_reps`** | 3 | Referenced at least this many times ⇒ `open` **regardless of age**, because a span referenced repeatedly is a hot span that happens to be old. | **This is the dial.** 2→6 moves the answer 18 points. 3 is the conservative setting. |

## 5. The three measurements per output (and the one that's easy to get wrong)

| Term | Means |
|---|---|
| **ref count** | How many later turns used something this output introduced. Feeds `open_reps`. |
| **ref age / recency** | How many messages ago the **last** reference was, counted **from the head of the transcript** (i.e. from *now*). Feeds `closed_dist`. |
| **consume lag** | How many messages **after the output** its last reference was — i.e. how long it stayed live. A *different* axis, reported separately. |
| **used fraction** | Of the identifiers the output introduced, the share the model actually carried forward. Measured median ~19%: "took a value, dropped the rest" confirmed. |

!!! note "recency ≠ consume lag, and conflating them is the bug"
    "Recent messages vs early messages" is a statement about **now**, so recency must be measured from the
    head. The tempting alternatives — the output's own depth, or the gap from output to reference — are
    different quantities. An early output referenced immediately has a *small* consume lag and a *large*
    ref age; swapping them makes every ancient output look freshly used. (This mis-modelling was caught by
    the fixture, not by inspection.)

## 6. The three tiers of reference (what's detectable)

| Tier | Signal | Detectable? |
|---|---|---|
| **Tier 1** | Literal carry-over — a path, symbol, id, hash or error string reappearing verbatim | **Yes, exactly, no LLM.** This is all that ships. |
| **Tier 2** | **Transformed** carry-over — the model summed the rows, converted the units, reworded the finding. No substring match exists | No. This is the deterministic ceiling. |
| **Tier 3** | **Semantic** — "as I noted earlier", "per the schema" | LLM only. |

Tier 2 is the objection raised on the original thread (values drift through paraphrase and unit
conversion). Measured at ~2% of model turns on interactive traffic — real, small, and the reason
`unreferenced` is an upper bound.

## 7. The cache economics (why the component has this shape)

| Term | Means |
|---|---|
| **cache-read / cache-write** | Re-sending an unchanged prefix is a cheap cache-**read**. Mutating it breaks the prefix hash and forces a cache-**write** of everything after the mutation. |
| **11.5×** | One cache-write costs 11.5 cache-reads: `($2.50 − $0.20) / $0.20` per MTok. |
| **`S`** | Tokens the cut removes (the saving, collected on every later turn). |
| **`W`** | The **rewritten suffix** — tokens from the shallowest cut to the end of what the provider already cached. The cost. |
| **`T`** | Turns remaining in the session, i.e. how many times the saving gets collected. Nobody has this, so it's estimated from how fast the transcript has been growing. |
| **break-even** | `S × T > 11.5 × W`. |

**Why this reshapes everything:** a single early cut can never clear it — 5k cut from 20% depth of a 150k
transcript needs `T` > 276 turns. Three consequences, and they *are* the design:

| Term | Means |
|---|---|
| **batching** | One rewrite must serve **every** cut in the pass, so `S` is the sum of all of them. That's what makes break-even reachable (60k of a 150k transcript needs `T` > 23). Hence a rare, threshold-triggered pass — never a per-output, per-turn decision. |
| **`min_batch_frac`** | The operational form of that: the pass must cut at least this fraction of the request (default 0.15) or it declines and leaves the request byte-identical. |
| **`rewrite_budget`** | Prefix-rewrite passes allowed per session (default 3). `coref` is the only component that spends cache-writes **on purpose**, so the spend is capped and reported. |
| **step reduction** | The real prize. `corr(Δsteps, Δcost) = +0.95`; unique token removal is ~0.02% of the bill. The objective is **steps and reward, not bytes**. |
| **deferring agent compaction** | Claude Code compacts itself at ~167k on a 200k model. Staying under that avoids a full-transcript summarization — a large cache event *and* a quality loss. Plausibly the biggest win. |

**The counter-intuitive consequence:** firing at 90% of the context window means `T` ≈ 0 — paying a
rewrite for a saving collected once. **The profitable moment to compact is earlier than the moment of
maximum pressure.**

## 8. Mechanism terms (how it stays cache-safe)

| Term | Means |
|---|---|
| **latching** | The decision is stored per session and **replayed byte-for-byte** thereafter, never re-derived. A co-reference decision depends on *history*, so re-deriving it against a longer transcript could emit different bytes — which is exactly the prefix flip that costs a second cache-write. |
| **one-way / monotonic** | Keep → cut only. New evidence can never un-cut, because un-cutting is another rewrite. Monotonicity is a cost requirement, not tidiness. |
| **`freeze` / `reapplyFrozen`** | The mechanism that does it: record the replacement text against the original's content hash, and replay it on every later turn at any depth. |
| **`TailOnly`** | The rule every *other* age-based offloader follows: never touch the already-cached prefix. `coref` deliberately violates it — that's its purpose — which is why the spend is budgeted. |
| **`repairLostFreeze`** | A repair `mask`/`failed_run` may use: re-derive a lost decision at depth, safe because their output is a pure function of `(content, config)`. **`coref` must never use it** — its decision is history-dependent, so re-deriving is the very byte-flip the repair exists to prevent. |
| **marker / `<<cg:HASH>>`** | What's left in place of cut content, resolvable back to the stashed original via `context_guru_expand`. |
| **head peek** | A one-line snippet of the cut output left inside the marker, so the model knows *what* went missing without a blind `expand` round-trip. |
| **kept-verbatim** | Once the agent expands something, it's marked never-re-cut — otherwise it expands again every turn (an **expand loop**). |
| **`expand` rate** | The precision metric that matters. A wrong cut isn't a wrong answer, it's one `expand` round-trip plus a cache-write — observable on any traffic with no benchmark scoring, no seeds, no n=30. |
| **fail open** | Any error reverts this component only; the original request is always forwardable. |

## 9. Where things live

| Thing | Where |
|---|---|
| The reference index (pure, shared definition) | `internal/coref/coref.go` |
| The offload component | `components/offload/coref.go` |
| Offline measurement | `deploy/harbor/coref.py` |
| Known-answer fixture + negative control | `deploy/harbor/coref_fixture.py`, `internal/coref/coref_test.go` |
| Claude Code transcript → capture | `deploy/harbor/cc_capture.py` |
| Benchmark run log → capture | `deploy/harbor/runlog_capture.py` |

The Go index and the Python script must agree on what a reference is. If they drift, the thresholds the
measurement produces are calibrated for a different algorithm than the one that ships — silently. That's
why the same fixture and the same false-positive regression cases exist on both sides.

## 10. Status in one line

Mechanism implemented and tested; `cut_unreferenced` on by default and justified (21% of mass on
interactive traffic, ~70% on benchmark traffic); `cut_closed` **off** until measured on the eval-box
corpus; component **opt-in, in no preset**.
