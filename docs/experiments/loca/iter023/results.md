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

## 3. ARM B IS STILL CONFOUNDED, and now systematically rather than noisily

| arm | `extract_llm` runs | acted | extraction calls |
|---|---|---|---|
| A | 333 | **0** | 0 |
| B | 389 | **239** | 101 |
| C | 413 | **0** | 0 |

`extract_llm` ran in all three arms on an identical, unpinned config, and its pressure trigger fired
**only in arm B**. Zero in A, zero in C, 239 acts and 101 extraction calls in B.

This is worse than iteration 022's confound, not better. There the swing (15 vs 108) was concentrated in a
few long runs and read as a high-variance statistic. Here it is perfectly segregated by arm across 1,135
component runs — which is not variance. Unpinning removed the #134 forcing as intended, and in doing so
exposed a systematic interaction between the sweep and `extract_llm`'s pressure trigger.

**Unexplained, and stated as such.** The obvious direction is wrong: the sweep *removes* content, which
should *lower* context pressure and make `extract_llm` fire less, not more. Candidate mechanisms, none
established:

* **expand.** The sweep's markers get expanded by the agent, restoring full outputs into the transcript,
  which raises pressure and hands `extract_llm` fresh candidates. But `coref` leaves markers too (68 acts
  in arm C) and arm C shows zero, so markers alone do not explain it.
* **the economic gate.** `extract_llm` runs with `economic_gate: true`, and the gate prices a candidate by
  POSITION against cache state. The sweep mutates the prefix, changing `max_cached_idx` and cold-cache
  status on later turns, which changes how the gate prices tail candidates — plausibly unlocking a path
  that stays shut in A and C.

The second is the more likely and is testable from the logs now that `cg.sweep.ask` carries
`max_cached_idx`.

**So B−A still does not isolate `evidence` + `econ_trigger`**, and the cost of the confound is visible:
arm B added **5,452 ms per request** against arm A's 53 ms. Whatever that buys, it is not attributable to
the two knobs under test while 239 uncontrolled extraction acts sit in the same arm.

Arm B also carried the run's only **5 capture-hop 400s**, against 0 in A and C.

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
