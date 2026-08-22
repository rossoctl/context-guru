# Experiment log

Chronological, per-run record of every measurement, in the `iterNNN` notation used by the
`forever` project so the two are readable side by side. One directory per run, never edited
after the fact except to add a retraction banner.

**These are the runs. The [results](../RESULTS.md) pages are the *arguments*** — they synthesise
across runs, get rewritten as understanding changes, and are where a reader should start. An
iteration page is the opposite: what was executed, what came back, what it does and does not
prove. If the two ever disagree, the iteration page is the record of fact.

Each page carries the same sections: **Result** (a table), **What this proves**, **What this does
NOT prove** (the caveats, in full), **Next levers**, **Artifacts** (paths, so a number can be
traced back to the bytes that produced it).

## Index

| Iteration | Date | What | Headline | Status |
|---|---|---|---|---|
| [captures/iter001](captures/iter001/results.md) | 2026-08-20 | Component gating on `capture-swebench`, tail vs non-tail | The tail gate costs `mask` 93% of its effect and `failed_run` all of it; `extract_llm` cannot fire on SWE-bench at all | ✅ |
| [loca/iter001](loca/iter001/results.md) | 2026-08-20 | First LOCA replay, one request per conversation | `mask` 52.3% vs `coref` 6.5% | ⚠️ **retracted** — cold-start artifact |
| [loca/iter002](loca/iter002/results.md) | 2026-08-20 | Sequential LOCA replay, 197 turns, 5 arms, summarizer at a context max | Deferral: 72% fewer summarizations, delivered by `coref` | ⚠️ **config invalid** — the pipeline 400s in production (iter004) |
| [loca/iter003](loca/iter003/results.md) | 2026-08-20/21 | LOCA reward integration; band characterisation | 8K saturates at 1.0, 128k collapses to 0.0, 64k partial. Three rig bugs, two mine | ✅ |
| [loca/iter004](loca/iter004/results.md) | 2026-08-21 | Reward, 3 arms × 12 tasks @64k | **Invalid as a reward test** — every error tracked a `summarize` firing. But **`extract_llm` acted for the first time** (prefix reach) | ⚠️ |
| [loca/iter004b](loca/iter004b/results.md) | 2026-08-21 | Same, `summarize` removed | **Reward parity: per-task outcomes byte-identical to baseline**, 17.1% removed, 31% cheaper, 0 model calls | ✅ |
| [loca/iter005](loca/iter005/results.md) | 2026-08-21 | Deferral on live traffic; `summarize` chained in its own proxy | **Blocked — three shape defects in `summarize`, each masking the next; it 400s on every use** | ⚠️ → fixes `80e95d5`, `0971a32`, `2d6902d` |
| [loca/iter006](loca/iter006/results.md) | 2026-08-21 | Stage 1 ablation: `off` / `format` / `+coref`, 75 tasks @64k | Launched; see `iter007` for its outcome | → iter007 |
| [loca/iter007](loca/iter007/results.md) | 2026-08-21 | Stage 1 checkpoint: `format` (n=**75**) + `coref` (n=**62**), then stopped | **Stopped** — correctly, for the shim bug: HTML 400s root-caused to my *own* replay shim (chunked bodies dropped), not `format`/LOCA. `coref` acted on 4.2% of requests (~981k tokens). ⚠️ Its power argument is **retracted** by iter010: n and cost were wrong 5× (state0-only reads); re-paired it gives 48 pairs, 11 discordant, 7:4 for `coref` | ~$215, shim fixed |
| [loca/iter008](loca/iter008/results.md) | 2026-08-21 | 32k band headroom probe, matched 15 tasks × 5 seeds, no CG in path | **The band was the problem.** Over all 75 runs 32k solves **52.7%** vs 64k's **33.3%**, with **0 errors** (confirms the shim fix live) at **$1.13/run**. Still 45-56k peak contexts, so pressure remains | $85.10 |
| [loca/iter009](loca/iter009/results.md) | 2026-08-21 | Re-score the selection experiment: floor symmetry + deterministic Tier-2 ground truth | **Merged stays refuted.** Floor symmetry moves live-kept 0-2pts (overrides 6-23/885); Tier-2 widening (408→473 referenced) raises every arm's false-drop and does not close the 36pt gap. `cut_unreferenced`'s error floor revised **11% → 21-24%** | **$0** |
| [loca/iter010](loca/iter010/results.md) | 2026-08-21 | First live reward measurement: `format` vs `format`+`coref`, 32k, n=75/arm, pre-registered | **No reward effect either way** — task-clustered 4 harm / 4 gain, p=1.000, harm bound ≤51% (the ≤10% target needed zero harm events). **23% of pairs flipped** with no direction. `coref` adds ~1pp removal (6.8% of requests) and cost **rose** $93→$98. Errors 6→0 | $191.03 |
| [loca/iter011](loca/iter011/results.md) | 2026-08-21 | Deferral experiment, 3 arms pre-registered | **Aborted at arm 1** — 28/75 runs invalid. Root-caused to `apply.rebuildCountChanged` dropping the body message holding **parallel** `tool_result`s; `summarize` exonerated by test. Saved ~$180 | ~$8 |
| [loca/iter012](loca/iter012/results.md) | 2026-08-21 | The fold (`+extract_llm` full-body) vs lossless, 32k, n=75 | **`savings_pct` is inflated 3–8×.** Unique-token ordering *inverts*: fold removes **5.26M** vs lossless **6.98M**. Components cannibalise each other. Apparent $ saving is trajectory noise, not compaction. Reward 3 harm/6 gain, p=0.51 | $89.52 |

## Before designing an arm

[**What can and cannot be measured**](../results/measurement-limits.md) is the prerequisite read: the
statistical power actually available (reward at n=12 detects only 50pp effects; realistic ones need
~200–350 tasks), the measured cost model, the benchmark-suitability matrix, why replay cannot catch
schema defects, why a reported yield measures `coref`'s economic *throttle* rather than its
capability, and a table of rig traps that each produced a valid-looking wrong number.

## Conventions

- **Retractions stay.** A wrong run is deleted from the argument, never from the log — the
  retraction and its cause are usually more instructive than the number was.
- **Cost is always stated**, even when it is $0, because "free" is a property worth knowing.
- **Every arm names its binary.** A stale binary silently produced a null result in
  `loca/iter002`; recording the build is the cheapest guard against repeating that.
- **Replay is not validation.** `/compact` runs the pipeline and returns the body *without
  forwarding upstream*, so no provider ever checks it. Replay can tell you what a component
  removes; it **cannot** tell you the result is a valid request. `loca/iter005` found a shipped
  component that 400s on every use, invisible to every replay-based measurement here.
- **Pre-register the reading.** For anything with arms, commit the design *and* how each outcome will
  be interpreted **before** the numbers exist (`loca/iter004`, `loca/iter006`). Cheap insurance, and
  it is what stopped a +1-task difference at n=12 being written up as an improvement.
- **A green test suite only covers the dialect its fixtures use.** Every unit test passed while 37% of
  live Anthropic runs failed, because the fixtures hand-build messages in the OpenAI shape
  (`ToolCalls` populated) and the wire dialect carries tool calls as `tool_use` content blocks that
  bifrost cannot represent at all (`loca/iter011`). Assert on wire bodies in the dialect that actually
  runs, not on the internal representation.
- **Ask "would this check have failed?", not "did it come back clean?"** Two checks in one night could
  not have detected what they were written for: a baseline-reuse validation that compared data to
  itself (`loca/iter012`), and a fix verification that reported zero failures while the component under
  test never fired (`loca/iter011`).
- **Quote unique savings, never `savings_pct`, for anything non-deterministic.** The same removed
  content is re-credited on every turn that replays a frozen rewrite, inflating `coref` by 4–8× and
  `extract_llm` by 2.8× while `format` (deterministic, in place) stays at exactly 1× (`loca/iter012`).
  The two numbers even *order the arms differently*.
- **Per-arm benchmark cost cannot price a component.** Two arms differing by one component came out
  $4.57 more expensive and $3.71 cheaper in successive iterations, both dominated by how long the
  agent's own path happened to run. Attribute cost from the token counters, never from the bill.
- **Report three yield numbers, not one** — eligible, acted, and refused-for-economics. A single
  figure cannot distinguish "nothing left to remove" from "economically throttled", and those call
  for opposite responses.
- **State power in the design, not the caveats.** An arm that cannot detect the effect it is looking
  for should say so before it runs.
- **Check what a results glob actually matches before quoting an n.** `tasks/*/state0/eval.json`
  read 15 of 75 completed runs across two iterations, because `state0`…`state4` are seeds. It
  understated n by 5× and overstated cost per run by 5×, and nothing about the output looked wrong
  (`loca/iter010` amendment 1). Count the files, compare against the config length, and reconcile the
  two before drawing an economic conclusion from either.
- **Count the headroom before counting n.** Harm can only show on tasks that currently pass;
  improvement only on tasks that currently fail. At 64k the entire harm signal had to come from 3
  tasks, so no n was going to help. Binary-outcome sensitivity peaks at a 50% base rate, which is
  why 32k (53%) measures for less money than 64k (25%) — see
  [measurement-limits §1](../results/measurement-limits.md).
- **A metric change that makes the story more interesting deserves an audit before it is believed.**
  A Tier-2 matcher that collapsed paths onto their stem reported the index's false-drop tripling
  (11% → 53%). It was an artifact: the key `yaml` matched 42 distinct identifiers, so any later YAML
  mention scored as reuse of a different file (`loca/iter009`). Audit which inputs a new rule actually
  fires on, and put the conservative default on the side of the expensive error.
- **"The benchmark cannot do X" is a hypothesis about your configuration.** It was stated as a fact
  about LOCA, then disproved by a matched run on the *same 15 tasks* at a different data volume:
  25% → 53% (`loca/iter008`). That $85 run saved several hundred dollars of underpowered arms.
- **Suspect your own harness before the thing under test.** Three HTML 400s were attributed to a
  component, then to the benchmark, and were caused by a body-framing bug in the replay shim
  (`loca/iter007`). The tell was there in the data: the arm with *more* components had *fewer*
  errors. When error counts move opposite to the amount of machinery, look at the transport.
- **Choose the endpoint the budget can afford.** Savings are continuous and cheap to measure to high
  precision; reward is binary and expensive. Superiority on a binary outcome at a 20% base rate costs
  4–20× what bounding harm costs, and bounding harm is usually the actual claim — so state a
  non-inferiority margin up front and buy that (`loca/iter007`).
