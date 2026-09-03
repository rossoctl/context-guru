# Iteration 017 — pre-registration: the expand loop, fixed

**Written before the run.** Merged arm only, per direction; a matched baseline follows only if this one
improves and shows no new defects.

## What was wrong

The proxy replayed the model's own `tool_use` to the client whenever **no** expand id resolved
(`proxy.go`, the `got == 0` branch). LOCA has no `context_guru_expand` tool — the proxy injects it — so
LOCA answered `Tool 'context_guru_expand' not found`. The model lost its recovery path and **re-ran the
original tool** instead: a full tool execution plus fresh output, enlarging the transcript that provoked
the cut.

Measured: **17 of 38** attempts refused in the separate arm, **48 of 108** in the merged arm, with
exact-repeat tool calls at **9.9%** and **25.3%** against **0.4%** in the lossless baseline.

## The three changes (`0afa393`)

1. **`got == 0` sends the continuation with placeholders** rather than relaying. The placeholder text
   already existed per call id, so the model is told the content is gone — actionable, unlike a missing
   tool. Capped at one such round.
2. **Interception no longer requires this request to have advertised the tool.** A session that has ever
   been offered it is remembered, and every **non-streaming** response is inspected — free, since a JSON
   body is read in full either way. The advertise gate is kept for SSE alone, where buffering has a real
   cost.
3. **Unresolved calls are counted**, split into `expand_unresolved_malformed` (the model invented an id)
   and `expand_unresolved_missing` (**a marker was issued and its stash is gone — reversibility silently
   failed**). Both in `/stats`. Neither existed before, which is why this went unnoticed for three
   iterations.

`INJECT_EXPAND` returns to **`auto`**: with interception decoupled, `always` is no longer needed to make
recovery work, and `auto` avoids advertising a tool on the ~99% of turns with nothing to expand.
**This costs the tools-array flap again** — a known trade, and criterion 5 measures it.

## Pre-declared criteria

| # | measurement | before | expected |
|---|---|---|---|
| 1 | `Tool '…' not found` in the client log | 48 | **0** |
| 2 | `expand_unresolved_missing` | invisible | **any value is new information**; >0 means reversibility is failing |
| 3 | exact-repeat tool calls | 9.9% | **< 9.9%** |
| 4 | steps/run | 39.2 | ≤ 39.2 |
| 5 | cache-read share | 47.5% | may **fall** vs iteration 016's `always`; compare to iteration 014's 47.5% under `auto` |
| 6 | solved (ITT) / total cost | 16 / $232.72 | ≥ 16 / < $232.72 |

**1 is the fix working. 2 is new visibility. 3–4 are the loop it was supposed to break. 5 is the trade
being accepted. 6 is whether it matters.**

Declared in advance: **if 1 passes and 3–4 do not move, the diagnosis was right and the consequence was
small.** Iteration 016 established that repeats explain only 11–26% of the extra steps, so 3–4 moving a
little is the expected case, not a triumph — and step count, whose cause is still unestablished, is the
dominant term.

## Threats

- **The volume story is already withdrawn** ([iteration 013](../iter013/results.md)): CG arms send 32–35%
  *fewer* tokens upstream than the baseline and are billed only 12–14% more, because the cost is a
  cache-tier shift, not volume. So criterion 6 is about the tier mix, and no large cost win is expected.
- **One arm, no matched baseline**, and it differs from iteration 014 by binary and by these fixes.
- **The merged design still barely trims** (`merged_trim` 1 of 4,441). This iteration tests recovery, not
  selection quality.
