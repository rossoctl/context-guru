# Results — operating modes (sync vs async vs observe)

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`, `codesmart`
pipeline, cache-aware billed cost (fresh $2/M · cache-read $0.20/M · cache-write
$2.50/M · output $10/M) recomputed from each trial's token tiers. See
[REPRODUCE.md](REPRODUCE.md).

**Scale caveat, stated up front.** Nine trials total: 2 SWE-bench tasks and 2
Terminal-Bench tasks per mode at n=1, plus one real Claude Code session per mode. Enough
to validate the *mechanism* and to answer the latency and cache questions, nowhere near
enough for a cost or solve-rate claim. The 50-task arms in the other results pages are the ones to
cite for savings. What is measured here is whether each mode does what it says.

## SWE-bench Verified — 2 tasks, n=1

| | `sync` | `async` | `observe` |
|---|---|---|---|
| solved | 2/2 | 2/2 | 2/2 |
| mean steps | 15.5 | 21.5 | 22.5 |
| **added latency / req** | **1,599.4 ms** | **25.3 ms** | **0.062 ms** |
| content savings (enforced) | 0.82% | 4.17% | — (0 by construction) |
| projected savings | — | — | 6.40% |
| cache-read | 1,464,729 | 2,186,100 | 2,300,699 |
| **cache-write** | **52,287** | **42,980** | 127,589 |
| cache-write per 1M cache-read | 35,697 | **19,661** | 55,458 (not enforcing) |
| cache-hit rate | 96.55% | **98.07%** | 94.74% |
| fresh input | 54 | 78 | 86 |
| output | 10,249 | 10,711 | 11,522 |
| billed cost | $0.5263 | $0.6519 | $0.8945 |
| context-guru's own LLM cost | $0.0122 (1 call) | $0.0435 (4 calls) | $0.0779 (7 calls) |
| off-path compaction time | — | 80.8 s | 75.0 s |
| deferred compactions committed | — | 1 | 0 (never commits) |
| `async_realized_saved_tokens` | — | 15,962 *(retracted — circular, see below)* | — |
| queue `{dropped, stale_discarded}` | — | `{0, 0}` | `{0, 0}` |

## The four questions

### 1. Does async reduce added latency without increasing cache-write?

**Latency: yes, decisively.** 1,599.4 ms → 25.3 ms per request, a **63x reduction**.
The mechanism is visible per component: `extract_llm` costs 15,014 ms on sync's request
path and 71.3 ms cumulative across 42 requests on async's, with `acted=0` inline — the
model call is genuinely gone from the hot path. 80.8 s of compaction ran off-path,
charged to nobody's request.

**Cache-write: it was lower, but this measurement does NOT establish that the policy
caused it.** Absolute cache-write 52,287 → 42,980 (−17.8%) on ~49% more cache-read
traffic; normalised, 19,661 per 1M cache-read against sync's 35,697. Terminal-Bench showed
the same direction (13,185 vs 21,689).

Those numbers stand as measurements. The causal claim does not, and it was withdrawn after
review found the mechanism was not doing what the arms were credited to:

- On this workload the protection was **inert**. claude-code sets its own breakpoint on the
  newest message, and the policy only pruned positions `cacheinject` itself wanted — so the
  doomed tail was cache-written anyway. Whatever moved cache-write here, it was not the
  tail protection.
- Two other defects pushed cache-write down for uninteresting reasons: a session's first
  turn placed **zero** breakpoints, and with `cache_mode: off` breakpoints were suppressed
  entirely. Writing fewer breakpoints lowers cache-write; that is arithmetic, not a policy
  win.
- The arms are not paired: cache-read differs by 49%, trajectories by 6 mean steps, and
  the async arm was re-run separately after a port collision.

All four defects are now fixed, and the policy is proven **structurally** — unit tests
assert no breakpoint survives at or beyond the protected span, including one the caller
placed, and that the protection declines rather than pretends when it may not strip. The
*measured* confirmation needs a re-run on a 50-task paired arm with
`strip_caller_breakpoints: true`. Until then: mechanism verified by test, effect
unmeasured.

### 2. Does async reach the same steady-state savings as sync, just later?

**On this evidence it reached more, not less** — 4.17% enforced against sync's 0.82% —
but do not read that as async being better at compaction. Both numbers are small and
noisy at n=1, and the arms took different trajectories (different step counts, so
different traffic). **The `async_realized_saved_tokens` = 15,962 figure I originally cited as "the entire
enforced saving" was circular and is retracted.** The counter was recorded on every async
turn that saved anything, with no check that the saving came from deferred work — so it
re-reported the inline saving and necessarily equalled the total. Review demonstrated it
reading `saved=396, realized=396` on the first turn of a model-free pipeline, where no
deferred compaction existed at all.

It is now gated on the session having had a compaction actually land, and a test asserts it
is a **strict** subset of `saved_tokens` (equality is the signature of the old bug). Under
the corrected counter a controlled five-turn session reports `realized=4125` of
`saved=4948` with 1 legitimate `stale_discarded` — deferral demonstrably contributing part
of the saving, which is the honest form of the claim.

So this question is **not** answered by the arms above; the deferral mechanism is verified
by test, and its steady-state contribution on real traffic is unmeasured. What the arms do
show is where the work went: 80.8 s of compaction ran off-path against 25.3 ms/req on-path.

### 3. Does observe add measurable latency to the enforced path?

**No — 0.062 ms/req**, against sync's 1,599.4 ms on the same benchmark. Four orders of
magnitude, and it is structural rather than tuned: the request path does not run the
pipeline at all, so the only cost is copying the body and an enqueue. Confirmed
independently in a real Claude Code session: 0.209 ms in observe against 28.964 ms in
sync.

Observe is not *free* — it moved 75.0 s of compaction off-path and spent $0.0779 of
cheap-model tokens to do the measuring. It costs money and CPU, just not request
latency.

### 4. Do observe's projections match what sync actually achieved?

This is the question that validates the mode, and answering it honestly found **two
real bugs** — the most valuable thing the benchmark did.

The first comparison read **9.53% projected against 0.82% enforced**, an 11x
overstatement. Cause: the observe job ran without the session tracker, so its
cached-prefix boundary was unknown, the tail gate never fired, and 50 `extract_llm`
candidates passed where sync allowed 5. A projection that ignores cache-awareness
projects what a *cache-blind* proxy would do and overstates by exactly what
cache-awareness costs. Fixed by sharing the tracker.

That exposed a second error in the opposite direction: observe then **under**-projected
by ~3x, because it ran against a discarded buffer and so lost the frozen decisions
offloaders replay on every later turn — where most of the sustained saving lives. Fixed
by giving observe its own persistent-but-disjoint store.

After both fixes, on identical traffic through the real handler, projection and actual
agree **exactly**: 10,020 tokens / 23.06% each. A test pins it, and that test fails at
ratio 0.33 if the shadow store is removed.

On the SWE-bench arms the remaining gap is **6.40% projected vs 0.82% enforced**, and
that gap is *not* explained away — it is the honest discrepancy this section owes:

- The two arms are different agent trajectories (22.5 vs 15.5 mean steps), so the
  traffic differs. Observe saw 46 requests and 492,652 baseline tokens; sync saw 35 and
  244,319. These are not the same conversations.
- Observe's projection never pays a bounce. Nothing is offloaded, so no `expand` round
  trip can claw savings back, and `wasted_tokens` is structurally 0. Under `sync`, some
  savings do come back. Observe's projection is an **upper bound** on content savings,
  and is documented as one.
- 2 tasks at n=1 cannot separate a real bias from trajectory noise.

The Terminal-Bench arms add a **negative control**, which is the more convincing shape of
this evidence: there sync achieved 1.02% and observe projected **0%** — on traffic with
almost nothing to save, observe correctly reports almost nothing rather than inventing a
number. A projection that only ever agreed on high-savings traffic would be far weaker.

So: the controlled same-traffic test shows exact agreement, Terminal-Bench shows correct
agreement near zero, and the SWE-bench arms are consistent but too small and too
differently-shaped to confirm independently. A 50-task paired run is the honest next
step.

## Terminal-Bench 2.0 — 2 cache-sensitive tasks, n=1

A second, independent benchmark, and the reason it is worth reporting despite being
even smaller: it replicates the cache-write result on different traffic.

| | `sync` | `async` | `observe` |
|---|---|---|---|
| **added latency / req** | 26.9 ms | 26.8 ms | **0.076 ms** |
| cache-read | 3,144,887 | 6,311,918 | 3,200,254 |
| cache-write | 68,211 | 83,222 | 93,403 |
| **cache-write per 1M cache-read** | **21,689** | **13,185 (−39.2%)** | — (not enforcing) |
| cache-hit rate | 97.87% | 98.70% | 97.15% |
| content savings (enforced) | 1.02% | 0.11% | — (0 by construction) |
| projected savings | — | — | 0% |
| pipeline runs | 60 | 110 | 64 |
| context-guru's own LLM calls | **0** | **0** | **0** |
| off-path compaction time | — | 2.5 s | 0.6 s |
| `async_realized_saved_tokens` | — | 1,156 | — |
| queue `{dropped, stale_discarded}` | — | `{0, 0}` | `{0, 0}` |

Two things to read here.

**The cache result replicates.** Normalised cache-write fell 39.2% under async, against
45% on SWE-bench. Two different benchmarks, two different traffic shapes, same
direction. That is the strongest evidence in this page that the tail policy is doing
what it was designed to do.

**Async's latency benefit is proportional to how much LLM work the pipeline does, and
here it is zero.** `extract_llm` made **no** model calls on these tasks, so there was
nothing expensive to defer and added latency is identical (26.9 vs 26.8 ms). This is a
useful negative result rather than a disappointment: async buys back the compaction
model call, so on traffic that never triggers one it buys nothing. It also does not
*cost* anything there, which is the important half.

**Observe's projection is 0%, and that is the right answer.** This is the more
convincing half of the projection-accuracy question than the SWE-bench arms were,
because it is a negative control: on traffic where sync achieves almost nothing (1.02%),
observe correctly projects almost nothing (0%) rather than inventing a headline. A
projection that only ever agrees on traffic with large savings would be far weaker
evidence. Observe also reported the overhead sync *would* have added as 9.1 ms/req,
correctly small here since the pipeline made no model calls — while itself adding
0.076 ms.

Reward is not quoted from this arm: these are two hard tasks, one hit an
environment-build exception in each configuration, and the trajectories diverged sharply
(30 / 55.5 / 32.5 mean steps). At this scale the step counts drive the cost column
entirely.

## Real Claude Code sessions (one per mode)

Same prompt and workspace through each mode, live gateway:

| | `sync` | `async` | `observe` |
|---|---|---|---|
| requests (enforced) | 4 | 5 | **0** |
| `sync_enforced` / `async_enforced` | 4 / 0 | 0 / 5 | 0 / 0 |
| added latency / req | 28.964 ms | 17.797 ms | **0.209 ms** |
| baseline tokens | 6,025 | 8,124 | 6,025 *(as `actual_baseline_tokens`)* |
| queue `{dropped, stale_discarded}` | — | `{0, 0}` | `{0, 0}` |
| task completed correctly | yes | yes | yes |

All three produced the correct answer, so no mode broke the agent.

Two details worth noting. Observe's `actual_baseline_tokens` = 6,025 is *exactly*
sync's `tokens_before` = 6,025 on the same prompt — the hypothetical namespace accounts
for identical traffic identically, measured independently. And observe reports
`requests: 0` with every enforced aggregate at zero, which is the machine-readable form
of "context-guru did not modify anything".

## Metric namespace separation, verified in production

From the SWE-bench observe arm's `/stats`:

- enforced: `requests: 0`, `saved_tokens: 0`, `sync_enforced: 0`, `async_enforced: 0`,
  `components: {}` — all zero, all empty;
- hypothetical: `observe_hypothetical_requests: 46`, `actual_baseline_tokens: 492652`,
  `projected_optimized_tokens: 461112`, `potential_saved_tokens: 31540`,
  `potential_components: {…}` — fully populated.

No aggregate over the enforced rollups can reach a hypothetical, because they are
different accumulators with disjoint serialized names.

## Bugs the benchmark found that the tests did not

Recorded because the tests passed while all four were live:

1. **Deferred runs double-counted.** Off-path reports were stamped `async` and entered
   the enforced rollups even though nothing was forwarded — then entered again when a
   later turn replayed the decision on-path.
2. **`cacheinject` corrupted turn state off-path.** Its per-message divergence digests
   are turn state; a deferred job commits several turns later, so committing them
   replayed turn N's digests over turn N+2's.
3. **Observe overstated by 11x** (missing cache boundary).
4. **Observe then understated by 3x** (discarded frozen state).

Each is now covered by a test that fails without its fix.

## What is not established here

- Any cost or solve-rate claim per mode. 2 tasks, n=1. The billed-cost column tracks
  trajectory length more than it tracks mode.
- The cache-write result's *magnitude*. Its direction is solid and now replicated on two
  benchmarks (−45% normalised on SWE-bench, −39.2% on Terminal-Bench), but a quotable
  figure needs a 50-task paired run.
- Async under concurrency pressure: `dropped` and `stale_discarded` were 0 on every
  arm, so the drop and stale-discard paths are exercised only by tests, never yet by
  production load.
