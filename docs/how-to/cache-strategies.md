# Choose a cache strategy

A cache strategy sets how Context Guru keeps your prompt cache alive while you are away. This page
describes every strategy. It has two sections: the **main agent** and the **subagents**. Each strategy
treats them differently.

Pick one with `/context-guru:cache-strategy-picker`, or set `cache.strategy` in the configuration. For the
keys, the pings and the settings page, see [Keep an idle prompt cache warm](cache-keepalive.md).

## Terms

- **Cache tier.** Anthropic keeps a cached prompt for **5 minutes** (the default tier) or **1 hour**. A
  write costs **1.25x** the input price at 5 minutes and **2.0x** at 1 hour. A read costs **0.1x**.
- **Ping.** A request the proxy sends while you are away. It re-reads the cached prompt, so the entry stays
  alive. A ping costs one read.
- **Rebuild.** The next request after the entry died. It writes the whole prompt again at the write price.
- **Long wait.** The agent waits for a person, a teammate, background work, or a cause the proxy cannot name.
  A **short wait** is a tool, or a retry.
- **Main agent.** The thread you talk to. **Subagent.** A thread the main agent starts.

## All strategies at a glance

| Strategy | Main agent | Subagents | Spends on pings |
|---|---|---|---|
| `none` | nothing | nothing | no |
| `auto` (default) | pings at 4 minutes, at most 2; 1 ping at 59 minutes if the entry is at 1 hour | the same | yes |
| `1-ping` | at most 1 ping per idle span | the same | yes |
| `2-ping` | at most 2 pings per idle span, also on a 1-hour entry | the same | yes |
| `1-hour-head` | 1-hour tier on tools and system only, for prompts of 50k tokens or more; **no pings** | the same | no |
| `1-hour-on-wait` | `auto`, but on a long wait the first ping moves the entry to 1 hour, and it is the only ping (mode A). On a costly route: mode D | `auto` | yes |
| `1-hour-always` | 1 hour from the first request, no pings (mode C). **Opt-in; it does not pay for most users** | `auto` | yes |
| `30-min-ping` | OpenAI only: a ping at 28 minutes | the same | yes |

## The main agent

### `none`

No pings. The cache lives 5 minutes after your last request. A pause of more than 5 minutes means a rebuild.
Pick it when you want no spend at all.

### `auto`, `1-ping`, `2-ping`

The proxy pings the main agent when it has been idle for 240 seconds, up to 2 times (`auto`, `2-ping`) or 1
time (`1-ping`). That keeps the entry alive for about 13 minutes. A ping costs 0.1x of the prompt. A rebuild
costs 1.25x. So one ping pays for itself about 11 times over, if you come back in time.

The number of pings follows the wait. A tool wait gets more pings. A wait for a person gets one more than the
base. A wait for a finished answer gets none. The prompt must be at least 80k tokens for a wait for a person,
and 20k for a tool wait.

If the entry is at the 1-hour tier (the client asked for it), the first ping goes out at 59 minutes.

### `1-hour-head`

The 1-hour tier on the `tools` and `system` blocks only, and only for requests of 50k tokens or more. It
turns the pings off. The message history stays on the 5-minute tier, so it goes cold after 5 minutes. It
covers a small part of a long prompt. Use it only if the head is most of your prompt.

Whether the provider grants the 1-hour tier depends on the model and the provider. On 2026-11-19 the gateway
granted it for `claude-sonnet-5`, `claude-sonnet-5-5`, `claude-opus-4-8`, `claude-opus-5-5` and
`claude-haiku-5-5`. An earlier test refused `claude-sonnet-5` (0 of 48,212 tokens). See
[#1243](https://github.com/rossoctl/context-guru/issues/1243).

### `1-hour-on-wait`, mode A (a free switch)

`auto`, with one change. On a **long wait** the first ping, at the usual time, carries every cache marker at
the 1-hour tier. It is the **only** ping of that wait: there is no 59-minute ping. A tool wait behaves as
`auto`.

On the gateway measured on 2026-11-19, the move from 5 minutes to 1 hour is a **read**, not a rewrite (15 of
15 runs). So one read at 0.1x keeps the prompt for an hour, and no request pays the 2.0x write.

The proxy does not trust this blindly. For each provider, route and model:

1. The first long wait is a **probe**. Only one wait probes at a time. The band shows `1h?` until it is proven.
2. The thread returns more than 5 minutes later and still reads the prompt: the route is **proven**.
3. The return is a cold rebuild: the gateway downgraded the tier. The route is switched off and uses `auto`.

### `1-hour-on-wait`, mode D (a costly route)

If the probe finds that the switch **rewrites** the prompt (expected on the Anthropic first-party API, not
tested), mode A stops on that route. Mode D takes over:

- The main thread runs as `auto`.
- At its first **cold rebuild of a prefix of at least 150,000 tokens**, the request is sent with every cache
  marker at 1 hour. It is written at 1 hour.
- From then on, every request of the thread carries 1-hour markers, and the thread gets **no pings**.

The proxy decides the rebuild **before it sends the request**. The thread's last touch (its last response, a ping that read or re-wrote the
entry, or a summary or sweep call that took the place of a ping) is older than the lifetime of its entry, and its last prefix was at least 150,000 tokens. The
threshold is `keepalive_one_hour_rebuild_tokens`. A negative value (`-1`) turns D off: a costly route then uses plain `auto`. After a compaction the real prefix can be smaller than the
last one: the rebuild is then a little more expensive than planned.

If a 1-hour request that wrote tokens is billed at 5 minutes, the mode stops for that route and model.

### `1-hour-always`, mode C (opt-in)

The main agent writes at 1 hour **from its first request**, on any route. Every cache marker of every
request carries 1 hour. No marker is added. The main thread gets **no pings**.

> **On most users it does not pay off.** It pays the 1-hour write premium (2.0x instead of 1.25x) on every
> write, and most sessions have few long waits. In the simulation below it costs **22% more** than `auto` on
> hosted traffic: 77 of 97 main sessions are worse, and the premium is 39% of the cost. It is never a
> default. Pick `1-hour-on-wait` first.

It wins when your sessions often idle for more than 5 minutes between turns. On the owner's laptop it saves
25%.

## The subagents

Every strategy except `none` and `1-hour-head` pings a subagent exactly as `auto` does:

- A subagent has its own cache entry. It gets the 5-minute pings of `auto`. The prompt must be at least 20k
  tokens.
- A subagent that ends its turn waits for its main agent, which is working. That wait is short.
- The `1-hour-on-wait` and `1-hour-always` strategies **never** change a subagent request. No 1-hour marker,
  no 1-hour ping.
- `1-hour-head` changes the head of a subagent request too, if it is 50k tokens or more, and sends no ping.

## What it costs: the simulation

The production-monitor session simulated each mode on real traffic. It counts **main threads only**. It
assumes Bedrock (a free switch). It compares each mode with a simplified `auto`, at each model's input price.

| Mode | Hosted (97 main sessions, 16 tenants, `auto` $755.99) | Owner's laptop (33 main sessions, `auto` $505.28) |
|---|---|---|
| **A**: `1-hour-on-wait` | **-24%**; 44 better, 8 worse; no tenant worse | **-36%**; 11 better, 0 worse |
| B: keep 1 hour after the first long wait | -17%; 35 better, 24 worse | -29% |
| **C**: `1-hour-always` | **+22%**; 20 better, **77 worse** | -25%; 7 better, 26 worse |
| **D**, threshold 150,000 tokens | **-12%**; 12 better, 12 worse | -25%; 4 better, 2 worse |

- C on hosted: the premium is $359.02 of the cost (39%), rebuilds over 60 minutes $73.38, reads $488.19.
- D beats `auto` on both data sets, but it costs 15% to 18% more than A. It needs no switch, so it works on a
  costly route.
- Aggregates only. The sources are [#1243](https://github.com/rossoctl/context-guru/issues/1243#issuecomment-7037261430)
  and [#1261](https://github.com/rossoctl/context-guru/issues/1261).

## Which one should I pick?

- You want no spend: `none`.
- You want the default: `auto`.
- You often pause for more than 5 minutes, on Bedrock: `1-hour-on-wait`.
- You use the Anthropic first-party API: `1-hour-on-wait` (mode D protects you if the switch is costly).
- Your sessions are long and nearly always idle between turns: you may try `1-hour-always`. Measure it first.
