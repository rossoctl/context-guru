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

**Two components share one result cache, and it recovers 57% of the spend. MECHANISM NOW ESTABLISHED.**

What is measured: `extract_llm` books **$11.58 at $0.00**, from **0 fresh calls and 364 replays**, in arm B
only — 4,749 calls avoided at a 99.2% cache hit rate.

**The sharing is documented, not accidental.** `store.ResultPrefix` carries its own comment:

```go
ResultPrefix = "cg:res:" // extract_llm's replayed result (projection + summary, one key)
```

The sweep's drop path calls `putResult(...)`, and `putResult`/`getResult` are **shared helpers** in
`components/offload/state.go`, keyed `cg:res:<session>:<content-id>`. So the sweep writes its verdicts into
a namespace the code names as *extract_llm's*, through a helper both components call. Not a keyspace
collision — one cache with two writers, by construction.

**Which resolves the ordering objection.** `extract_llm` runs BEFORE `extract_llm_sweep` within a request,
so the sweep cannot feed it in the same turn — and an earlier draft of this file asserted a mechanism that
foundered on exactly that. The resolution is that the cache is **content-addressed, not positional**, and
persists across turns: `extract_llm` never needs to have seen the content first, only to encounter a
content id on some later turn that the sweep already ruled on.

**And the control flow says which ids those are.** In `extract_llm.go` the marker skip is at **line 848**
and the cache lookup at **line 868** — the skip comes first, so a marker-bearing message never reaches
`getResult`. Every one of the 364 replays was therefore on content carrying **no marker**, which leaves one
possibility: the same content recurring at a fresh position, which is routine agent behaviour (re-reading a
file, re-running a command) and is consistent with a 99.2% hit rate.

Two candidate paths were checked and closed on the way here, and are recorded so they are not re-proposed:
**expand-restore** fails because expansion is far higher in arm B (`kept_verbatim_after_expand` **9,478
against 3,522**) but that gate makes `extract_llm` *skip* expanded content; **the tail/prefix boundary
moving** fails because `cached_prefix` dominates at **29,302**, so candidates are overwhelmingly skipped as
not-tail.

**TWO CAVEATS ADDED AFTER REVIEW OF #188, and the second one may make the recovery partly illusory.**

*Semantics — a drop replayed as a compaction.* `extract_llm` stores a **compaction** (a property of the
content, reusable wherever that content appears); the sweep stores a **drop** — "spent for this
transcript's obligations" — which is why the sweep never publishes to the cross-session `cg:xres:`
namespace. When `extract_llm` replays a sweep record it applies an *obligation* judgement as a *content*
projection, at a fresh position, possibly many turns later.

The specific hazard is not staleness in general but its direction: **recurrence is evidence against
spentness.** Those replays can only land on unmarked content, so the ids are content reappearing at a
fresh position — and content reappears because the agent re-ran the command or re-read the file. An agent
re-fetches because it needs it *now*. So the replay applies "was spent when the sweep looked" at precisely
the moment the agent has demonstrated renewed need, and the sweep's own contract defines spent as needed
for none of "(a) the step you are on right now" among others. A re-fetch is fairly direct evidence that
(a) has changed. If a tag is ever added here it should distinguish **compaction from drop**, which carries
meaning, rather than component from component, which does not.

*Cost — the replays may not be free.* This section records the channel as **$11.58 at $0.00**, free
because a replay makes no model call. But the replay path reaches `apply` **without passing the cache-tail
depth gate** (`extract_llm.go`: replay at :890, gate at :932), and that bypass is justified per-MESSAGE —
"this session already sent these compacted bytes" — while the lookup is keyed per-CONTENT. At a fresh
position the provider holds the **original**, not the compacted bytes. Splicing the compaction there
changes bytes inside the cached prefix and forces a cache-write of the suffix: a real cost, on a ledger
`extract_llm` does not carry, and the same cost the sweep's econ trigger prices at 11.5x a cache read.

**All 364 replays are in that category** — the marker skip at :848 precedes the lookup at :868, so a marked
message never reaches `getResult`. This is not a tail of the channel; it is the whole of it. If the
hypothesis holds, the 57% recovery is not free but charged somewhere nobody was reading.

**It cannot be settled from this run:** `cache_read_tokens`, `cache_write_tokens` and `fresh_input_tokens`
all read **0** in every arm-seed — unpopulated in this build, not measured as zero. So the one measurement
that would decide it is absent from the run that raises it. A re-run with those live settles it directly:
cache-write volume in arm B against arm A, where no replays exist.

One thing review did establish in the channel's favour: the **bytes are identical**. A sweep record
replayed by `extract_llm` produces exactly what the sweep's own splice produces — same descriptor, the
sweep's empty summary taking the no-summary branch, the same `tryMark` key, the same literal recovery hint.
So the cross-component path cannot flip content, and a component tag would prevent no byte difference. The
open questions are semantic and economic, not correctness.

**What this means for the cost figure, and it is not reassuring.** Those `cg:res:` entries are *pinned*, so
#187's eviction defect never touched them — but #188's fix **refuses removals** when the payload reserve is
full, and no removal means no `putResult`, which means no entry to replay. So the recovery scales with how
often the sweep actually acts, and it moves in both directions under that fix: a larger store
(1,000 → 5,000 entries) admits more removals and more recovery, while refusals under pressure suppress
both. **The −$7.95 should not be expected to reproduce.**

`acted_fresh` vs `acted_replay` is what separates the two components: `extract_llm` is 364 replay / **0
fresh**; the sweep is 95 fresh / 0 replay (seed 1). Before #178 both were pooled and neither was visible.

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
reward in fewer turns, so end-user latency improves. **This run does not support it, and cannot cleanly
test it.** Composition explains about half the gap (B does solve longer tasks A abandons), but on the 33
pairs where *both* arms solved, B still takes 13% more turns.

**CONFOUNDED, and an earlier draft of this file called it "refuted" — that was too strong.** Section 2b's
209 unresolved expands are ~2.8 per run against ~33 turns per run, and every one of them is a turn the
agent spent asking for content and receiving a placeholder, possibly asking again. That could account for
a meaningful share of the 13%. So the honest statement is **not supported, and partly attributable to a
reversibility defect** (#187) rather than to the mechanism. Separating them needs a re-run after that fix;
until then no latency claim should be made in either direction.

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
3. It costs 13% more turns on comparable work — but that figure is confounded by #187 and cannot yet be
   attributed to the mechanism. The latency claim is unsupported, not refuted.

**What it does not establish.** One benchmark, one band, 15 clusters. And the arms carry
`min_inventory: 3` and sweep `min_tokens: 100` — neither a shipped default — so this is **not** a claim
about the shipped configuration. Nor is it a claim about `coref`-the-component, which was not in either arm.

## 5. Limits

1. **Not the shipped config.** See above; identical across arms so it cannot bias B−A, but B is not
   `housellm` and `extract_llm` runs unpinned (#120/#134).
2. **The recovery channel is a shared-cache behaviour that nothing tests** (section 2). The mechanism is
   now established, but no test pins it, and #188's refusal path can suppress it. 57% of the cost recovery
   rests on a documented-but-unguarded interaction between two components.
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
