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
| [loca/iter007](loca/iter007/results.md) | 2026-08-21 | Stage 1 checkpoint: `format` (n=15) + `coref` (n=14), then stopped | **Stopped.** HTML 400s root-caused to my *own* replay shim (chunked bodies dropped), not `format`/LOCA. Benchmark can't power a reward comparison: $7.59/task, 20% base solve rate, binary accuracy, 1/10 discordant → p=1.00. `coref` acted on 4.2% of requests (~981k tokens) | ~$215, shim fixed |
| [loca/iter008](loca/iter008/results.md) | 2026-08-21 | 32k band headroom probe, matched 15 tasks, no CG in path | **The band was the problem.** 32k solves **53%** vs 64k's 25%, with **0/15 errors** (confirms the shim fix live) and cheaper at $5.67/task. Still 45-56k peak contexts, so pressure remains | $85.10 |

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
- **Report three yield numbers, not one** — eligible, acted, and refused-for-economics. A single
  figure cannot distinguish "nothing left to remove" from "economically throttled", and those call
  for opposite responses.
- **State power in the design, not the caveats.** An arm that cannot detect the effect it is looking
  for should say so before it runs.
- **Suspect your own harness before the thing under test.** Three HTML 400s were attributed to a
  component, then to the benchmark, and were caused by a body-framing bug in the replay shim
  (`loca/iter007`). The tell was there in the data: the arm with *more* components had *fewer*
  errors. When error counts move opposite to the amount of machinery, look at the transport.
- **Choose the endpoint the budget can afford.** Savings are continuous and cheap to measure to high
  precision; reward is binary and expensive. Superiority on a binary outcome at a 20% base rate costs
  4–20× what bounding harm costs, and bounding harm is usually the actual claim — so state a
  non-inferiority margin up front and buy that (`loca/iter007`).
