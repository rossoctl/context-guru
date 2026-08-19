# dedup

!!! info "Offload — lossy, reversible"
    Replaces a tool output byte-identical to an earlier one in the same request with a short pointer + marker.

## How it works

`dedup` replaces a tool output byte-identical to an earlier one in the same request with a short
pointer + `<<cg:HASH>>` marker. Exact match only (near-duplicate is deferred).

## Before → After

```
before:  <big config dump>  … (later, identical) <same big config dump>
after:   <big config dump>  … [identical to an earlier tool output] <<cg:1c8e…>>
```

## Lossiness

Lossy but reversible — the duplicated output is stashed and recovered via `context_guru_expand` /
`GET /expand`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 100 | Skip outputs smaller than this token count. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

## When it shines

Agents that re-read the same file/command output repeatedly.

## Measured on real traffic, and why the hash stays exact

Across **4,149 real requests** (31,241 in-request candidates above `min_tokens`) there was exactly
**one** byte-identical duplicate and **zero** duplicates that differed only in whitespace.

Switching the hash to `extract.ContentKey` — the whitespace/marker-insensitive key `freeze` uses —
was considered and rejected on both halves of that. It would gain nothing measured, and it is not
lossless in the way that matters: `ContentKey` collapses *every* whitespace run, so two outputs that
differ only in indentation (the same Python file read before and after a re-indent, two diffs with
different leading space) would hash equal, and the later one would be replaced by the words
*"identical to an earlier tool output"*. The bytes are still recoverable through the marker, but the
sentence the model reads would be false until it expands. Exact bytes is the right key for a claim
of identity.

## When it's inert

No exact repeats, small outputs.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
