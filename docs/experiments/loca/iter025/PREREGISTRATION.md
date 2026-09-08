# Iteration 025 — pre-registration: does the reward result replicate once every measurement gate is closed?

**Written before the run is launched, and before any iteration-025 number beyond the pre-flight is
read.** The pre-flight numbers that are quoted below are quoted deliberately and are all that has been
seen; every endpoint here is declared against them rather than after them. Any conclusion drawn from a
number read earlier than this file's commit is inadmissible.

## Why this iteration exists

Iteration 024's headline was a **reward** finding — 8 tasks better, 0 worse, clustered sign test
p = 0.0078 — and its cost and latency figures were **withdrawn**, because five separate defects meant
they measured something other than what they claimed. All five are now closed and merged to `main`:

| defect | what it made unreadable | closed by |
|---|---|---|
| dangling marker after a refused stash | removals recorded that never happened | #187 → #188 |
| pooled per-component accounting | `calls`, `avg_latency_ms`, net value summed two components | #190 → #204 |
| `$0` cost on the sweep's own ledger row | the component whose case is cost looked free | #200 → #205 |
| `marker_or_kept_verbatim` conflated two causes | expansion could not be separated from marker presence | #201 → #208 |
| `saved` not wired to the wire | reported savings were not the ones that shipped | #195 → #216 |

Merging them into this branch (`d62962f`) exposed a **sixth**, in the branch's own code: `coref` ignored
`commitMark`'s return and spliced plus froze regardless, which since #188 would stamp a marker pointing
at nothing. Fixed, and `coref` is now driven by `TestNoStateIsRecordedBeforeTheCommitGate` rather than
exempt from it.

**And the pre-flight found a seventh, which is the substantive change this iteration measures.** The econ
trigger's break-even charged the cache-write a removal forces and **never the model call that decides what
to remove**. Measured on the pre-flight: 9 asks authorised out of 9 offered, six of which removed nothing,
$0.4339 spent against $0.0017 of value. `2285a5f` makes the ask a term and discounts the candidate mass by
a measured approval rate.

So iteration 024's reward result was obtained through a gate that authorised every ask put to it. **This
iteration asks whether that result survives a gate that declines.**

## Arms — two

| arm | pipeline config | differs by |
|---|---|---|
| **A — baseline** | `cfg-iter023-A-baseline.yaml` | — |
| **B — merged** | `cfg-iter023-B-merged.yaml` | `extract_llm_sweep: {evidence: true, econ_trigger: true}` |

**Configs are byte-identical to iterations 023 and 024.** The code changed; the configuration did not.
That is what keeps B−A attributable and what allows a comparison against iteration 024 at all.

**A third arm is deliberately NOT run, and this is a cost decision rather than a scientific one.** An arm
at `econ_ignore_ask_cost: true` would attribute this iteration's difference specifically to the ask
charge, at +$230 and +50% wall clock. The charge is already evidenced by unit tests, ten mutations, and a
pre-flight in which the estimator converged to within 3% of an independently measured figure. The knob
ships, so that arm can be run later against the same binary with no rebuild if the results make the
attribution worth buying.

## Scope

* `i024-64k-s{1..5}.json` — **all five seeds** × 15 tasks × 2 arms = **150 runs**.
* Interleaved by arm within each seed, so drift lands on both arms equally.
* `INJECT_EXPAND=always`, `CACHE_MODE=on`, 64k declared window, LOCA clearing at 64k, `--max-workers 8`.
* **Estimated ~$460**, from the pre-flight's measured $3.070/task. An extrapolation from **n=1 in the
  more expensive arm**, not a measurement — iteration 024 estimated $170 on iteration 008's $1.13/task
  and that figure is now known to be 2.7x low.

## Endpoints

### Primary — does the reward result replicate?

Paired per-task means over 15 clusters × 5 seeds, **two-sided** exact sign test, α = 0.05, with
improved / worsened / unchanged counts reported. **A Clopper-Pearson 95% upper bound on the proportion of
worsened pairs above 25% blocks any positive claim**, whatever else moved. No minimum effect size is
claimed as a win; a null at this n is "underpowered", not "no effect".

Iteration 024 reported 8/0/7 with a harm bound of 11.2%. **Replication means the same direction with the
harm bound still clear, not the same p-value.**

### Secondary — cost and latency, now that the gate declines

1. `extraction_cost_usd` and `avg_latency_ms` **per component**, with `cost_source`. Must read `component`
   for both components or the cost reading is void (see admissibility).
2. **The gate's own behaviour**: econ decisions, authorised vs declined, and declines split by
   `econ_ask_not_repaid` vs `prefix_rewrite_not_repaid`. Pre-flight: 20 decisions, 12 fired, 8 declined,
   **all 8 on the ask**.
3. **The ledger's convergence**: final `askUSD` and `approval` per arm-seed, and what share of decisions
   were still on the warm-up prior. Pre-flight: $0.04984 and 0.328, with 3 of 20 on the prior.
4. Turns to completion and wall clock, **split by run outcome** — iteration 023's turn saving sat on the
   *failure* path, and that split must be re-tested rather than assumed.

### Recorded, not tested — the evidence #229 needs

Firing pressure distribution via `deploy/harbor/sweep_when.py`, plus how many firings a 30/50/70% floor
would have blocked. **No hypothesis is registered about it.** It is collected because #229 cannot be
decided without it and this run produces it for free. On LOCA the answer is already expected to argue
against a floor — every pre-flight ask fired between 13.9% and 29.3% — and a LOCA-only answer does not
settle a question raised about long coding sessions.

## Two questions declared open in advance, so neither becomes a headline by default

**1. The value model is unsettled, and this run does not settle it.** `net_value_usd` prices the saving
**once**, at the cache-read rate, while the mechanism's own thesis is that a removal is collected on every
remaining turn (S·T). On the pre-flight those two disagree about the SIGN: −$0.581 one-time, versus
≈+$0.04 under S·T at the observed T≈120. **Both will be reported, neither as the headline**, with T stated
explicitly, because a derived figure whose sign depends on a modelling choice is not a result. Deciding
which is right is its own work and is not in this run's scope.

**2. Latency roughly doubled between pre-flights and is unexplained.** 27,459 ms per sweep call against
13,214 ms, on the same task config and the same 64k band. I have no mechanism for it and will not offer
one. If it holds at n=150 it is the next design question; if it does not, the two pre-flights differed in
something not controlled (their transcripts grew at very different rates — `have` 113–149 versus 38–53).

## Admissibility — what voids this run

Declared now so no result can be rescued after the fact:

1. **`cost_source` anything but `component`** on either component voids every cost conclusion.
   `unpriced_components` then names the culprit. **Check on the first completed arm-seed, not at the end.**
2. **Any non-zero HTML 400 count** at the capture hop voids the run — that is the shim fix, and it has
   held on all three pre-flights (59/59 and 53/53 status 200).
3. **Zero sweep firings in arm B** means the charged gate is strangling the component, which is a knob
   problem and not a result. Report it, do not publish B−A.
4. **Any code change before all ten arm-seeds finish invalidates the whole run**, not one arm.
5. `cheap_model_price_unconfigured` must stay at 0 — `CHEAP_MODEL_PRICE_*` is exported by `stage022.sh`.

## Pre-registered reading

| outcome | conclusion | next |
|---|---|---|
| reward replicates (same direction, harm bound clear) **and** the gate declines a real share | the mechanism helps, measured through a gate that can say no | write it up; take PR #80 out of draft |
| reward replicates, gate declines **nothing** | inadmissible for B−A per (3): the gate is not being exercised | tune the estimator, re-run; draw no conclusion |
| reward null, harm bound clear | underpowered or the effect was iteration 024's noise | report as a null; the 5-seed design is the ceiling available at this band |
| reward worse, or harm bound > 25% | **blocked** | no positive claim; the ask charge may be declining asks that were earning their keep — run the `econ_ignore_ask_cost` arm to find out |
| cost: net positive under **both** value models | the mechanism pays, unambiguously | the strongest result this branch could produce |
| cost: net sign differs by value model | as expected from the pre-flight | report both; the value model becomes the next piece of work, not a conclusion |
| latency gap holds at n=150 | the ask is the expensive leg and the batch/latency trade is the next design question | size it before proposing a fix |

## Frozen inputs

| | |
|---|---|
| binary | `cg-i025-proxy-v03`, SHA-256 (first 32) `fc42520260d70e834bc2adac94372974` |
| code commit | `5d58575` — `feat/coref-recut`, `origin/main` merged at `d62962f` |
| arm configs | `deploy/harbor/cfg-iter023-{A-baseline,B-merged}.yaml`, unchanged since iteration 023 |
| task configs | `i024-64k-s{1..5}.json`, one per seed, 15 tasks each — same as iteration 024 |
| rig | `~/cg-loca` (off `/tmp`), `stage022.sh` + a `run025.sh` mirroring `run024.sh` |
| analysis | `deploy/harbor/sweep_when.py`, `armstats.py` |
| gateway | benchmark key + int litellm endpoint; **not** Context Guru |
| launched | *to be recorded at launch* |

## Limits, stated in advance

1. **Arm configs carry `min_inventory: 3` and sweep `min_tokens: 100`**, neither the shipped default.
   Identical across arms so neither can bias B−A, but **B is not measuring a shippable configuration**.
   Deliberately unchanged despite #229 observing that `min_inventory` is the free early-session filter:
   raising it now would confound with the gate change.
2. **The ask ledger is process-wide**, so tasks later in an arm-seed inherit an estimate the earlier ones
   paid to learn. That is intended, but it means the first ~3 asks per proxy are priced optimistically and
   the arm is not homogeneous in time. `estFromMeasurement` makes the share visible.
3. **`extract_llm` is unpinned**, so arm A is not `housellm` verbatim and **no comparison with iteration
   022's arm A is admissible**.
4. **The pre-flight is n=1 per configuration.** Every figure quoted from it — approval 0.328, the pressure
   range, the latency gap — is a single sample on a benchmark that has produced 0.200 and 0.800 on
   identical config.
5. **`/metrics` remains aggregate** (#180); nothing here cross-checks against a Prometheus series.
6. **Reward is scored by LOCA's own task solving**, which is blind to nothing but is also low-resolution:
   ≤75 pairs separates only large effects.
