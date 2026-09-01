# coref

!!! info "Offload — lossy, reversible"
    Co-reference-aware compaction: at a threshold crossing, cut the tool outputs that no later turn ever carried anything forward from — in one batched pass, under an explicit cache-write budget.

!!! warning "Opt-in, in no preset, not yet measured on the eval-box corpus"
    `coref` is the implementation of a proposal (`docs/proposals/coref-compaction.md`). Its mechanism is
    tested, and the measurement pass has run on three corpora — Claude Code sessions, UltraHorizon and
    LOCA-bench — but **not on the `capture-swe`/`capture-tb` captures** the acceptance criteria are written
    against ([results and caveats](../results/coref-density.md)). The `closed` cut stays **off by default**
    because its yield ranges from 15% of mass on interactive traffic to **0% on LOCA**, and a knob that
    varies that much by workload has no defensible default. `cut_unreferenced` (the default)
    needs no threshold and is justified **on yield** — 13% of tool-output mass on interactive traffic,
    51% on UltraHorizon, 22% on LOCA-bench.

    **On accuracy it is not free.** Against held-out ground truth over 885 real tool outputs,
    `cut_unreferenced` removes content the agent later used **11% of the time** — not a boundary
    artifact (57% of those references land 51+ turns past the firing point) and not reducible with
    the features the index has. Since ground truth is Tier-1 exact matching, 11% is a **lower
    bound**. Budget for it: [the selection
    experiment](../results/coref-selection-experiment.md).

    Two things that measurement already settles, before you tune anything: **`closed_dist` is nearly
    inert** (a 10× sweep moves the answer 2–3 points) and **`open_reps` is the dial** (2 → 6 moves it 18).
    Tuning the recency threshold is wasted effort.

## How it works

Every other offloader here decides from *content* (is this a duplicate, is it noise, is it superseded)
or from *age*. `coref` decides from **back-references**: for each tool output, which identifiers it
*introduced*, and whether any later model turn used them.

1. **Index (Tier 1, exact, no LLM).** For each tool output, collect the identifier-ish tokens
   it introduced — paths, symbols, ids, hashes, error codes. "Introduced" is the load-bearing word:
   a token already present at or before the producing turn is an **echo**, not something taken from
   the output. If the agent calls `Read(src/auth.py)`, the path is in the tool-call argument, echoed
   by the result, and used again by a later `Edit(src/auth.py)` — an exact matcher scores that as a
   reference, but nothing was ever lifted out. Tokens spread across many outputs are dropped as
   session furniture.
2. **Classify**, into one of four: `opaque` (introduced nothing trackable — **no evidence**, never
   cut) · `unreferenced` (introduced identifiers, no later turn used any) · `closed` (used once or
   twice, and not for a long time) · `open` (used recently, repeatedly, or **too new to have had the
   chance**).
3. **Cut, once, in a batch.** Only the classes in the cut set, and only when the batch is large
   enough to be worth the cache-write it costs (below).
4. **Latch.** The decision is stored per session and replayed byte-for-byte thereafter. It is never
   re-derived.

Recency is measured **from the head of the transcript**, not from the output's own position: "referenced
recently" is a statement about *now*. A span referenced three times forty turns ago is a hot span that
happens to be old, which is why repetition (`open_reps`) overrides age.

## Why it is batched, budgeted, and rare

`coref` is the only component that **mutates the already-cached prefix on purpose**. Every age-based
offloader refuses to (`Ctx.TailOnly`) because breaking the prefix hash at index *i* forces the provider
to cache-**write** the suffix, at 11.5× the price of a cache-read. Cutting *S* tokens with *W* tokens of
transcript after the cut and *T* turns left in the session:

```
cost    = 11.5 x W    (cache-read-equivalents)
benefit =  S x T
worth it when   S x T  >  11.5 x W
```

A single early cut cannot clear that: 5k cut from 20% depth of a 150k transcript needs *T* > 276 turns.
The same transcript cut by 60k in **one pass** needs *T* > 23, which a long session reaches. Hence:

- `min_batch_frac` — one rewrite has to serve the whole pass.
- `rewrite_budget` — the spend is capped per session and answerable, not incidental.
- `break_even` — the inequality itself, with *T* estimated from how fast the transcript has been
  growing. Its consequence is counter-intuitive and deliberate: **firing at 90% of the window means
  *T* ≈ 0**, so the profitable moment to compact is *earlier* than the moment of maximum pressure.

## Before → After

```
after:  [tool output compacted: no later turn referred back to it; starts: 0 tree_line_0 = compute_tree_0(arg_0) …] <<cg:…>> [full output: call context_guru_expand]
after:  [tool output compacted: the value taken from it survives in a later turn; starts: …] <<cg:…>> [full output: call context_guru_expand]
```

The second form is the `closed` cut (`cut_closed: true`). Its claim about a surviving witness is free
rather than asserted: `coref` only ever cuts **tool outputs**, and references live in **model turns**,
which it never cuts — so "a later turn referred back to this" and "the value taken from it is still in
the request" are the same fact.

## Lossiness

Lossy but reversible — cut outputs are stashed and recovered via `context_guru_expand` / `GET /expand`,
and an expanded output is marked kept-verbatim so it is never re-cut.

!!! warning "Reversibility is a capability, not a guarantee"
    Expansion is **model-initiated**: the tool is advertised and the host loop only answers a call.
    Nothing detects a bad cut. So a wrong cut costs one `expand` round-trip **only when the model
    notices** — and if it doesn't, it answers from less than it had, silently.

    That makes `expand` rate a precision metric for *noticed* errors only. It is still the right inner
    loop (cheap, no scoring, any traffic) but it is blind to the silent case by construction, so a
    falling expand rate is ambiguous. **Reward is the only instrument that sees it**, which is why it
    is a gate here rather than one number among several. Tier 3 is where the silent case lives: a
    missing semantic reference gives the model nothing to look up, so nothing prompts recovery.

### What the marker leaves behind

Two rules, both aimed at the gap between "notices and expands the right thing" and "notices but
cannot tell which marker":

- **It describes the shape of structured content**, not just its first line —
  `200 records, fields: address, id, name`. That is *addressable*: an agent hunting for an address
  can tell this is the output to expand. A head peek of one arbitrary row cannot do that, though it
  works well for a file read or a traceback, which is what the peek is still used for.
- **It never claims the cut was safe.** An earlier version wrote *"no later turn referred back to
  it"* — precisely the claim that is false whenever the reference was transformed or semantic — which
  read as reassurance and discouraged the expand call that would have repaired the mistake. A marker
  that talks the model out of recovering is worse than an opaque one. A test enforces this.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `trigger` | fires always | Gates **new** cuts only (`min_request_frac` against the resolved context window is the natural dial). Replay of latched decisions is never gated — a latched cut that stops being replayed flips the prefix, which is the churn the whole design avoids. |
| `min_tokens` | 300 | Per-output floor. Matches `coref.py`'s `min_output` so the component and the measurement consider the same population. |
| `cut_unreferenced` | `true` | Cut outputs that introduced trackable identifiers which nothing later referred back to. The honest ceiling of a zero-LLM implementation; needs no calibrated threshold. Note this excludes `opaque` outputs by construction — an output the index cannot see into is never cut, at any setting. |
| `min_later_turns` | 8 | Opportunity floor: an output with fewer model turns after it is treated as `open`. Near the tail "no references yet" and "recent" are the same thing, so without this a batched pass would preferentially cut the newest context. `mask`'s `keep_recent`, expressed in turns. |
| `cut_closed` | `false` | Cut the `closed` class — the large, early, case-A cut. **Off by default**: measured yield is 15% of mass on interactive traffic, 8% on UltraHorizon and 0% on LOCA, so enable it per config for a measured arm rather than globally. |
| `closed_dist` | 12 | A reference is `closed` once its last use is this many messages ago (from the head). **Measured to be nearly inert** — leave it alone. |
| `open_reps` | 3 | Used at least this many times ⇒ `open` regardless of age. The dial that matters; 3 is the conservative setting and each step up trades ~5 points of cuttable mass for reclassifying genuinely repeated spans. |
| `min_batch_frac` | 0.05 | The pass must cut at least this fraction of the request, or it declines and leaves the request byte-identical. It implements the *token* break-even argument and is a **poor proxy for the deferral argument** — it cannot ask whether this cut is the decisive one (design, unbuilt, `docs/proposals/coref-compaction.md`). Was 0.15, which [measurement showed admits 1 of 19 real sessions](../results/coref-density.md) — a gate no traffic can clear is an off switch, not a conservative default. The right value is an experimental result; this is a starting point. |
| `rewrite_budget` | 3 | Prefix-rewrite passes allowed per session. `0` disables new cuts entirely (replay continues). An unreadable counter reads as **exhausted**, never as zero. |
| `break_even` | `true` | Apply `S × T > 11.5 × W` with an estimated *T*. Ignored when the context window is unknown, like every other fraction-based threshold. |
| `keep_head_chars` | 96 | Head-peek left inside the marker so the model knows what was cut without a blind `expand`. `0` for the opaque marker. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

## What it deliberately does not do

- **It does not consult `repairLostFreeze`.** `mask` and `failed_run` may re-derive a lost decision at
  depth, because their replacement is a pure function of `(content, config)` and so reproduces the bytes
  the provider already cached. A co-reference decision is **history-dependent by construction** —
  re-deriving it against a longer transcript can yield a different class and different bytes, which is
  exactly the prefix flip the repair exists to prevent. A lost `coref` freeze declines.
- **It never resurrects a span.** New evidence cannot un-cut, because un-cutting is a second rewrite.
  Monotonicity here is a cache-cost requirement, not tidiness.
- **It does not cut what it cannot see.** An output that introduced no trackable identifier is `opaque`,
  not `unreferenced`, and is never cut at any setting. Measured, that is 8% of tool-output mass on
  interactive traffic and **40% on LOCA-bench** — bulk record dumps of plain values like
  `[{"name":"david","id":123,"address":"foobarbaz"}]`, where short lowercase words and 3-digit numbers
  are exactly what the identifier rules exclude. Absence of evidence is not evidence of deadness.
- **It cannot tell an anchor reference from a payload reference.** If the model says *"remember david 123
  address"*, the reference is real but the value needed (`foobarbaz`) was never copied into a model turn.
  So a *low* `used_frac` is ambiguous, and this is the substantive reason `cut_closed` ships off.
- **It does not see Tier 2.** A value that was summed, unit-converted or reworded before being restated
  leaves no exact match. `unreferenced` means "no later *exact* use" and must never be read as "unused";
  the LLM escalation for that case is an open question in the proposal, not shipped.

## When it shines

Long sessions that cross a context threshold with a lot of survey-and-discard traffic — directory
listings, wide searches, exploratory reads that the agent never returns to. It is complementary to
`mask`: `mask` drops the *old*, `coref` drops the *never-used*, and an old-but-hot span is exactly the
case `mask` gets wrong and `coref` protects.

## When it's inert

Below the `trigger`; no output above `min_tokens`; every large output referenced (`open`); a batch below
`min_batch_frac`; the rewrite budget spent; or the break-even inequality unmet — which, near the window
edge, is the common and correct outcome.

See also: [cheat sheet: every term on one page](../reference/coref-glossary.md) ·
[the held-out selection experiment](../results/coref-selection-experiment.md) ·
the proposal and its derivation (`docs/proposals/coref-compaction.md`) ·
[Components overview](../components.md) · [mask](mask.md) · [dedup](dedup.md)
