# Keeping a Prompt Cache Warm Is Not a Prediction Problem

### Evidence from 41 days of production agent traffic

**Status of this draft.** All results below are measured or replayed on real production
data and have been through an adversarial review that found and corrected five defects.
The **Related work** section is not yet written — a literature pass is running separately,
and until it lands every novelty claim in Section 9 should be read as provisional.

---

## 1. What this paper is about, in plain words

When you talk to an AI assistant, the model re-reads your entire conversation every time
you send a message. In a long session that becomes the dominant cost. So providers offer a
deal: they keep a ready-made copy of your conversation, and reading that copy costs about a
tenth of re-reading it from scratch.

The catch is that the copy is thrown away after five minutes of silence. Step away for
coffee and you pay full price to rebuild it.

That suggests an obvious trick. When a conversation goes quiet and the copy is about to
expire, send a tiny artificial request purely to make the provider touch the copy again,
which resets the clock. We call this a **ping**, or **keep-alive**.

**The question is whether pinging saves money.** This paper answers it on 41.4 days of a
real multi-tenant service: 264,163 requests, 18 paying tenants, 14,412 sessions.

The short answer is that pinging *can* save money, that no machine-learning model helps you
do it better, and that the reason is not the one anybody expected.

---

## 2. How the money works

The entire economics fits on an envelope, and the single number it produces explains most of
the paper.

```
Say re-reading a conversation from scratch costs 100 units.

  reading the cached copy instead    =  10 units   (0.10x)
  making a fresh cached copy         = 125 units   (1.25x)

The conversation goes quiet. The five-minute clock is running out.

  do nothing, they come back   -> copy gone, pay    125
  keep it alive, they return   -> copy there, pay    10
                                                   ----
  so keeping it alive is worth                      115
  and each ping costs                                10

  ping if  (chance they return) x 115  >  10
           chance  >  10 / 115  =  8.70%
```

**8.70% is the bar.** Above it, pinging pays; below it, it burns money.

One trap worth naming because it is easy to fall into: the answer is *not* 10/125 = 8.0%.
What a ping avoids is not the whole cost of a new copy, it is the **difference** between
making a new copy and reading the old one. Getting this wrong understates how good a
predictor must be.

---

## 3. Experiment design

### 3.1 Data

| | |
|---|---|
| Source | read-only snapshot of the production store, taken 2026-09-28 00:15 UTC |
| Window | 2026-08-18 → 2026-09-28, 41.4 days |
| Requests | 264,163 (252,422 real, excluding pings) |
| Sessions | 14,412 |
| Tenants | 18 with traffic |
| Real pings sent | 2,261 |

The live service was never written to, restarted or reconfigured. The snapshot was read with
SQLite's documented read-only mode; it was deliberately **not** opened with `immutable=1`,
which corrupts reads on a database that is actively checkpointing its write-ahead log.

### 3.2 Unit of analysis

**Figure 1 — `figures/gap_distribution_and_survival.svg`**

How long people actually stay away, and the survival curve that follows from it. Most gaps
close in seconds; a long thin tail stretches for hours. Every policy in this paper is an
attempt to act on the middle of that distribution.

A **decision point** is a request that has a later request after it in the same conversation
*and on the same model*. The model matters: a cached copy cannot transfer between models, so
treating one session that switched models as a single conversation silently invents cache
hits. There are **246,484** such decision points. This figure was independently reproduced
three times by separate analyses before anything was built on it.

### 3.3 What we are predicting

"Will they come back?" is not one question but three, and they have different answers:

1. **Will another request arrive at all?**
2. **If it does, will it still match the cached copy?** (A new topic means the copy is
   useless even though the person returned.)
3. **Would a ping actually have saved money?** (Only if the copy really expired and really
   would have been reused.)

A model can be excellent at (1) and worthless at (3). We kept them as separate labels
throughout, and Section 6 shows why that mattered.

### 3.4 Protocol

- **Chronological splits.** Train on the earlier 60% of elapsed time, test on three folds
  spanning the rest. Never a random split — that would let the model see the future.
- **Everything fitted inside the training fold**: preprocessors, encoders, thresholds,
  calibrators. One final fold was held out untouched and inspected once.
- **Grouped by conversation.** Every fit, split and bootstrap resamples at the conversation
  level, never the row level, so one conversation's later rows cannot leak into its own past.
- **Censoring handled explicitly.** A conversation still open at the window's edge is
  *unresolved*, not "never returned".
- **Leakage guarded structurally, then tested.** Features receive the present-tense state
  plus a list of earlier rows sliced so that its length equals its capacity — in Go this
  cannot be widened to reach the current or future rows, even by a feature deliberately
  trying. It was then tested empirically: the corpus was truncated at ten different cut-offs
  and every surviving row's features recomputed. Across **1,333,633 row comparisons** the
  largest difference was **0.0**.
- **Multiple comparisons corrected.** Every sweep reports Benjamini–Hochberg-adjusted
  results alongside raw, with the survival count stated.

### 3.5 Baselines, and why the usual one is wrong

Most work here compares a policy against *never pinging*. That is a flattering baseline,
because **pinging everything** already beats it by 5.7% with no model involved. So every arm
in this paper is scored against **both** never-ping and ping-everyone, and the second is the
one that matters.

---

## 4. Most quiet moments never reach a decision

**Figure 2 — `figures/funnel.svg`**

Before any cleverness is possible the moment has to reach the system at all. Of 219,650
quiet gaps in the window, only **37,418 (17%)** last long enough to be offered a ping
decision; the other 83% end before the 280-second timer fires. Of those that survive,
41.4% have no budget to spend.

And only **29.4%** of the survivors are *addressable* — backed by a copy that genuinely
expired. The pot is small, which is the first half of why the accounting noise in Section 7
can swamp it.

---

## 5. Result: no model beats counting to two

The best policy found is:

> **Ping at most twice per quiet gap. Set the cap per customer. Switch it off for the one
> customer where it cannot pay.**

It is worth **$619.77** over 41 days. It has no parameters, no training, and no features.

Roughly **thirty arms** were tried across four analyses that shared no code:

| Study | What it tried | Result |
|---|---|---|
| Feature ablation | leave-one-out and forward selection over 15 features | best subset loses by **$102.52** [+49.74, +174.86] |
| Seven model families | survival, hazard, forests, multinomial, shrinkage, ensemble, cost-aware | **all eight arms lose**, every interval excluding zero |
| Per-feature sweep | 18 features against three outcomes | 11 harm as a gate, 7 no signal, **none helps** |
| Dedicated hunt | exclusion rules, budget priority, value regression, boosting, multi-ping schedules | best is a **tie**; the tie is a verified no-op |

**Figure 3 — `figures/net_dollars_vs_ping_budget.svg`**

The ping-budget sweep shows the shape: going from one ping to two is worth a real $78; after
that the curve flattens and then falls, crossing into loss by sixteen pings. Two and three
differ by **$1.37** against a confidence interval roughly **$590** wide — noise, not a
finding, and a tempting thing to over-tune on.

**Figure 7 — `figures/per_tenant_gain_loss.svg`**

The same policy, customer by customer. Note that it does not help everyone equally, and that
one customer is harmed — which is why the recommended policy carries a per-customer
off-switch rather than a single fleet-wide setting.

### 5.1 The high-water mark for machine learning

The closest a genuine model came was a **value regression** — one that predicts the dollar
worth of pinging rather than the probability of a return, which is the theoretically correct
target. On the untouched holdout it scored **−$4.24 [−19.34, +10.79]**. That interval
straddles zero, so the honest description is *indistinguishable from doing nothing clever*.

### 5.2 Adding features stops helping after two

**Figure 4 — `figures/learning_curves.svg`**

Greedy forward selection: one feature reaches $365.68, two reach $389.74 — the peak — and
the remaining thirteen oscillate between $358 and $390 without ever beating it. The full
fifteen-feature model ($363.41) is **worse** than the two-feature one.

---

## 6. Why no model could win

Three independent measurements converge on the same explanation.

### 6.1 Coverage is the binding constraint

At the moment the system asks "should I ping?", roughly **19%** of those moments would
genuinely be rescued — more than twice the 8.70% bar. So the opportunity is real.

But keep-alive is only switched on for a **median 8.9% of the week**. All 30 live strategies
are identical where it matters — they all wait 280 seconds and all require a 20,000-token
conversation — and differ *only* in their schedules, which are weekday-only blocks like
11:00–13:00 plus 17:00–18:00, ranging from 5 to 30 hours a week.

| | |
|---|---|
| Replayed value, real schedules | **$360.19** |
| Replayed value, no schedule restriction | **$1,405.78** |

**No feature a model can look at is "which admin window is active right now."** A predictor
cannot win back money a schedule withholds before the request ever reaches it. This is a
structural explanation, not a statistical one: it explains *why* every learned arm lost
rather than merely recording that they did.

### 6.2 The best signal is used backwards

The most informative single feature is **why the AI stopped talking last time**. The shipped
rule uses it as a filter — and the filter loses money.

Among conversations that reach a ping decision:

| Last response | Rescued | What the rule does |
|---|---|---|
| paused to run a tool | **79.0%** | **excluded** |
| stopped at a stop-sequence | **73.2%** | **excluded** |
| finished an answer | **53.1%** | the only one it pings |

The rule keeps the weakest of the three and discards the two strongest. Measured against
never-ping it looks good (**+$361.47**); measured against ping-everyone it **loses $249.45
[−374, −152]**.

The mechanism is *not* that the 280-second wait already used the information — the spread
between groups is **9.2 points** across all requests and **25.9 points** among those
reaching a decision, i.e. **2.8× wider**. The mechanism is **forgone coverage**: excluding a
group takes all of its rescues with it, and those cost more than the pings saved. Which makes
this the same finding as Section 6.1 one layer down — the schedule withholds coverage by the
clock, the gate withholds it by the signal.

**Figure 10 — `figures/feature_bucket_heatmap.svg`**

Every feature against every time bucket, over 261,895 decision points. Read it for shape
rather than detail: a few rows carry visible structure and most are close to flat.

**Figure 11 — `figures/feature_strategy_matrix.svg`**

The same features against each live strategy's outcome. The confound to keep in mind is that
all 30 strategies differ *only* in their time windows, so some of this is a feature predicting
whether the clock was inside a window rather than predicting anything about the conversation.

### 6.3 Accuracy is negatively associated with money

**Figure 5 — `figures/auc_ranking.svg`**

Scored on one identical test population of 72,302 rows, the seven model families span
**AUC 0.7047 to 0.8438**. Against their dollar results:

| Family | AUC | Behind the cap |
|---|---|---|
| calibrated ensemble | 0.8438 | −$85.12 |
| hierarchical shrinkage | **0.8322** | **−$191.26** (worst) |
| random forest | 0.8004 | −$90.61 |
| multinomial | 0.7781 | −$95.75 |
| discrete-time hazard | 0.7715 | −$86.80 |
| Weibull survival | **0.7047** (worst) | **−$82.96** (best) |

**Figure 8 — `figures/calibration_reliability.svg`**

The best learned model is genuinely well calibrated: when it says 17%, reality comes out 17%.
This is not a bad model being beaten by a good rule. It is a good model that cannot reach the
money.

**r(AUC, dollars) = −0.449.** The worst-ranking family delivers the best dollar result; the
second-best-ranking delivers the worst. With six families that correlation is suggestive
rather than significant, but the two extremes need no statistics.

A team optimising for accuracy on this problem would have walked steadily away from the money.

---

## 7. The number itself will not settle

**Figure 6 — `figures/per_ping_cost_percentiles.svg`**

Per-ping cost on this corpus is extremely heavy-tailed. Of the 2,261 real pings sent, half
cost under **$0.054**, but the dearest single ping cost **$4.40**, and the **dearest 10%
carry 41.8% of all ping spend**.

On a distribution like that, the *methodology* swamps the effect. Five independent
implementations of the same quantity produced answers from **losing $409** to **saving
$620**, crossing zero twice. A sixth rebuilt one of those calculations from scratch — same
stated method, no shared code — and disagreed with the original by **10–40%**.

**Generalisable claim: for a policy whose cost distribution is this heavy-tailed,
retrospective replay cannot resolve the sign.** The fix is not more arithmetic. It is a live
randomised experiment, which this work did not run.

This is why the paper's confident claims are all **rankings** rather than levels. Four
studies agree on the ordering; none agree on the magnitude.

---

## 8. An operational defect, found by accident

While measuring ping costs we found that the expensive tail was not expensive-but-useful
pings. It was **failures**.

**32 pings** billed a cache *write* with a cache *read* of zero — they arrived to find no
copy and paid full price to create one. They are 1.4% of pings and **20.1% of ping spend**.

Diagnosis, using the cached copy's own fingerprint rather than a timing proxy:

| Cause | Pings | Waste |
|---|---|---|
| **arrived too late** | **27** | **$44.73** |
| no copy had ever been made | 1 | $0.00 |
| **the copy had changed** | **0** | **$0.00** |
| genuinely unexplained | 4 | $3.97 |

On all 27 the fingerprint was **unchanged** — so not one was caused by a superseded copy.
And they miss by *seconds*: the smallest overshoot is **696 milliseconds** past a
300-second lifetime, and the most expensive ping in the corpus missed by 18.2 seconds. The
trigger fires at 280 seconds, leaving a 20-second margin, and every one of the 27 consumed
that margin in full.

**Unlike everything else here, this one needs no dollar estimate.** A late ping pays 1.25 to
write the copy; the person then reads it for 0.10, total 1.35. Without the ping they would
have written it themselves for 1.25. So a late ping costs *more than doing nothing even in
its best case*, and the whole 1.25 is wasted if they never return. **It cannot pay off in any
outcome.**

The obvious fix — fire earlier — was costed before being proposed, and loses:

```
move the trigger 280s -> 220s
  waste recovered     +$30.75
  newly wasted pings -$132.58   (1,524 conversations return on their own in
                                 that band and would now be pinged needlessly)
  net                -$101.83   -> 4.3x worse
```

**So the fix is the delivery, not the timer**: make the ping actually land inside the
existing window. That adds no new pings at all.

---

## 9. What is new here (provisional)

The literature pass is still running, so these are claims to be tested rather than
established. Listed in descending confidence:

1. **Coverage rather than prediction as the binding constraint**, with a quantitative
   decomposition from the naive conditional rate down to the replayed figure.
2. **A negative association between ranking accuracy and dollar value** across unrelated
   model families on one population.
3. **Denominator mismatch as a first-class hazard** — see Section 10.
4. **A categorical level that is a client-dialect artefact rather than a behaviour** — see
   Section 10.
5. **Non-proportional hazards over the idle gap** (Cox global *p* = 0.0): a covariate's
   influence on returning changes as the silence lengthens. This is consistent with 6.2's
   widening spread and is why no single fixed threshold captures the decision.
6. That a zero-parameter rule beats every learned model, and that heavy-tailed replay cannot
   resolve a sign, are both likely **incremental** given prior keepalive-economics work.

---

## 10. What we got wrong

Twelve claims were published during this work and then withdrawn. They are reported because
the pattern is more useful than any single result.

**Four of the twelve were the same error: comparing two rates with different denominators.**
On this corpus the same underlying quantity reads as **1.96% / 2.60% / 3.65% / 19% / 41.9%**
depending only on the population you condition on — a **21× range, every value correct**.
Comparing any two of them produces nonsense. The most consequential instance was published
as "the clearest actionable result in this body of work" and inverted on correction: the
customer to switch off was not the one named.

**The rule that would have prevented all four:** before comparing two rates, write down what
each is a percentage *of*.

**Figure 9 — `figures/band_rate_tenant_mix_verification.svg`**

A trend that turned out not to exist. The pooled rate appears to double over the window while
the per-customer average stays flat — the apparent trend is one large customer's share of
traffic falling from 50.6% to about 20%, not any change in behaviour. A plain per-week chart
of the pooled line would have shown a clean and entirely spurious result.

**One was a client-dialect artefact.** A group of conversations appeared to be a dead end,
rescued 0.03% of the time, and the paper nearly recommended excluding it. In fact all 13,007
of those rows come from OpenAI-dialect clients, belong to **one** tenant, and span a ten-day
integration burst that ended entirely inside the training window. The word in question is
that SDK's spelling of a *normal completion*. The proposed experiment is therefore
**untestable on this corpus** rather than wrong — and anyone building features over a field
that spans several client libraries should check the client breakdown *per category* before
believing the category means anything.

**An adversarial review found five more**, two material: a baseline bug that shifted an
absolute figure by $8.56 across two studies, and an "information ceiling at AUC 0.78" that
does not exist (families span 0.70–0.84). Notably, **the central conclusion survived because
it was expressed as a difference rather than a level**, making it immune to the baseline bug
that corrupted the absolute numbers.

---

## 11. Limits

- **Everything is replay, not measurement.** No live randomised experiment ran. The
  ranking is robust across four studies; no magnitude is.
- **Every ping is modelled as succeeding**, refreshing for its full lifetime, with no early
  eviction. This is unverified optimism and it biases every figure in the same direction —
  towards keep-alive looking better than it is.
- **Roughly 19 of 38 candidate features do not exist** in this schema and cannot be built:
  no subagent parent/child graph (0 of 264,163 session ids encode a parent), no per-tool-call
  timing, no agent phase or task outcome, no tenant timezone, no project identity.
- **The deployed binary is not built from the main branch**, so code readings describe
  something slightly different from what runs.
- **One three-day window at the start of the corpus** carries two unrelated billing
  artefacts and should be excluded from any time-series figure.
- **Related work is not yet surveyed**, so Section 9 is provisional.

---

## 12. What to do

1. **Ask the provider why the one-hour cache tier is never granted.** The client requests it
   **8,248 times** and receives it **zero** times. Bridging an hour by pinging costs ~12
   pings × 0.10 = 1.20× the prefix; the one-hour copy costs 0.75× once. Worth ~**$1,134** per
   41 days with no model, no code and no predictor.
2. **Fix ping delivery so pings land inside the existing 20-second margin.** ~**$44.73** of
   provable pure waste, and it adds no new pings.
3. **Widen the keep-alive schedules** for customers that clear break-even, and use the
   off-switch for the one that never will. Not the full $1,045 headroom — see below.
4. **Run the live randomised experiment.** It is the only way any of these numbers becomes
   measured rather than modelled, and it is cheaper here than in comparable studies because a
   ping never mutates the client's own request bytes, so it cannot contaminate the comparison.
5. **Do not build a predictor.** Four studies, thirty arms, and none beats a two-ping cap.

**Combined ceiling: $1,180–$2,224 per 41 days** (~$10,500–$20,000/year). The range is honest
rather than lazy: items 1 and 3 are *alternatives competing for the same token mass*, not
additions, and the width of the range is exactly the unknown overlap between them. Summing
every component would give $2,224 and no configuration could deliver it.

**None of the three actionable items is machine learning.** They are a request the provider
is ignoring, a schedule switched off nine-tenths of the week, and 27 pings missing a
twenty-second window.

---

## 13. Reproducing this

All figures are regenerated from committed scripts against the read-only snapshot. Every
script opens the store `mode=ro` and writes nothing outside its own output directory.

| Script | Produces |
|---|---|
| `deploy/harbor/kv_ttl_cost_model.py` | baselines, the oracle, the cost model |
| `deploy/harbor/kv_ttl_ka_arms.py` | the arms table, calibration, generalisation |
| `deploy/harbor/kv_ttl_ablation.py` | leave-one-out, forward selection, the leakage audit |
| `deploy/harbor/kv_ttl_families.py` | the seven model families |
| `deploy/harbor/kv_ttl_feature_univariate.py` | per-feature evidence against three outcomes |
| `deploy/harbor/kv_ttl_featcorr.py` | feature × time-bucket and feature × strategy |
| `deploy/harbor/kv_ttl_predhunt.py` | the twelve novel arms |
| `deploy/harbor/kv_ttl_examples.py` | per-feature examples, drift, ping diagnosis |

Machine-readable aggregates behind every figure are in `exp2/`, `exp3/` and `exp4/`, each
recording the exact command that produced it.

Privacy: tenants appear only as pseudonyms `T01`–`T18`, assigned by sorted raw identifier. No
script in this study reads message content, and every output was scanned for raw identifiers
and session UUIDs before release.
