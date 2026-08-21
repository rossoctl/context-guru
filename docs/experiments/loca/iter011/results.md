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

**This is a fourth message-shape defect in `summarize`**, after the three fixed in
[iteration 005](../iter005/results.md) (`80e95d5`, `0971a32`, `2d6902d`). It survived a dedicated
schema validator (`schema.ValidateShape`, rules `system-position` / `answered-tool-use` /
`paired-tool-result`) and a test asserting all 11 presets emit shape-valid requests — so the validator
has a gap, or the test's fixtures do not reach the shape that breaks.

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
