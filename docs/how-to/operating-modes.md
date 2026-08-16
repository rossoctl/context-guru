# Sync & observe

context-guru runs in one of two modes. `sync` compacts your requests; `observe` measures
what compaction *would* have done without touching anything.

## Which one do I want

| You want | Mode | It costs |
|---|---|---|
| Savings now | `sync` (default) | request latency — near zero on an all-deterministic pipeline, ~450 ms with the LLM trimmer on — plus any `extract_llm` spend |
| To find out what context-guru would save on your traffic, risk-free | `observe` | CPU and any `extract_llm` spend. **No request latency, no request modification.** |

**Start in `observe`.** It is how you evaluate a configuration: every request reaches the
provider byte for byte untouched while a copy runs off-path to record what compaction would
have achieved. Read the `potential_*` numbers, then switch the same config to `sync`.

## Steps

1. Set the mode. Either in the config file:

    ```yaml
    mode: observe          # sync | observe
    observe:
      max_queue: 256
      workers: 1
    ```

    or on the proxy binary, which wins over the file:

    ```sh
    context-guru-proxy --preset codesmart --mode observe
    ```

2. Run your agent against the proxy as usual.

3. Read the projection:

    ```sh
    curl -s localhost:4000/stats | jq '{observe_notice, actual_baseline_tokens,
      projected_optimized_tokens, potential_saved_tokens, potential_savings_pct}'
    ```

4. Happy with the projection? Restart with `--mode sync` and the same config.

The mode is always explicit — nothing infers it from the rest of your configuration — and
it is reported in `/stats` so a consumer never has to guess which regime produced a number.
Mode is per process, not per request.

## Reading observe numbers

Hypotheticals live under their own keys and never share a key with an enforced metric:

| Key | Means |
|---|---|
| `observe_notice` | The banner. Present whenever hypotheticals are reported. |
| `observe_hypothetical_requests` | Requests observed. |
| `actual_baseline_tokens` | What the agent really sent. |
| `projected_optimized_tokens` | What it would have sent under this pipeline. |
| `potential_saved_tokens` / `potential_savings_pct` | The difference, absolute and relative. |
| `potential_components` | Per-component hypothetical contributions. |
| `potential_overhead_ms_avg` | What compaction *would* have added per request — so, what `sync` would cost you. |

Every enforced savings aggregate reads zero in observe mode by construction: `requests`,
`tokens_before`, `tokens_after`, `saved_tokens`, `sync_enforced` and the `components` map.
That zero is the machine-readable form of "context-guru did not modify any request".

Two enforced fields stay real, because they are measurements rather than hypotheticals:
`cg_added_ms_avg` (the actual added latency on the enforced path — ~0, which is the point)
and `llm_calls` / `llm_input_tokens` / `llm_output_tokens` (context-guru's own model spend,
labelled by `observe_llm_notice` as the cost of measuring rather than of enforcing).

<details markdown="1">
<summary>What observe cannot tell you</summary>

- **Cache effects are projected, not measured.** The forwarded request is the agent's own,
  so the provider's real cache behaviour is the *baseline's*. `potential_saved_tokens` is a
  content-token figure; the cache consequence of actually enforcing is not measured here.
- **Reversibility is not exercised.** Nothing was offloaded, so no expand bounce can happen
  and `wasted_tokens` stays zero. Under `sync` some savings do come back as bounces. Treat
  the projection as an **upper bound** on content savings.
- **Measuring is not free.** A pipeline with `extract_llm` in it spends cheap-model tokens in
  observe mode too (see `observe_llm_notice`). It costs money and CPU — just not request
  latency.

</details>

<details markdown="1">
<summary>Troubleshooting</summary>

**Observe projects far more than sync delivers.** Check that the projection is running with
the cached-prefix boundary — observe shares the per-session boundary the enforced path uses.
Without it every message looks compactable and the projection overstates savings by exactly
what cache-awareness costs (measured: 9.5% projected against 0.8% achieved on the same
tasks).

**Observe projects far less than sync delivers.** Most sustained saving comes from frozen
decisions replayed on later turns, so observe keeps a persistent store of its own. Discard
that state each turn and it sees only the current tail and under-projects by roughly 3×.

**`dropped` is climbing in the observe counters.** Observations run on a bounded worker pool;
a full queue drops the measurement rather than blocking the request path, which has already
been forwarded. Raise `observe.max_queue` or `observe.workers`. A drop costs a measurement,
never correctness.

**Did observe leak into my live state?** It cannot: observe never writes a byte into the
live store, and a test asserts it. Its store is disjoint from the enforced one.

**State did not survive a restart.** Frozen decisions and stashes live only as long as the
store does — in memory by default, so a restart starts cold in either mode.

</details>

## An async mode is designed but not shipped

Deferring compaction off the request path is implemented on a separate branch
([#35](https://github.com/rossoctl/context-guru/pull/35)) and deliberately held back: its
benefit came almost entirely from deferring the LLM trimmer, which no longer runs on
prompt-caching backends, and it has to decline on agents that set their own cache
breakpoints — which Claude Code does.

See also: [Measure savings](measure-savings.md) · [Config & environment](../reference/config.md) ·
[Observe-mode results](../results/observe-mode.md)
