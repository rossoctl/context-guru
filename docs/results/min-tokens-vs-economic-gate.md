# The floor and the gate: `min_tokens` vs `economic_gate`

`min_tokens` and `economic_gate` pull against each other. **That interaction predates PR #80 by
weeks** — but it only became a *correctness* bug when PR #80 added a selection mode that makes one
model call per request.

| | |
|---|---|
| **Pre-existing** (since 2026-08-10) | The two filters overlap, and `min_tokens` silently stops mattering below the gate's implied economic floor. Redundant and hard to observe — **not wrong**. |
| **New in PR #80** | `selection_mode: merged` breaks the assumption the gate is built on, so the gate suppresses candidates that cost almost nothing to include. |

Provenance: `min_tokens` on `extract_llm` arrived in `7f0379a` (2026-07-25); `economic_gate` in
`2adb476` (2026-08-10, #28/#51); `selection_mode: merged` is PR #80 and not yet in `main`.

## What each one does

**The floor** — `min_tokens`, resolved through `Trigger.OutputFloor(window, min_tokens)` — is a size
test on each tool output: is it big enough to be worth considering? Smaller outputs are skipped and
counted as `below_output_floor`.

**The gate** — `economic_gate` — is a cost/benefit test on each candidate that clears the floor. It
compares an expected saving (size × the compression ratio this workload has actually been achieving,
learned rather than assumed) against an expected cost (a model call's tokens), and charges for a cache
write when the removal falls inside an already-cached prefix.

## Why they pull against each other

Lowering the floor admits *more* candidates, but each one is *smaller*. Smaller candidates have
smaller expected savings, so the gate rejects more of them. **Widening the floor to increase coverage
feeds the gate exactly the candidates it is designed to refuse.**

Measured on this workload after dropping `min_tokens` from 3000 to 800 — expecting more work, getting
less:

```
suppressed: cache-aware, saving below call cost   1497
allow: recurring content, amortised                613
allow: expected saving exceeds cost                 29

candidates that reached the model                  224
```

## Why this was not a bug before PR #80

In the per-output design — the only one that existed until now — the pipeline makes **one model call
per candidate**. So a candidate *is* a call, and pricing a candidate against the cost of a call is
exactly right. When the gate refuses a small output it is making a true statement: that output cannot
repay its own call.

On that design the floor and the gate are two filters **agreeing**. The gate is simply the stricter and
better-informed of the two, since it knows the workload's real compression ratio and the cache cost
while the floor knows only a byte count.

Two things do deserve attention independently of PR #80:

* `min_tokens` is effectively **advisory** below the gate's economic floor. An operator who lowers it
  to widen coverage may see no behavioural change at all.
* The override is **invisible unless you know which counter to read**. Nothing reports "your floor was
  honoured and then overruled on economic grounds" — you have to notice `economic_gate` in the gate
  map and compare it against what reached the model.

## What PR #80 changed

`selection_mode: merged` makes **one model call per request**, adjudicating the whole batch together,
because the comparative judgement it relies on needs peers to compare against. The marginal cost of
adding the seventh candidate is therefore its share of one prompt — not a call.

With prefix asks (also PR #80) the outputs are read from the provider's cached transcript rather than
shipped, so a candidate's marginal cost is a single inventory line, on the order of thirty tokens.

The gate still prices each candidate against a whole call. In this configuration that estimate is wrong
by two to three orders of magnitude, and it rejects candidates that are nearly free to include — which
starves the batch and removes the very comparison the design depends on.

## Evidence that the floor is a real lever

Same three tasks, same everything, only `min_tokens` varying, with the gate disabled so the floor could
be seen in isolation:

| `min_tokens` | candidates / call | batch hit the 12-cap |
|---|---|---|
| 800 | 1.29 | no |
| 300 | 2.22 | 4× |
| **120** | **5.97** | 11× |

**Only the candidates-per-call column is trustworthy here.** The three runs diverged in trajectory —
total traffic spread over 7×, and `summarize` fired on 57%, 66% and 2% of requests respectively — so
drop counts and token savings are not comparable across rows.

## Suggested changes

1. **Make the gate batch-aware.** Compare the batch's total expected saving against *one* call's cost,
   and treat a candidate's marginal cost as prompt tokens. The only change that fixes the cause rather
   than working around it.
2. **Log when the gate overrules the operator's floor.** A configured `min_tokens` that has no effect
   should say so, in either design.
3. **Report candidates per call.** Verdicts divided by calls is one line of arithmetic, and it would
   have caught this at the first arm rather than the third.

The third point is the one with history. Three separate experiments were run and interpreted as "the
model declines to act" when the model was being shown roughly one candidate at a time — first by a
co-reference pre-filter, then by a per-request call cap, then by this gate. All three are the same
family of defect: **cost machinery written for per-output calls, applied to a one-call design.**

## Where the numbers come from

Gate counters read from the proxy's `/stats` during a 128k-band LOCA arm; the floor sweep was run
offline on a 3-task configuration. Full detail in `docs/experiments/loca/iter019/results.md` and
`docs/experiments/loca/iter020/`.
