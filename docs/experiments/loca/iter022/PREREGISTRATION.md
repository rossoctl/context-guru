# Iteration 022 — pre-registration: does anything on the coref branch move a shipped baseline?

**Written before the run.** Stage 0 of two. This iteration deliberately does **not** try to prove a
reward effect; it asks whether the branch's mechanisms *fire at all* against the config the service
actually ships, and measures the endpoints where iteration 021 showed real movement.

## What iteration 021 established, and what it did not

Stating this first because it decides what this iteration can claim.

* **It was a null on its primary endpoint.** Clustered p = **1.0000** over 15 clusters, 14.00 vs 15.00
  ITT solves, at **+2%** total cost. The pre-registered reading for exactly that outcome was *"close
  merged; keep the deterministic pipeline; report the ceiling honestly."*
* **`coref` was in neither arm.** Its own pre-registration says so. Nothing has ever measured the
  co-reference mechanism in a reward arm.
* **What did move was operational**, and by a lot: errored runs 17 → 10, `summarize` 71% → 56%,
  requests 3,446 → 2,466 (**−28%**), 6.5M unique tokens removed with 717 restores and **0** unresolved.
* **The reward comparison was underpowered by construction.** Eleven of fifteen tasks scored **zero in
  both arms** at 128k, so the comparison rested on four tasks with two moving each way.

So "replicate the result" means the **operational** result. Reward is carried here as a harm gate only.

## The three arms — alternatives, not a stack

One binary, one task set, one seed set. Configs committed before launch.

| arm | differs from A by | tests |
|---|---|---|
| **A — baseline** | — | `housellm` (the service config) + `summarize` + `collapse` |
| **B — merged** | `extract_llm_sweep: {evidence: true, econ_trigger: true}` | PR #80's merged design: index as EVIDENCE, model keeps the veto |
| **C — cutter** | `+ coref` component, after the sweep | the zero-LLM deterministic cutter |

**Why B and C are not combined.** `coref` cuts and leaves a marker; the sweep skips marked content
(`empty_or_marker_present`). Upstream, it would hide its cuts from the model — structurally the
`prefix_still_referenced` thinner that removed 149,681 candidates, left about one per request, and
silently turned a bulk arm into the per-output shape refuted at 6% live-kept. Evidence and cutting are
two philosophies for one job. **B vs C is the interesting comparison** and it is genuinely open: the
offline scoring made the index the best discriminator of ten arms, but its ground truth was Tier-1
exact matching, blind to precisely the transformed reuse the model in B is there to catch.

## Band: 32k, not 128k

128k left 11/15 tasks at zero in both arms — *"no configuration change could have produced a detectable
difference."* Iteration 008 settled the band: **52.7%** solve rate at 32k over all 75 configs against
**33.3%** at 64k, and a paired signed-rank test's power is maximal near a 50% base rate. 32k still
builds 45–56k-token contexts, so there is material to act on. Per-run cost there was **$1.13**.

The trade-off, stated: less context means less to remove, so **absolute savings will be smaller at 32k
than at 64k**. Savings are a 64k/128k question; this is the reward-and-behaviour band.

## Scope

* `task-configs/final_32k_set_config.json` is 15 tasks × 5 seeds (42, 123, 456, 789, 2024) = 75 configs.
* **Stage 0 runs seed 42 only — 15 runs per arm, 45 total.** Iteration 008 validated single-seed
  probing: 52.7% over all 75 configs against 53% on `state0` alone.
* Estimated cost **$50–80** at $1.13/run plus our own model spend. An extrapolation, not a measurement.
* `INJECT_EXPAND=always`, `CACHE_MODE=on`, 32k declared window, LOCA clearing at 32k, `--max-workers 8`.

## Endpoints

**Primary — high-n, and the reason this iteration exists.** These have n in the thousands of requests
or the tens of runs, not 15 clusters:

1. **Turns to completion and wall-clock, SPLIT BY RUN OUTCOME.** Iteration 021's −28% requests is the
   strongest signal it produced, but its own text attributes it to *"fewer runaway sessions burning
   turns"* and arm B also had 7 fewer errored runs. A runaway burns many turns before dying, so the
   drop may be "failed less" rather than "solved faster" — and an end-user latency claim needs exactly
   that distinction. Splitting by outcome is what iteration 021 could not do.
2. Requests, errored runs, unique tokens removed, expand restored / **unresolved**, total cost.

**Mechanism — the gate on Stage 1.** A mechanism that does not fire cannot be worth a paid reward arm,
and three iterations were lost to machinery that never fired being read as "the model declines to act":

3. `prefix_rewrite_repaid` vs `prefix_rewrite_not_repaid` — does trigger two fire outside the
   pre-expiry window, and how often? **If it never fires, arm B's econ half is untestable and no
   reward arm can detect it.**
4. Sweep **coverage** (verdicts answered / candidates offered) against iteration 021's 61%. That
   iteration's own conclusion names a *smaller* batch as the untested lead — a probe answered 6 of 6
   on six candidates against ~61% on twelve.
5. `evidence_no_index_record` — how often the index has no opinion on a candidate it was asked about.
6. Whether `coref`'s `min_batch_frac` gate ever clears at 32k contexts (0.05 admitted 16/19 sessions
   when measured, at a different band).
7. **The warm tail path's net at `min_tokens: 3000`**, which resolves #120 with data.

**Reward — a harm gate, not a headline.** Paired per-task means over 15 clusters, two-sided signed-rank,
α = 0.05, reported with improved/worsened/unchanged counts. **A Clopper-Pearson 95% upper bound on the
proportion of worsened pairs above 25% blocks any positive claim**, whatever the point estimate does —
iteration 007's failure. No minimum effect size is claimed as a win: at this n the honest reading of a
null is "underpowered", not "no effect". Previous harm bounds reached ±39%.

## Two defects the baseline carries on purpose

Both are in the **shipped** config, identical across all three arms, so neither can bias B−A or C−A.
Recorded because a reader would otherwise take them for mistakes in this file.

* **#134 — pinning `min_tokens`/`trigger` disables the pressure trigger.** `shouldFire()` returns
  `true, "explicit min_tokens/trigger configured"` unconditionally, and `housellm` pins both, so
  `extract_llm`'s tail pass fires on **every request**. Beyond cost, it has a specific hazard for arm
  B: the tail pass leaves markers, the sweep skips marked content, so a pass firing every turn can
  progressively **starve the sweep**. Whether it actually does is the pre-flight's gate below.
* **#120 — the tail floor is 3000 where its own comment derives 8000.** Below 8000 that path is
  measurably a loss (at 1000: 5 warm calls, 4 accepted, 8,718 tokens saved for $0.0771 — net
  −$0.036). Left at what ships, and measured (endpoint 7) rather than guessed.

## Pre-flight gate — this run does not start until it passes

The venv was rebuilt with `--no-deps`, so pip has not verified the dependency graph, and the two
failures that wasted passes on this box *look like a result rather than a broken rig: tasks run,
requests flow, numbers come out*. One task, one seed, arm B, and all four must hold:

1. a **real MCP tool call** succeeds (not merely that the server started);
2. at least one **non-zero component action** in `/stats`;
3. **zero** HTML 400s at the capture hop;
4. the sweep is **offered candidates** — if `empty_or_marker_present` accounts for everything, #134 has
   starved arm B and the arm is redesigned before spending, not after.

## Pre-registered reading, written before the numbers exist

| outcome | conclusion | next |
|---|---|---|
| turn/latency gain holds among **successful** runs, mechanisms fire | iteration 021's operational win replicates on a shipped baseline and is attributable | Stage 1 reward arm, 5 seeds, on whichever of B/C moved |
| turn/latency gain is only among errored runs | the −28% was "failed less", not "solved faster" — the latency claim is not established | keep `collapse`; re-ask with error rate controlled |
| mechanisms fire but nothing operational moves | the machinery works and buys nothing at this band | do **not** buy a reward arm; report the ceiling |
| a mechanism never fires | untestable, not refuted | fix the trigger or the floor first; no reward arm |
| harm upper bound > 25% in any arm | blocked | no positive claim, whatever else moved |

## Frozen inputs

Recorded before launch. Any code change before all three arms finish invalidates all three, not one.

| | |
|---|---|
| binary | *(filled at launch)* |
| SHA-256 | *(filled at launch)* |
| code commit | *(filled at launch)* |
| arm A config | `deploy/harbor/cfg-iter022-A-housellm.yaml` |
| arm B config | `deploy/harbor/cfg-iter022-B-merged.yaml` |
| arm C config | `deploy/harbor/cfg-iter022-C-coref.yaml` |
| task config | `final_32k_set_config.json`, seed 42 subset (15 runs/arm) |
| rig location | `~/cg-loca` — moved off `/tmp`, which is on a 10-day cleaner that has already destroyed one iteration's frozen binary |
