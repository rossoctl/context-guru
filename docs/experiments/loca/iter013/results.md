# Iteration 013 — 128k band, realistic summarize config (IN PROGRESS)

**Pre-registered:** the corrections in [iteration 011's retraction](../iter011/results.md).
Four arms, `cg-proxy-v7`, 128k declared window, `min_request_frac 0.78` (~100k),
`resummarize_tokens 20000`, LOCA clearing at 128k, baseline first.

## Correction: `shim repairs` is the WRONG indicator for "the baseline is lossy"

The pre-run gate used the shim's `repairs` count to check that LOCA's clearing was firing, on the
reasoning that clearing orphans `tool_use`/`tool_result` pairs. **That indicator does not work**, and
it reported `repairs=0` in every arm, which nearly led to recording the baseline as lossless for a
second time.

The right counter is **`compaction_resets`**, which counts turns whose cached-prefix boundary restarted
because *the transcript shrank under a stable session id* — i.e. the agent compacted its own
transcript. In the blunt-reset arm:

| signal | value | meaning |
|---|---|---|
| `compaction_resets` | **319** | LOCA's clearing fired ~4.3× per run |
| `llm_calls` | **0** | no CG model call — so the shrinking is not CG's doing |
| components acted | `format` only (lossless) | CG removed no information |
| shim `repairs` | 0 | LOCA's `clear_tool_uses` evidently rewrites tool-output CONTENT rather than deleting messages, so no pair is ever orphaned and the shim has nothing to repair |

**So the baseline in this iteration IS genuinely lossy** — 319 agent-side compactions — which is
exactly what [iteration 010](../iter010/results.md) could never achieve and what the whole move to
128k was for. The design works; the instrumentation choice was wrong.

Recorded because it is the same class of error as the three vacuous checks already logged: a signal
was chosen for its plausibility rather than verified to move when the thing it measures moves.

## Arm 1 — `[format]`, blunt reset (complete)

| | |
|---|---|
| requests | 1,171 (mean arriving **86,125** tokens) |
| `format` acted | 481 (41.1%/req), **14,500,733** unique tokens |
| `compaction_resets` | **319** |
| cache write / fresh input | 7,403,630 / 37,666,936 |
| CG model calls | 0 |

Arms 2–4 and the merged arm ([iteration 014](../iter014/PREREGISTRATION.md)) are queued.
