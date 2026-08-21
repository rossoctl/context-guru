# What can and cannot be measured here

This page exists because the hardest part of evaluating compaction turned out not to be building
it, but establishing what any given experiment is *capable* of showing. Several results in this
repo were invalid for reasons that had nothing to do with the components — sample sizes with no
power, a harness blind to a whole class of defect, and yields that measured a component's
*economic throttle* rather than its capability.

Read this before designing an arm or believing a number.

## 1. Statistical power — reward at small n shows nothing

Reward on LOCA is **binary per task**, so a paired comparison uses McNemar's exact test on
discordant pairs. That test is brutal at small n.

[iteration 004b](../experiments/loca/iter004b/results.md), 12 tasks:

| comparison | gained | lost | discordant | p |
|---|--:|--:|--:|--:|
| deterministic vs baseline | 0 | 0 | **0** | 1.000 |
| + prefix reach vs baseline | 2 | 1 | 3 | **1.000** |

**What n=12 can detect at all:**

| discordant pairs, all one direction | p |
|---|--:|
| 4 | 0.125 |
| 5 | 0.0625 |
| **6** | **0.031 ← first significant** |

So compaction must flip **≥6 of 12 tasks (50 percentage points)** to register. Nothing smaller is
detectable **in either direction**, which means a null result at this size is not evidence of
safety.

**What a realistic effect needs:**

| discordant rate | split | n (80% power, α=.05) |
|---|---|--:|
| 25% | 70/30 | **194** |
| 25% | 65/35 | **347** |
| 40% | 65/35 | 216 |

**~200–350 tasks per arm.** LOCA has 75, so even the full set detects only ~20pp effects.

### Two consequences that keep being forgotten

**Zero discordance is stronger evidence than a p-value.** The deterministic arm's per-task outcome
string was *byte-identical* to the baseline — `010000101010`, same tasks solved, same tasks failed.
That is not "no significant difference", it is "no difference", and it is the most reassuring number
in this work. Still n=12, but qualitatively unlike a 4-vs-5 split.

**"Reward-neutral" is a non-inferiority claim.** It needs *more* power than showing a difference,
because failing to find an effect is not finding its absence. Every reward statement here should be
read as **unmeasured**, not confirmed.

### The affordable version of the claim, priced

Superiority on a binary reward is the expensive claim; **bounding harm is the cheap one, and is
usually what is actually being asked.** Measured on LOCA at $7.59 per task per arm
([iteration 007](../experiments/loca/iter007/results.md)):

| pairs | cost (2 arms) | upper 95% bound on harm if 0 tasks harmed |
|---|---|---|
| 10 | $152 | ≤ 26% — worthless |
| 20 | $304 | ≤ 14% |
| 40 | $607 | ≤ 7% |
| 60 | $911 | ≤ 5% |
| 100 | $1,518 | ≤ 3% |

Compare the superiority cost at the observed ~10% discordance rate: 119 pairs at a realistic 80/20
split, i.e. **$1,800**, to detect an effect nobody claimed. **The margin must be declared before the
run and the budget chosen to buy it** — otherwise the run silently purchases the top row.

### Contrast: token measurements are near-exact

Component savings (`format` at 92–99% of total, `extract_llm` 38 firings/494k tokens, `coref` 61/638k)
are counts over a fixed request set from deterministic components, not sampled estimates. They need
no significance testing. **The asymmetry is the point: we know precisely what is removed and almost
nothing about what it costs.**

## 2. Replay is not validation

The `/compact` endpoint runs a pipeline and returns the rewritten body **without forwarding it
upstream**. No provider ever validates it. So replay can tell you *what a component removed* and is
**structurally incapable** of telling you *whether the result is a sendable request*.

This blind spot hid three provider-rejecting defects in `summarize`
([iteration 005](../experiments/loca/iter005/results.md)), each masked by the previous one, found
only by sending live traffic one at a time. It silently covered every `/compact`-based result here:
[density](coref-density.md), [the eval-box pass](coref-evalbox.md),
[component gating](component-gating.md), and
[iteration 002](../experiments/loca/iter002/results.md) — whose deferral figure came from a pipeline
that 400s in production.

**Mitigated, not solved.** `schema.ValidateShape` + the all-presets test now catch this class
statically in 0.07s (verified against the real defects). Live runs remain the other half, but no
longer carry the whole burden.

## 3. Eligible ≠ acted — yield measures the throttle, not the capability

`coref` acts only after clearing **four** economic gates, all on by default:

| gate | default | refuses when |
|---|---|---|
| `trigger` | request shape | the request is too small to bother |
| `min_batch_frac` | **0.05** | the batch cuts <5% of the request |
| `break_even` | **true** | `S × T ≤ 11.5 × W` |
| `rewrite_budget` | **3** | 3 prefix rewrites already spent this session |

So a reported yield is **"what `coref` could remove *and* justify"**, never "what `coref` could
remove". Three numbers must be reported separately or the result is uninterpretable:

- **eligible mass** — from `class_*` gates: what the index judged dead
- **acted mass** — what survived all four gates
- **refused-for-economics** — `batch_too_small` + `break_even` + `rewrite_budget`

**Why it changes the conclusion.** If acted-yield is negligible *but* refused-for-economics is
large, the finding is **not** "`coref` does not work" — it is "`coref` is economically throttled".
Those demand opposite responses, and without the gate breakdown they are indistinguishable.

## 4. Break-even is sometimes vacuous

`S × T > 11.5 × W` prices a cache-write the mutation *causes*. When the provider's cache has already
expired, the prefix is rewritten **regardless** — the mutation costs nothing incremental and needs
no break-even test at all. Those moments arise whenever the inter-turn gap exceeds the cache TTL: a
slow tool, a queue, a human thinking.

Combined with §3: **`coref`'s ceiling is set by break-even, and break-even is sometimes wrong.** Its
real capability is higher than any measured yield, by exactly the mass refused for economics at
moments when the rewrite was already free.

Measured on LOCA (`deploy/harbor/cache_opportunity.py`, from per-step `cache_read_input_tokens`):
**zero such moments** — LOCA drives local mock MCP servers, so turns land seconds apart and a
5-minute TTL never lapses. A property of the benchmark, not an answer. See
[the proposal's design notes](../proposals/coref-compaction.md#two-design-notes-from-review-neither-implemented).

## 5. Cost model, measured

Per-task cost at the 64k band, from real runs — **not** estimates:

| arm shape | $/task | 75 tasks | model calls |
|---|--:|--:|--:|
| passthrough baseline | 1.78 | **134** | 0 |
| `format` only | ~1.15 | **~86** | 0 |
| deterministic (`+dedup +extract`) | ~1.22 | ~92 | 0 |
| **`+coref`** | ~1.25 | ~94 | **0** |
| `+extract_llm` (tail) | 1.89 | ~142 | haiku |
| `+extract_llm` (prefix) | 1.89 | ~142 | haiku |

**Deterministic arms cost *less* than the baseline** — 31% less in one measured case — because
compaction reduces the agent's own token bill and makes no model calls. `coref` is deterministic, so
only the two `extract_llm` arms carry LLM spend.

Six arms ≈ **$690** at k=1. Earlier I quoted "$315/arm", which was actually the *three-arm total* and
ignored that deterministic arms undercut the control.

**But removing tokens is not saving money.** The prefix-reach arm removed 20.3% and cost *more* than
baseline ($22.64 vs $21.34): model calls plus pipeline overhead outweighed the saving.

## 6. Benchmark suitability

| benchmark | long context | tool outputs big enough | reward signal | slow tools (TTL) | verdict |
|---|---|---|---|---|---|
| SWE-bench Verified | **no** (max 46k) | **no** (max 2,760 tok) | binary, n=500 | **yes** | ruled out for compaction; **right for the TTL question** |
| Terminal-Bench 2.0 | no (~6k) | no (max 1,906) | binary, n=89 | yes | ruled out |
| **LOCA-bench** | **yes** (dial 8k→256k) | **yes** (max 59,857) | deterministic but **binary**, and n=75 is **n=15 by default** (see below) | **no** | the only viable vehicle *for savings*; cannot power reward |
| UltraHorizon | yes (200k+) | yes | **LLM-judged** | ? | noise we cannot afford; no licence |
| Claude Code transcripts | yes | yes | **none** | yes | no reward → cannot gate |

**The unclosable gap: nothing is both long-context and Tier-1-rich.** LOCA is long but Tier-2/3-heavy
(adverse to an exact-match detector); SWE-bench is Tier-1-rich but short. So a null result on LOCA
**cannot be generalised**, and that limit should be stated in advance rather than discovered.

LOCA band behaviour, measured:

| band | runs | baseline accuracy | usable |
|---|---|---|---|
| 8K (`debug`) | yes | **1.0** saturated | regression control only; only `format` fires |
| **64k** | **yes** | **20% (2/10 clean tasks)** | pressure yes, headroom **thin** — see below |
| 128k | yes (needs the pairing shim) | **0.0** collapsed | zero floor measures nothing at feasible n |

### Two properties of LOCA that cap what any reward arm here can conclude

**`group_by_seed` silently divides your n by five.** It defaults to `True` and is **not exposed as a
CLI flag** — it is a parameter of `run_claude_api` that the Typer wrapper does not surface. A "75-task"
config therefore runs **15** tasks. Reaching n=75 means patching LOCA's source *and* paying 5× per arm.

**The 64k base solve rate is ~20%, and accuracy is binary.** `format` solved 2 of 10 clean tasks; every
accuracy value is exactly 0.0 or 1.0, so there is no partial-credit signal to recover power from. A 20%
ceiling means most tasks fail for reasons no context-management component can affect — they cannot
register improvement *or* degradation, so they consume budget while contributing nothing but a tie.
Trajectories were 9–53 tool calls, well short of the ~106 ceiling, and the agent terminated on its own:
these are genuine task failures, not truncation.

Together these are why [iteration 007](../experiments/loca/iter007/results.md) was stopped rather than
completed.

## 7. Rig traps that produced valid-looking wrong numbers

Each of these yielded a numerically plausible result with a broken cause. Check them before
believing any benchmark output.

| trap | symptom | real cause |
|---|---|---|
| Claude Code under QEMU | `NonZeroAgentExitCodeError`, `reward=0` on every task | bun binary segfaults; amd64-only images on arm64 |
| `\| tail` on LOCA's stdout | `[Errno 11] write could not complete without blocking` on every band >8K | **my pipe**, not LOCA's MCP transport |
| `.venv/bin/loca` invoked directly | tools silently absent; agent reports an empty workspace | `.venv/bin` not on `PATH`, so spawned MCP servers got system python 3.9 |
| stale proxy binary | a feature arm behaves exactly like the arm without it | binary predated the feature; **every arm must record its build** |
| `summarize` in a shared pipeline | HTTP 400s | it must run alone — and separately had 3 shape defects |
| back-to-back arms | later arms look cheaper | they inherit the earlier arm's **prompt cache**; run the baseline last and never read cost as a clean saving |
| `pkill -f <pattern>` | command dies mid-script, silently | the pattern matches **its own** command line; use `arms6[4].sh` |
| `tool_success_counter: 0` | looks like broken tools | appears on **any** failure; not diagnostic |
| `--max-tool-uses` | looked like a cap | agent stopped at ~106 calls regardless of a 4× cap change |

**The meta-lesson:** a broken *environment* produces a plausible transcript and a real-looking score.
Before believing a benchmark number, confirm the tools worked — per-task `eval.json`, gate counters,
and a transparency assertion on the `off` arm (0 saved, no component acted).

See also: [the proposal](../proposals/coref-compaction.md) ·
[experiment log](../experiments/README.md) ·
[selection experiment](coref-selection-experiment.md)

### Untracked scratch tooling is the least-reviewed code in the measurement path

`repair_shim.py` — an ~80-line HTTP hop between LOCA and the gateway, living only in `/tmp` on the
eval box, never committed, never reviewed, never tested — silently dropped the body of every request
the client happened to send with `Transfer-Encoding: chunked`, and forwarded `chunked` together with
`Content-Length: 0`. Result: intermittent `400 Bad request` HTML that was attributed first to a
component under test and then to the benchmark, across two iterations.

Two structural lessons, not one:

1. **Error counts that move opposite to the amount of machinery indict the harness.** The arm running
   `format`+`coref` produced *one* such error; the arm running only `format` produced *three*, and
   succeeded on none of the three tasks the other arm passed. No component-caused defect has that
   shape. That signal was in the data before the root cause was found.
2. **A permissive component will hide a protocol violation until it reaches a strict one.** A local
   Python echo server accepted the contradictory framing with a 200. Only the real gateway rejected
   it. Testing the shim against a lenient stand-in would have "passed."

The shim is scratch tooling by necessity (it repairs bodies for replay), but everything in the
measurement path deserves the same suspicion as the thing being measured — and it is the code *not*
in the repo that gets none.
