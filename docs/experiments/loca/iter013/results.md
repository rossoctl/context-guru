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

### RETRACTED: the "proxy suppresses the agent's clearing" explanation

The first explanation offered here was that CG's compaction keeps requests under the provider's
`clear_tool_uses` trigger, so the agent's own clearing never fires and its raw history grows unchecked.
**That is wrong, and the check that refutes it is one line:**

| arm | trajectories | with `context_management_events` | total events |
|---|---|---|---|
| `[format]` blunt reset | 75 | **0** | **0** |
| separate components | 75 | **0** | **0** |
| merged | 75 | **0** | **0** |

**Server-side context management never ran in ANY arm, including the baseline.** So the baseline was
never "protected by the provider's unconditional trigger" — there was no trigger firing anywhere. The
retracted claim had the shape of a good explanation and was asserted without checking the one counter
that records the mechanism.

LOCA does request it (`context_management` edits plus `betas:
["context-management-2025-06-27"]`), and CG passes the parameter through byte-identically (verified by
`apply.TestContextManagementParamSurvivesCountChange`). So the feature is asked for and forwarded, yet
never applied. Whether the gateway strips the beta, or the Bedrock-routed `aws/claude-sonnet-5` does not
implement it, is under direct test.

**What still stands, and what does not.** The measurements stand: the baseline had **0** `prompt is too
long` errors against the CG arms' **5**, and mean arriving requests of **86,125** against **692,613** on
identical tasks. What does not stand is the *causal* story. With no server-side clearing anywhere, the
difference must come from the trajectories themselves — the CG arms ran 3× more requests, each ~8×
larger — which is the same trajectory-divergence confound
[iteration 012](../iter012/results.md) established makes per-arm behaviour hard to attribute.

So the honest position is narrower: **at this band, pipelines including CG compaction reached contexts
that exceeded the model's window while the lossless baseline did not, and why is not established.**

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

## ANSWERED: why CG arms carry 8× the context — the agent re-runs tools instead of expanding

With server-side clearing inert everywhere, the size difference cannot be a reset. Measured directly:

| arm | steps/run | duplicate tool calls/run | **% of calls that are exact repeats** | `cg_expand` calls |
|---|---|---|---|---|
| `[format]` lossless baseline | 15.5 | 0.1 | **0.4%** | **0** |
| merged (keeps 94% of candidates) | 39.2 | 2.8 | **9.9%** | **0** |
| separate (4 components, most removal) | 47.0 | 8.2 | **25.3%** | **0** |

**The repeat rate is dose-dependent in removal.** ~~And `expand` was called zero times in 225 runs.~~
**That was wrong — it was grepped for `cg_expand`, and the tool is named `context_guru_expand`.** The
model calls it constantly and is REFUSED; see the section below.

The loop: CG removes content and leaves a `<<cg:HASH>>` marker plus an `expand` tool. The agent does
not expand — it **re-issues the same tool call with the same arguments**. That returns *fresh* output,
so the transcript grows rather than shrinking, which invites more compaction, which removes more, which
provokes more repeats. Hence 3× the steps and 8× the mean request, ending in five requests that exceeded
the model's window outright.

The baseline stays small for an unremarkable reason: nothing is removed, so nothing is lost, so nothing
is repeated (0.4%).

### This is a fourth outcome the design did not enumerate

`corefstub.go` reasons carefully about a wrong cut having three outcomes:

1. the model notices and expands the right marker — one round-trip plus a cache-write;
2. it notices something is missing but cannot tell WHICH marker holds it — several expands, or it gives up;
3. it never notices, and answers from less information than it had.

The measured outcome is none of these. **The model notices, does not expand, and re-runs the tool.**
That is *worse than (1)* — a full tool execution plus fresh output rather than a cached round-trip — and
unlike (3) it **compounds**, because each repeat enlarges the transcript that provoked it.

It also undercuts the reversibility argument that justifies lossy compaction here — though for a
different reason than first recorded. Recovery is not declined; it is **broken**. See below.

### Caveats

- Duplicate detection is regex-based over each step's action payload. The **monotone ordering** across
  arms (same regex, same corpus) is the load-bearing part; the absolute percentages are approximate.
- Longer trajectories offer more opportunity for repeats, but the figure quoted is repeats as a **share
  of calls**, so it is not merely a length artifact.
- **Not established:** that the specific repeated calls are the ones whose output was compacted. That
  needs per-marker correlation, which the current capture does not record.
- `INJECT_EXPAND=auto` was set in every CG arm, so the tool was present and advertised. Whether a
  different prompt or a more legible residue would get it used is untested.

## WHY the agent re-runs tools: expand is CALLED and REFUSED

An earlier draft of this document claimed `expand` was never called. That was a grep error — it searched
for `cg_expand`, and the injected tool is named **`context_guru_expand`**. Corrected:

| arm | `context_guru_expand` mentions | **`Tool '…' not found` rejections** |
|---|---|---|
| `[format]` lossless baseline | 0 | 0 |
| separate components | 38 | **17** |
| merged | **108** | **48** |

**The model reaches for recovery constantly and roughly half those calls are refused outright.** The
refusal text is the client's: `"content": "Tool 'context_guru_expand' not found"`.

### The mechanism, and it is architectural rather than a prompt problem

1. `expand.Inject` in `InjectAuto` advertises the tool **only when the outgoing request carries a
   marker**, and `serve` ties interception to the same test — `advertised := expand.HasTool(provider,
   body)` on the OUTGOING body. One condition, deliberately, so a tool is never advertised without
   being intercepted *on that request*.
2. **The client's history never carries the markers.** LOCA stores its own original content and resends
   it every turn — confirmed: **0 of 75** trajectories contain `<<cg:`. CG rewrites in flight only.
3. So marker presence, and therefore tool advertisement, is **unstable turn to turn**. The model sees
   the tool on turn N and calls it on turn N+1, which may carry no marker — so the tool is not
   advertised, `HasTool` is false, and **nothing intercepts**.
4. The raw `tool_use` is relayed to the client, which has no such tool, and answers "not found".
5. With recovery refused, the model does the only other thing available: **re-runs the original tool**.

The proxy logs contain **no expand lines at all** across either arm, consistent with zero interceptions.
The freeze counters corroborate the underlying cause — **frozen_misses 773,201 against 1,249 hits** in
the separate arm — CG is repeatedly re-deriving rewrites for content that keeps arriving fresh.

### Why this matters beyond the rig

The single-condition design is correct *for a client that persists what the proxy sent*. It assumes the
marker survives in the conversation the client replays, so the tool stays advertised as long as the
marker is live. **A client that keeps its own originals breaks that assumption**, and the failure is
silent and expensive: the model is offered a recovery path, takes it, is told the tool does not exist,
and falls back to re-executing work.

Any agent framework that maintains its own message history — which is most of them — has this shape.
`docs/proposals/coref-compaction.md`'s reversibility argument depends on recovery being *available*;
here it is advertised and then withdrawn mid-conversation, which is worse than never offering it.

**Open, and the right next question:** whether interception should key on the *session* having live
stashes rather than the current request carrying a marker. That would keep the tool resolvable for as
long as anything is recoverable, at the cost of advertising it on marker-free requests — which
`expand/inject.go` documents as its own hazard (a call that resolves nothing, replayed to the client,
reads as an empty summary). Both failure modes trace to the same root: **the proxy cannot see whether
the client kept its rewrites.**
