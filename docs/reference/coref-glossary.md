# `coref` cheat sheet

Every term the co-reference work uses, in one page, in the order you meet them. Full argument in
[the proposal](../proposals/coref-compaction.md); measured numbers in
[the density pass](../results/coref-density.md) and [the selection
experiment](../results/coref-selection-experiment.md); config in [the component reference](../components/coref.md).

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

## 3. The four verdicts (what the classifier outputs)

For each tool output, exactly one of:

| Verdict | Means | Cut it? |
|---|---|---|
| **`opaque`** | The output introduced **nothing the index can track** — so there is no evidence either way. | **Never.** Absence of evidence is not evidence of deadness (see the box below). |
| **`unreferenced`** | It **did** introduce trackable identifiers, and no later turn used any of them. | **Yes — the shipped default** (`cut_unreferenced`). No threshold, no model call. **Not free:** measured **11% false-drop** against held-out ground truth — see the box below. |
| **`closed`** | Referenced **once or twice, and not for a long time**. Whatever the model took survives in the turn that took it, so the original is redundant *with content still in the request*. This is **case A** made checkable. | Optional (`cut_closed`, **off by default**). |
| **`open`** | Referenced **recently, or repeatedly**. Still load-bearing. This is **case B**. | **No.** |

!!! warning "`unreferenced` never means 'unused' — and it is wrong 11% of the time"
    It means "no later **exact** use". A value the model summed, converted or reworded leaves no substring
    to match, so it lands here too. Always an **upper bound** on what is safe to cut.

    Now measured, not argued. Held out the future of 885 real tool outputs and asked how often
    "unreferenced at the firing point" was contradicted later: **11%**. It is **not a boundary
    artifact** — 0% of those errors are one turn past the firing point and **57% are 51+ turns
    past it** — and it is **irreducible with the features the index has**: demanding more
    introduced identifiers makes it *worse* (11% → 21%), demanding longer dormancy barely moves
    it while costing 2.5× the mass. An output that lies dormant for a hundred turns and is then
    used carries no signal, at the moment of decision, that distinguishes it from one dormant
    forever.

    Since ground truth is Tier-1 matching only, **11% is a lower bound.** Full method and the
    ten arms it was measured against: [the selection
    experiment](../results/coref-selection-experiment.md).

!!! danger "`opaque` vs `unreferenced` — the distinction that took a review to find"
    Both have zero references, and they are opposites. "Introduced 200 identifiers, nobody touched one" is
    *evidence of deadness*. "Introduced nothing I can see" is *absence of evidence*.

    It matters because it is common, not exotic. A tool returning
    `[{"name":"david","id":123,"address":"foobarbaz"}]` yields **no** trackable tokens — short lowercase
    words and 3-digit numbers are exactly what the precision rules exclude. Measured: 8% of tool-output
    mass on interactive traffic, 20% on UltraHorizon, **40% on LOCA-bench**. Folded into `unreferenced`,
    all of it was a silent vote to delete under the default config.

**Why "closed" looks cheap to establish:** `coref` only ever cuts *tool outputs*, and references live in
*model turns*, which it never cuts. So a reference is always a surviving copy of *something* — no separate
search for the **witness** is needed.

!!! danger "…but a surviving copy of *something* is not a copy of what's *needed*"
    Given the records above and a model that says *"I need to remember david 123 address"*, the reference
    (`david`, `123`) is real — and the value actually needed (`foobarbaz`) was **never copied into a model
    turn**. The model referenced an **anchor** in order to point at a payload it did not restate.

    An exact matcher cannot tell an anchor reference from a payload reference. That is the real reason
    `cut_closed` ships **off**, and it is why a low `used_frac` is *ambiguous* rather than evidence for
    case A: "took the value, rest is chaff" and "took an anchor, still needs the payload" look identical.

## 4. The two thresholds that decide `closed` vs `open`

| Knob | Default | Means | Verdict from the data |
|---|---|---|---|
| **`closed_dist`** | 12 | How many messages **ago** the last reference must be before the output counts as `closed`. Newer than this ⇒ `open`. | **It is load-bearing but flat.** Set it to 0 and the `closed` class stops existing, so it *matters*; but anywhere in 4–40 gives the same answer within 2–3 points, so there is no gain from tuning it. Leave it at the default and spend the effort on `open_reps`. |
| **`open_reps`** | 3 | Referenced at least this many times ⇒ `open` **regardless of age**, because a span referenced repeatedly is a hot span that happens to be old. | **This is the dial.** 2→6 moves the answer 18 points. 3 is the conservative setting. |
| **`min_later_turns`** | 8 | The **opportunity floor**: an output with fewer model turns after it is `open` regardless of everything else. | **Justified structurally, not by safety.** Near the tail, "no references yet" and "recent" are the same thing, so without it a batched pass preferentially cuts the **most recent** context — the worst possible choice, and `mask`'s `keep_recent` idea expressed in turns. But it does **not** buy accuracy: swept against held-out ground truth, `min_later=0` yields *more* mass (363k vs 207k) at a *lower* false-drop rate (8% vs 11%). Keep it for the structural reason; do not claim it as a safety margin. |

## 5. The three measurements per output (and the one that's easy to get wrong)

| Term | Means |
|---|---|
| **ref count** | How many later turns used something this output introduced. Feeds `open_reps`. |
| **ref age / recency** | How many messages ago the **last** reference was, counted **from the head of the transcript** (i.e. from *now*). Feeds `closed_dist`. |
| **consume lag** | How many messages **after the output** its last reference was — i.e. how long it stayed live. A *different* axis, reported separately. |
| **used fraction** | Of the identifiers the output introduced, the share the model actually carried forward. Measured median ~19% — but see the anchor box in §3: a *low* value is **ambiguous**, not evidence for case A. |
| **later turns** | How many model turns follow the output — its **opportunity** to be referenced. Feeds `min_later_turns`. |

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
conversion). Measured at ~2% of model turns on interactive traffic — but only via a numeric proxy, and
**Tier 3 is not measured at all**, so treat that 2% as a floor on invisible references rather than a
total. Real, small on this traffic, and the reason
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
| **`min_batch_frac`** | The operational form of that: the pass must cut at least this fraction of the request (default 0.05) or it declines and leaves the request byte-identical. A correct implementation of the *token* argument, and a **poor proxy for the deferral argument** — it cannot express "is my cut the decisive one?". See the box below. |
| **`rewrite_budget`** | Prefix-rewrite passes allowed per session (default 3). `coref` is the only component that spends cache-writes **on purpose**, so the spend is capped and reported. |
| **step reduction** | The real prize. `corr(Δsteps, Δcost) = +0.95`; unique token removal is ~0.02% of the bill. The objective is **steps and reward, not bytes**. |
| **deferring agent compaction** | Claude Code compacts itself at ~167k on a 200k model. Staying under that avoids a full-transcript summarization — a large cache event *and* a quality loss. Plausibly the biggest win. |

**The counter-intuitive consequence:** firing at 90% of the context window means `T` ≈ 0 — paying a
rewrite for a saving collected once. **The profitable moment to compact is earlier than the moment of
maximum pressure.**

### What all this means at 200k vs 1M

Raised in review, and the answer is less obvious than "bigger window, more to cut".

**The break-even inequality is scale-invariant.** Rearranged, `S × T > 11.5 × W` says `T > 11.5 × (W/S)`
— the turns you need depend only on the **ratio** of rewritten suffix to cut mass, never on absolute size.
On the measured interactive corpus that ratio is ~15 (a 10.5k cut against a 157k suffix), hence `T > 138`.
A 1M-token transcript with the same *density* of cuttable mass has the same ratio and the same required
`T`. So a larger window neither rescues nor damns the token economics; it only moves **when** the trigger
fires. What actually improves the ratio is cutting a larger share of what lies *after* the shallowest cut —
which is an argument for cutting deep and rarely, not for cutting more.

**Three things genuinely do change:**

| At a 1M window | Effect |
|---|---|
| Cache-read is the whole bill | Re-reading ~1M cached tokens every turn dominates cost long before the window is a constraint. That makes `coref` a **cost** play at 1M rather than a *fit* play, and it is the strongest argument for it there. |
| The agent's own compaction recedes | Claude Code compacts at ~967k instead of ~167k, so the deferral prize becomes **rarer but much larger** — avoiding one summarization of a 1M transcript. As you note, it is also the one prize that is cheap to *measure* deterministically: compare the API-reported usage against the documented threshold and count the turns of headroom the cut bought. No benchmark scoring, no seeds. That belongs in the metrics, and it is not there yet. |
| The index gets 5× more expensive | Recomputing the reference index over 1M tokens per firing turn is the open latency question, and it scales linearly with the window. An incremental per-session index stops being an optimization and becomes a requirement. |

**And the prize is a step function, not a slope.** You either drop below the agent's compaction
threshold or you don't; cutting 90% of what was needed is worth nothing. Worse, clearing it *exactly*
buys one turn — the next turn grows past it again, and then you pay a **second** cache-write at the
point where `W` is largest. So the real requirement is
`(usage − threshold) + growthPerTurn × headroomTurns`, which on measured traffic works out at 20–25%
of the request for 40–60 turns of headroom — and Tier-1 matching finds only 4–10%.

!!! danger "The gate is measuring the wrong thing, and the right thing is unmeasured"
    `min_batch_frac` asks "is my cut large relative to the request?". The question that matters is
    "**does my cut change the outcome?**" — because `coref` is the only component paying a prefix
    rewrite, while `mask` and friends do 12–27% from the cache-safe tail for free. So `coref` should
    cut **only when it is decisive**: not when the pipeline is already under the threshold (the prize
    is won, a rewrite buys nothing), and not when even `coref` cannot get it under (the agent
    compacts anyway, so we pay the write *and* eat the compaction).

    That reduces to one scalar — **tokens until the agent compacts** — which is hard because the
    threshold is compared against the provider's *reported usage*, including `system`, tool
    definitions and last turn's output, none of which a component can see.

    Fully worked through, with the measured numbers and a three-step order of attack, in the
    proposal's [deferral gate](../proposals/coref-compaction.md#the-deferral-gate-designed-unquantified).
    Unbuilt on purpose: **how often the prize is even reachable has never been measured**, and that
    measurement needs nothing new.

## 8. Mechanism terms (how it stays cache-safe)

| Term | Means |
|---|---|
| **latching** | The decision is stored per session and **replayed byte-for-byte** thereafter, never re-derived. A co-reference decision depends on *history*, so re-deriving it against a longer transcript could emit different bytes — which is exactly the prefix flip that costs a second cache-write. |
| **one-way / monotonic** | Keep → cut only. New evidence can never un-cut, because un-cutting is another rewrite. Monotonicity is a cost requirement, not tidiness. |
| **`freeze` / `reapplyFrozen`** | The mechanism that does it: record the replacement text against the original's content hash, and replay it on every later turn at any depth. |
| **`TailOnly`** | A helper on `Ctx` that answers "may I safely modify the message at index *i*?" It returns false for anything the provider has already cached (index ≤ `MaxCachedIdx`), because editing cached content breaks the prefix hash and forces a cache-write. Every *other* age-based offloader (`mask`, `failed_run`, `collapse`) consults it and simply declines. `coref` deliberately ignores it — reaching into the cached prefix **is** its purpose, since by the time a session crosses the threshold all the mass is back there — which is exactly why its spend has to be budgeted (`rewrite_budget`) instead of forbidden. |
| **`repairLostFreeze`** | Background: an offloader `freeze`s its replacement text against the original's content hash and replays it every turn, so the bytes stay stable. If the store *drops* that record (TTL, eviction), the offloader would normally decline to act at depth — but then the message reverts to full text, which is *itself* a prefix change. So `mask` and `failed_run` are allowed to re-derive the decision even deep in the prefix: their replacement is a pure function of `(content, config)`, so re-deriving reproduces byte-for-byte what the provider already cached. **`coref` must never use this.** Its decision depends on the whole transcript, so re-deriving against a longer one can yield a different class and different bytes — the precise byte-flip the repair exists to prevent. A lost `coref` freeze therefore declines and the output stays verbatim. |
| **marker / `<<cg:HASH>>`** | What's left in place of cut content, resolvable back to the stashed original via `context_guru_expand`. |
| **head peek** | A one-line snippet of the cut output left inside the marker, so the model knows *what* went missing without a blind `expand` round-trip. |
| **kept-verbatim** | Once the agent expands something, it's marked never-re-cut — otherwise it expands again every turn (an **expand loop**). |
| **`expand` rate** | The precision inner loop: how often the model asks for cut content back. Cheap, needs no scoring, no seeds, no n=30. **But it counts *noticed* errors only** — see the box below — so a falling expand rate is ambiguous rather than good news. |
| **fail open** | Any error reverts this component only; the original request is always forwardable. |

!!! danger "Reversible does not mean recovered"
    Expansion is **model-initiated**. The stash guarantees the bytes *can* come back; only the model
    decides to ask, and nothing detects a bad cut. Three outcomes, not one:

    1. it notices and expands the right marker → one round-trip + a cache-write;
    2. it notices but cannot tell which marker holds it → several expands, or it proceeds without;
    3. **it never notices** → it answers from less than it had, silently.

    Tier 3 is where (3) lives: a missing semantic reference leaves nothing to look up, so nothing
    prompts recovery. (3) is invisible to every counter this component keeps, which is exactly why
    reward is a **gate** and not one metric among several.

    What the design can influence is the 1-vs-2 gap, which is what the marker's structural descriptor
    (`200 records, fields: address, id, name`) is for — and why the marker never asserts that the cut
    was safe.

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

Mechanism implemented and tested; `cut_unreferenced` on by default, justified on yield (21% of mass on
interactive traffic, ~70% on benchmark traffic) and now bounded on accuracy (**11% false-drop**);
`cut_closed` **off** until measured on the eval-box corpus; component **opt-in, in no preset**; no
reward measurement exists, which remains the gate.

## 11. Experiment terms (the held-out measurement)

Introduced by [the selection experiment](../results/coref-selection-experiment.md), and worth having
here because they are how any future claim about this component should be scored.

| Term | Means |
|---|---|
| **firing point `F`** | The message index a compaction pass is imagined to fire at. Everything before it is a candidate; everything after it is ground truth the decider never sees. |
| **evidence window** | `(output, F]` — the only thing any arm may look at. |
| **held-out future** | `(F, end]` — what the agent actually went on to do. Scoring only. |
| **false-drop** | Of what an arm removed, the share that *was* referenced later. A definite error. Bounded above by the base rate, which is why it is misleading alone. |
| **live-kept** | Of what *was* referenced later, the share an arm correctly kept. **The discrimination metric**, and base-rate independent. |
| **null baseline** | Drop everything. Scores exactly the base rate at 0% live-kept. A high removal rate paired with a base-rate false-drop rate means an arm is doing nothing but deleting. |
| **base rate** | The share of candidates referenced after `F` at all. 4% on LOCA, 11% on UltraHorizon, 46% on interactive traffic — so **the same arm scores wildly different false-drop rates on different corpora**, and comparing arms across corpora without this control produces artifacts. It produced two in this work. |

!!! danger "Where this metric stops being valid"
    It scores **verbatim survival**, so it can compare arms that keep-or-drop text and **cannot**
    score one that *paraphrases*. A summary reading "found the grace-period bug in the auth module"
    has preserved the information while containing none of the identifiers. Any comparison between
    selective compaction and summarization needs **downstream task outcome**, not this.
