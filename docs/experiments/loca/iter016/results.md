# Iteration 016 — always-advertise expand: STOPPED at 70/75, and it corrects two earlier claims

**Pre-registered:** `459e532`. **Stopped deliberately** once the pre-registered criteria were answered;
everything remaining would have characterised a configuration already shown to be wrong.

## Criterion 1 FAILED, in the direction the pre-registration warned about

| criterion | before (`auto`) | with `always` |
|---|---|---|
| 1. `Tool 'context_guru_expand' not found` | 48 | **180 at 62/75 (~215 projected)** |
| expand calls attempted | 108 | **371** |
| 2. tools-array flap transitions | uncounted | **0** ✅ |
| tool advertised | ~70% of turns | **100%** (2,299/2,299) |
| **marker present** | — | **1.2%** |

The flap is genuinely fixed. But the tool is now advertised on every request while **only 1.2% carry
anything to expand**, so the model calls it 3.4× more often, CG cannot resolve it, and the raw `tool_use`
is relayed to a client that has no such tool.

**This is exactly the hazard `expand/inject.go` documents**, and which the pre-registration named as the
reason criterion 1 might fail: *"Advertising the tool on a request with nothing to expand invites a call
that resolves nothing, and the host then has to replay the model's raw tool_use to the client."* The
change traded a certain failure for a **more frequent** one.

**The inject mode was never the root cause.** Both modes fail identically: under `auto` the tool appears
rarely and the model calls it from memory on other turns; under `always` it appears constantly and the
model calls it constantly. Either way **CG replays an unresolvable expand call to a client that will
always reject it.** The fix is in the proxy's expand loop — answer every call itself, resolving from the
session stash or replying that the content is still present — not in a mode flag. `always` remains the
right end state once that lands, because it is what removes the cache flap; it is simply unsafe before it.

## CORRECTION 1: "the merged design declines to act" was misleading about YIELD

| | acted | **unique tokens removed** |
|---|---|---|
| separate: `coref` + `extract_llm` | 513 + 223 | **2,828,052** |
| merged (iteration 014) | 1,827 | **18,065,171 — 6.4× MORE** |

Both statements are true and were conflated:

- **by verdict count** the merged design keeps ~94% (`keep` 4,181, `drop` 259, `trim` 1);
- **by tokens** its few drops are the large ones, so it removes **6.4× more mass** than deterministic
  `coref` plus separate `extract_llm` combined.

"Declines to act" described the verdict distribution and gave the wrong impression about yield. The
accurate statement: **it removes a small fraction of candidates but the biggest ones, so it out-removes
the deterministic index by mass — while almost never trimming.** `merged_trim` = **1 of 4,441 decisions**.
That last figure is the real gap: trimming is the judgement an exact matcher cannot make, and in practice
the design is all-or-nothing.

It also explains the 1.2% marker rate: drops touch ~10% of requests but concentrate on the largest, so a
handful of requests carry several markers each.

## CORRECTION 2: repeated tool calls are NOT the main driver of context growth

| arm | steps/run | repeats/run | repeats as share of the EXTRA steps |
|---|---|---|---|
| lossless baseline | 15.5 | 0.1 | — |
| merged | 39.2 (+23.7) | 2.8 (+2.7) | **11%** |
| separate | 47.0 (+31.5) | 8.2 (+8.1) | **26%** |

An earlier section presented the repeat loop as the explanation for the CG arms carrying 8× the context.
**It explains 11–26% of the extra steps at most.** The dominant term is simply that the agent takes
2.5–3× more steps, and since every request carries all prior outputs, context grows superlinearly — 3×
the steps gives roughly 7–8× the mean request, which accounts for 86,125 → 647,795 with no repeat loop
required.

The repeat rate is real and dose-dependent, which is why it looked causal. In absolute terms it is ~3
extra calls per run out of ~24 extra steps.

**What is ruled out:** differential success (15 vs 16 solved). **What is not established:** whether
compaction makes the agent explore more with *non-identical* calls — the repeat metric only catches exact
duplicates — or whether the lossless baseline simply terminates early for its own reasons. Distinguishing
them needs the sequence of distinct calls per run, not duplicate counts.

## Status

- `INJECT_EXPAND` should return to `auto` until the proxy-side interception fix lands; `always` is
  measurably worse in the meantime.
- The baseline arm is **not** run: comparing against an arm whose recovery path is broken in a new way
  would not be informative.
- The flap instrumentation stays — it converted an inference into a count, and it is what showed the
  fix worked on its own terms while failing on the one that mattered.
