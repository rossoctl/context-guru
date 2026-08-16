# Benchmark: context-guru vs headroom vs rtk vs baseline

**SWE-bench Verified · 50 tasks · `claude-code` agent on `aws/claude-sonnet-5`**, run live.
All **50 tasks** scored (no infrastructure exception) under all **four** arms, so every
number below is apples-to-apples on the same tasks. Reproduce: [REPRODUCE.md](REPRODUCE.md).
Per-config detail: [baseline.md](baseline.md) · [context-guru.md](context-guru.md) ·
[headroom.md](headroom.md) · [rtk.md](rtk.md). Component internals & real examples:
[components.md](components.md).

Three of the four arms are compaction layers of two different kinds:

- **context-guru** and **headroom** are **request-stream proxies** — they compact the whole
  `messages` array (system prompt + history + tool outputs) *after* it's assembled, on the
  network path to the model.
- **rtk** (Rust Token Killer) is a **shell-level hook** — it rewrites the agent's Bash
  commands (`cat`→`rtk read`, `pytest`→`rtk pytest`, …) and compresses their output *at the
  source, before it ever enters the transcript*. It is not on the network path.

Cache-aware billed **input** cost = fresh $2/M · cache-read $0.20/M · cache-write $2.50/M,
recomputed from each trial's own token tiers; output billed at $10/M. **Total** adds the
tool's own compaction-LLM cost.

These figures record what ran, and the comparison against headroom and rtk is
apples-to-apples. The shipped `codesmart` pipeline has changed since, so a claim about
today's default needs a re-measurement — see [Benchmarks](../RESULTS.md).

## Headline

**context-guru is the cheapest *and* highest-reward arm** — but the striking result is that
**rtk, a simple deterministic shell filter, is the 2nd-cheapest arm and beats the
sophisticated headroom proxy on both cost and reward**, at zero request-path latency and
zero tool cost.

![headline](../img/benchmark/headline.png)

| dimension | baseline `off` | **context-guru** | headroom | **rtk** | winner |
|---|--:|--:|--:|--:|:--|
| reward (solved / 50) | 43 (86%) | **44 (88%)** | 40 (80%) | 43 (86%) | **context-guru** |
| **total billed cost** | $31.98 | **$27.77 (−13.2%)** | $30.30 (−5.3%) | $29.09 (−9.0%) | **context-guru** |
| cache-read tokens | 102.8M | **84.5M (−17.8%)** | 96.4M (−6.3%) | 91.7M (−10.8%) | **context-guru** |
| cache-write tokens | 1.855M | 1.847M | 1.839M | **1.835M** | ≈ tie (all within 1.1%) |
| cache-read $ | $20.57 | **$16.90** | $19.28 | $18.33 | **context-guru** |
| cache-hit rate | 98.14% | 97.73% | 98.01% | 97.94% | ≈ tie |
| mean steps / task | 36.1 | **31.1** | 35.1 | 33.2 | **context-guru** |
| mean agent wall / task | 380 s | **352 s** | 364 s | 384 s | **context-guru** |
| compaction added latency / req | — | 117 ms | 63 ms | **0 ms** | **rtk** |
| tool's own LLM cost | $0 | $0.31 | **$0** | **$0** | rtk / headroom |
| content removed (per req) | 0 | 1.09% | 2.64% | 65.8% *of bash only* | — (different denominators) |
| exceptions (of 50) | 0 | 0 | 0 | 0 | tie |

The **content removed** row is not comparable across the two kinds: context-guru/headroom
measure removal against the **whole request**; rtk's 65.8% is of **bash output only** (its
`bytes/4` estimate), a small slice of a ~98%-cached agent's context — which is exactly why
its 65.8% bash reduction nets to −9% on the bill, not −66%.

### Verdict

- **context-guru wins the dollar-and-reward metrics** — lowest billed cost ($27.77,
  **−13.2%** vs baseline), lowest cache-read, fewest steps, *and* the most tasks solved (44).
- **rtk is the surprise** — the **2nd-cheapest** arm (**−9.0%**), it **matches the baseline
  reward** (43, reward-neutral), uses **fewer steps** (33.2), and does it with **zero
  request-path latency and $0 tool cost** because it runs in the shell, not on the model
  path. It **beats headroom on both cost and reward** despite being far simpler.
- **headroom** lands **third on cost** (−5.3%) and lowest on reward (40). Its one remaining
  edge over context-guru is deterministic hot-path latency (63 vs 117 ms/req) — but rtk
  matches baseline latency (0 ms added to the request) *and* undercuts headroom on cost, so
  headroom is squeezed from both sides here.
- **Cache-write is a four-way wash** (1.855M / 1.847M / 1.839M / 1.835M — within 1.1%): none
  of the arms busts the cache. rtk is inherently cache-safe — it shrinks output *before* it
  is ever cached, so there is no cached prefix to invalidate (the problem the proxies must
  engineer around with freeze-and-replay).
- All four arms ran all 50 tasks with **zero** infrastructure exceptions.

### Why the ranking comes out this way

- **context-guru** removes the least *per request* (1.09%) yet is cheapest because it
  **freezes each compaction and replays it byte-identically every turn**, so the reduction
  compounds across the whole session's cache-reads (−17.8%), and it compacts the *entire*
  request — including large file reads via the built-in `Read` tool that never touches a shell.
- **rtk** only sees output the agent routed through the **Bash** tool (`cat`, `grep`, test
  runners); Claude Code's built-in `Read`/`Grep`/`Glob` bypass its hook. That is its
  structural ceiling — and yet, because bash `cat`/`grep`/test output is a real chunk of a
  coding session, a deterministic 66% cut of it still compounds (it enters the cache
  pre-shrunk on every turn) to −10.8% cache-read and −9% cost.
- **headroom** removes the most *raw* content per request (2.64%) but does not target the
  biggest file reads as deeply as context-guru's LLM skeletonizer, and it lost 3 more tasks.

## Cost decomposition

Where every dollar goes (matched total): cache-read is the dominant term on a ~98%-cached
agent. context-guru shrinks it the most (at a $0.31 haiku bill); rtk shrinks it
deterministically for free; headroom shrinks it least of the three compaction arms.

![cost decomposition](../img/benchmark/cost_decomposition.png)

## Per-task

Per-task billed cost (baseline ◆ · headroom ● · context-guru ● · rtk ■) and the per-task
deltas vs baseline — context-guru is at/below baseline on nearly every task:

![per-task cost](../img/benchmark/per_task_cost.png)
![per-task cost delta](../img/benchmark/per_task_dcost.png)
![per-task step delta](../img/benchmark/per_task_dsteps.png)

## Per-component / per-compressor

context-guru's savings come from `extract_llm` (LLM skeletonization of large file
reads/logs) + the deterministic `extract` (ANSI/CR + noise) + `cmdfilter`/`dedup`;
headroom's from its deterministic `text` and `code_aware` (AST) compressors; **rtk's from
`cat`→`read` file skeletonization (62%) and `grep` grouping/truncation (24%)**, then git /
ls / pytest. Cumulative vs unique tokens (context-guru re-applies the same compaction every
turn, so cumulative ≫ unique):

![components](../img/benchmark/components.png)

Full component internals, trigger conditions, and real before→after examples for all three
tools are in **[components.md](components.md)**.
