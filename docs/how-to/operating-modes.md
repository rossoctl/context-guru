# Operating modes: sync, async, observe

context-guru runs in one of three modes. `sync` is the default and reproduces the
behavior that existed before modes did, byte for byte.

```yaml
mode: sync            # sync | async | observe
async:
  cache_uncompacted_tail: false   # safe default: protect cache-write economics
  strip_caller_breakpoints: false # true is REQUIRED for async to do anything with claude-code
  max_queue: 256
  workers: 1
```

Or `--mode` / `MODE=` on the proxy binary, which wins over the config file.

The mode is always explicit. Nothing infers it from the rest of your configuration,
because the three modes make materially different promises about your requests and a
guess about which one you wanted is not a thing you should have to debug.

## Which one do I want

| You want | Mode |
|---|---|
| Maximum savings from the first turn, and can absorb the latency | `sync` |
| Savings without paying compaction latency on the request path | `async` |
| To find out what context-guru would save, without it touching anything | `observe` |

## sync — compact inline

The request path runs the pipeline and forwards its output. The caller waits.

That wait is real: measured on Terminal-Bench, **~450 ms per request**, almost all of
it the `extract_llm` model call. Over one arm it summed to ~1,592 s.

Sync is the right default anyway, because the wait buys the full saving immediately
and a decision computed once is replayed for many turns. But it does mean the *first*
turn pays for a compaction that mostly benefits turn five onward, which is the trade
`async` exists to change.

## async — defer the expensive part

The request path still runs the pipeline, but with **no model clients**. Every
`NeedsModel` component (`extract_llm`, `summarize`) degrades to its deterministic path
or no-ops — a contract those components already had. So the inline pass costs
deterministic time only, and what it *does* produce is the replay of decisions an
earlier turn's off-path job already froze.

The expensive compaction is then queued. When it finishes, its decisions are frozen
into the session's state, and the **next** turn replays them. Savings arrive later,
not never; `/stats` reports `async_realized_saved_tokens` so the deferred value is
attributable rather than invisible.

### The cache trade-off — read this before enabling async

A cache-write costs **11.5x** a cache-read: `($2.50 − $0.20) / $0.20`.

So a naive async implementation is *strictly worse* than sync. It lets the
not-yet-compacted tail get provider-cached, then replaces that tail when the
compaction lands, and the provider has to re-write the span it had committed to. A
0.1x read becomes a 1.25x write. This is not a hypothetical failure: it is exactly
what tripled headroom's cache-write on Terminal-Bench — 12.37M against a 4.01M
baseline — by rewriting the live zone.

context-guru's default therefore refuses to place a cache breakpoint at or beyond the
tail a pending compaction is going to replace. `cacheinject` drops those positions
and anchors at the highest index below them instead, so the whole stable prefix is
still written and nothing the provider commits to is later rewritten.

!!! warning "With claude-code you must choose: `strip_caller_breakpoints`, or async does nothing"
    The protection only works if it controls the breakpoints. claude-code sets its **own**
    breakpoint on the newest message — inside exactly the span a pending compaction will
    replace. context-guru will not silently override a directive the agent placed, so by
    default it **declines to defer that turn at all**, counted as
    `async_tail_unprotected_turns`. On such a workload async is inert and `sync` is what
    you are effectively running.

    Set `async.strip_caller_breakpoints: true` to let context-guru take that breakpoint
    back and actually get async's benefit. The trade is explicit: you override the agent's
    caching choice on the newest message, in exchange for not paying an 11.5x rewrite of
    it.

The cost of the protection is one breakpoint position: the newest messages are not
cached until their compaction lands. On an append-only agent transcript that is a
small, bounded loss, and it is bounded by construction — the protection only covers
the tail past the previous turn's boundary.

`async.cache_uncompacted_tail: true` turns the protection off. Set it only for a
backend you have **confirmed** does not cache prompts, where the protection costs a
breakpoint slot and buys nothing. On any Anthropic-family backend, leaving it false
is the difference between async being cheaper than sync and being worse than it.

### What async guarantees

- **One useful job per session per generation.** A session carries a compaction
  generation that advances on **every turn**; a job records the generation it was built
  from. Enqueue dedups on `(session, generation)`, with the pending slot claimed before
  the job is observable in the queue, so a concurrent enqueue of the same key cannot slip
  past.
- **Stale results are discarded, never applied.** The job writes into a buffered
  overlay of the store and that buffer is committed only if the session is still on the
  turn the job was built from — checked under the same lock that advances it. A result
  computed from a superseded snapshot is thrown away and counted as `stale_discarded`.

    Expect this to happen often. At agent turn rates (seconds) a compaction taking tens
    of seconds is usually superseded before it lands, so async pays for work it then
    discards. That is the deliberate trade: never apply a decision computed against a
    transcript the session has moved past. If `stale_discarded` dominates `processed`,
    your turns are too tight for deferral and `sync` is the honest choice.

- **Unproductive sessions stop paying.** After a few deferred jobs in a row that produce
  nothing, a session stops enqueueing them. Each job is a real cheap-model call, and
  traffic that simply does not compact would otherwise buy an attempt every turn forever.
- **The request path never waits and never blocks.** A full queue drops, counted as
  `dropped`. The request has already been forwarded, so a drop costs savings only.
- **Bounded, owned workers.** One queue and a fixed worker count owned by the proxy,
  not a goroutine per request. Cancellation on shutdown returns every worker.
- **Fail-open everywhere.** A panicking job is contained; the worker survives.

### Reading the async counters

```
"async_queue": {
  "queued": 0, "pending": 1, "processed": 42,
  "dropped": 0, "errors": 0, "stale_discarded": 3
}
```

- `dropped` and `stale_discarded` are the counters that say *we silently gave up
  savings*. They are surfaced deliberately. (headroom's dashboard shows only
  `queued`, which hides precisely this.)
- A high `stale_discarded` relative to `processed` means turns arrive faster than
  compaction finishes, so most deferred work is computed and then thrown away. Expect
  this at agent turn rates. Raise `workers`, or use `sync` — deferral cannot pay off if
  nothing ever lands.
- `async_tail_unprotected_turns` counts turns async **refused** to defer because the
  agent had cache-written the span a compaction would replace. Non-zero and climbing
  means async is inert on this workload; see `strip_caller_breakpoints` above.
- `async_realized_saved_tokens` counts savings a turn got by replaying deferred work, and
  counts at all only once a compaction has actually landed for that session. It is a
  strict subset of `saved_tokens`; if it ever equals it, the counter is lying.
- A rising `dropped` means `max_queue` is too small for your concurrency.
- `errors` counts jobs that ran and failed. Non-zero with zero `processed` means the
  compaction path itself is broken — check the cheap model's credentials.

## observe — measure without enforcing

The agent receives its request **untouched, byte for byte**. The request path does not
run the pipeline at all, and does not inject the expand tool either — injecting a tool
declaration would modify the request, which is the one thing this mode promises never
to do. A copy of the request runs off-path against observe's own state store — disjoint
from the live one — purely to record what compaction *would* have achieved.

This is the answer to "will context-guru help *my* traffic" that does not require
enforcing it in production and comparing against history.

### How to read observe numbers

Observe-mode numbers live under their own keys and **never share a key with an
enforced metric**:

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
`requests`, `tokens_before`, `tokens_after`, `saved_tokens`, `sync_enforced`,
`async_enforced` and the `components` map. That zero is the machine-readable form of
"context-guru did not modify any request".

Two enforced-namespace fields are deliberately **not** zero, because they are real
measurements rather than hypotheticals:

- `cg_added_ms_avg` — the actual latency added to the enforced path, which in observe mode
  is ~0 precisely because that path does no pipeline work. Zeroing it would hide the mode's
  headline result.
- `llm_calls` / `llm_input_tokens` / `llm_output_tokens` — context-guru's own model spend.
  Observe measures off-path, and that measuring costs real money. The spend is not
  hypothetical, so it stays where cost tooling already reads it, labelled by
  `observe_llm_notice` as the cost of measuring rather than of enforcing.

A mislabelled hypothetical is worse than no number at all, because it silently
inflates a savings claim. The separation is therefore structural — two physically
separate accumulators with disjoint serialized names — and a test asserts that no
enforced aggregate can reach an observe result.

### Why observe's numbers should match sync's

The projection is measured under the **same** conditions an enforcing mode would run
under, because that agreement is what validates the mode. Two things are required and
neither is obvious:

- **The same cached-prefix boundary.** Observe shares the per-session boundary the
  enforced path uses. Without it, cache-awareness never gates anything, every message
  in the transcript looks compactable, and the projection overstates savings by exactly
  the amount cache-awareness costs — measured at 9.5% projected against 0.8% actually
  achieved on the same SWE-bench tasks.
- **State that accumulates across turns.** Offloaders *freeze* a decision and replay it
  on every later turn, which is where most of the sustained saving comes from. So
  observe keeps a store of its own — as persistent as the live one, and completely
  disjoint from it. Discarding its state each turn instead makes it see only the
  current tail and **under**-project by ~3x.

The live store stays pristine either way: observe never writes a byte into it, or a
later real request could replay a decision that was never enforced — a request
modification arriving by the back door. Both properties are asserted by tests.

### What observe cannot tell you

- **Cache effects are projected, not measured.** The forwarded request is the
  agent's own, so the provider's real cache behavior is the *baseline's*, not the
  compacted one's. `potential_saved_tokens` is a content-token figure; the cache
  consequence of actually enforcing is not measured here.
- **Reversibility is not exercised.** Nothing was offloaded, so no expand bounce can
  happen and `wasted_tokens` stays at zero. Under `sync`, some savings come back as
  bounces. Treat observe's projection as an upper bound on content savings.
- **Off-path compaction may make model calls.** Observe does not modify requests, but
  its measurement is real work: if your pipeline includes `extract_llm`, observe mode
  spends cheap-model tokens. `llm_calls` reports it.

## Switching modes

Mode is per-process, not per-request: it decides what happens to every request the
proxy handles, and the mode is reported in `/stats` so a consumer never has to guess
which regime produced a number.

Session state (frozen decisions, stashes) carries across a restart only as far as the
store does — in-memory by default, so a restart starts cold in every mode.
