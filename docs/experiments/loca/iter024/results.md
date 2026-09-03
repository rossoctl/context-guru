# Iteration 024 — results

**Status: COMPLETE.** 150 runs — 2 arms × 15 tasks × 5 seeds at 64k. **The first significant positive
reward result this line of work has produced**, and the first honest per-component cost measurement.

| | |
|---|---|
| binary | `cg-i024-proxy-v01`, SHA-256 (first 32) `44e6d6a182a4aeda8bd2744299c5e78b` |
| code commit | `be79553` — `feat/coref-recut` with #178 merged |
| arms | `cfg-iter023-{A-baseline,B-merged}.yaml` |
| B differs by | `extract_llm_sweep: {evidence: true, econ_trigger: true}` |

## 1. Reward — significant on the governing test, with zero tasks regressed

| | |
|---|---|
| ARM A | **36.00 / 75** (0.480) |
| ARM B | **46.00 / 75** (0.613) |
| paired, 75 pairs | net **+10** — 13 improved, 3 worsened, 59 unchanged |
| per-pair exact sign test | p = **0.0213** |
| harm — CP 95% upper on worsened | **11.2%** — does **not** block |
| **clustered, 15 tasks (GOVERNING)** | net **+2.00** · **8 better, 0 worse**, 7 tied · **p = 0.0078** |

The pre-registration names the clustered test as governing. It returns **p = 0.0078 with no task worse in
either direction**. Iteration 021's equivalent was p = 1.0000.

**And the harm gate finally cleared for a structural reason, not a lucky one.** At n=15 the
Clopper-Pearson floor is 21.8% *with zero worsened pairs*, so iterations 022 and 023 were blocked by
arithmetic rather than by evidence. Five seeds put n at 75 and the bound at 11.2%. That is what the seeds
bought.

## 2. Cost — and the value does not appear where the component's own accounting looks

| component (arm B) | spend | gross value | net | calls | tokens removed |
|---|---|---|---|---|---|
| `extract_llm_sweep` | $20.26 | $0.72 | **−$19.53** | 629 | 2,407,680 |
| `extract_llm` | **$0.00** | $11.58 | **+$11.58** | **0** | — |
| **combined** | | | **−$7.95** | | |

`cost_source: component` on every arm-seed, so the pre-registered primary endpoint **passed** — these are
attributable figures, which nothing before iteration 024 could produce. Across 75 runs the net is
**−$0.11/run**, about **9%** on LOCA's measured $1.13/run.

**The sweep's own accounting understates it by construction.** 2,407,680 removed tokens are valued at
**$0.72**, because a removal banks at cache-read rates. So the mechanism's return does not show up in its
token ledger at all — it shows up in reward. −$19.53 of measured loss bought +10 solves.

**Two caches share a keyspace, and it recovers 57% of the spend — accounting confirmed, MECHANISM NOT.**

What is measured: `extract_llm` books **$11.58 at $0.00**, from **0 fresh calls and 364 replays**, in arm B
only — 4,749 calls avoided at a 99.2% cache hit rate. The sweep is the only writer to that extraction
result cache (its drop path calls `putResult(c, cands[k].id, desc, "")`, keyed by content id, so its
verdicts replay on later turns), and `extract_llm` reads the same cache so the tail pass does not re-call
the model on content it already compacted. So the entries come from the sweep, across turns.

**What is NOT established is the path.** An earlier draft of this file asserted one, and it does not
survive its own pipeline order: `extract_llm` runs **before** `extract_llm_sweep` within a request, so the
sweep's same-turn decisions cannot feed it. Two candidate cross-turn paths were checked and both are
closed by the counters:

* **expand-restore** — sweep drops, the agent expands, the restored content becomes a fresh tail candidate.
  Expansion *is* far higher in arm B (`kept_verbatim_after_expand` **9,478 against 3,522**), so the
  expanding happens — but that gate means `extract_llm` **skips** expanded content rather than compacting
  it, so the path is closed.
* **the tail/prefix boundary moving as the transcript grows** — closed by `cached_prefix` dominating at
  **29,302**, i.e. candidates are overwhelmingly skipped as *not tail*.

So the recovery is real in its accounting and unexplained in its mechanism. **That matters commercially,
not just intellectually:** 57% of this configuration's cost recovery rests on a behaviour nobody has
identified, in a cache neither component documents sharing. If either component's keying changes it could
vanish with no change to the sweep at all, taking the cost story with it. It needs the mechanism found and
then either a test pinning it or an explicit decision to depend on it.

`acted_fresh` vs `acted_replay` is what separates the two: `extract_llm` is 364 replay / **0 fresh**; the
sweep is 95 fresh / 0 replay (seed 1). Before #178 both were pooled and neither was visible.

## 2b. A reversibility failure, arm B only — 175 expands the agent could not resolve

`expand_unresolved_missing`: **175 in arm B, 0 in arm A** (seeds 1-3).

The agent asked for removed content back 175 times and did not get it. That is the single failure the
"every lossy Offload must be reversible" invariant exists to prevent, and it is not new — iteration 023
recorded **60** in its Cp1 pass, against 0 everywhere else, in that iteration's worst-scoring pass.

It does not invalidate section 1: arm B still won 8 tasks to 0 with the harm bound at 11.2%. But it is a
first-class limit rather than a footnote, and it raises a question that has to be answered before any
production claim: **is the mechanism winning partly by removing content irrecoverably?** A drop the agent
can undo and a drop it cannot are different products, and the reward number cannot tell them apart.

Filed for investigation. On this evidence it looks like a defect rather than a tuning artifact — 0 in the
arm without the treatment, and it recurs across two iterations.

## 3. Latency — the hypothesis does not hold

| | A | B | |
|---|---|---|---|
| paired turns, all 75 | 24.1 | **29.4** | +22% |
| paired turns, **both arms solved** (33) | 29.3 | **33.2** | **+13%** |

The standing hypothesis — from iteration 021's −28% requests — was that the mechanism reaches the same
reward in fewer turns, so end-user latency improves. **It does not.** Composition explains about half the
gap (B does solve longer tasks A abandons), but on the 33 pairs where *both* arms solved, B still takes
**13% more turns**. The mechanism buys accuracy by letting the agent work longer on a managed context, not
by making it faster.

That closes a question open since iteration 021, which could not make this split at all. Iteration 023
found the turn saving sitting on the *failure* path; with five seeds and a paired both-solved comparison,
there is no turn saving to attribute anywhere.

## 4. Reading against the pre-registration

The reading table's row for this outcome: *"`cost_source: component` throughout, B's net value
negative → the mechanism works and does not pay at this band → report the ceiling."* That row is half
right and needs amending in light of the reward result, which it treated as a harm gate rather than as a
possible finding: **the mechanism does not pay in tokens and does pay in reward.** Those are not the same
axis, and this iteration is the first able to see both at once.

**What this establishes**

1. `evidence` + `econ_trigger` improve task success on LOCA at 64k: +10 solves over 75 pairs, 8 tasks
   better and 0 worse, clustered p = 0.0078, harm bounded at 11.2%.
2. It costs about 9% net, after a recovery channel returns 57% of the spend.
3. It costs 13% more turns on comparable work. The latency claim is refuted, not merely unsupported.

**What it does not establish.** One benchmark, one band, 15 clusters. And the arms carry
`min_inventory: 3` and sweep `min_tokens: 100` — neither a shipped default — so this is **not** a claim
about the shipped configuration. Nor is it a claim about `coref`-the-component, which was not in either arm.

## 5. Limits

1. **Not the shipped config.** See above; identical across arms so it cannot bias B−A, but B is not
   `housellm` and `extract_llm` runs unpinned (#120/#134).
2. **The recovery channel's mechanism is unidentified** (section 2). Its accounting is confirmed, its
   cause is not, and two candidate paths were checked and closed. 57% of the cost recovery rests on it.
3. **175 unresolved expands in arm B, 0 in arm A** (section 2b), recurring from iteration 023's 60. Under
   investigation as a suspected defect.
4. **`coref` was not measured.** Its own question is still open from iteration 023.
5. **The sweep's ~18s/ask stands unexplained in one respect** — whether that mean is uniform or
   tail-driven. #177's per-call record makes it answerable but this run's sample was not examined for it.

## 6. Next

The pre-registered next step for a positive outcome is to put this branch up for review. Note the PR
situation: **#80 tracks `feat/coref-compaction`**, the archived pre-recut branch (`f31b451`), not this
work. `feat/coref-recut` has no PR. Opening one, or repointing #80, is an open decision.

Before shipping, the two claims worth testing at the shipped defaults are `min_inventory` and the sweep's
floor — this result is at 3 and 100, and the shipped values are 10 and 1000, which iteration 022 measured
as unreachable and batch-starving respectively on this workload.
