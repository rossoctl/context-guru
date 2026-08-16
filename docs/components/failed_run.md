# failed_run

!!! info "Offload — lossy, reversible"
    Keeps the most recent test/build run in full, collapses every earlier superseded run to a pointer + marker.

## How it works

`failed_run` recognizes test/build run output (regex: `N passed/failed`, `BUILD SUCCESS/FAIL`,
`Traceback`, `FAILED`, `panic:`, `npm ERR!`, pytest session banners). It keeps the **most recent**
run in full and collapses every earlier run **that failed** to a pointer + `<<cg:HASH>>` marker —
a superseded failure is safely recoverable, while an earlier run that *passed* is a distinct result
the agent may still reference and is kept verbatim. Needs ≥2 run-like outputs.

### Markers are anchored to line starts

Every structurally line-initial marker — `BUILD SUCCESS/FAIL`, `=+ FAILURES`, `=+ test session`,
`Traceback (most recent`, `FAILED`, `panic:`, `npm ERR!` — must appear at the **start of a line**
(leading indentation allowed). Only `N passed/failed/error` stays unanchored, because pytest pads
its summary line (`==== 1 failed, 40 passed in 12.31s ====`) and that marker is mid-line by
construction.

They used to match **anywhere** in the blob, and that is a behaviour change worth knowing about if
you have compared runs across it.

!!! warning "Unanchored markers mislabelled source reads as test runs"
    A replay over **1,795 real SWE-bench requests** found **9 of 81 collapses** were
    misclassifications rather than superseded runs: a 22,698-byte line-numbered read of astropy's
    `qdp.py`, a sympy source read, an xarray test file, and a `git show` diff. Each was collapsed
    and **labelled "superseded by a later failed→re-run"** — because the phrase
    `Traceback (most recent` occurred inside the *source text*, and `=+ failures` matched an
    ordinary `= failures` assignment.

    The old note here said a false positive "costs only an expand round-trip, never data". True,
    and it undersold the cost: that round-trip lands on the file the agent is mid-patch on, and the
    label asserts something false about the content. Anchoring removes all four observed shapes for
    free — a line-numbered read begins every line with its number, so nothing structural can start
    the line.

## Before → After

```
before:  [run 1] 3 failed, 5 passed …   [run 2 after fix] 8 passed
after:   [superseded by a later run] <<cg:7d1c…>> [full output: …]   [run 2] 8 passed
```

## Lossiness

Lossy but reversible — superseded runs are stashed and recovered via `context_guru_expand` /
`GET /expand`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 100 | Skip runs smaller than this token count. |

## When it shines

Iterative fix→re-run loops.

## When it's inert

<2 runs detected, small outputs, and earlier runs that **passed** (gate
`earlier_run_passed`) — those are kept verbatim by design.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
