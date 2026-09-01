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

### Headroom: n is not the only thing that limits detection

Before any n calculation, ask how many tasks are *able* to show the effect at all. Direction matters,
and the two pools are disjoint:

- **Harm can only appear on tasks that currently pass.** A task that already fails cannot be broken
  visibly — it fails either way.
- **Improvement can only appear on tasks that currently fail.** A task that already passes cannot
  pass more.

Measured pools, from the matched 15-task comparison in
[iteration 008](../experiments/loca/iter008/results.md):

| band | pass → **can show harm** | fail → **can show gain** |
|---|---|---|
| 64k | **3** (of 12 clean) | 9 |
| 32k | **8** (of 15) | 7 |

At 64k the whole harm signal had to come from **three tasks**. At a 10% harm rate that is 0.3
expected visible events, which is why [iteration 007](../experiments/loca/iter007/results.md) could
bound harm only at ≤26% — there was almost nothing for harm to act on. Nor were the 9 failing tasks
useful: they failed under `format` alone, which is **lossless**, so they fail for reasons no
compaction component can influence. They cost full price and return a tie.

**Why ~50% is the optimum rather than merely "better".** For a binary outcome the available variance
is `p(1-p)`, maximal at `p = 0.5`:

| base rate | p(1-p) |
|---|--:|
| 0.25 | 0.19 |
| **0.53** | **0.25** |
| 1.00 | 0 |

For the *same* underlying effect, the observable signal is largest near 50%. The whole band table in
section 6 is this one fact:

| band | rate | failure mode |
|---|---|---|
| 8k | 100% | **ceiling** — only harm can appear, improvement is invisible |
| **32k** | **53%** | both pools populated |
| 64k | 25% | near-**floor** — harm pool nearly empty |
| 128k | ~0% | **floor** — nothing passes, so nothing can degrade |

This bites hardest on the two-sided claim below. If deferring a wipe can *raise* accuracy, that needs
a pool of currently-failing-but-recoverable tasks — and at a 25% base rate the large failing pool was
mostly tasks failing for unrelated reasons.

### The headroom limit was ours, not the benchmark's

Worth recording as a reasoning error, not just a result. The conclusion drawn at 64k was "LOCA lacks
the headroom for this" — a claim about the *benchmark*. The 32k run used the **identical 15 tasks**,
same classes and seeds, differing only in data volume (600 games / 6 teams instead of 1200 / 8), and
scored 53%.

Same benchmark, same tasks, one config knob, **25% → 53%**. The limitation was a property of the
chosen configuration that had been generalised into a property of the benchmark. That is the same
shape as the three rig artifacts in section 7 (chunked bodies, EAGAIN, the 128k "collapse"): each
looked like a fact about the world and was a fact about the setup.

**Standing rule: treat "the benchmark cannot do X" as a hypothesis about your own configuration until
a matched run says otherwise.** A matched run is cheap — this one cost $85 and saved several hundred.

(Caveat kept visible: the 64k figure is 3/12 because three tasks errored on the broken shim, so its
true rate carries some uncertainty. The 25%-vs-53% gap is far too wide for that to account for.)

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

### The effect is two-sided: compaction can *raise* accuracy, not only risk it

The non-inferiority framing below is necessary but incomplete, and taking it as the whole story
understates what compaction can do.

"coref can only harm accuracy" is true against a baseline that keeps the **full** context. That is
not the baseline in any run here. LOCA's native trimmer **drops whole messages** once a request
outgrows its limit — that is precisely why `loca_repair_shim.py` has to repair `tool_use`/`tool_result`
pairing at all. So at any band where the trimmer fires, **the baseline is itself lossy**, and
selective removal that keeps what is still referenced can preserve *more* usable context than a blunt
drop. That is a mechanism for coref to be **better**, not merely not-worse.

**The same argument applies to summarization, and that is the core of the deferral claim.** Replacing
a blunt context wipe with `summarize` does not remove the phenomenon — a summary is lossy too, and
lossy in a way that is arguably *worse* for co-reference: a trim removes messages wholesale, so what
is gone is at least knowable, whereas a summary paraphrases exact identifiers into prose, silently
corrupting the literal tokens Tier-1 matching depends on. Summarizing early therefore destroys
exactly what selective removal would have kept, which is why deferring it is expected to help rather
than merely cost less.

Two consequences for design:

1. **Use a two-sided test, not a one-sided harm bound.** A two-sided test has more power at the same
   n, and a one-sided non-inferiority frame cannot register a gain even when one occurs.
2. **The baseline arm must be named precisely.** "vs baseline" is ambiguous between *full context*
   (where only harm is possible) and *lossy default trim or summary* (where gain is possible). These
   are different experiments with different expected signs, and conflating them makes any result
   uninterpretable.

### The affordable version of the claim, priced

Superiority on a binary reward is the expensive claim; **bounding harm is the cheap one, and is
usually what is actually being asked.** Priced below at $7.59 per task per arm, which was **wrong by
5×** — the true figure is **$1.52 per run** ([iteration 010](../experiments/loca/iter010/PREREGISTRATION.md)
amendment 1). Divide every dollar figure in this table by ~5; the *ratios* between rows, which are the
point, are unaffected:

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
the proposal's design notes (`docs/proposals/coref-compaction.md`).

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
| **LOCA-bench** | **yes** (dial 8k→256k) | **yes** (max 59,857) | deterministic but **binary**; n=75 runs, but only **15 independent tasks** (5 seeds each) | **no** | viable for savings; reward limited by 15 clusters, not by n |
| UltraHorizon | yes (200k+) | yes | **LLM-judged** | ? | noise we cannot afford; no licence |
| Claude Code transcripts | yes | yes | **none** | yes | no reward → cannot gate |

**The unclosable gap: nothing is both long-context and Tier-1-rich.** LOCA is long but Tier-2/3-heavy
(adverse to an exact-match detector); SWE-bench is Tier-1-rich but short. So a null result on LOCA
**cannot be generalised**, and that limit should be stated in advance rather than discovered.

LOCA band behaviour — **three of these five rows were never actually measured:**

| band | runs | baseline accuracy | usable |
|---|---|---|---|
| 8K (`debug`) | yes | **1.0** saturated | regression control only; only `format` fires |
| 32k | **never measured** | — | probe died in the rig, not the band |
| **64k** | **yes** | **20% (2/10 clean tasks)** | pressure yes, headroom **thin** |
| 96k | **never measured** | — | same rig failure as 32k |
| 128k | attempted | reported **0.0** / errors | **suspect** — collected during the broken-shim window |

All three bad rows were my rig, not the benchmark. The 32k and 96k probes died with
`[Errno 11] write could not complete without blocking` — the EAGAIN pipe trap in the table below —
with `messages: 0`, i.e. before the agent ever ran. The 128k row was collected while
`repair_shim.py` was silently dropping chunked bodies, so its zero floor is not established.

**Consequence for design:** "64k is the only band with pressure *and* headroom" was never tested
against its neighbours. Solve rate falls from 1.0 at 8k to 0.20 at 64k, so an intermediate band
plausibly has both, and the headroom problem in section 1 may be an artifact of having picked 64k.
Measuring 32k on the fixed rig is the cheapest available move, and should precede any decision to
buy more pairs at 64k or to move the agent to a larger model.

### Two properties of LOCA that cap what any reward arm here can conclude

**~~`group_by_seed` silently divides your n by five.~~ FALSE — and the real trap is the opposite one.**
It defaults to `True` and is genuinely not exposed as a CLI flag, but it groups for *reporting* only:
**all 75 configs execute either way.** The actual trap is on the reading side — `state0`…`state4` are
the 5 seeds, so globbing `tasks/*/state0/eval.json` silently reads 15 of 75 completed runs and
**overstates per-run cost by 5×**. That is what iterations 007 and 008 did
([iteration 010](../experiments/loca/iter010/PREREGISTRATION.md) amendment 1).

**What actually limits reward power here is 15 independent tasks, not n.** 75 runs are 5 seeds of 15
tasks, so they are clustered: extra seeds buy precision *within* a task and do not add independent
observations. Any harm bound must be quoted on the task-clustered end (~≤18% at zero events), and
tightening it needs more distinct tasks, which LOCA's 32k set does not have.

**The 64k base solve rate is ~33%, and accuracy is binary.** (Measured over all 75 runs; the "20%"
first reported came from reading `state0` only.) Every
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

See also: the proposal (`docs/proposals/coref-compaction.md`) ·
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
