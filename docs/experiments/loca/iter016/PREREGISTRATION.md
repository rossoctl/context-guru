# Iteration 016 — pre-registration: merged arm with the expand tool always available

**Written before the run.** Merged arm only, by direction; a matched baseline follows **only if** this
one shows improvement and no new defects.

## What changed and why

`INJECT_EXPAND=always` instead of `auto`. Under `auto` the expand tool is advertised only on turns whose
outgoing body carries a `<<cg:…>>` marker, and interception is deliberately tied to the same
per-request test (`expand.HasTool(body)`). A marker appears only when a component actually acted that
turn — `summarize` 70.8%, `coref` 14.5%, `extract_llm` 6.3% — so roughly a third of turns had no marker,
no tool and no interception. The model treats the tool as a persistent capability (its own history shows
it calling it) and called it on those turns; the raw `tool_use` was relayed to LOCA, which has no such
tool, and answered **`Tool 'context_guru_expand' not found` — 48 of 108 attempts.** Recovery refused,
the model re-ran the original tool instead.

Verified before running: the advertise rule (`expand.TestWhenIsExpandAdvertised`) and end-to-end, that a
**marker-free** request now leaves the proxy with `n_tools=2, has_expand=true`.

## Pre-declared success criteria

| # | measurement | current | expected if the diagnosis is right |
|---|---|---|---|
| 1 | `Tool '…' not found` in the LOCA log | **48** | **0** |
| 2 | `has_expand` transitions (flap log) | inferred, uncounted | **0** |
| 3 | cache-read share of input | **47.5%** | **> 47.5%**, toward the lossless arm's 69.2% |
| 4 | exact-repeat tool calls | **9.9%** | **< 9.9%** |
| 5 | solved (ITT, n=75) | 16 | ≥ 16 |
| 6 | total cost | $232.72 | < $232.72 |

**1 and 2 are the fix working. 3 and 4 are the mechanism it was supposed to break. 5 and 6 are whether
any of it matters.** If 1 and 2 pass but 3–6 do not move, the diagnosis was right and the consequence
was small — which is a real answer, not a failure.

## New instrumentation

The capture hop now records, per request, how many tools were advertised, whether expand was among them,
and whether a marker was present. **This turns the flap from an inference into a count** — it had been
argued from the advertise rule plus per-arm action rates, never observed per request.

Stated limit: transitions are counted in arrival order across interleaved sessions, so the figure is an
**upper bound** on per-session flapping. Under `always` it should be 0 regardless, which is why it is
still a usable check.

## Threats

- **`always` has its own hazard**, named in `expand/inject.go`: an unresolvable expand call on a
  marker-free turn gets replayed to the client and "reads as the summary came back empty". That hazard is
  already occurring in its worst form, so this trades a certain failure for a possible one — but if
  criterion 1 does not reach 0, this is why.
- **One arm, no matched baseline yet.** Comparisons are against iteration 014's merged arm, which ran on
  `v8` with `auto`. Two things differ (binary and inject mode), so a difference cannot be attributed to
  the inject mode alone. The baseline arm that follows will fix that.
- **The merged design itself still declines to act** (`merged_keep` 94%). This iteration tests the
  recovery path, not the selection quality.
