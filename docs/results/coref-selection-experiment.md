# Does an LLM decide better than the reference index? A held-out experiment

The co-reference proposal (`docs/proposals/coref-compaction.md`) and the design discussion around it
produced one claim that decides the whole component, and it had never been measured: **if a single
model call may either *drop* a tool output entirely or *trim* it, does that produce better decisions
than the deterministic reference index — and does it pay for itself?**

This is that experiment. **$43.88** of model spend, 8,105 recorded decisions, ten arms. Every number
below is reproducible from `~/cg-coref-experiment-20260819/` (code, all decisions, logs).

Two of its results **retract earlier claims in this repo's own docs**, and they are marked as such.
Confidence is labelled per finding: **measured**, **measured, with caveat**, **structural argument**
(arithmetic, not observation), or **withdrawn**.

!!! warning "What could not be run, and why it matters"
    SWE-bench Verified and Terminal-Bench 2.0 — the two benchmarks this repo reports — **were not
    runnable**. [`REPRODUCE.md`](REPRODUCE.md) requires Linux, a Harbor checkout at a fixed path, and
    Docker Hub authentication; the work was done on macOS with no Harbor. So this experiment measures
    **decision quality on captured traffic**, never task outcome. It cannot speak to reward, and
    reward is the axis the proposal itself says is decisive.

## Method: decide at a firing point, score against the future

The design is what makes the numbers trustworthy, so it is worth stating precisely.

For each captured transcript, pick a **firing point `F`** at 60% of the way through its model turns —
the point where a compaction pass would plausibly fire. Every tool output at a message index below
`F` and at least 300 tokens becomes a candidate. Then split the transcript:

| window | contents | who sees it |
|---|---|---|
| **evidence `(i, F]`** | which identifiers the output introduced, which later turns reused them, how many times, how long ago, how many turns elapsed | every arm |
| **held-out future `(F, end]`** | what the agent actually went on to do | nothing — ground truth only |

Every arm decides `drop` / `trim` / `keep` from the evidence window alone. Scoring:

- **false-drop** — of the outputs an arm removed, the share that *were* referenced by exact match in
  `(F, end]`. A definite error: we removed something the agent went on to use.
- **live-kept** — of the outputs that *were* referenced in `(F, end]`, the share an arm correctly
  kept. **This is the discrimination metric**, and it is the one that separates the arms.
- **null baseline** — drop everything. Scores exactly the base rate, and reveals that a high
  removal rate paired with a base-rate false-drop rate means *no discrimination at all*.

`false-drop` alone is misleading because it is bounded by the base rate; `live-kept` is not.

### Corpora

| corpus | candidates | mass | referenced after `F` |
|---|---|---|---|
| LOCA-bench | 140 | 254,244 tok | 4% |
| UltraHorizon | 94 | 116,764 tok | 11% |
| **Claude Code** | **885** | **943,747 tok** | **46%** |

Claude Code carries the conclusions: at a 4% base rate there is almost nothing to discriminate, so
LOCA and UltraHorizon cannot separate a good arm from a reckless one.

### Arms

Two deterministic (the `coref` classifier, with and without `cut_closed`) and eight model arms across
two models × four prompt shapes:

- **digest** — goal + output content + reference evidence, per output.
- **nodigest** — same, evidence withheld (ablation: does the index earn its place?).
- **evidence** — reference evidence only, *no output content* (tests whether cost can collapse ~10×).
- **strict** — digest, but the prompt states the real cost of a wrong removal instead of reassuring
  the model that cuts are recoverable, and insists on verbatim copying.
- **bulk** — one call adjudicates ~15 outputs together, so the model judges comparatively.

## Results (Claude Code, n=885, null = 46%)

| arm | removed | false-drop | **live-kept** | $/call | break-even |
|---|---|---|---|---|---|
| *null: drop everything* | *100%* | *46%* | *0%* | — | — |
| **deterministic, unref only** | 11.8% | **11%** | **95%** | $0 | — |
| **deterministic, +closed** | 26.5% | 21% | 81% | $0 | — |
| sonnet bulk | 33.6% | 29% | 57% | $0.0054 | 10,675 |
| haiku bulk | 40.2% | 36% | 58% | **$0.0010** | 1,606 |
| **sonnet strict** | 49.0% | 30% | 58% | $0.0118 | **15,987** |
| haiku strict | 64.0% | 34% | 32% | $0.0028 | 2,952 |
| haiku nodigest | 88.2% | 45% | 11% | $0.0026 | 1,966 |
| haiku digest | 90.9% | 40% | 6% | $0.0027 | 1,984 |
| haiku evidence-only | 90.9% | 43% | 9% | $0.0012 | 844 |
| sonnet digest | 87.3% | 42% | 14% | $0.0107 | 8,205 |

## Findings

### 1. The per-output merged design is refuted — **measured**

This is the design the discussion converged on: one call, seeing the output and the reference
evidence, deciding drop-or-trim. It fails. Haiku keeps **6%** of what the agent actually needed;
dropping everything blindly keeps 0%. Sonnet is *worse* (14% live-kept, 42% false-drop). Both sit
inside the null model's error bar.

Giving a model the content **and** the evidence, per output, does not produce discrimination. It
produces a machine that drops nearly everything.

### 2. Two interventions genuinely work — **measured**

**Bulk adjudication** lifts live-kept from 6% → **58%**, at the *lowest* cost per output (overhead
amortises across ~15 outputs). Comparative judgment beats absolute judgment: shown fifteen outputs
side by side with their evidence, the model ranks them; shown one, it just drops it.

**Prompt framing is worth ~26 points of live-kept.** The first prompt reassured the model that cuts
"stay recoverable on request". That single clause produced 91% removal at 6% live-kept. Replacing it
with the real cost — *"the agent usually does not notice the gap and answers from worse information
instead of asking for it back"* — moved haiku to 64%/32% and sonnet to 49%/**58%**. Telling a model
its mistakes are cheap makes it careless.

### 3. The deterministic index is the best discriminator measured — **measured**

95% live-kept at 11% false-drop, at zero marginal cost. **No combination of index and model beat
it**: intersecting the index with the best model arm gives 8% removed at 10% false-drop — strictly
worse than the index alone. Every model-involving operating point is either lower yield or higher
error than a deterministic one, up to 26.5% removal.

This reverses the design conclusion the discussion had reached, which was to demote the index to an
evidence supplier and move the verdict into the model's prompt.

### 4. `cut_unreferenced` is not the "free safe cut" it ships as — **measured**

It has an **11% false-drop rate** under Tier-1 exact ground truth: outputs dormant at `F` that the
agent used later. "Unreferenced" is a claim about the past, and the future contradicts it one time in
nine.

**Revised upward to 21–24%** by [iteration 009](../experiments/loca/iter009/results.md), which widened
ground truth with deterministic normalization (numeric reformatting, case, substring; the 24% figure
additionally allows path-basename matching and is an upper bound). So the "free safe cut" is roughly
**twice as lossy as first published**, and since Tier-3 semantic reuse is still invisible, even 21% is
a lower bound.

It is **not a boundary artifact**, which was the first thing to check:

| first future reference | share of false drops |
|---|---|
| 1 turn after `F` | **0%** |
| 2–5 turns | 5% |
| 6–10 turns | 21% |
| 11–25 turns | 10% |
| 26–50 turns | 5% |
| **51+ turns after `F`** | **57%** |

Discarding every reference within 50 turns of the boundary still leaves 6%. The errors are genuine
long-range dormancy — an output goes a hundred turns untouched, then the agent reaches for it.

**And it is irreducible with the features available.** Requiring more introduced identifiers makes it
*worse* (11% → 21%); requiring longer observed dormancy barely helps while cutting mass 2.5×. An
output dormant for 100+ turns and then used carries no signal at `F` distinguishing it from one
dormant forever.

### 5. `min_later_turns` does not earn its keep on this metric — **measured**

Sweeping the opportunity floor added earlier:

```
min_later=  0: drops=352  false= 8%  mass=362,915
min_later=  8: drops=210  false=11%  mass=207,331   <- the shipped default
min_later= 20: drops=162  false= 9%  mass=146,209
min_later= 80: drops= 95  false=14%  mass= 57,042
```

Zero gives *more* mass and *lower* false-drop than the default. The floor was added for a different
reason — stopping a batched pass from preferentially cutting the newest context — and that rationale
stands, but it is not buying safety and the component docs should not imply it does.

### 6. The trim contract as specified is unusable — **measured**

Asking a model to return the content worth keeping produced text that was **not verbatim in the
original** in 94–172 of every ~140 trim verdicts. Models paraphrase, reformat, and reconstruct. This
is exactly what `internal/extract/contain.go` exists to catch, and it means the "return the kept
text" contract must be replaced by `extract_llm`'s sandboxed-filter mechanism, which enforces
containment structurally.

**The anchor guard also failed.** The prompt instruction — *"if a later turn referred to something
from this output in order to point at a value it did not restate, that value must be kept"* — did not
work: over half of trims on future-referenced outputs dropped the very identifier the agent needed.

### 7. The cost/capability bind — **measured**

| | $/call | break-even | live-kept |
|---|---|---|---|
| haiku strict | $0.0028 | 2,952 tok | 32% |
| sonnet strict | $0.0118 | 15,987 tok | **58%** |

**The model cheap enough to pay for cannot discriminate; the model that discriminates cannot be paid
for.** No prompt moves this — it is cost against capability.

And almost no real output clears the higher bar:

| corpus | > 2,952 tok | > 15,987 tok | max output |
|---|---|---|---|
| LOCA | 11 (7%) | **5 (3%)** | 39,713 |
| UltraHorizon | 0 | **0** | 2,500 |
| Claude Code | 62 (7%) | **0** | 11,399 |

### 8. The break-even mechanism is real; its magnitude was overstated — **measured, correcting this repo**

`B = callCost / (ratio × (1 + reuses) × perToken)`, so break-even is inversely proportional to the
effective ratio. `extract_llm` measures ratio 0.10–0.12 because *trim* is its only legal outcome.
Adding a *drop* outcome raises the population ratio, and `B` falls proportionally. **The mechanism is
confirmed.**

But the eye-catching figures (844–2,952) belong to arms removing 64–91% of mass at 34–43% false-drop
— correct arithmetic on an unusable policy. At the best defensible operating point (sonnet strict,
ratio 0.49) break-even is **15,987 tokens**: 2× better than `extract_llm`'s measured 30,500, not the
10–15× first claimed. **Holding call cost fixed, the ratio improvement buys ~4.5×.**

And break-even here is denominated in *cache-read savings*, which this repo already measured as 0.024%
of billed input. Clearing it means only that the call cost stopped being the binding constraint — not
that the pass is worth making.

### 9. Selection cannot replace a summarizer — **structural argument**

Sustained selective removal is `f × g`, where `f` is the fraction of arriving mass that ever becomes
removable and `g` is the growth rate. Since `f < 1`, removal is always **less** than growth: selection
cannot hold the line, only slow the approach, by `1/(1−f)`.

| removable fraction of request | session extension |
|---|---|
| 4.4% (deterministic unref) | 1.05× |
| 9.6% (deterministic +closed) | 1.11× |
| 18% (sonnet strict) | 1.22× |
| 24% (haiku strict) | 1.32× |

Even the aggressive arms buy 22–32% more turns. That is **deferral, not replacement**. A summarizer
achieves ~96% because it compresses **live** content; selection can only remove **dead** content. To
replace a summarizer you must paraphrase, which means being one.

### 10. The incumbent comparison is **withdrawn** — the metric cannot make it

An attempt to score the agent's own auto-compaction the same way (7 real `isCompactSummary` events,
640k tokens of tool output → 27.7k of summary) first appeared to show the summarizer dominating every
selective arm at 96% removal and 20% false-drop. **That was an artifact of a missing base-rate
control** — the same error this document warns about in its own method section. With the null added,
the population's base rate is 23% against the summarizer's 20% false-drop: the summary rescued **3
percentage points**, i.e. 9% live-kept.

**Neither figure is trustworthy, and the comparison should not be made with this instrument.**
Identifier matching scores *verbatim survival*. A prose summary that reads "found the grace-period
bug in the auth module" has preserved the information while containing none of the identifiers, so
the metric punishes paraphrase by construction and cannot bound how much the summary really carried.
It is valid *within* the selective arms — they all keep-or-drop verbatim — and invalid across the
verbatim/paraphrase boundary.

**One number does survive**, because it does not require scoring the summary's content:
**11% of post-compaction model turns reference identifiers the summary did not carry and that were
not re-read** — the agent visibly reaching for something it no longer has. That is the incumbent's
operational damage rate.

## Corrections to earlier claims in this repo and its discussion

| claim | status |
|---|---|
| "`coref` amortises one cache-write across a batch; `extract_llm` pays per output" | **Wrong.** `extract_llm` applies all projections in one request, so its write cost is also one rewrite. `coref`'s real advantage is zero model calls. |
| "Break-even collapses 10–15×" | **Overstated ~3×.** ~4.5× at a defensible operating point. |
| "Sonnet is dramatically safer (4% false-drop)" | **Artifact.** It had only processed LOCA (4% base rate). On Claude Code it is 42%. |
| "The summarizer dominates every selective arm" | **Withdrawn** (finding 10). |
| "`cut_unreferenced` is the free safe cut, no calibrated threshold needed" | **False.** 11% false-drop, irreducible, lower bound. |
| "Fold the verdict into the model's prompt; demote the index to evidence supplier" | **Refuted** (findings 1, 3). |

## Limitations

- **No reward, no benchmark.** Decision quality on captured traffic only. The proposal's own
  acceptance criteria put reward first, and nothing here touches it.
- **Ground truth is Tier-1 exact matching**, so it cannot see transformed or semantic reuse. Every
  false-drop figure is a **lower bound**, for every arm. **Partly quantified**
  ([iteration 009](../experiments/loca/iter009/results.md)): widening it with deterministic
  normalization (numeric reformatting, case, substring) moves referenced candidates 408 → **473**
  (+16%) and raises every arm's false-drop — `cut_unreferenced`'s goes **11% → 21%**, see finding 4.
  The gap between the index and the best model arm does not narrow; it slightly widens. The
  deterministic slice of Tier-2 turns out to be nearly empty, so closing this bias properly needs a
  judge, which reintroduces the noise that ruled out UltraHorizon
  ([measurement-limits §6](measurement-limits.md)). The bound is tighter, not removed.
- **One firing point** (`F` = 60% of model turns). A real pass fires at a threshold crossing, at
  varying depth with varying future remaining.
- **An asymmetry that flatters the deterministic arms:** `min_later_turns` is a hard structural guard
  present only in them. Model arms received `later_turns` as information with no enforced floor.
  **Now done** ([iteration 009](../experiments/loca/iter009/results.md), $0, no new model calls): it
  changes almost nothing. The floor overrides only 6–23 of 885 decisions per arm and moves live-kept
  by 0–2 points; removing it from the deterministic arms costs them one point. The asymmetry was a
  reasonable suspicion and is not the explanation for the gap.
- **`n = 7` compaction events** for finding 10, from 4 sessions, one contributing 3.
- **The Claude Code corpus is read linearly out of a tree-structured transcript**, so abandoned
  `--resume`/edit branches are present. Measured contamination is small — exact-duplicate tool
  outputs are 3% of mass pooled, 2% median ([reachability §2](coref-reachability.md)) — so class
  shares and the arm comparison hold. But it introduces a bias *opposing* the Tier-1 one: an
  abandoned branch can supply a "later reference" that the live conversation never made, which
  inflates false-drop. So the 11% is bracketed by two biases of unknown relative size rather than
  being a clean lower bound, and the earlier claim that it is purely a lower bound was too strong.
- **"Referenced later" ≠ "the agent was harmed."** Removed content is recoverable via `expand`, so
  the true cost is a round-trip *if the model notices*. Treating a later reference as an error is an
  assumption inside the metric.

## What this settles, and what it does not

**Settled:** the per-output merged design does not work; bulk adjudication and cost-honest prompting
are the only model shapes worth pursuing; the deterministic index is the strongest discriminator
measured; `cut_unreferenced` carries an irreducible ~11% error floor; the trim contract needs a
sandboxed filter, not free-text; selection cannot replace a summarizer.

**Not settled — and each needs a different instrument:** whether any of this improves *reward*;
whether recovery via `expand` actually fires often enough to make a 30% recoverable false-drop
preferable to a 20% permanent one; how often the agent's own compaction is reachable at all
(`modes.Tracker` reset detection, free, still not run); ~~and whether a floor-symmetric comparison
narrows the gap between the index and the bulk arm~~ — **answered: it does not**
([iteration 009](../experiments/loca/iter009/results.md)).

See also: [co-reference density](coref-density.md) · the proposal (`docs/proposals/coref-compaction.md`)
· implementation status (`docs/proposals/coref-implementation.md`)
