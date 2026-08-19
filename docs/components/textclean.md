# textclean

!!! info "Reformat — lossless"
    Strips terminal display control out of plain-text tool output: ANSI escape sequences and `\r` progress redraws. No marker, no stash, no floor.

## How it works

Most tool output is not JSON. Measured over **1,748 distinct tool outputs** from every capture
available here (SWE-bench, Terminal-Bench and Claude Code sessions), **1,724 are plain text** — so
[`format`](format.md) and [`toon`](toon.md), which both require a JSON document, never see them.

`textclean` is the lossless Reformat for that mass. On a tool message that is ≥ `min_tokens` it
removes two kinds of pure terminal display control, and nothing else:

- **ANSI/VT100 escape sequences** — colour, cursor moves, OSC strings. Display control, never
  content. `grep --color=always`, coloured `pytest`, `npm` and `cargo` output are full of them.
- **Carriage-return redraws** — a progress line rewritten in place. The bytes before the last `\r`
  on a line were overwritten before anything displayed them, so keeping only the final rendered
  segment loses nothing the agent could ever have read. A **trailing** `\r` is the CRLF separator
  surfacing after a split on `\n`, not a redraw: CRLF text comes back byte-identical.

Both transforms already existed inside [`extract`](extract.md), an **Offload** — so they cost a
`<<cg:HASH>>` marker, a store round-trip and, above all, they were refused below extract's
300–400 token floor. As a Reformat there is no marker, no stash and no floor.

### Verify then adopt

A candidate is adopted only if **every informative line survives it byte-identical** (each line
that is non-blank once display control is resolved, in the same order) *and* the result is strictly
smaller in tokens. Anything else leaves the message exactly as it arrived, with a gate reason.

## Before → After

```
before:  \x1b[35m/src/app.go\x1b[m\x1b[36m:\x1b[m\x1b[32m41\x1b[m\x1b[36m:\x1b[m  return \x1b[1;31mnil\x1b[m
         downloading:  10%\rdownloading:  60%\rdownloading: 100%

after:   /src/app.go:41:  return nil
         downloading: 100%
```

## Lossiness

None, and nothing is stashed. Stripping an escape sequence is one-way but non-semantic (there is
nothing to restore — it was never content), and a redraw's overwritten bytes were never displayed.
The runtime check above is what enforces that; the tests prove it over **real** terminal output
(a real `grep --color=always` run and a real `\r` progress writer), not hand-typed fixtures.

Three neighbouring transforms are deliberately **out of scope**:

- dropping progress-bar lines and collapsing repeated lines/blocks remove whole lines, so they need
  `extract`'s stash to stay recoverable — that is `extract`'s job, not this one's;
- trimming trailing whitespace changes meaning in a unified diff (a context line whose only content
  is its leading space) and in markdown (the two-space line break);
- collapsing runs of blank lines is safe but **measured worthless**: 21 tokens over the whole
  1,748-output corpus, against 16,721 for the ANSI strip and 498 for the redraws.

## Measured

`TestCorpusMeasureReformatters` / `TestCorpusMeasureTextCleanBreakdown` in
`components/reformat`, run against a capture with `CG_CORPUS` (captures are never committed):

| | outputs | tokens |
|---|--:|--:|
| corpus | 1,748 | 1,077,024 |
| `textclean` fires | 12 (0.7%) | 18,694 → 1,475 (**−17,219**, 1.60% of the corpus) |
| of that: ANSI strip | 9 | −16,721 |
| of that: `\r` redraws | 3 | −498 |
| below extract's 400-token floor (saving nobody takes today) | 10 | −1,353 |
| above it (same saving, now with no marker or stash) | 2 | −15,866 |

It fires rarely and pays hugely when it does: on the outputs it touches it removes **92%** of the
tokens, because escape-sequence bytes tokenize badly.

## Cache posture

Deterministic — a pure function of the content, so the same output is rewritten to the same bytes
on every turn and after a process restart. No `TailOnly` gate and no freeze are needed, exactly
like [`format`](format.md).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

## When it shines

Coloured search/build/test output, and any progress-bar-writing command.

## When it's inert

Output with no escape sequences and no interior carriage return — which is most output, and costs a
single regex probe (`no_terminal_noise`).

## Presets

Shipped in **`general`**, right after the JSON reformatters. It is deliberately **not** added to
`codesmart` / `codesafe`: those name-lists are the SWE-bench study's published arms, and
`config/config.go` states that a change to them is a reason to re-measure rather than a
documentation edit.

See also: [Components overview](../components.md) · [`extract`](extract.md) · [Choose a preset](../how-to/choose-a-preset.md)
