# mask

!!! info "Offload — lossy, reversible"
    Age-based garbage collection: keep the newest few tool outputs verbatim, replace older ones with a short marker + stash.

## How it works

`mask` is age-based garbage collection: it keeps the newest `keep_recent` tool outputs verbatim and
replaces older ones (≥ `min_tokens`) with a short `<<cg:HASH>>` marker + stash. It is complementary
to the content-based offloaders.

## Before → After

```
after (older):  [older tool output masked; starts: 700 701 def __rmul__(self, m): 702 …] <<cg:…>> [full output: call context_guru_expand]
```

The head-peek comes from `keep_head_chars` (default 96); with `keep_head_chars: 0` the marker
is the bare `[older tool output masked]`.

## Lossiness

Lossy but reversible — masked outputs are stashed and recovered via `context_guru_expand` /
`GET /expand`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `keep_recent` | 3 | Newest tool outputs kept verbatim. |
| `min_tokens` | 100 | Only mask older outputs at least this large. |
| `keep_head_chars` | 96 | Characters of the hidden output left inside the marker as a one-line head-peek, so the model knows *what* was masked without a blind `expand` round trip — evidence showed a bare marker on a masked source-file read forces needless expands. Set `0` for the opaque marker (≈2pp more savings). |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

## When it shines

Long agent trajectories where old tool results are unlikely to matter, **on non-caching traffic**.

!!! danger "`mask` is not the `agent` preset's biggest lever, and never was"
    This page used to claim ~27% content-token savings for `mask`. That number belongs to
    **`extract_llm`** — some of the team call its LLM trimming of large file reads the "programming
    masker", and the name collision put the figure here. Three things settle it: the arm that
    produced the number (`codesmart`) contains **no `mask`**;
    [comparison.md](../results/comparison.md#per-component--per-compressor) attributes the savings
    to `extract_llm` + `extract` + `cmdfilter`/`dedup` and never mentions `mask`; and `mask` is
    structurally incapable of it on caching traffic (below).

## When it's inert

≤ `keep_recent` tool outputs, small outputs — **and, structurally, every request on a
caching backend.** That last one is not a tuning caveat; it is geometry, and it was found while
measuring [`coref`](coref.md).

!!! danger "On sequential caching traffic `mask` masks nothing, for any `keep_recent ≥ 1`"
    `mask` consults `TailOnly` before modifying a message, because editing content the provider
    has already cached breaks the prefix hash and forces a cache-write. `TailOnly` permits index
    `i` only when `i > MaxCachedIdx`, and on a sequential conversation `MaxCachedIdx = prevLen − 1`
    — everything the *previous* request contained.

    But `mask`'s candidates are, by definition, the outputs **older** than the newest
    `keep_recent`. On turn *N* every one of those was present in turn *N−1*, so every candidate
    sits at `i ≤ prevLen − 1` and every candidate is refused. **The candidate set and the
    permitted set are disjoint by construction**, and no value of `keep_recent ≥ 1` separates
    them. A probe over captured sequential traffic masked **0 of 8** eligible outputs.

    `repairLostFreeze` is what makes it *appear* to work in production: once a mask has been
    frozen, the replacement is a pure function of `(content, config)`, so it can be re-derived
    byte-identically at any depth and replayed. That covers **maintaining** existing masks — it
    cannot **create** the first one at depth. So `mask` earns its savings on the turns before the
    prefix is cached, then coasts.

    An earlier version of this box said the published 12.5% / 27.5% figures "straddle a behaviour
    change". That was too generous to `mask`: **those figures were never `mask`'s at all** (see the
    box above). The geometry here is the mechanism that makes the misattribution impossible to
    sustain — a component whose candidate and permitted sets are disjoint on every caching request
    cannot be any preset's biggest lever on caching traffic. What `mask` actually saves there has
    never been measured, and the honest number is currently unknown.

    This is also the cleanest statement of why [`coref`](coref.md) is shaped the way it is: it is
    the only offloader that *deliberately ignores* `TailOnly`, because on a long session all the
    cuttable mass is behind `MaxCachedIdx`, and an offloader that refuses to reach there can only
    ever work on traffic that is not being cached.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
· [coref](coref.md)
