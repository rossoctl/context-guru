# cacheinject

!!! info "Reformat — lossless. **In no preset: opt in explicitly.**"
    Places Anthropic `cache_control` breakpoints at the positions that minimise billed input
    cost, so the provider KV cache is read rather than re-processed. Placement has never been
    shown to help, so no shipped preset enables it — the presets carry
    [`cachesplit`](cachesplit.md), which enables the *measured* volatile-tail split. See
    [Configuration](#configuration).

## How it works

Placement is the solution to a cost minimisation, not a heuristic. With `R = 0.1` (cache
read), `W = 1.25` (5m cache write) and `1.0` (plain input) as multipliers on the base input
price, four facts fix the policy:

1. **Write anything resent even once.** A token resent `k` more times costs `W + kR` written
   and `1 + k` unwritten, so writing wins for `k > (W−1)/(1−R) = 0.28`. Hence a breakpoint on
   the **last block**, every turn.
2. **Writes bill as a span**, `(highest breakpoint − read point)`, not per breakpoint. An
   extra breakpoint *below* the top costs exactly zero and adds a position a later turn's
   backward read-walk can land on, so none of the four slots is left idle.
3. **Divergence is the large lever.** If a turn differs from the previous one at block `d`,
   every block above `d` is unmatchable and the whole prefix bills at 1.0x instead of 0.1x. A
   breakpoint at `d−1` recovers it — worth 12.5x more than any positional tuning, because it
   restores read→read rather than read→write.
4. **Anchors must be turn-stable.** A position is readable on turn `t+1` only if it was
   written on turn `t`, so anchors are counted up from the start, never back from the end.

```
before:  [ tools ][ system ]…[ msg d−1 ][ mutated msg d ]…[ newest turn ]

after:   [ tools ][ system ]…[ msg d−1 {cache_control} ]…[ newest turn {cache_control} ]
                              ^ rescues the stable head    ^ writes this turn's growth
```

The component tracks a per-message digest per session to find `d`, and **keeps every
breakpoint the caller already set** — an agent that places its own is usually already at the
optimum, and the provider's 4-breakpoint cap means a fifth is a 400.

`5m` TTL by default. A `1h` write costs 2.0x instead of 1.25x and only pays when the chance of
an entry lapsing before its next reuse exceeds 83.3%. Agent turns are seconds apart (measured
median 7.6 s over 1,905 real turns) and every read refreshes the TTL for free, so set
`ttl: 1h` only when reuse is genuinely sparse — a low-concurrency sweep with task starts more
than 5 minutes apart, or a deployed agent handling a few sessions per hour.

## What placement is actually worth

**Effectively unmeasured, and the one live reading is mildly negative — which is why it ships
off.** One `cacheonly` vs `off` pair on SWE-bench Verified (`aws/claude-sonnet-5`, n=1 per arm,
one task completed in both arms):

| metric | off | cacheonly | delta |
|---|--:|--:|--:|
| steps | 14 | 11 | −21.4% |
| billed cost | $0.17607 | $0.14931 | −15.2% |
| cache-read | 609,968 | 461,844 | −24.3% |
| cache-write | 8,695 | 11,062 | +27.2% |
| cache-hit | 98.59% | 96.66% | −1.93 pp |

The −15.2% is **not a saving**: the agent took 3 fewer steps, and on this traffic cost tracks
steps at corr 0.95. Per step, cost is +7.9% and cache-write +61.9%. Read that as one task,
once, with a degenerate control — and as a reason to leave placement off, not as a measurement
of the policy.

**Against an agent that already caches well, expect exactly 0%.** Claude Code marks its own
final message on 466 of 472 measured requests (98.7%), so rule 1 is already satisfied and
every position this component would choose collides with an existing breakpoint. The component
earns its keep on agents that do *not* mark their own tail, and when a prefix mutates
mid-conversation.

## Simulated comparison

A simulation over a captured 91-request SWE-bench stream, calibrated to within 2.10 pp of a
live 50-task run's cache-hit rate. It models the billing rule credibly; it is not a measurement
of the policy.

| policy | cost | hit | vs the agent's own placement |
|---|--:|--:|--:|
| claude-code's own breakpoints | $0.9270 | 96.04% | — |
| a breakpoint before the newest turn | $0.9780 | 95.23% | **+5.50%** |
| **this policy** | $0.9315 | 96.04% | **+0.49%** |

Placing a single breakpoint *before* the newest turn shortens the cached prefix on every turn —
a real cost increase, and the reason the policy marks the last block instead.

## The 4-breakpoint budget is computed by the host

The provider caps `cache_control` at **4 across `system` + `tools` + `messages` together**, and
a component sees none of the first two. So `apply` counts them structurally from the raw body
and passes the total as `Ctx.ExistingBreakpoints` for the component to budget against; the
count covers the Bedrock `cachePoint` spelling too. `apply` also counts the output and logs an
error if *we* pushed the request over 4 — a request that arrived over the cap is forwarded
as-is and is not reported as ours.

On real Claude Code traffic the agent spends 3 of the 4 slots itself (1,771 of 1,794 requests
carry exactly `system=2, tools=0, messages=1`), leaving this component one to place.

`cache_control` is metadata, not content, so `apply` writes it as a targeted `sjson` key on the
original raw bytes after verifying the component's only change to that message was adding
`cache_control` keys. Any wider change is discarded and counted in `discarded_changes`.

## Lossiness

None. It attaches cache directives only; model-visible content is unchanged, asserted by
comparing model-visible text byte-for-byte across all 91 captured requests.

Messages whose content is a bare string cannot carry a block-level directive; they are skipped
rather than restructured, and the breakpoint falls back to the nearest markable block below so
the prefix is still written.

## Configuration

```yaml
components:
  cacheinject:
    ttl: 5m        # or 1h — see the TTL rule above. Anything else is rejected at load.
```

`K=4` breakpoints and `L=20` lookback blocks are provider facts, not tunables.

`cacheinject` is in **no** preset: placement has never been shown to help, and shipping it on
by default would enable an unmeasured policy on every request. The presets carry
[`cachesplit`](cachesplit.md) instead, which enables the volatile-tail split — the measured
part — without the placement. Add `cacheinject` by hand to run the placement study, or when
your agent does not set its own `cache_control`.

## When it's inert

On OpenAI- and Gemini-shaped wires, where `cache_control` does not exist at all. Also when four
breakpoints are already present anywhere in the request (including `system` and `tools`), and on
string-content messages.

## The volatile-tail split

Enabling **`cachesplit`** (or `cacheinject`) switches on a body-level repair in
`apply/prefixsplit.go` that no breakpoint placement can achieve, because a cache entry hashes
*everything before* its breakpoint and no position can exclude part of a single block. This is
the mechanism the default presets keep, and it is the one with strong evidence.

Claude Code appends a live environment snapshot to the **end** of its main system block:

```
Current branch: main
...
Recent commits:
0898367954 SWE-bench
```

Across 50 SWE-bench tasks that block is ~7,017 tokens, of which the first **6,921 (98.4%)** are
byte-identical across sessions — but it is one cacheable unit with its breakpoint at the end, so
the hash covers the churning tail and the shared 98.4% is re-written every session.

The tail is real content and cannot be dropped without lying to the model about the repo state.
It can be **split**: `[stable][volatile]` as two text blocks with the same concatenated text,
breakpoint on the first. Adjacent text blocks concatenate, so the model sees a byte-identical
prompt while the provider gains a hash boundary that excludes the churn.

**Explicit-breakpoint providers only** (the Anthropic family). Under an implicit longest-prefix
cache (OpenAI, Gemini) the match already ends at the divergence, so a block boundary buys
nothing.

### What the split is worth

**Is there a target?** Across 50 real Claude Code sessions the stable half of that block takes
**1** distinct value while the volatile tail takes **50** — so without the split, the hash
covers a value unique to each session and the 6,877-token stable half can never be read from
another session's cache.

**Does the provider actually cache it?** Two sessions differing only in the git snapshot, same
breakpoints, same order, judged by `cache_read_input_tokens` from the API. Three runs,
byte-identical results:

| arm | session-2 read | session-2 write | hit |
|---|--:|--:|--:|
| without split | **0** | 8,882 | **0.0%** |
| **with split** | **8,597** | **0** | **96.7%** |

At Sonnet 5 rates that first request of a warm session costs **$0.022205 → $0.001719** —
**$0.0205 saved per session (−92.3%)**. Per model: Sonnet 5 **$0.0205** (**$0.0307** from
2026-09-01), Opus 5 **$0.0512**, Haiku 4.5 **$0.0102**.

The saving lands **once per request carrying the system prompt**, not once per turn: within a
session, turns 2…n already read the prefix turn 1 wrote in both arms. A cold first session pays
the same either way.

**End to end in a real agent run** — Terminal-Bench, `fix-code-vulnerability`, 3 trials:

| trial | without | with split | saved |
|---|--:|--:|--:|
| 1 | $0.2003 | $0.1368 | $0.0636 (31.7%) |
| 2 | $0.1828 | $0.1198 | $0.0630 (34.5%) |
| 3 | $0.2003 | $0.1280 | $0.0723 (36.1%) |
| **mean** | **$0.1945** | **$0.1282** | **$0.0663 (34.1%)** |

The end-to-end saving is 3.2× the isolated per-session figure because Claude Code issues
sub-agent and side requests that each re-send the same system prompt, and each was
independently re-writing it.

Treat 34.1% as one task measured three times, not a fleet average: a second Terminal-Bench task
was within noise, its numbers are not quoted, and all figures are Sonnet 5 rates on one
benchmark. Placement contributes **$0** of it — every dollar above is the split.

See also: [Components overview](../components.md) ·
[Choose a preset](../how-to/choose-a-preset.md)
