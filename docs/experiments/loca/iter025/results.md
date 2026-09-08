# Iteration 025 — results: the reward result does not replicate through a gate that can decline

**Pre-registered** in `PREREGISTRATION.md` before launch, with Amendment 1 recorded before the run and
the drift check's target frozen before its pass. Every number below was read after that commit.

## Headline

| | iteration 024 | **iteration 025** |
|---|---|---|
| clustered, 15 tasks (GOVERNING) | 8 better, **0 worse**, 7 tied · p = **0.0213**/0.0078 | **6 better, 3 worse, 6 tied · p = 0.5078** |
| harm — CP 95% upper on worsened, **at the governing clustering** | 21.8% (0 of 15) | **48.1% (3 of 15)** |
| harm gate (>25% blocks any positive claim) | clears | **BLOCKED** |
| sweep firings, arm B | 151 per arm-seed, **85.7%** of decisions | **34 across five seeds, 2.9%** |

**The registered reading is unambiguous: blocked, no positive claim.** Not "the mechanism does not
work" — the mechanism barely ran.

## 1. The gate declined almost everything

| seed | requests | econ decisions | fired | % | declined: rewrite | declined: ask |
|---|---|---|---|---|---|---|
| 1 | 405 | 262 | 14 | 5.3% | 177 | 71 |
| 2 | 271 | 148 | 9 | 6.1% | 90 | 49 |
| 3 | 381 | 246 | 4 | 1.6% | 165 | 77 |
| 4 | 359 | 218 | 4 | 1.8% | 152 | 62 |
| 5 | 373 | 279 | 3 | 1.1% | 167 | 109 |
| **pooled** | 1,789 | **1,153** | **34** | **2.9%** | **751** | **368** |

Iteration 024's gate authorised 85.7% of the decisions put to it. This one authorised 2.9%, and the rate
*fell* across seeds (5.3% → 1.1%). Seeds 3, 4 and 5 removed **402, 228 and 1,245 tokens** — the component
was present and effectively inert.

## 2. Why: the charge is self-reinforcing, and the indirect effect is twice the direct one

`econ_ask_not_repaid` is the ask charge's own doing: **368 declines**. But `prefix_rewrite_not_repaid`
fires only when the decline holds *with a free ask and no discount* — i.e. decisions iteration 024's
arithmetic would also have refused. There are **751** of those, and iteration 024 recorded 25.

The pressure distribution says where they are. **568 of the 1,119 declines (51%) were taken at or beyond
100% of the declared window**, where `estimateTurnsRemaining` returns 0 by construction and nothing can
repay anything.

```
ask charge declines 368 asks
  → less content removed
    → transcripts grow (18,972 → 25,348 input tokens per request, +34%)
      → requests reach and pass the 64k window
        → T = 0, so 568 further declines on the ORIGINAL arithmetic
          → still less content removed
```

Every step is locally correct — at T=0 sweeping genuinely cannot pay. But the reason T=0 is that it
declined earlier, when T was large and sweeping *would* have paid. **The gate locks itself out, and the
evidence justifying each decline is produced by the previous one.** The same effect appears from the
other side: `sweep_inventory_below_min` fell from 389 to ~130 per seed, because outputs iteration 024
turned into markers are still sitting in the inventory here — which is also why *more* decisions reached
the gate (1,153) on *fewer* requests.

## 3. Cost — negative under both value models, which settles the question the pre-flight left open

| seed | calls | spend | tokens removed | $/1M removed | net (one-time) |
|---|---|---|---|---|---|
| 1 | 14 | $0.5057 | 48,549 | $10.42 | −$0.4960 |
| 2 | 9 | $0.2698 | 28,851 | $9.35 | −$0.2640 |
| 3 | 4 | $0.0746 | 402 | **$185.57** | −$0.0745 |
| 4 | 4 | $0.0724 | 228 | **$317.54** | −$0.0724 |
| 5 | 3 | $0.0550 | 1,245 | $44.18 | −$0.0547 |
| **pooled** | **34** | **$0.9775** | **79,275** | **$12.33** | **negative in every seed** |

The preregistration declared the value model open because the pre-flight's two answers disagreed about
the sign. **They no longer disagree.** Under S·T at the observed T≈30 for firing decisions, the gross is
≈$0.48 against $0.98 spent — negative, as the one-time model already said. The sweep lost money here on
either accounting, and the reason is visible in seeds 3 and 4: at $185 and $318 per million tokens
removed, the surviving asks are the *expensive* ones.

`cost_source: component` on both components in all five seeds; `unpriced_components` empty. The primary
cost endpoint is valid — it simply reports a loss.

## 4. Latency — the pre-flight's "doubling" was noise

Mean sweep latency **13,436 ms** across seeds (17,584 / 14,300 / 12,634 / 10,660 / 12,001) against
iteration 024's 17,894 ms. The pre-flight's 27,459 ms was a single-sample artifact and the open question
recorded in the preregistration closes in the boring direction — **but on 34 calls, so it is weak**.

## 5. #229: a pressure floor is refuted on this workload, at 1,153 decisions

| pressure | fired | declined |
|---|---|---|
| 0–10% | 12 | 9 |
| 10–20% | 7 | 5 |
| 20–30% | 8 | 81 |
| 30–40% | 6 | 90 |
| 40–50% | 1 | 67 |
| 50–100% | 0 | 299 |
| **≥100%** | **0** | **568** |

Firings: min 0.015, p50 **0.142**, p90 0.340, max **0.448**. **Not one firing above 45% pressure.** A 30%
floor would block 27 of 34 (79%); a 50% floor blocks all of them.

The econ arithmetic already does what a floor is meant to do, and in the correct direction: T shrinks as
pressure rises, so high-pressure asks are refused on their own merits. A floor would remove the only
firings that happen. **The failure mode here is sweeping too LATE, not too early** — which is the opposite
of the intuition #229 was raised on, and it is now measured rather than argued. Still LOCA-only; it does
not settle the long-coding-session case.

## 6. A defect in this iteration's own preregistration, recorded rather than quietly dropped

**The registered harm gate is very nearly unclearable at 15 clusters.** Exact Clopper-Pearson upper
bounds on the worsened proportion: 0 of 15 → 21.8% (just clears 25%), **1 of 15 → ~31.9% (blocks)**,
3 of 15 → 48.1%. So the gate passes only on a *perfect zero*, and any single worsened task blocks the
iteration regardless of effect size.

Iteration 024 cleared it at 11.2% — which is **correct arithmetic for 3 worsened of its 75 unclustered
pairs**, verified. But its effect came from the clustered test that its own preregistration names as
governing, while its harm bound came from the unclustered one, whose 75 pairs are 5 seeds of the same 15
tasks — correlated, and its own preregistration says so. At the governing clustering iteration 024's bound
is **21.8%, not 11.2%**: still clear, but by 3.2 points rather than 13.8.

This does not change iteration 025's conclusion — the effect is null (p = 0.5078) with or without the harm
gate. It does mean **"blocked" here should be read as "null, and the harm gate could not have cleared
anyway"**, and that the gate needs re-specifying before it is used as a decision rule again.

## 7. What this licenses, and what it does not

**It does not show the merged sweep is worthless.** It shows that *with the ask charged as currently
calibrated*, the component almost never acts, and a component that does not act produces neither benefit
nor harm — 6 tasks better and 3 worse is what a coin looks like.

The preregistration named the next step for exactly this outcome: **run the `econ_ignore_ask_cost` arm**
(~$260, no rebuild — the knob ships) to separate *"the charge suppressed a real benefit"* from *"there was
no benefit"*. That is now the highest-value dollar available, because iteration 024's 151-firing arm B and
this iteration's 34-firing arm B differ in both the code and the dose, and only that arm holds the dose
fixed while changing the code.

Two design changes are indicated and neither should be made on this evidence alone:

1. **A distinct gate for decisions past the window.** 568 declines currently land on
   `prefix_rewrite_not_repaid`, which reads as "the cache-write did not pay" when the truth is "we were
   asked far too late to pay for anything". That is the same two-causes-one-counter defect
   `econ_ask_not_repaid` was split out to fix, reintroduced one level down.
2. **The charge is right in isolation and wrong in aggregate.** Pricing the ask is correct — the
   pre-flight's 9-of-9 authorisation was indefensible — but a per-decision correction that changes the
   *distribution of future decisions* needs to be evaluated on the loop, not the decision.

## Frozen inputs and cost

| | |
|---|---|
| binary | `cg-i025-proxy-v03`, SHA-256 (first 32) `fc42520260d70e834bc2adac94372974`, verified before every pass |
| code commit | `5d58575` |
| passes | `i025As1` (drift check) + `i025Bs{1..5}` — 90 runs |
| measured spend | **$247.48**, against ~$301 estimated |
| drift check | 14 of 15 tasks identical, p = 1.0000, mean cost −9.3% → **no large drift detected** |
| admissibility | `cost_source: component` ×5, `stash_refused: 0` ×5, **0** HTML 400s, `cheap_model_price_unconfigured: 0` |

## Limits

1. **15 clusters.** Every conclusion is at n=15 tasks; the seeds add precision within a task, not clusters.
2. **Four of five seeds pair against a six-day-old baseline.** The drift check licensed it but is itself
   underpowered on accuracy (it needs ≥6 tasks moving one way to fail), so "no large drift detected" is
   the strongest available claim.
3. **Seed 2's baseline errored on one task** (`NhlB2bAnalysisS2LEnv`); that pair is dropped as unpairable,
   not scored 0. Counting it as 0 credited arm B with a win it did not earn and inflated the pooled
   discordant count by one before it was caught.
4. **Latency and the ≥100%-pressure finding rest on 34 firings and 1,153 decisions respectively** — the
   second is well-powered, the first is not.
5. **Arm configs are not shippable defaults** (`min_inventory: 3`, sweep `min_tokens: 100`), held identical
   across arms so they cannot bias B−A.
6. **`/metrics` remains aggregate** (#180); nothing here cross-checks against a Prometheus series.
