# smartcrush

!!! info "Offload — lossy, reversible"
    Statistical JSON-array compressor: keep first/last items plus any error item, drop the rest, stash the full original.

## How it works

`smartcrush` is a statistical JSON-**array** compressor: it parses the array, keeps `keep_first` +
`keep_last` items plus any item whose raw JSON carries an error signal, drops the rest, and stashes
the full original behind a `<<cg:HASH>>` marker. Kept items are verbatim (schema-preserving). v1
uses fixed anchors (headroom's Kneedle adaptive-K is a documented refinement).

## Before → After

```
before:  [ {…}, {…}, … 200 items … ]
after:   [ item0, item1, item2, item198, item199 ] [5 of 200 items shown; full array: call …] <<cg:…>>
```

## Lossiness

Lossy but reversible — the full original array is stashed and recovered via `context_guru_expand` /
`GET /expand`. Kept items are byte-verbatim.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_items` | 5 | Minimum array length before crushing. |
| `min_tokens` | 200 | Output floor before crushing. |
| `keep_first` | 3 | Leading items kept verbatim. |
| `keep_last` | 2 | Trailing items kept verbatim. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

## When it shines

Long homogeneous JSON arrays (list endpoints, search hits) — the `mcp` preset.

## Measured on real traffic

`smartcrush` needs a top-level JSON array of at least `min_items`. Across **1,748 distinct tool
outputs** from every capture available here (SWE-bench, Terminal-Bench, Claude Code), 15 outputs
*start* with `[` and **none of them parse as JSON** (they are log lines and Python list reprs), and
no envelope string field holds a JSON array either. So the component has zero eligible input on
that corpus.

That is also why it does **not** descend the tool-runner envelope the way [`format`](format.md) and
[`toon`](toon.md) do. The descent was proposed on the strength of the 673/673 envelope finding, but
adding it would have changed nothing measurable here: there are no arrays inside those envelopes
either. Its value, if any, is in MCP-style traffic that returns record arrays — measure that traffic
before adding the descent, and note that the note+marker would have to be placed *inside* the
string field for the envelope to keep parsing.

## When it's inert

Non-array output, fewer than `min_items`, nothing to drop.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
