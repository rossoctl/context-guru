# collapse

!!! info "Offload — lossy, reversible"
    Content-agnostic fallback for an oversized tool output: keep a head + tail window, stash the full original.

## How it works

`collapse` is the content-agnostic fallback for an oversized tool output nothing more specific
handled: it keeps a `head_lines` + `tail_lines` window and stashes the full original behind a
`<<cg:HASH>>` marker. It runs late (after `cmdfilter`/`format`) and skips content already marked.

## Before → After

```
before:  <2,000-line log>
after:   <first 20 lines>
         ... (1960 lines omitted) <<cg:44ab…>> [full output: call context_guru_expand]
         <last 20 lines>
```

## Lossiness

Lossy but reversible — the full original is stashed and recovered via `context_guru_expand` /
`GET /expand`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `max_tokens` | 2000 | Threshold above which an output is collapsed. |
| `head_lines` | 20 | Lines kept from the start. |
| `tail_lines` | 20 | Lines kept from the end. |
| `max_frac` | 0 (off) | Threshold as a **fraction of the model's context window**. When the window is known this wins over `max_tokens`. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |
| `cold_cache` | `false` | On a turn whose prompt cache has **provably expired** (idle past the provider TTL), act at any depth instead of only in the uncached tail. Free when the cache really is gone — the whole transcript is re-billed anyway — and the decision is frozen so later warm turns replay it. Off by default because a *wrong* cold reading costs a cache-write of the whole suffix. See [Freeze / cold_cache](../design.md#the-one-turn-where-depth-is-free-cold_cache). |

## When it shines

A catch-all last stage for huge outputs.

## When it's inert

It now names its reason instead of passing silently: `below_max_tokens`, `too_few_lines` (head/tail
would not help), `marker_or_kept_verbatim`, `non_text_blocks`, `marker_no_win`, or `cached_prefix`.

`cached_prefix` is new. `collapse` used to carry **no depth restriction at all** — it re-derived the
whole transcript on every turn, which contradicted the cache-safety contract every other
supersession offloader obeys. It got away with it because the rewrite is deterministic, but not
quite: `max_tokens` is resolved through `max_frac` × the model's context window, so a `max_frac`
config plus a mid-session window change (a model swap, or `modelinfo` resolving differently after a
refresh) would silently re-threshold messages **inside** the cached prefix. It never ran in
production, so this cost nothing; it is fixed before it can. `collapse` now decides each output once,
on the turn it arrives in the uncached tail, and freezes that decision so later turns replay the same
bytes at any depth — measured byte-for-byte identical removal on real captures.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
