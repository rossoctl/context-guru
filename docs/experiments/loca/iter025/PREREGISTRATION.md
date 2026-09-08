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

---

## Amendment 1 — reuse iteration 024's arm A for four of five seeds (pre-launch)

**Recorded before launch, and before any iteration-025 number beyond the pre-flight is read.**

### The change

Arm A runs at **seed 1 only**. Seeds 2–5 pair arm B against **iteration 024's already-recorded arm A** on
the same task configs. 90 runs instead of 150.

Cost, recomputed from iteration 024's actual per-task spend rather than the pre-flight's single task —
which is the better estimator and the one this file should have used from the start:

| | runs | cost |
|---|---|---|
| iteration 024, as run | 150 | **$471.13** (mean $3.141/run, median $1.895, max $37.67) |
| iteration 025 as originally registered | 150 | ~$471 |
| **iteration 025 amended** | **90** | **~$301** (arm B 5 seeds ≈ $263 + arm A seed 1 ≈ $38) |

The original ~$460 estimate was right, but by luck: the pre-flight task's $3.070 happened to sit near a
mean of $3.141 drawn from a distribution whose median is $1.895 and whose maximum is $37.67. An n=1
extrapolation from a long-tailed distribution is not an estimate, and this replaces it.

### Why reuse is admissible, having checked rather than assumed

1. **The gateway difference is accounting, not routing.** Iteration 024 ran against
   `…vpc.res.ibm.com` and this run uses `…vpc-int.res.ibm.com`; @davidamid confirms both terminate at the
   same Anthropic endpoint and exist to separate billing inside the organisation. This was the objection
   that would have blocked reuse outright, and it does not hold.
2. **Iteration 024's arm A recorded no extraction activity at all** — `extract_llm` `calls: 0`,
   `cost_source: none`, `gross_saved_tokens: 0` across 378 and 316 requests. So there are no per-component
   cost or latency figures in arm A for the five accounting fixes to have invalidated. Its contribution is
   purely behavioural: reward, turns, wall clock, total cost.
3. **Every merged change is inert or accounting-only for arm A's active components.** #204, #205 and #208
   are accounting and labels. #216 is `extract_llm`-specific and `extract_llm` made zero calls in arm A.
   The ask charge and the pressure logging touch `econ_trigger`, which arm A does not enable. That leaves
   **#188**, which gave `commitMark` a refusal used by `cmdfilter`, `collapse`, `dedup`, `extract` — a real
   behaviour change in principle, but `stash_refused` has been **0** on all three pre-flights, so the
   reserve is never exhausted at this scale. Recorded as a limit rather than dismissed.
4. **Per-task pairing is possible.** `results.json` → `per_config` carries `avg_accuracy`, `avg_steps` and
   `avg_cost_usd` keyed by environment name, for all 15 tasks of every arm-seed.

### The drift check, declared in advance

One risk survives all of the above and cannot be tested retroactively: **six days of drift** behind the
`aws/claude-sonnet-5` alias. Seed 1's arm A is re-run to test it.

**Comparison is A-against-A on identical config, so the expected difference is zero.** Criterion, fixed
now: a **two-sided exact McNemar test on the discordant tasks** between `i024As1` and `i025As1`, α = 0.05,
plus mean per-task cost within ±30%.

| drift check | conclusion | next |
|---|---|---|
| not significant, cost within ±30% | no detectable drift; iteration 024's arm A is a valid control | pair seeds 2–5 against it as planned |
| significant, or cost outside ±30% | drift is real and the reused baseline is contaminated | run arm A at seeds 2–5 (+~$170); draw no B−A conclusion until it completes |

**The frozen target** — recorded here so it cannot be selected after the fact:

| task | avg_accuracy | avg_steps | avg_cost_usd |
|---|---|---|---|
| `ABTestingS2LEnv` | 0.0 | 49.0 | $4.47 |
| `AcademicWarningS2LEnv` | 1.0 | 97.0 | $10.44 |
| `ApplyPhDEmailS2LEnv` | 1.0 | 24.0 | $2.26 |
| `CanvasArrangeExamS2LEnv` | 0.0 | 14.0 | $2.04 |
| `CanvasListTestS2LEnv` | 0.0 | 14.0 | $1.31 |
| `CourseAssistantS2LEnv` | 1.0 | 15.0 | $1.05 |
| `ExcelMarketResearchS2LEnv` | 1.0 | 23.0 | $0.83 |
| `FilterLowSellingProductsS2LEnv` | 0.0 | 12.0 | $1.46 |
| `MachineOperatingS2LEnv` | 1.0 | 45.0 | $2.46 |
| `NhlB2bAnalysisS2LEnv` | 0.0 | 17.0 | $0.94 |
| `PayableInvoiceCheckerS2LEnv` | 1.0 | 15.0 | $0.42 |
| `SetConfCrDdlS2LEnv` | 1.0 | 10.0 | $3.60 |
| `UpdateMaterialInventoryS2LEnv` | 1.0 | 13.0 | $3.79 |
| `WoocommerceNewWelcomeS2LEnv` | 0.0 | 15.0 | $1.77 |
| `WoocommerceStockAlertS2LEnv` | 0.0 | 15.0 | $1.61 |

solved-equivalent (sum of avg_accuracy) = 8.000 of 15 tasks
summary: {"avg_accuracy": 0.5333, "avg_steps": 25.2, "total_cost_usd": 38.469868, "total_error": 0, "total_input_tokens": 6603311, "total_output_tokens": 499145, "total_success": 15}

### Limits this amendment adds

1. **The pairs are not homogeneous in time.** Seed 1's pair is same-day; seeds 2–5 pair against a
   six-day-old baseline. The drift check licenses that, it does not remove it, and any B−A result must say
   so.
2. **#188's refusal path exists in arm B's binary and not in the reused arm A's.** Inert at
   `stash_refused: 0`, and that counter must be reported for arm B to show it stayed inert.
3. **Latency comparisons across the reused seeds carry the gateway hop's difference**, even with the same
   Anthropic endpoint behind it: the two litellm deployments are different hosts. Seed 1 is the only clean
   latency pair, so the latency endpoint effectively falls to n=15 unless the full baseline is run.
4. If the drift check fails, the amended run costs **more** than the original design would have
   (~$301 + ~$170 = ~$471, plus a re-analysis). That is accepted deliberately: the check buys a falsifiable
   answer about drift, which the original design assumed away.
