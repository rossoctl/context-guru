# Operating modes: sync and observe

context-guru runs in one of two modes. `sync` is the default and reproduces the behavior
that existed before modes did, byte for byte.

```yaml
mode: sync            # sync | observe
observe:
  max_queue: 256
  workers: 1
```

Or `--mode` / `MODE=` on the proxy binary, which wins over the config file.

The mode is always explicit. Nothing infers it from the rest of your configuration,
because the two modes make materially different promises about your requests and a guess
about which one you wanted is not a thing you should have to debug.

## Which one do I want

| You want | Mode | It costs |
|---|---|---|
| Savings, and can absorb compaction latency on the request path | `sync` | request latency (~450 ms with the LLM trimmer on; near-zero on an all-deterministic pipeline), plus any `extract_llm` spend |
| To find out what context-guru *would* save, without it touching anything | `observe` | CPU and any `extract_llm` spend — **no request latency, no request modification** |

**`observe` is how you try a configuration.** It measures what a pipeline *would* have
saved on your real traffic while every request goes to the provider byte-for-byte
untouched, so evaluating a config costs you no risk to a running agent and no A/B against
history. Start there, read the `potential_*` numbers, then switch the same config to
`sync`. There is a third mode designed but [not shipped](#an-async-mode-is-designed-but-not-shipped).

## sync — compact inline

The request path runs the pipeline and forwards its output. The caller waits.

That wait is real: measured on Terminal-Bench, **~450 ms per request** in the
configuration where the LLM-based trimmer runs, almost all of it that model call.

## observe — measure without enforcing

The agent receives its request **untouched, byte for byte**. The request path does not run
the pipeline at all, and does not inject the expand tool either — injecting a tool
declaration would modify the request, which is the one thing this mode promises never to
do. A copy of the request runs off-path against observe's own state store, disjoint from
the live one, purely to record what compaction *would* have achieved.

Byte-identity is therefore **structural**, not a property of careful copying: there is no
code path in observe mode that could alter a forwarded body, because the pipeline never
sees it. A test asserts it anyway.

This is the answer to "will context-guru help *my* traffic" that does not require
enforcing it in production and comparing against history. Neither reference implementation
offers it — headroom has no observe/shadow/dry-run mode at all; its `token` and `cache`
modes are both enforcing, and its only control arm is a 10% output-shaper holdout.

### How to read observe numbers

Observe-mode numbers live under their own keys and **never share a key with an enforced
metric**:

| Key | Means |
|---|---|
| `observe_notice` | The banner. Present whenever hypotheticals are reported. |
| `observe_hypothetical_requests` | Requests observed. |
| `actual_baseline_tokens` | What the agent really sent. Actual, not hypothetical. |
| `projected_optimized_tokens` | What it would have sent under this pipeline. |
| `potential_saved_tokens` | The difference. |
| `potential_savings_pct` | The difference as a percentage. |
| `potential_components` | Per-component hypothetical contributions. |
| `potential_overhead_ms_avg` | What compaction *would* have added per request — measured off-path, so it is what `sync` would cost you, not what `observe` costs you. |

In observe mode every enforced **savings** aggregate is zero by construction:
`requests`, `tokens_before`, `tokens_after`, `saved_tokens`, `sync_enforced` and the
`components` map. That zero is the machine-readable form of "context-guru did not modify
any request".

Two enforced-namespace fields are deliberately **not** zero, because they are real
measurements rather than hypotheticals:

- `cg_added_ms_avg` — the actual latency added to the enforced path, which in observe mode
  is ~0 precisely because that path does no pipeline work. Zeroing it would hide the
  mode's headline result.
- `llm_calls` / `llm_input_tokens` / `llm_output_tokens` — context-guru's own model spend.
  Observe measures off-path, and that measuring costs real money. The spend is not
  hypothetical, so it stays where cost tooling already reads it, labelled by
  `observe_llm_notice` as the cost of measuring rather than of enforcing.

A mislabelled hypothetical is worse than no number at all, because it silently inflates a
savings claim. The separation is therefore structural — two physically separate
accumulators with disjoint serialized names — and a test asserts that no enforced savings
aggregate can reach an observe result.

### Why observe's numbers should match sync's

The projection is measured under the **same** conditions an enforcing mode would run
under, because that agreement is what validates the mode. Two things are required and
neither is obvious:

- **The same cached-prefix boundary.** Observe shares the per-session boundary the
  enforced path uses. Without it, cache-awareness never gates anything, every message in
  the transcript looks compactable, and the projection overstates savings by exactly the
  amount cache-awareness costs — measured at 9.5% projected against 0.8% actually
  achieved on the same SWE-bench tasks.
- **State that accumulates across turns.** Offloaders *freeze* a decision and replay it on
  every later turn, which is where most of the sustained saving comes from. So observe
  keeps a store of its own — as persistent as the live one, and completely disjoint from
  it. Discarding its state each turn instead makes it see only the current tail and
  **under**-project by ~3x.

The live store stays pristine either way: observe never writes a byte into it, or a later
real request could replay a decision that was never enforced — a request modification
arriving by the back door. Both properties are asserted by tests.

### What observe cannot tell you

- **Cache effects are projected, not measured.** The forwarded request is the agent's own,
  so the provider's real cache behavior is the *baseline's*, not the compacted one's.
  `potential_saved_tokens` is a content-token figure; the cache consequence of actually
  enforcing is not measured here.
- **Reversibility is not exercised.** Nothing was offloaded, so no expand bounce can
  happen and `wasted_tokens` stays at zero. Under `sync`, some savings do come back as
  bounces. Treat observe's projection as an **upper bound** on content savings.
- **Measuring is not free.** If your pipeline includes `extract_llm`, observe mode spends
  cheap-model tokens (see `observe_llm_notice`). It costs money and CPU — just not request
  latency.

### Reading the off-path queue counters

Observations run on a bounded, owned worker pool rather than a goroutine per request:

- a full queue **drops** and counts it, never blocks the request path — the request has
  already been forwarded, so a drop costs a measurement, not correctness;
- enqueue dedups by key, with the pending slot claimed before the job is observable in the
  queue, so dedup is atomic against a concurrent enqueue;
- jobs run under the pool's own context, not the request's (which is cancelled the moment
  the response is written);
- a panicking observation is contained and counted in `errors`; the worker survives.

`dropped` is the counter that says *we silently gave up a measurement*, and it is surfaced
deliberately — headroom's dashboard shows only `queued`, which hides exactly that.

## Switching modes

Mode is per-process, not per-request: it decides what happens to every request the proxy
handles, and the mode is reported in `/stats` so a consumer never has to guess which
regime produced a number.

Session state (frozen decisions, stashes) carries across a restart only as far as the
store does — in-memory by default, so a restart starts cold in either mode.

## An async mode is designed but not shipped

A third mode — deferring compaction off the request path so subsequent turns use the
result — is implemented and reviewed on a separate branch ([#35][pr35]), and deliberately
held back. Its measured benefit came almost entirely from deferring the LLM-based trimmer,
which no longer runs on prompt-caching backends, so on the primary workload there is
nothing expensive left to defer. It also has to decline on agents that set their own cache
breakpoints, which claude-code does.

It is held rather than discarded because the hard parts — per-session compaction
generations, a bounded worker pool, and a cache policy that refuses to write a breakpoint
onto a span it is about to replace — survived a hostile review intact. What it needs is a
paired benchmark arm establishing a benefit, not more code.

[pr35]: https://github.com/rossoctl/context-guru/pull/35
