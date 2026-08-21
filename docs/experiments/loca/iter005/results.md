# LOCA-bench — iteration 005 (the deferral number, and a shipped bug)

**Date:** 2026-08-21 · **Goal:** re-earn [iteration 002](../iter002/results.md)'s deferral figure on
**live, provider-validated** traffic, after [iteration 004](../iter004/results.md) showed the
pipeline that produced it returns HTTP 400 in production.

**Outcome: the experiment was blocked by a defect in `summarize`, and the defect is worth more than
the measurement.** Fixed in `80e95d5`; the re-run is iteration 005b.

## Design — chaining, so `summarize` runs alone

[`components.md`](../../../components.md) says `summarize` must **run alone**, because it
restructures the transcript and changes the message count. iteration 002 and 004 both violated that.
Rather than drop it, give it its own proxy and chain them — which is also the deployable shape, a
compaction service in front of a summarizer:

```
LOCA → repair shim → cg-proxy A (compaction) → cg-proxy B (summarize alone) → gateway
```

Deferral then reads directly off proxy B: does A reduce how often B fires? Both proxies report
`/stats` independently, and every request reaches a real provider.

| arm | proxy A | proxy B |
|---|---|---|
| `d-sum` | `off` | `summarize` alone — baseline firing rate |
| `d-det` | `codesmart` − `extract_llm` | `summarize` alone |
| `d-full` | + `coref` + prefix-reach `extract_llm` | `summarize` alone |

## What happened

The chain worked mechanically — both stages reporting, **0 pairing repairs needed**:

| arm | proxy A saved | proxy A components | proxy B `summarize` firings |
|---|--:|---|--:|
| `d-det` | 1,137,902 (12.7%) | `format` 132, `dedup` 14 | **4** |
| `d-full` | 4,279,701 (**34.6%**) | `format` 165, **`coref` 61**, `extract_llm` 35, `dedup` 26 | **3** |
| `d-sum` | 0 | — | *(stopped early)* |

But **every arm errored tasks**, including the baseline — which has no compaction at all:

```
d-det   solved 3, errors 3   01000EE01E10
d-full  solved 2, errors 3   010001E00E0E
d-sum   errors 2 of first 3  EE0          ← no compaction, still failing
```

All 400s, all the same shape:

```
messages.1: role 'system' must precede an 'assistant' message or end the array
```

**The baseline arm erroring is what settles it.** With `A=off`, the only component in the path is
`summarize`. So this is not a pipeline interaction and not `coref` or `extract_llm` — it is
`summarize`'s own output.

## The defect

`summarize.go`, both the fresh-summary and checkpoint-replay paths:

```go
summaryMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleSystem}
...
out = append(out, msgs[0], summaryMsg)   // [msg0, summary, last-K]
```

The summary is a **`system`-role message at index 1**. Anthropic wants system content in the
top-level `system` field; a system role inside `messages` must precede an assistant message or end
the array. At index 1, followed by the kept tail, it does neither — so the provider rejects the whole
request. When `msgs[0]` is itself the system prompt, which is the normal case, this fires every time
`summarize` acts.

**`summarize` therefore cannot work on live Anthropic traffic at all**, and the shipped `summarize`
preset would fail immediately in production.

Fixed by emitting the summary as a **user** message — valid, and what Claude Code's own compaction
does. Both paths changed together, since a replayed checkpoint must match the shape of the turn that
created it or the bytes differ.

## Why it shipped — the part worth keeping

Two independent gaps, and the second is structural:

1. **Nothing asserted the summary's role.** The existing tests mention `ChatMessageRoleSystem` only
   for the *input* system prompt at index 0. Now guarded by `summarize_role_test.go`, verified as a
   real guard by temporarily restoring the old role and confirming it fails.

2. **Every prior measurement replayed through `/compact`, which never forwards upstream.** A body no
   provider ever validates cannot fail schema validation. This is not a one-off oversight but a
   **structural blind spot in replay-based measurement**, and it silently affected every
   `/compact`-based result in this branch — [density](../../../results/coref-density.md),
   [the eval-box pass](../../../results/coref-evalbox.md),
   [component gating](../../../results/component-gating.md) and
   [iteration 002](../iter002/results.md). Replay measures *what components remove*; it is
   incapable of telling you whether the result is a **valid request**. Both halves matter.

## What this does NOT prove

- **No deferral number.** The comparison is void: every arm lost tasks to the defect, and the
  baseline lost them too, so firing counts cannot be compared. iteration 005b re-runs it fixed.
- **`coref` and `extract_llm` are not implicated.** They ran clean here — `coref` 61 firings for
  637,825 tokens, `extract_llm` 35 for 680,166, 0 pairing repairs, and the compaction-free arm
  failed identically.
- **The firing counts observed (4 and 3) are not evidence of deferral** even between themselves: the
  arms took different trajectories (260 vs 286 requests), so raw counts are not comparable. Per
  request they are 1.54% and 1.05%, and with a broken baseline neither means anything.

## Next levers

1. **iteration 005b** — the same three arms on the fixed binary. Yields the deferral figure and
   reward together.
2. **Audit the other `/compact`-only results** for schema validity, now that replay is known to be
   blind to it.
3. `summarize`'s interaction with `cachesplit` (which edits the top-level `system` field) is
   untested and adjacent to this defect.

## Artifacts

`/tmp/cg-loca/out-defer2.log`, `proxyA-d-*.log`, `proxyB-d-*.log`, `st-d-*-{A,B}.json`,
`chain.sh`, `sum-only.yaml` on the eval box. Fix and guard: `80e95d5`.
