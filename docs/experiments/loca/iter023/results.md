# Iteration 023 — results

**Status: COMPLETE.** 9 passes, 45 runs, 3 arms × 15 tasks (seed 42) at 64k. One seed by design: analyse
before deciding whether to buy five.

| | |
|---|---|
| binary | `cg-i023-proxy-v01`, SHA-256 (first 32) `998477de132817270abc088e2fcbaccd` |
| code commit | `bd438f9` |
| arms | `cfg-iter023-{A-baseline,B-merged,C-coref}.yaml` |
| changes from 022 | `extract_llm` **unpinned** (#120 + #134); sweep `min_tokens` 1000 → **100** |

## 1. The headline: coverage HOLDS at a larger batch

| | offered | verdicts | batch | coverage |
|---|---|---|---|---|
| iteration 021 | 3,461 | 2,114 | ~12 | **61%** |
| iteration 022 | 132 | 132 | 4.4 | 100% |
| **iteration 023** | **644** | **643** | **6.7** | **99.8%** |

96 asks against iteration 022's 30, and **426,638 tokens removed** against 187,472. Coverage did not
degrade as the batch grew, on 5× the candidate count. That is the question this iteration was launched to
answer and it is answered in the direction the design wanted.

**Two caveats that keep it short of settling iteration 021's 61%.** The batch reached **6.7**, not the
11.6 the floor-100 availability calculation predicted — because `extract_llm` was far more active in arm B
(below), and its markers remove candidates the sweep would otherwise have named. So 61%-at-12 versus
99.8%-at-6.7 still spans two variables, not one. And iteration 021's ask was built by a different
mechanism (`selection_mode: merged` on `extract_llm`), not by this component.

**The fresh share improved unprompted: 40% → 26%** (5,248,808 cache-read against 1,860,398 fresh). Not a
change anyone made deliberately, so it is an observation rather than a result, and the joinable
`cg.sweep.ask` records added for exactly this question are now in the logs to explain it.

## 2. The affordability rule fires — 5 prunes, all in arm B pass 1

`drop_unaffordable_pruned` fired **5** times, and only in arm B where the econ trigger runs. So depth
does bite on real traffic: five voted drops would have extended the rewrite span past what they repaid,
and were withheld. Small, but it is the difference between a rule that is exercised and one that is
merely present.

## 3. The `extract_llm` asymmetry — traced, and the counters do not survive the trace

**This section replaces an earlier version that called arm B "still confounded" and attributed a
5,452 ms/request latency cost to it. Both claims are withdrawn.** The trace below is what changed them.

The observation: `extract_llm` ran in all three arms on an identical unpinned config, and `/stats`
credited it with 0 acts in A, **239** in B, 0 in C, plus 101 model calls in B alone.

**Hypothesis, and it is REFUTED.** The proposed mechanism was that the sweep mutates the prefix, moving
the cached boundary so that content A and C skip as `cached_prefix` becomes tail-eligible in B.
`cg.extract_llm` already carries the boundary, so this was directly testable:

| arm | `maxCachedIdx` median | mean | max | share unknown (−1) |
|---|---|---|---|---|
| A | 31.0 | 44.6 | 164 | **0.0%** |
| B | **34.0** | 49.3 | 286 | **0.0%** |
| C | 38.5 | 46.4 | 154 | **0.0%** |

The boundary is essentially identical across arms, B sits between A and C, and it was **never** unknown in
any arm. If the sweep were moving the boundary or pushing the cache cold, this is where it would appear.
It does not. The hypothesis is refuted rather than merely unconfirmed.

**And the trace found something worse: the counters contradict each other.** For the same arm and run:

| source | says |
|---|---|
| `components.extract_llm` | `runs: 389`, `acted: 239`, `saved_tokens: 6,077,421` |
| `extract` nested map | `calls: 101`, `avg_latency_ms: 59,009`, `net_value_usd: −1.162` |
| `cg.extract_llm` records | **`cands: 0` on all 374**, `skip_tail: 7,487`, `skip_floor: 692` |

`cands` is `len(cands)` logged after the gate loop, so `cands: 0` plausibly means no candidate survived —
consistent with `skip_tail` absorbing 7,487 of them. But a component with no surviving candidates cannot
make 101 calls. `cands` was 0 in **every one of 692 records** across the two arms checked.

The likely resolution is that the `extract` nested map is not scoped to one component: the sweep made **96
asks** in this arm, close to the 101 `calls`, and `saved_tokens: 6,077,421` against
`saved_tokens_unique: 597,764` is an overcount ratio of **31.58**, the signature of replays being counted
repeatedly (`reapplied_same_session: 2,291`). If so, the 59-second latency and the −$1.162 are the
**sweep's** figures, not the tail pass's — which is why the earlier attribution of 5,452 ms/request to the
sweep, then to `extract_llm`, was wrong in both directions.

Filed as **#176** (counters disagree) and **#177** (no per-call record to arbitrate them with).

**What survives about the 0/239/0 split.** Only the replay explanation: `reapplied_same_session: 2,291`
with `calls_avoided: 2,291` and 10,538 cache lookups at a 66.8% hit rate, all arm-B-only. The sweep's
`putResult` populates the shared extraction result cache and the tail pass replays those decisions. That
is the treatment's own verdicts persisting, which is what freezing exists to do — **not an independent
confound.** The 101 real calls remain unexplained and, per #177, are currently untraceable.

**Consequence for this iteration's cost and latency figures: treat them as unattributed.** The mechanism
results in sections 1 and 2 are unaffected — coverage, batch size, prunes and removed tokens all come from
`cg.sweep.ask`, which is per-ask and self-consistent.

## 4. Reward — null again, both bounds still block

| | net | improved | worsened | unchanged | p (exact sign) | CP 95% upper |
|---|---|---|---|---|---|---|
| B vs A | **+1** | 3 | 2 | 10 | 1.0000 | **40.5%** |
| C vs A | **0** | 1 | 1 | 13 | 1.0000 | **31.9%** |

A **5.00/15**, B **6.00/15**, C **5.00/15**. Both blocked by the pre-registered 25% harm bound, exactly as
iteration 022 predicted for any single-seed design: the floor at n=15 is 21.8% with zero worsened pairs.

Note arm A fell from iteration 022's 7.00 to 5.00. That is expected rather than alarming — unpinning made
`extract_llm` inert in A, so this baseline is a different, weaker configuration. It is not comparable to
iteration 022's arm A, and no comparison across iterations should be drawn from it.

## 5. What to do next

**The mechanism results are good enough to justify the five seeds; the arm design is not yet.** Coverage
holding at 99.8% on 5× the candidates, 2.3× the tokens removed, and an affordability rule that
demonstrably fires are all real. But paying 5× for a confounded B−A would buy a precise measurement of
something uninterpretable.

The cheap next step is free: the 0/239/0 split is now traceable from this run's own logs, because
`cg.sweep.ask` carries `max_cached_idx` and the component records carry per-request cache state. Settle
the mechanism first, control it, and only then buy seeds.

If the economic-gate hypothesis holds, the fix is likely `economic_gate: false` on `extract_llm` in all
arms — which iteration 021 also ran deliberately, and said in advance was not a shippable setting.
