# Iteration 022 — results

**Status: COMPLETE.** 9 passes, 45 runs, 3 arms × 15 tasks (seed 42) at 64k. **Zero** errored runs and
**zero** HTML 400s in every arm — the first iteration in this series with a clean error column, which
matters because errored runs score 0 in ITT and had been the dominant noise term. That came from adding
`collapse`, and §4b records what it cost: `summarize` never fired, so iteration 021's deferral endpoint
is not measurable here.

## Frozen inputs

| | |
|---|---|
| binary | `cg-i022-proxy-v01`, 40,910,697 bytes |
| SHA-256 (first 32) | `d5bb95582c9893da408de06e4e872f5d` |
| code commit | `143cf73` |
| arms | `cfg-iter022-{A-housellm,B-merged,C-coref}.yaml` |
| task config | `final_64k_set_config.json`, seed 42, in 3 interleaved passes of 5 |
| rig | `~/cg-loca` (off `/tmp`), `deploy/harbor/{stage022,run022}.sh` |
| environment | `INJECT_EXPAND=always`, `CACHE_MODE=on`, 64k declared window, clearing at 64k, `--max-workers 8` |

## 1. The mechanism gate — PASSED, which is this iteration's real product

The pre-flight found both econ counters at **0/0**: the trigger was not refused, it was never *reached*.
After Amendment 1 it fires, and it repays every single time:

| | Bp1 | Bp2 | Bp3 | total |
|---|---|---|---|---|
| `cg.sweep.econ` evaluated | 23 | 5 | 2 | **30** |
| `prefix_rewrite_repaid` | 23 | 5 | 2 | **30** |
| `prefix_rewrite_not_repaid` | 0 | 0 | 0 | **0** |
| `cg.sweep.ask` (real adjudications) | 23 | 5 | 2 | **30** |
| `sweep_unparseable` | 0 | 0 | 0 | **0** |
| `evidence_no_index_record` | 0 | 0 | 0 | **0** |

`coref` acted **391** times in arm C (146 / 183 / 62), so `min_batch_frac: 0.05` does clear at 64k.

**Arm A is a genuinely inert control**: `econ=0`, `asks=0` in all three passes. `extract_llm_sweep` is in
the shipped `housellm` pipeline and, under benchmark load, *never runs* — `not_in_pre_expiry_window`
fired on every request, because eight workers never leave an idle gap for a cache entry to approach
expiry. So B−A is not "a better sweep versus a sweep"; it is **"a sweep that runs" versus "a sweep that
cannot"**. That is worth stating plainly as a property of the shipped configuration under continuous
load, quite apart from this branch.

Two incidental measurements, both firsts:

* **`sweep_answered_via_tool: 0`, `via_prose: 30`.** The verdict tool PR #137 added is offered on every
  ask and the model never once used it; it answers in prose every time. #137 added these counters
  precisely to detect this, and this is their first live data.
* The econ trigger's offline feasibility estimate held: 3–4× margin predicted from the capture,
  **30/30 repaid** observed. The estimate was not merely directionally right.

## 2. Primary endpoint — the latency claim does NOT replicate

This is the endpoint iteration 021 could not produce, and the reason it was built: turns per run,
**split by run outcome**.

| arm | turns, all runs | turns, SOLVED | turns, UNSOLVED | `cg_added_ms_avg` |
|---|---|---|---|---|
| **A** baseline | 20.7 | **20.1** | 21.1 | 203 |
| **B** merged | **18.9** (−9%) | **19.8** (−1.5%) | 18.5 (−12%) | **1,396** |
| **C** coref | 26.1 (+26%) | 25.5 (+27%) | 26.7 (+27%) | 353 |

**Arm B's turn reduction is almost entirely among runs that FAILED** (but see §2b: this arm is confounded, and the reduction is not attributable to the treatment). Among solved runs it is 20.1 →
19.8, which is 1.5% and inside noise; among unsolved runs it is 21.1 → 18.5. B also solved *fewer*
tasks (5 vs 7). So the shape of B's "efficiency" is **giving up sooner on tasks it does not solve**, not
solving faster — precisely the alternative reading pre-registered for this outcome.

That resolves the open question from iteration 021 directly. Its headline −28% requests was read as a
latency win; its own text attributed it to *"fewer runaway sessions burning turns"*, and it could not
distinguish the two. Measured properly here, with the split it lacked, **the win is on the failure path.**

And on wall-clock the direction is worse, not merely absent: B adds **1,396 ms per request** of proxy
time against A's 203 ms — about **1.2 s of added latency on every request** — while buying 1.5% fewer
turns on the runs that matter. B's own model spend is 962k input tokens against A's 92k, roughly 10×.

**Arm C moved the other way**: +26% turns, and 62% more LOCA cost ($55.35 vs $34.06). Its median is 17
against A's 19, so the mean is carried by a long tail — a few runs that went much longer.

## 2b. ARM B IS CONFOUNDED — B−A does not isolate the treatment

Raised by David on reading §1–§2, and it invalidates the attribution in §2 rather than qualifying it.

**The sweep acted 11 times.** 30 econ firings, 30 asks, 132 candidates offered, 132 verdicts, and only
**11 asks removed anything** (37%) for 187,472 tokens. Eleven removal events across 284 requests and 15
runs cannot carry a −2 solve difference or a turn profile.

**And a much larger uncontrolled change sits in the same arm.** `extract_llm` acted **108** times in B
against **15** in A — 38.0% of requests against 4.8% — saving 1.19M tokens against 0.32M, on a
byte-identical config:

| act rate / request | A | B | C |
|---|---|---|---|
| `extract_llm` | 4.8% | **38.0%** | 9.5% |
| `extract_llm_sweep` | 0% | 3.9% | 0% |
| `collapse` | 62.7% | **33.5%** | 54.2% |

`extract_llm` runs BEFORE `collapse`, so it — not the sweep — is what left `collapse` half as much to do
(collapse's truncation savings fell 3.10M → 1.49M). **The collapse reduction is real and is a deferral of
exactly the wanted kind — blunt head/tail truncation displaced by model-guided compaction — but it is not
attributable to `evidence` or `econ_trigger`.**

Why extract_llm's activity swung 7× on an identical config is **unexplained**. It cannot be the pressure
trigger, which #134 forces on in both arms. The plausible route is trajectory divergence seeded by the
sweep's 11 removals, but a 93-event downstream swing from 11 upstream events is not something this run's
aggregate counters can establish.

**Consequence for §2.** The turn figures there are correct as measurements, and the outcome split is
still the right way to read them. But attributing B's turn profile to the merged design is NOT supported:
the arm differs from A in two ways, and the larger one is not the treatment. A clean B−A needs
`extract_llm` controlled — most simply removed from all arms, since #134 makes it fire on every request
regardless.

## 2c. What IS attributable, and one anomaly worth chasing

**Coverage: 132 of 132 verdicts, 100%, at mean batch 4.4.** Iteration 021 measured **61% at batch 12**
and its conclusion named a smaller batch as the untested lead, on a probe that answered 6 of 6 at six
candidates. Coverage is a property of the ask itself, so unlike the reward and turn numbers it does not
depend on trajectory.

**But the batch size is a CONFIG choice, not a property of the band, and that partly confounds the
coverage result.** The cap is `maxAskItems = 12` here and was 12 in iteration 021, so the difference is
candidate availability, and availability at 64k is set by the sweep's floor. Measured on arm B's own
traffic (6,708 tool outputs, 23.6 per request):

| sweep `min_tokens` | candidates per request | total candidate tokens/request | mean candidate size |
|---|---|---|---|
| 3000 | 2.7 | 35,278 | 13,097 |
| **1000** (what this ran, "as shipped") | **4.5** — matches the observed 4.4 | 38,077 | 8,370 |
| 600 | 6.1 | 39,294 | 6,439 |
| 300 | 8.6 | 40,386 | 4,699 |
| **100** | **11.6** — at the cap | **41,001** | 3,530 |

**The count and the mass move at completely different rates, and that decides what a lower floor is
FOR.** `min_tokens` is a per-output bar, not a budget, so lowering it can only admit more outputs --
monotonically, since the set at 100 is a strict superset of the set at 1000. But the outputs it newly
admits are by definition the smallest ones: the 2,007 additions between 100 and 1000 average **414
tokens** against **8,370** for those already in. So 1000 -> 100 is **+155% candidates for +8% mass**.

In a prefix ask the model reads each output from cache, so the marginal wire cost of naming one more
candidate is one inventory line, on the order of thirty tokens -- not the output. A lower floor therefore
buys **peers to compare against** almost free, which is the axis this mechanism lives on (6% live-kept at
a batch of one against 58% at ~15). What it does NOT buy is removable mass, and a dropped 414-token
output leaves a descriptor behind, so its net recovery is small. Same mechanism
`docs/results/min-tokens-vs-economic-gate.md` records from the economic gate's side.

So 64k does supply a batch of ~12; the 1000-token floor excluded it. Iteration 021 reached ~12 because
its `extract_llm` ran with `min_tokens` UNPINNED, letting the derived pressure floor fall low. **The
honest statement is therefore "coverage was 100% at batch 4.4", not "the smaller batch fixed coverage"**
-- batch size and the rest of the configuration moved together. Lowering the floor to **100** (300 reaches only 8.6) and
re-measuring coverage at batch ~12 is a one-config-line experiment and is the single most informative
cheap follow-up this iteration suggests.

**Latency is the sweep, and it is 15.2 s per ask.** `extract_llm_sweep.duration_ms` is 454,661 ms in B
against 35 ms in A — 1,601 ms per request once amortised over 284, which accounts for the whole
`cg_added_ms_avg` gap (203 → 1,396). Concentrated, not spread: 30 requests paid ~15 s each. And 19 of
those 30 asks returned "keep everything", so roughly 290 s of it bought nothing.

**The anomaly: 40% of every ask is paid FRESH.** Mean input per ask is 42,409 cache-read **plus 28,764
fresh**. A prefix ask appends a question to a byte-identical prefix, so fresh should be little more than
the question itself. The likely cause is self-inflicted — each removal mutates the prefix, so the next
ask no longer matches what the provider cached — which would make the mechanism progressively more
expensive the more it succeeds. This is the design's central economic premise and it is not behaving as
designed. It deserves an issue of its own.

At list price the 30 asks cost **$3.44** (cache-read $0.38 + fresh $2.59 + output $0.47) to remove
187,472 tokens, i.e. 54,465 tokens per dollar. B's end-to-end LOCA spend was still lower than A's
($27.49 vs $34.06), so total cost fell ~$3 — but at 5 solves against 7 that is **$6.19 per solve against
A's $4.88**.

## 3. Reward — both arms blocked by the pre-registered harm gate

Accuracy here is 0/1 on every task, so the signed-rank test reduces to an exact sign test.

| | net solves | improved | worsened | unchanged | exact two-sided p | CP 95% upper bound on worsened |
|---|---|---|---|---|---|---|
| **B vs A** | **−2** | 1 | 3 | 11 | 0.6250 | **48.1%** — blocks |
| **C vs A** | **+1** | 2 | 1 | 12 | 1.0000 | **31.9%** — blocks |

Totals: A **7.00/15**, B **5.00/15**, C **8.00/15**.

**Neither arm may claim a positive reward effect**, per the bound declared before the run. And note the
structural fact this exposes, which is more useful than either p-value: at n=15 with a single seed, the
best achievable bound is **21.8%** — that is with *zero* worsened pairs. One worsened pair puts it at
**31.9%**. So this design can essentially never support a positive claim, whatever the effect. That is
not a property of these mechanisms; it is a property of running 15 pairs, and it is the argument for
five seeds (n=75) in Stage 1 rather than a wider task set.

## 4. A defect worth an issue, not a footnote

**`expand_unresolved_missing: 60` in Cp1**, against 0 in every other pass and both other arms — 60 of
that pass's 112 expand calls could not be resolved. Cp2 and Cp3 are clean, so this is not a uniform
property of `coref`. But Cp1 was also arm C's worst pass (1/5 solves against A's 2/5), and an
unresolvable expand means the agent asked for removed content back and did not get it, which is the one
failure the "every lossy Offload must be reversible" invariant exists to prevent. The correlation
between the two is unexplained and is the first thing to chase.

## 4b. `summarize` never fired — the deferral endpoint is dead, and `collapse` is why

**summarize acted 0 times in 986 requests**, across all three arms: 311 / 284 / 391 runs, every one
`verdict: declined`, and it appears in `top_passthrough` in all nine snapshots. So iteration 021's
clearest operational result — summarization falling from 71% to 56% of requests — **cannot be measured
here at all**. It was kept in the baseline specifically to make that measurable, and it is inert instead.

**Not the trigger.** 129 of arm A's 311 requests exceeded `min_request_frac: 0.78`, and requests reached
228k–453k tokens (the declared 64k window governs the proxy's pressure maths; the real model window is
far larger, which is also why there were no 400s). summarize was eligible and declined anyway.

**The likely cause is `collapse`, which this iteration added.** What acted, per arm:

| arm | components that acted |
|---|---|
| A | **collapse 195**, format 163, dedup 52, toon 16, extract_llm 15, searchfold 6 |
| B | format 141, extract_llm 108, **collapse 95**, dedup 54, extract_llm_sweep 11 |
| C | **collapse 212**, format 202, coref 78, extract_llm 37, dedup 27 |

Iteration 021 had no `collapse` and summarize fired on 71% of requests. `collapse` is documented as "the
last-resort catch-all for anything still oversized"; it takes the big outputs first and leaves a marker,
and summarize skips marked content while protecting `keep_last`. By the time it runs there is nothing
oversized left.

**So the two results reported above are the same intervention, and the trade must be stated as one.**
Adding `collapse` bought the clean error column — 0 errored runs and 0 HTML 400s, against iteration 021's
17 and 10 — *and* it cost the summarize endpoint. In iteration 021 those oversized single-line outputs
became runaway 400s; here they are capped before summarize sees them. `collapse` was chosen as a shared
control on the grounds that being identical across arms means it cannot bias B−A or C−A. That reasoning
holds and is not the error. The error was not checking whether it left summarize any substrate.

**This is a hypothesis, not a measurement.** The component records carry no gate field, so `declined` is
unattributed — the decline reason is not in this run's data. Confirming it needs gate-level logging for
summarize, or a fourth arm identical but for `collapse`. Until then the honest statement is: summarize
was inert in all three arms, `collapse` acted heavily in all three, and iteration 021's only comparable
run had `collapse` absent.

## 5. Reading against the pre-registration

The pre-registered table anticipated this row: *"turn/latency gain is only among errored runs → the
−28% was 'failed less', not 'solved faster' — the latency claim is not established → keep `collapse`;
re-ask with error rate controlled."* Error rate **was** controlled here (0 errored runs, 0 400s), and
the gain still sits on the failure path. So the conclusion is stronger than "not established": on a
clean-error run, arm B's turn saving is a *failure-path* artifact.

**What this iteration establishes**

1. Both new mechanisms work as built. The econ trigger reaches and repays (30/30); evidence reaches
   every candidate; nothing is unparseable. The branch's machinery is not broken.
2. `extract_llm_sweep` as shipped never fires under continuous load. Its only trigger needs an idle gap.
3. The end-user latency claim does not replicate. B's turn reduction is on failed runs, and it costs
   1.2 s per request.
4. LOCA at 15 pairs cannot license a reward claim in either direction.

**What it does not establish.** Nothing about reward, in either direction — B's −2 and C's +1 are both
null (p = 0.625, p = 1.000). "Underpowered" remains the honest word, not "no effect".

## 6. Limits

1. **One seed.** 15 pairs, and the harm bound floor is 21.8%. Cross-pass comparison within an arm is
   meaningless — arm C scored 0.200 on pass 1 and 0.800 on pass 2 with identical config, purely from
   chunk difficulty. Only the within-pass paired structure is interpretable, which is why the runner
   interleaved by arm.
2. **`min_inventory: 3`, not the shipped 10.** Identical in all three arms so it cannot bias B−A or
   C−A, but B is not measuring the shipped floor. At batch 3–6 the model is measured *unwilling to
   act*, so B's small-batch behaviour may understate what a larger inventory would do — and on this
   benchmark a larger inventory is unreachable at any floor.
3. **B bundles two changes.** `evidence` and `econ_trigger` are both on, and since arm A's sweep never
   fires, B−A cannot separate "the sweep ran at all" from "the evidence improved it". Splitting them
   needs an arm with `econ_trigger` on and `evidence` off.
4. **64k, not 32k.** Reward headroom is better at 32k (52.7% vs 33.3%), but the mechanism cannot be
   reached there. That tension is the subject of the next section.

## 7. What Stage 1 should be — and the question that now outranks it

The obvious Stage 1 is 5 seeds at 64k on arm C (the only arm with a positive point estimate), which
would take n to 75 and make the harm bound clearable. It is worth roughly $170 at the $1.13/run this
band cost.

**But a prior question has been sharpened by this run and should be settled first.** The band where
these mechanisms can act is not the band with reward headroom:

* at **32k**, the sweep cannot reach its trigger at all (measured: inventory never reaches even 3 at the
  shipped floor, and the pre-flight's econ counters were 0/0);
* at **64k**, it reaches and repays 30/30 — but reward headroom drops to 33% and 6 of 15 tasks score
  zero in all three arms;
* at **128k** (iteration 021), 11 of 15 scored zero in both arms.

So LOCA offers either a measurable mechanism or measurable reward, not both. Iteration 021's own
conclusion already named this — *"a benchmark with room to move"* — and this iteration has now
quantified it from both sides. Buying a $170 reward arm on a band with 33% headroom and 6 dead tasks
risks a fourth underpowered null. **Choosing the benchmark is the higher-value next step than choosing
the arm.**
