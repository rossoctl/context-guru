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

## Iterations 005b and 005c: two fixes, a third defect, and a deliberate stop

Fixing the system-role defect did not unblock the experiment. It let `summarize` act more often,
which surfaced the next violation — and then the next.

| attempt | fix applied | task errors | new failure |
|---|---|--:|---|
| 005 | — | 3 | `messages.1: role 'system' must precede…` |
| 005b | user-role summary (`80e95d5`) | **5** | `messages.0.content.2: unexpected tool_use_id in tool_result blocks` |
| 005c | + drop orphaned results (`0971a32`) | 3 | `messages.1: tool_use ids without tool_result blocks immediately after` |

Each defect **masked the next**. While the system-role bug fired, `summarize` barely got to act;
fixing it raised the error count to 5, which looked like a regression and was really the component
finally running far enough to break differently.

The three are one family — **`summarize` does not maintain the provider's message-shape
invariants**:

1. a `system`-role message spliced mid-array;
2. `tool_result` blocks kept whose `tool_use` was deleted (orphaned results);
3. `tool_use` blocks kept whose `tool_result` was deleted (unanswered calls) — the converse of (2),
   arising because `msgs[0]` is preserved verbatim and its results may sit inside the removed span.

Both fixes are real, tested, and worth keeping. **They do not make `summarize` usable.** (3) needs
phase 2 of a pairing repair — synthesising a placeholder result, or declining to preserve an
unanswered call — and I explicitly argued *against* synthesising when writing (2), on the grounds
that a fake result is "a second lie on top of the summary". That reasoning was wrong on the
decisive point: **an invalid request is worse than an imperfect one.** It is still a design decision
rather than a patch, and it needs to be made deliberately.

**So I stopped rather than attempt a third fix in the same session.** Three defects in one component,
each revealed only by fixing the last, is a signal about the component's readiness, not a queue of
chores. A fourth patch written at speed would more likely add a fourth defect than reach a working
state.

One further error in 005c was *not* of this family and is unexplained: a raw
`<html><body><h1>400 Bad request</h1>` with no Anthropic error body, so it did not come from the
model API. Recorded, not diagnosed.

## Status of the deferral claim

**Unmeasurable until `summarize` maintains message-shape invariants.** That is the honest position.
The claim itself remains plausible — [iteration 002](../iter002/results.md) showed the *mechanism*
(compaction reduces how often a context max is reached) on replayed traffic — but no valid live
measurement exists, and the component the claim depends on cannot currently send a request a
provider will accept.

`coref` and `extract_llm` are **not** implicated in any of this. They ran clean in every arm, and the
arms containing neither of them failed identically.

## Next levers

1. **Give `summarize` a real pairing-repair pass**, both directions, as a deliberate design step:
   decide what an unanswered preserved call becomes. Until then the component should arguably be
   marked unusable on providers that enforce pairing.
2. **A schema-validity test that does not need a provider.** All three defects are checkable
   statically against Anthropic's documented rules — system position, results answered, calls
   answered. A validator run over pipeline output in tests would have caught every one, and would
   close the replay blind spot without requiring live traffic for every measurement.
3. **Audit the other `/compact`-only results** for schema validity, now that replay is known to be
   blind to it.
4. `summarize`'s interaction with `cachesplit` (which edits the top-level `system` field) is
   untested and adjacent to defect (1).

## Artifacts

`/tmp/cg-loca/out-defer2.log`, `proxyA-d-*.log`, `proxyB-d-*.log`, `st-d-*-{A,B}.json`,
`chain.sh`, `sum-only.yaml` on the eval box. Fix and guard: `80e95d5`.
