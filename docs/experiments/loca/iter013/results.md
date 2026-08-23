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

## The 128k band exposes a NEGATIVE INTERACTION between the proxy and the agent's own context management

| arm | solved | errors | mean arriving request | LOCA $ | CG model $ | total |
|---|---|---|---|---|---|---|
| `[format]` blunt reset | 15 / 72 (20.8%) | **3** | 86,125 | 189.95 | 0 | **189.95** |
| `[format, coref, extract_llm, summarize]` | 16 / 60 (26.7%) | **15** | **692,613** | 204.94 | 22.66 | **227.60** |

The extra twelve errors are not random. Five of them are:

```
prompt is too long: 1,584,405 … 6,035,894 tokens > 1,000,000 max
```

**The agent's transcript reached 6 MILLION tokens — 6× the model's real window — and compaction could
not bring it back under.** One further error is the known `thinking`-block defect
([iteration 011](../iter011/results.md)).

**The likely mechanism, stated as a hypothesis because this design cannot isolate it:** CG compacts
each *request*, but the agent keeps its own full history. LOCA decides whether to clear from the token
usage the provider *reports*, which reflects the compacted request — so the better CG is at shrinking
what it sends, the less LOCA believes it needs to clear. The agent's raw history then grows unchecked
until it outruns what any per-request compaction can rescue.

The blunt-reset arm does not have this problem precisely *because* CG removes nothing there:
`compaction_resets=319` with `llm_calls=0` and only lossless `format` acting, so LOCA sees its own
uncompacted sizes and clears on schedule. Mean arriving request is **86,125** tokens against the CG
arm's **692,613** — an 8× difference on identical tasks.

**If that hypothesis holds it is the most operationally important result in this work**, and it cuts
against deploying a compaction proxy in front of an agent that manages its own context: the two
mechanisms are not additive, they are *substitutive*, and the proxy silently disables the agent's
safety net while taking on a job it cannot always finish. It also reframes iteration 011's finding
that summarisation drove contexts *toward* exhaustion — same shape, one band lower.

**Caveats, because this is one arm.** `compaction_resets` cannot separate CG's own shrinking from
LOCA's clearing in the CG arms (summarize acted 2,511 times, and each shrink registers), so the reset
counts are not comparable across arms — only the baseline's 319 is unambiguous. And the two arms
differ by four components at once. Isolating this needs an arm where CG compacts but LOCA's clearing
threshold is set from the *pre*-compaction size, which no current knob exposes.

## Yield at this band, in unique tokens

| component | acted | reported saved | **unique** | overcount |
|---|---|---|---|---|
| `summarize` | 2,511 (70.8%/req) | 2,184,615,650 | **1,592,800,663** | 1.37× |
| `format` | 1,118 (31.5%/req) | 177,283,065 | 177,283,065 | 1.00× |
| `extract_llm` | 223 (6.3%/req) | 23,131,709 | **1,953,083** | **11.8×** |
| `coref` | 513 (14.5%/req) | 16,559,942 | **874,969** | **18.9×** |

`coref`'s unique contribution is **874,969 of 2.46 billion tokens before — 0.04%** — while its reported
figure overstates it **19-fold**. `extract_llm`'s is 0.08%. On unique tokens at this band the two
components under study are rounding error against `format` and `summarize`.
