# Iteration 025 — results: the substantive endpoints are uninterpretable, and the rig is why

**Pre-registered** in `PREREGISTRATION.md` before launch, with Amendment 1 recorded before the run and
the drift check's target frozen before its pass. Every number below was read after that commit.

## Headline

| | iteration 024 | **iteration 025** |
|---|---|---|
| clustered, 15 tasks (GOVERNING) | 8 better, **0 worse**, 7 tied · p = **0.0213**/0.0078 | **6 better, 3 worse, 6 tied · p = 0.5078** |
| harm — CP 95% upper on worsened, **at the governing clustering** | 21.8% (0 of 15) | **48.1% (3 of 15)** |
| harm gate (>25% blocks any positive claim) | clears | **BLOCKED** |
| sweep firings, arm B | 151 per arm-seed, **85.7%** of decisions | **34 across five seeds, 2.9%** |

**The registered reading is blocked, no positive claim — but see section 0 before reading any of it as a
test of the mechanism, because it is not one.** The mechanism barely ran, and it was prevented from
running in exactly the conditions it was designed for.

## 0. CORRECTION, recorded after the run: half these decisions were taken in a state no gate could act in

**Raised by @davidamid: if the 64k window is imaginary, was `estimateTurnsRemaining` not simply wrong?**
It was, and the consequence reaches the headline of this document.

`estimateTurnsRemaining` returns 0 whenever `reqTokens >= CtxWindow`, and `CtxWindow` here is the
**declared** 64k, not the model's real capacity. At a request of 88,378 tokens it therefore reported "no
turns remain" while the agent continued for dozens more and one transcript reached 612,290. Splitting
every econ decision on that term:

| region | decisions | fired | rate |
|---|---|---|---|
| all | 1,153 | 34 | 2.9% |
| **valid** (`haveTurns > 0`) | 520 | 34 | **6.5%** |
| **structurally dead** (`haveTurns == 0`) | **633 (55%)** | **0** | **0.0%** |

So the 2.9% quoted below is diluted by 633 decisions where firing was **impossible by construction**. The
defensible figure is **6.5%**.

**And iteration 024 was exposed to the identical false T** — 97 of its 589 asks were also above 64k. It
fired anyway, because the `rewritten <= 0` free-pass branch skipped the T comparison entirely. So this
branch's fix removed the bypass and thereby made T load-bearing exactly where T is least trustworthy. Its
85.7% authorisation rate is inflated in the opposite direction, by firings that happened only because the
gate ignored T, which means **6.5% and 85.7% are not comparable either** and no clean valid-region
comparison is available: iteration 024's binary records no pressure field.

**This is not a code defect.** In production `CtxWindow` is the real window from model-info, a request
cannot meaningfully exceed it, and the function is sound; in the *simulated* 64k world, sitting at 88k is
an impossible state and 0 is a defensible answer to an impossible input. The defect is that **the rig
produced impossible states and this iteration then measured the gate inside them** — see limit 2.

### The reward comparison is uninterpretable too, and that is the bigger casualty

A first draft of this correction said the reward null "stands, because it measures what actually ran".
@davidamid pushed further and is right: **a comparison in which the treatment did not operate cannot test
the treatment.** Arm B fired 34 times in five seeds, never on a request above 28,647 tokens, and was
structurally barred from acting on the deep, large transcripts the component exists for. "6 better, 3
worse" therefore measures *arm B as it behaved*, which is close to a tautology — a component that scarcely
acts produces scarcely any effect — and it says nothing about whether the mechanism helps when it runs.

**WITHDRAWN: "the reward result does not replicate."** That phrasing implies iterations 024 and 025 were
comparable tests of one mechanism. They were not: iteration 024 swept transcripts up to 348,869 tokens,
96 of its 589 asks above the band, while this iteration swept none above 28,647. The single condition that
differs most is the one where removal has the most to remove.

The pattern that hypothesis predicts is present, and is **not** offered as evidence for it:

| | mean delta | worse |
|---|---|---|
| 5 largest tasks (641k–1.47M baseline input tokens) | **−0.050** | 2 of 5 |
| 5 smallest tasks (57k–247k) | **+0.200** | **0 of 5** |

Directionally what "it helped where it could fire" would look like. But each regression is one or two
task-seed flips (`NhlB2bAnalysis` 2/4 against 1/4; `FilterLowSellingProducts` 1/5 against 0/5) on a
benchmark this repo has measured at 0.200 and 0.800 on identical config. Consistent with the hypothesis;
no support for it at this n.

**What this iteration does license.** The gate machinery works as built — it charges, measures, converges
to within 3% of an independently measured ask cost, declines, and attributes declines to the right term.
The admissibility instrumentation works: `cost_source: component` throughout, `stash_refused: 0`, zero HTML
400s. And it found that **the rig does not hold its own band**, which turns `estimateTurnsRemaining` into a
fiction above it. That last is the most valuable thing here and it was not what the run was bought for.

**What it does not license:** any statement about the merged sweep's effect on reward, any statement about
the size of the ask charge's suppression beyond "6.5% in the valid region", and any cost conclusion
generalised past the unrepresentative sample of asks that survived (small transcripts only).

## 1. The gate declined almost everything (see the correction above before reading these rates)

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

The pressure distribution says where they are. **571 of the 1,119 declines were taken at or beyond 100%
of the declared window**, where `estimateTurnsRemaining` returns 0 by construction and nothing can repay
anything. (An earlier draft said 568; that was the count beyond 101%, the bucket table's boundary.)
Counted by the term that actually blocks them, **603 of the 1,119 declines — 54% — had `haveTurns == 0`.**

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

### What the fix actually closed: the over-window firings

`req_tokens` recorded at the asks that FIRED, which is the sharpest available statement of the change:

| | asks | median | p90 | max | at or above 64k |
|---|---|---|---|---|---|
| iteration 024 arm B | 589 | 37,236 | 86,972 | **348,869** | **97 (16%)** |
| iteration 025 arm B | 33 | 8,557 | 21,753 | 28,647 | **0 (0%)** |

Iteration 024 swept transcripts up to 349k tokens; this iteration never swept one above 28.6k.

At `reqTokens >= ctxWindow`, `estimateTurnsRemaining` returns 0, so `need > have` for any positive need
and the ONLY route by which the old gate could authorise was the `rewritten <= 0` free-pass branch — the
unconditional `return 0, T, true` that this branch converted to a clamp. **So those 97 over-window asks
existed solely because of the branch that was closed**, and they were the asks on the largest transcripts,
i.e. the ones with the most available to remove.

The tension cuts both ways and nothing in this iteration resolves it. Those asks genuinely were unpriced:
the cache-write was free, because the candidates sat past the cache boundary, but the adjudication was
not, and the old test charged neither. The fix is right that they were not free. It may equally be true
that they were the valuable ones. "Correctly stopped paying for asks that lost money" and "stopped making
the only asks that mattered" predict exactly this data, which is what makes the `econ_ignore_ask_cost` arm
the next run rather than a nicety.

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

**It does not show the merged sweep is worthless, and it does not show the reverse.** It shows that in
this rig the component was prevented from acting where it matters, so no reward statement is available in
either direction. Fix the rig before spending anything on the mechanism's behalf.

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
2. **THE 64k BAND WAS NOT HELD, and this is the largest limitation here.** `ctxWindow` is 64,000 —
   declared to the proxy through the model-info file — but it is a measurement device, not a limit: the
   real model is sonnet-5 at 1M, so nothing rejects a larger request, and LOCA's clearing is client-side,
   triggered on its own accounting *between* turns, clearing *at least* 16k rather than resetting.
   Requests therefore ran far past the band: median 88,378 tokens at the over-window decisions, p90
   194,259, **maximum 612,290 — 9.6x the declared window.**

   The consequence is not cosmetic. `estimateTurnsRemaining` returns 0 whenever `reqTokens >= window`, so
   in 603 of 1,119 declines the econ trigger was being consulted in a regime where it is *structurally
   incapable of authorising anything*, whatever its terms. Section 2 describes the gate locking itself
   out; the fuller statement is that the rig presents it with a state it cannot act in, and iteration 024
   largely avoided that state **because** it was sweeping. Any future run at a declared band should
   measure how much of the run actually sat inside it before treating the band as the independent
   variable. The enforcement was never the model's: there is no API parameter that caps INPUT
   (`max_tokens` bounds output only), so the band can only be held by the client, and
   `--clear-trigger-tokens 64000` with `--clear-at-least-tokens 16000` did not hold it. LOCA's clear
   reclaims tool-use blocks only, so mass in text and thinking cannot be recovered by it at all — a
   lower trigger and a far larger `clear-at-least` are the levers, and why clearing cannot keep up is
   worth diagnosing before the band is used again.
3. **Four of five seeds pair against a six-day-old baseline.** The drift check licensed it but is itself
   underpowered on accuracy (it needs ≥6 tasks moving one way to fail), so "no large drift detected" is
   the strongest available claim.
4. **Seed 2's baseline errored on one task** (`NhlB2bAnalysisS2LEnv`); that pair is dropped as unpairable,
   not scored 0. Counting it as 0 credited arm B with a win it did not earn and inflated the pooled
   discordant count by one before it was caught.
5. **Latency and the ≥100%-pressure finding rest on 34 firings and 1,153 decisions respectively** — the
   second is well-powered, the first is not.
6. **Arm configs are not shippable defaults** (`min_inventory: 3`, sweep `min_tokens: 100`), held identical
   across arms so they cannot bias B−A.
7. **`/metrics` remains aggregate** (#180); nothing here cross-checks against a Prometheus series.
