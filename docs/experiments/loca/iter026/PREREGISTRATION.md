# Iteration 026 — pre-registration: does deferring lossy compaction buy anything a user would notice?

**Written and committed before launch.** Iteration 025's probe numbers are quoted where they justify a
design choice; nothing from this run's own data exists yet.

## Why this iteration exists, and why it is a fresh baseline

Iteration 025's substantive endpoints were **uninterpretable**, and the reasons were the rig rather than
the mechanism:

* **LOCA's compaction never fired, on any iteration.** `clear_tool_uses` is an Anthropic API beta sent as
  a `context_management` parameter; the benchmark gateway shape-checks it and does not forward it. A bogus
  edit type returns 200, which the API would reject. So no declared band has ever been *enforced* — a
  band is a scale for computing pressure, never a constraint. See `docs/results/measurement-limits.md`.
* **The econ trigger fired where its question was unanswerable.** With no floor it asked at 4% context
  pressure, on a 2,621-token transcript, and removed nothing — correctly, nothing had been superseded.
  Those are the ask ledger's calibration samples, so `approval` floored at 0.05, which multiplies
  turns-to-repay by twenty and shut the component down. Same config, same tasks, same seed: one pass
  removed 62,525 tokens and another 90.
* **And its measurements could not be compared to iteration 024's** — different gate, different code,
  and iteration 024's cost figures were already withdrawn for the five accounting defects.

So **nothing carries forward as a baseline** and this is a fresh two-arm run. The only thing held constant
across iterations is the task set.

## The thesis, stated as a threshold ordering

`summarize` is lossy. Deferring it should preserve accuracy. The sweep's job is to remove spent tool
outputs so the request stays below `summarize`'s trigger — and the trigger reads the **post-pipeline**
request (`summarize` runs last, the pipeline threads one request through in order), so this is mechanical
rather than hopeful: anything upstream that shrinks the request stops `summarize` firing.

```
sweep may act from   0.70 of the window   (arm B only, extract_llm_sweep.min_pressure)
summarize fires at   0.90 of the window   (both arms, summarize.trigger.min_request_frac)
```

`summarize` stays **window-relative** deliberately: deferring it is the thesis, so pinning its trigger to
an absolute token count would destroy the thing being measured.

## Arms — two, differing by three lines

| arm | config | differs by |
|---|---|---|
| **A — baseline** | `cfg-iter026-A-baseline.yaml` | — |
| **B — merged** | `cfg-iter026-B-merged.yaml` | `evidence: true`, `econ_trigger: true`, `min_pressure: 0.70` |

Verified with `diff`: three added lines, nothing else. `min_inventory: 7` and `summarize` at 0.90 are in
**both**. `extract_llm_sweep` stays in arm A's pipeline but inert — `econTrigger` false means candidates
are never collected under benchmark load — because removing the component would change the pipeline shape
and add a second difference.

**A fresh arm A is required and this is the only reason:** `summarize` moving from 0.78 to 0.90 changes arm
A's behaviour. `min_pressure` and `min_inventory` do not (arm A never collects candidates). Had `summarize`
stayed at 0.78, iteration 024's arm A would have been reusable and this run would have cost ~$200 less.

## Scope and order

* `i024-64k-s{1..5}.json` — 15 tasks × 5 seeds × 2 arms = **150 runs**, 64k declared band.
* **Interleaved A then B within each seed**, so drift lands on both arms equally AND every completed seed
  pair is a full 15-task paired comparison.
* Estimated **~$385–410**, from iteration 025's measured $41/pass over 10 passes. An extrapolation.

## Endpoints

### Primary — what a user would notice

1. **Reward.** Paired per-task means over 15 clusters, two-sided exact sign test, α = 0.05. Fifteen
   clusters is adequate for a strong effect: iteration 024 returned p = 0.0078 at 8 better / 0 worse, and
   the bar clears at **6 better / 0 worse (p = 0.0312)**, 8-1 (0.0391) or 10-2 (0.0386). Seeds do not add
   clusters — they stabilise each cluster's mean, which is what makes the sign test worth running.
2. **Cost.** Total per task, and `extraction_cost_usd` per component with `cost_source`. Must read
   `component` or the cost reading is void.
3. **Latency.** `avg_latency_ms` per component, and wall clock split by run outcome.

### Mechanism — why the primary moved, not a substitute for it

Deferral is a **by-product**. It validates the thesis; it is not itself value. Deferral achieved with no
reward gain and higher cost accomplishes nothing, and will be reported as nothing.

4. **Deferral:** `summarize acted / runs`, arm B against arm A. Direct, already instrumented.
5. **Sweep operation:** `prefix_rewrite_repaid` against `econ_ask_not_repaid`,
   `prefix_rewrite_not_repaid`, `sweep_below_min_pressure`, `sweep_inventory_below_min`.
6. **The estimator's trajectory:** `approval` and `askUSD` per arm-seed, and what share of decisions ran on
   the warm-up prior. Iteration 025's failure was approval collapsing to its floor; this is where that
   shows.
7. **Summarize's own economics**, newly countable: `summary_checkpoint_reused` (free) against
   `summary_tail_past_threshold` (the intended refresh) and `summary_covered_span_changed` (upstream
   churn — read against the session's turns, never against zero; one firing per upstream change is
   expected).

### Reported, not vetoing

**The harm bound is a figure this time, not a blocker.** A Clopper-Pearson 95% upper bound on the worsened
proportion will be reported. It will **not** veto a positive claim, because at 15 clusters that rule passes
only on a perfect zero — 21.8% at 0 of 15, **31.9% at 1 of 15** — so a result of 9 better and 1 worse is
significant at p = 0.0215 and would be vetoed anyway. The blocking threshold is an open question about
acceptable risk and is deliberately deferred, to be settled on grounds independent of this run's numbers.
Recorded now so the deferral cannot later look like a rule chosen to fit a result.

## Checkpoints, and what may stop the run

Read after **every completed seed pair**, five times. Each is a full 15-task paired comparison:
directional, not significant, and never to be reported as significant.

| checkpoint finding | action |
|---|---|
| arm B's sweep fires a real share and `approval` stays off its floor | continue |
| **sweep fires ~never, or `approval` floors at 0.05 again** | **STOP.** The mechanism is not operating; this is iteration 025 repeating and no B−A is admissible |
| `cost_source` reads anything but `component` | **STOP.** Cost is unattributable; fix and restart |
| any non-zero HTML 400 at the capture hop | **STOP.** The shim is broken |
| direction favours B, or is flat | continue to the end regardless |

**Stopping is permitted for futility or mechanism failure only, never to claim a positive result early.**
Interim looks that can stop for success inflate the false-positive rate; interim looks that can only stop
for failure do not. If direction looks good at seed 2, that is not a result and the run continues.

## Frozen inputs

| | |
|---|---|
| binary | `cg-i026-proxy-v10`, SHA-256 (first 32) `84d20e23b651e22f6df8b87086ddbd37`, verified before every pass |
| code commit | `8568bce` |
| arm configs | `deploy/harbor/cfg-iter026-{A-baseline,B-merged}.yaml` |
| task configs | `i024-64k-s{1..5}.json`, unchanged since iteration 024 |
| rig | `~/cg-loca`, `stage022.sh` + `run026.sh` |
| gateway | benchmark key on the int litellm endpoint; **not** Context Guru |
| launched | *recorded at launch* |

Any code change before all ten passes finish invalidates the whole run. `run026.sh` checks the binary's
SHA before each pass and refuses on mismatch.

## Limits, stated in advance

1. **The 64k band is not enforced** and cannot be — see above. It is a pressure *scale*. Report the share
   of sweep decisions that fell inside it before treating the band as an independent variable.
2. **15 clusters.** Adequate for a strong effect, not for a subtle one. A null is a null, not a hint.
3. **Three settings changed at once in arm B** (`evidence`, `econ_trigger`, `min_pressure`) plus two in
   both arms (`summarize` 0.90, `min_inventory` 7). B−A is attributable to the arm-B three as a bundle,
   never to one of them individually.
4. **Trajectory variance is large.** One task ran 75, 32 and 16 steps across three probe passes on
   identical inputs. No single-pass figure is evidence about a configuration; this is why checkpoints are
   directional only.
5. **`min_inventory: 7` is a compromise, not a measurement.** The adjudicator acts better at larger
   batches (about 94% kept shown one output, 58% dropped at ~15), but every point of floor is a request
   that asks nothing, and 10 has never been shown reachable here.
6. **`/metrics` remains aggregate** (#180); nothing cross-checks against a Prometheus series.
