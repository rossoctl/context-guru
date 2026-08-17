# Measure savings

Turn the dashboard on, run your agent, and read the numbers with their denominators named.

## Steps

1. Start the proxy with the dashboard:

    ```sh
    context-guru-proxy --preset codesmart --dashboard
    ```

2. Point your agent at `http://localhost:4000/anthropic` (or `/openai`) and give it a few
   turns of real work.

3. Open the dashboard:

    ```sh
    open http://localhost:4000/dashboard/
    ```

4. Read the Overview: tokens before/after, the four savings ratios, baseline vs actual
   dollars, and the cumulative-cost chart.

![The dashboard's Overview](../img/dashboard/01-overview.jpg)

For a scriptable snapshot instead, `curl -s localhost:4000/stats | jq`.

## Which number to quote

There is no single honest savings percentage, so context-guru reports four. Pick by the
question you are asking:

| If you want to know… | Use |
|---|---|
| Is compaction working when there is something to compact? | **of what we tried to compact** |
| What is the economic effect? | **of new provider-billed input** |
| What is the most conservative claim I can make? | **unique, of the whole request** |
| Why does my long session read ~0%? | **of the whole request (diluted)** — see below |

Savings are measured on **message content text** — what the model reads — not the JSON
envelope, so a control directive like `cacheinject` never looks "worse".

Quote **cost per solve**, not cost: an arm that spends less by solving fewer tasks has not
saved anything.

## Check what it cost you

The dashboard prices every request at write time and shows baseline cost, actual cost,
context-guru's own LLM spend, and net dollars saved — which goes negative if we spent more
than we saved. Beside it, **"What our own safety mechanisms cost"** reports compaction
skipped for cache safety, offloads the model asked back for, reverted component runs, and
our own latency.

Per-component economics (runs, act rate, unique vs gross saved, latency, verdict) show
which components earn their place on *your* traffic:

![Per-component economics](../img/dashboard/03-component-metrics.jpg)

Click a component to filter the request list, then open a request to see exactly what
changed as a Git-style diff:

![Git-style content diff](../img/dashboard/09-content-git-diff.jpg)

Content capture is **off by default** — enable it with `--dashboard-content`. It is the one
path that writes agent output to disk, so the operator opts in for their own transcripts.
Captured content is visible from loopback or a `--dashboard-trusted-cidrs` entry only.

<details markdown="1">
<summary>Reading the numbers: gross, unique, and the diluted ratio</summary>

Three savings figures, all true, all different:

| Figure | Meaning |
|---|---|
| **gross** | Every token removed, re-counted each turn the agent re-sends the same transcript. |
| **unique** | Each distinct compaction counted once. |
| **net of restores** | Unique, minus content the model asked back for via `context_guru_expand`. |

`overcount_ratio = gross ÷ unique` is how inflated the gross figure is: **7×** means the
gross number counted the same compaction seven times. Quote unique or net-of-restores; use
gross only when you say it is cumulative.

A whole-request ratio is dominated by re-sent history — a 200-turn session re-sends its
transcript every turn, so the denominator grows quadratically and the ratio trends toward
zero however well compaction performs. That is what a whole-request ratio *means*, not a
bug. This is also why a savings percentage with no stated denominator tells you nothing.

On a prompt-caching backend the request is ~99.95% cached and a cache write bills ~11.5× a
cache read, so removing unique tokens moves a fraction of a percent of the billed total
while cost tracks agent **steps**. Read the dollars, not only the tokens — see
[the SWE-bench study](../results/comparison.md).

A request is priced only when the provider reported all four token tiers and the model's
rates are known. Otherwise the row is `partial` or `missing` and its cost reads *unknown*,
never zero. Filter to `complete` before quoting a dollar figure.

</details>

<details markdown="1">
<summary>Troubleshooting: a component saved nothing</summary>

`acted: 0` on its own is not a diagnosis. `components.<name>.gates` in `/stats` names the
guard that turned each candidate away, which separates three different situations:

- **No legal opportunity** — `format: {not_json_shaped: 471}` (no JSON tool outputs
  exist), `dedup: {no_earlier_identical_output: 234}`, `failed_run: {fewer_than_two_runs: 44}`.
- **Correctly declining** — `marker_no_win` (the recovery marker would cost more than the
  rewrite saves), `cached_prefix` (cache safety froze the message), `economic_gate:*` (an
  LLM call would lose money here).
- **A gap worth closing** — `cmdfilter: {no_filter_match: N}` means nothing matched.
  Cross-check `cmdfilter_selector_misses`, which ranks the output shapes no filter claimed
  and tells you which filter to [write next](custom-dsl-filter.md).

`top_passthrough` lists components that ran and changed nothing. `cachesplit` always lands
there because its win is a provider-side cache hit, invisible to content-token counts; a
content offloader that never fires is a candidate to drop from your pipeline.

`top_discarded` is different and always worth investigating: the component mutated the
request and the writeback layer threw the change away before it reached the wire. Check the
per-component `discarded_changes` count.

If every savings field reads zero and `potential_*` fields are populated instead, you are
in [observe mode](operating-modes.md), which measures without applying anything.

</details>

<details markdown="1">
<summary>Reference: the <code>/stats</code> fields</summary>

`/stats` is in-memory and resets with the process; its field names are stable and guarded
by a golden test, because the benchmark harnesses parse it. For history, retention,
filtering, sessions and diffs, use the dashboard.

| Field | Meaning |
|---|---|
| `savings_pct` | Σ saved / Σ before — the whole-request (diluted) ratio |
| `savings_pct_attempted` | Σ saved / Σ attempted — "of what we tried to compact" |
| `savings_pct_new_input` | Σ saved / (fresh + cache-write + saved); **0** when the provider reported no usage |
| `attempted_tokens` / `frozen_tokens` | What compaction could touch, and what cache safety made us leave |
| `fresh_input_tokens` / `cache_read_tokens` / `cache_write_tokens` / `output_tokens` | The four billed tiers |
| `wasted_tokens` / `bounces` | Content offloaded then re-served via expand, and how often |
| `adjusted_saved` | `saved − wasted`; may be negative |
| `top_passthrough` / `top_discarded` | Components that changed nothing, and components whose changes were thrown away |
| `saved_tokens_unique` / `overcount_ratio` | Distinct compactions, and how many times each was re-counted |
| `components.<name>.gates` | Rejection histogram: gate name → candidates declined |
| `cg_added_ms_avg` / `upstream_ms_avg` / `upstream_ms_avg_bypassed` | Latency, split by whether the request bypassed us |
| `mode` / `sync_enforced` | The operating mode, and how many requests context-guru actually shaped |

Metrics are emitted through the `Emitter` interface, so the pipeline carries no telemetry
dependency: `Slog` (logs), `Aggregator` (the rollups behind `/stats`), `Tee` (fan-out),
`NopEmitter` (discard). The dashboard captures out of band from
[`apply.BodyTrace`](../design.md), so the aggregator stays a fast in-process counter.

</details>

## A real session, end to end

`scripts/cc-demo.sh` builds a tiny repo, starts the proxy, points Claude Code at it and
runs one task through it:

```sh
export ANTHROPIC_BASE_URL=...            # upstream Anthropic-compatible endpoint
export ANTHROPIC_AUTH_TOKEN=...
scripts/cc-demo.sh
# then open http://localhost:4000/dashboard/#sessions and click the session
```

To view a full harness run, point `--dashboard-bench-dirs` at its jobs root: each run's
`summary.json` + `rows-<arm>.json` is ingested, with cost-vs-reward per arm and per-task
drill-down.

![Benchmark comparison](../img/dashboard/11-benchmark-comparison.jpg)

See also: [Dashboard](../dashboard.md) · [Benchmarks](../RESULTS.md) ·
[Sync & observe](operating-modes.md)
