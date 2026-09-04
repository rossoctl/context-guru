# collapse

!!! info "Offload — lossy, reversible"
    Content-agnostic fallback for an oversized tool output: keep a head + tail window, stash the full original.

## How it works

`collapse` is the content-agnostic fallback for an oversized tool output nothing more specific
handled: it keeps a `head_lines` + `tail_lines` window and stashes the full original behind a
`<<cg:HASH>>` marker. It runs late (after `cmdfilter`/`format`) and skips content already marked.

The window is cut by **lines** when that actually shrinks the output, and by **characters** when it
does not. The character path matters because `collapse` is the last resort: if it declines, nothing
caps the output. The line-only version declined every output of `head_lines + tail_lines` lines or
fewer (40 by default) — which is exactly the shape of the largest tool results in practice, a
database or HTTP API response serialised as **one line** of JSON. Measured on a live 128k-band arm: 16 upstream
400 `prompt is too long` responses in 3,347 requests, on outgoing bodies of **2.6 MB to 14.8 MB**,
and 17 of 75 runs errored. Nothing else caught them — `extract` acted 0 times (`no_obvious_noise`
16,891; it only strips known noise patterns), `cmdfilter` 0 (`no_filter_match`), `toon` 21
(`not_uniform_object_array` 233,501), `dedup` 774 (exact duplicates only), `extract_llm` declines by
design once the output exceeds the compaction model's own window (`over_model_context`), and
`summarize` protects `keep_last`, which is where a fresh oversized output sits. `linecap`'s per-line
cap does not rescue it either: its `neverTruncate` allow-list exempts any line matching
`^\S+:\d+`, and `{"items":[{"id":1,...` matches that by accident ([#151](https://github.com/rossoctl/context-guru/issues/151)).

The choice between the two windows is made on **bytes, not on the line count**. Having more lines
than the window says nothing about their size: an output with a multi-megabyte line inside the kept
head or tail survived a line-count gate untouched, and *worse than being declined* — dropping the
small filler lines in the middle counts as acting, which stamps a marker, which then makes `linecap`
decline `already_marked`. Measured at 4,195,111 B in and 4,194,922 B out, 189 bytes saved
on a body forwarding 1.57 M tokens. The line window is therefore taken only when it at least halves
the output; anything else falls through to the character window. The test is in bytes rather than
tokens deliberately: `max_tokens` is resolved through `max_frac` × the model's context window, which
can change mid-session, so a token test could classify the same content differently on a later turn
and emit different bytes — trading a savings bug for a cache bug. Bytes depend only on the content
and `head_lines`/`tail_lines`, so the classification is replay-safe.

The character budget is the component's own token threshold expressed in characters — no new knob.
It starts at `max_tokens × 4` (the same ratio `internal/tokens` falls back to), is split between
head and tail in the `head_lines`:`tail_lines` ratio, and is then **measured** and tightened if the
assembled window overshoots `max_tokens`, because dense JSON tokenizes closer to 2.5 chars/token.
The cut is on rune boundaries, so a multibyte character is never split.

Two bounds on that loop are worth knowing, because they mean a very small `max_tokens` does not
deliver what it says. The tightening is bounded at **3 passes**, and the window has a **200-character
floor** — so `max_tokens: 50` or lower buys a 200-character window regardless, and `max_tokens: 100`
measures ~118 tokens out. The floor exists so a `max_tokens` of 0 cannot reduce an output to a bare
marker with no cue about what to expand; `head_lines: 0, tail_lines: 0` routes to the character
window for the same reason. The seed is also never allowed above the content itself: `max_tokens
× 4` overestimates on dense JSON, and testing the bail-out against the un-measured seed used to
decline content the fit loop would have cut (at `max_tokens: 3000`, what `codesafe` sets, one-line
JSON from 3,006 to 4,491 tokens was oversized and left entirely uncut). The never-worse guard
measures the real marker and declines rather than growing a message, which is what makes the
residual slack harmless. Negative `head_lines`/`tail_lines` are clamped to 0.

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

It now names its reason instead of passing silently: `below_max_tokens`,
`too_few_lines_and_chars` (no window that shrinks it: no usable line window *and* fewer characters
than the character window's floor — the only genuine decline left), `already_marked`, `kept_verbatim_after_expand`, `non_text_blocks`, `marker_no_win`,
or `cached_prefix`.

`too_few_lines` is **gone** and was not renamed: its population is now either handled (the
`char_window` *event*) or a real decline (`too_few_lines_and_chars`). Exporting a decline and a
success under one metric name gives a series that falls as the component works better.

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
