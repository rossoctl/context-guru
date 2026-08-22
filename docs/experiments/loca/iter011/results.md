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
