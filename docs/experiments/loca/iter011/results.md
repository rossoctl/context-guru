# Iteration 011 — ABORTED at arm 1: `summarize` has a fourth shape defect

**Date:** 2026-08-21 · **Pre-registered:** `77f36e3` · **Arms run:** `s3-sum` only (75 runs, ~$8)
**Arms 2 and 3 cancelled before spending**, per the pre-registration's own rule.

## What happened

Arm 1 (`[format, summarize]`) completed 75 runs. **28 of them — 37% — failed**, all with the same
provider rejection:

```
400 messages.1: `tool_use` ids were found without `tool_result` blocks immediately after: toolu_…
     Each `tool_use` block must have a corresponding `tool_result`…
```

The pre-registration named this exact case: *"`summarize` errors or emits invalid requests → the
iteration-005 failure recurred; fix, do not interpret."* So arms 2 and 3 were stopped rather than run
at a 37% failure rate, saving roughly $180.

~~**This is a fourth message-shape defect in `summarize`.**~~ **WRONG ATTRIBUTION — see the root cause
below. `summarize` is not at fault**; the defect is in `apply`, and it is one cause with two faces.

## Two things the aborted arm did establish

**1. `summarize` works mechanically, and the trigger was chosen correctly.** It fired **48 times**
across 1,201 requests with **35 model calls**, removing **1,668,795 tokens** and lifting total removal
to **29.6%** (against `format`-alone's 24.2% in [iteration 010](../iter010/results.md)). The
data-driven trigger — `min_request_tokens: 30000`, from measured p50/p75/p90 of 14k/34k/44k — put
firing where it was intended. Nothing about the *gating* needs revisiting.

**2. The HTML-400 from iteration 010 is a DIFFERENT error, and remains unattributed.** The 29 captured
failures are all proper JSON Anthropic errors (28 pairing + 1 timeout), served by `Server: uvicorn`.
The iteration-010 failures were raw HTML with no Anthropic body. Two distinct faults were being
conflated; this iteration separates them and solves neither.

## Why reasoning from the source did not converge

Both splice sites in `summarize.go` (the fresh path at 257–259 and the checkpoint-replay path at
315–317) *do* advance their boundary past leading tool messages, and `summarizeSpan` already drops the
head when `msgs[0]` is an assistant tool-call message. With `headCount = 1` the emitted layout is
`[msgs[0], summary, tail…]`, so index 1 is the *summary* — which cannot carry tool calls. The reported
orphan is at `messages.1` in **all 28** cases, so the emitted list is not the list the code appears to
produce.

**Diagnosis by reading was therefore abandoned in favour of instrumentation**, which is the lesson
from the day's earlier failures rather than a new one.

## Instrumentation: the capture had to move

`loca_repair_shim.py` sits **upstream** of context-guru, so it records the request *before* compaction —
the innocent copy — and it *repairs* tool pairing, which would mask this very defect. Its capture also
stored headers but not the body.

`deploy/harbor/capture_hop.py` (new) sits on the other side:

```
LOCA → repair shim → cg-proxy → [capture hop] → gateway
```

It repairs nothing and, on any ≥400, records a **structural digest** of the outgoing message list —
per message: index, role, block types, `tool_use` ids declared, `tool_result` ids answered — plus the
provider's error. That digest is what identifies an orphaned pair; the bodies run to hundreds of
kilobytes and are mostly irrelevant.

## Status

- Arm 1's reward numbers are **not reported**: a 37% invalid-request rate is a broken arm, not a result.
- The **deferral question remains unmeasured on live traffic**, for the second time, and for the same
  component's shape handling.
- Next: read the captured digest, fix the defect, extend `ValidateShape` and its test to cover the
  shape that escaped, rebuild, then re-run all three arms.

## ROOT CAUSE (found, fixed, `62126f4`): bifrost cannot represent an Anthropic tool call

Found by instrumenting the rebuild rather than reading it, after three rounds of source-reading failed
to converge.

**`bschemas.ChatContentBlock` has no `tool_use` type.** Its enum is `text` / `image_url` /
`input_audio` / `file` / `refusal`. An Anthropic assistant turn carries its calls as `tool_use`
**content blocks**, so after unmarshaling, the ids are simply *absent*. Everything that reasons about
tool pairing saw an assistant message with **zero calls** on every Anthropic request.

One cause, two defects:

| defect | consequence |
|---|---|
| `dropOrphanedToolResults` builds its answerable set from `ToolCalls` alone | that set was **always empty** on Anthropic traffic, so **every** `tool_result` looked orphaned and the "repair" **deleted all of them**. The provider then rejected the request for the unanswered calls left behind. |
| `schema.ValidateShape` was blind to the same ids | it could not see the breakage — which is why a test asserting **all 11 presets** emit shape-valid requests stayed green while 37% of live runs failed |

The instrumented rebuild made it plain — `summarize`'s output contained **no tool messages at all**:

```
out[0] user      bi=0   EMIT     (head)
out[1] user      NO-MATCH        (the summary)
out[2] assistant bi=1   EMIT     ← declares pa_a, pb_a
out[3] user      bi=17  EMIT     ("final question")
```

**Why every unit test passed while live traffic failed 28/75:** the offload tests hand-build
`ChatMessage`s with `ToolCalls` populated — the *OpenAI*-shaped representation — so they never
exercised the Anthropic path. The validator I had built to prevent exactly this class of bug was
validating a dialect the rig does not use.

**Fix:** `normalize` recovers the ids where the dialect is still known, rather than teaching every
consumer about Anthropic. It deliberately marks such messages non-lossless — honest, since bifrost
genuinely cannot round-trip them — which only makes the write-back guard more conservative. Full suite:
24 packages, 0 failures.

Also fixed on the way (`2ec2445`): `ValidateShape` rejected every ordinary **parallel** exchange,
because it inspected only `msgs[i+1]` while bifrost splits one Anthropic results message into a run of
`tool` messages. It emitted the *same error text* as the real defect, making it useless precisely where
it was needed.

## A verification that proved nothing, caught before it was believed

The first live check of the fix reported **`captured failures: 0`** and looked like a clean pass. It
was not: the same output showed `llm_calls=0` and no `[summarize]` line — **`summarize` never fired**,
so zero failures said nothing about the fixed path. Trajectory length varies, and the earlier
reproduction had happened to fire it twice.

Re-verified with a diagnostic-only trigger (`min_request_tokens: 5000` instead of 30000) so the
component is forced to act. **This is the same failure as iteration 012's vacuous baseline-reuse
check**: a check that cannot fail is not evidence, and both were written by the same reasoning — asking
"did the run come back clean?" instead of "would this have detected the problem?"

## Re-run with both fixes: verified on live traffic

The fix landed in two stages, because the first unmasked the second.

| stage | arm-1 failure rate | failure mode |
|---|---|---|
| pre-fix | **28/75 (37%)** | `tool_use` ids without `tool_result` — results all deleted |
| after fix 1 (`62126f4`, tool-call ids) | 13/39 (33%) | **different**: `messages: Unexpected role "tool"` |
| after fix 2 (`caf32d7`, role leak) | **0/16 (0%)** | none |

Fix 1 stopped `dropOrphanedToolResults` deleting every result. Those surviving synthetic `role=tool`
messages then reached the rebuild, which had never had to serialize one — and while the results were
being deleted, nothing could. **One defect had been masking the other.**

**The verification is not vacuous, and was checked for that specifically.** In the clean run
`summarize` is firing hard — **316 component log entries**, up to **39,313 tokens removed in a single
request** — with **zero** `Unexpected role` or pairing errors reaching the wire. That check exists
because two earlier "clean" verifications this session proved nothing: one where `summarize` never
fired, and one comparing a reused baseline against itself.

A 5-task probe **cannot** verify this fix: its trajectories are too shallow to meet `summarize`'s
`min_messages` and span floor, so it never fires even with the trigger lowered to 5,000 tokens. Arm 1
of the real experiment is therefore the verification, run behind a hard gate — more than 5 failures
and arms 2 and 3 are cancelled — so a failed fix costs one arm rather than three.

## A THIRD defect, same family, found in the clean run — documented, not fixed

The re-run's arm 1 shows 2 failures in 38 runs, and **neither is the pairing bug**:

| count | error |
|---|---|
| 1 | the still-unattributed `<html>400 Bad request` transport fault (6/75 in [iteration 010](../iter010/results.md)) |
| 1 | **new:** `messages.7.content.1.thinking: each thinking block must contain thinking` |

**Root cause hypothesis, same as the tool-call defect:** `bschemas.ChatContentBlock`'s type enum is
`text` / `image_url` / `input_audio` / `file` / `refusal`. It cannot represent a `thinking` block any
more than it can a `tool_use` one. So **any path that re-marshals an assistant message instead of
emitting its original raw bytes silently drops the thinking content**, and the provider rejects the
empty block.

In `rebuildCountChanged` that path is reached whenever a body-derived message fails to byte-match its
pre-pipeline form — the `matched < 0` branch, which exists for genuinely new messages (the summary)
and cannot currently tell them apart from a modified survivor.

**The general shape of all three defects is one fact:** bifrost's schema is a lossy model of an
Anthropic request, so *correctness depends on never re-serialising a message that came from the body*.
Two of the three defects are instances of that rule being broken; the third (`ValidateShape` blindness)
is the same gap in the checker.

**Deliberately not fixed now.** It is rare (1 in 38), it does not block the experiment, and two
substantive changes to this package already landed tonight without review. Stacking a third unreviewed
change to the byte-losslessness machinery raises the risk of a fourth defect more than it lowers the
risk of this one. A likely fix is to match assistant messages by their tool-call id set — as tool
messages now are — and to **decline the rebuild** rather than fresh-marshal anything body-derived.

## Arm 1 (`[format, summarize]`) — 94.7% "savings", 63% MORE money, same solves

Gate passed: **4 failures of 75**, against 28 before the fixes.

| | `[format]` (iter010) | `[format, summarize]` |
|---|---|---|
| solved | 33 / 69 clean (47.8%) | **33 / 71 clean (46.5%)** |
| **total cost** | **$93.23** | **~$152.09** ($148.89 LOCA + $3.20 CG calls) — **+63%** |
| requests | 2,362 | 2,613 |
| mean pre-compaction request | **~12.2k tokens** | **~141k tokens** — **11.5×** |
| reported `savings_pct` | 24.2% | **94.7%** (349.6M of 369.0M) |
| `summarize` unique saved | — | **129.0M** of 288.2M reported (**2.23× overcount**) |
| `compaction_resets` | — | **777** |

**This is the sharpest demonstration yet that removing tokens is not saving money.** `summarize`
reports removing 94.7% of all tokens and costs **63% more** for the **same number of solved tasks**.

**The mechanism is not the expand loop.** That was the obvious hypothesis — the marker invites the
model to call `cg_expand`, restoring the span and regrowing context — and it is **not supported**:
`expand` appears just **twice** in the whole run log.

What the counters do show is that the *trajectories diverged enormously*: mean pre-compaction request
size is **11.5× larger** than the lossless arm's on the identical tasks and band. A plausible reading,
**not established here**, is that lossy summarisation makes the agent redo work — re-reading files and
re-querying tools it can no longer see — so total context grows even as each individual request is
smaller. `cache_write=2.3M` against `fresh_input=59.4M` and `cache_read=53.9M` is consistent with the
prefix being invalidated repeatedly, billing fresh where the lossless arm billed cache reads.

**Two cautions on the 94.7%.** First, `tokens_before` = 369M is a *counterfactual* — it assumes the
same trajectory would have occurred without compaction, and the trajectory is exactly what changed.
Second, `summarize`'s own overcount ratio is **2.23×**, so its unique contribution is 129M, not 288M.
Both push the honest figure well below the headline.

**Bearing on the deferral question:** if this holds in arms 2 and 3, it strengthens rather than weakens
the case for *deferring* summarisation — a summary here is not merely lossy, it appears to be actively
expensive. Whether `coref` and the fold reduce `summarize`'s firing rate, and whether cost falls with
it, is precisely what the remaining arms measure.
