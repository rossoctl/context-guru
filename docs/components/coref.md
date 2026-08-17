# coref

!!! info "Offload — lossy, reversible"
    Co-reference-aware compaction: at a threshold crossing, cut the tool outputs that no later turn ever carried anything forward from — in one batched pass, under an explicit cache-write budget.

!!! warning "Opt-in, in no preset, not yet measured on the eval-box corpus"
    `coref` is the implementation of [a proposal](../proposals/coref-compaction.md). Its mechanism is
    tested, and the measurement pass has run on three corpora — Claude Code sessions, UltraHorizon and
    LOCA-bench — but **not on the `capture-swe`/`capture-tb` captures** the acceptance criteria are written
    against ([results and caveats](../results/coref-density.md)). The `closed` cut stays **off by default**
    because its yield ranges from 15% of mass on interactive traffic to **0% on LOCA**, and a knob that
    varies that much by workload has no defensible default. `cut_unreferenced` (the default)
    needs no threshold and is justified — 21% of tool-output mass was never referenced on interactive
    traffic, and ~70% on benchmark traffic.

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
2. **Classify.** `unreferenced` (no later turn used anything it introduced) · `closed`
   (used once or twice, and not for a long time) · `open` (used recently, or repeatedly).
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
and an expanded output is marked kept-verbatim so it is never re-cut. A wrong cut is therefore not a
wrong answer, it is one `expand` round-trip plus a cache-write, which makes **`expand` rate the primary
precision metric** for this component and the one that needs no benchmark scoring to read.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `trigger` | fires always | Gates **new** cuts only (`min_request_frac` against the resolved context window is the natural dial). Replay of latched decisions is never gated — a latched cut that stops being replayed flips the prefix, which is the churn the whole design avoids. |
| `min_tokens` | 300 | Per-output floor. Matches `coref.py`'s `min_output` so the component and the measurement consider the same population. |
| `cut_unreferenced` | `true` | Cut outputs nothing later referred back to. The honest ceiling of a zero-LLM implementation; needs no calibrated threshold. |
| `cut_closed` | `false` | Cut the `closed` class — the large, early, case-A cut. **Off by default**: measured yield is 15% of mass on interactive traffic, 8% on UltraHorizon and 0% on LOCA, so enable it per config for a measured arm rather than globally. |
| `closed_dist` | 12 | A reference is `closed` once its last use is this many messages ago (from the head). **Measured to be nearly inert** — leave it alone. |
| `open_reps` | 3 | Used at least this many times ⇒ `open` regardless of age. The dial that matters; 3 is the conservative setting and each step up trades ~5 points of cuttable mass for reclassifying genuinely repeated spans. |
| `min_batch_frac` | 0.15 | The pass must cut at least this fraction of the request, or it declines and leaves the request byte-identical. |
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
[the proposal and its derivation](../proposals/coref-compaction.md) ·
[Components overview](../components.md) · [mask](mask.md) · [dedup](dedup.md)
