# Choose a cache strategy

A cache strategy decides how Context Guru keeps your prompt cache warm while you are away.
Pick one with `/context-guru:cache-strategy-picker`, or set `cache.strategy` in the configuration.
For the keys and the settings page, see [Keep an idle prompt cache warm](cache-keepalive.md).

## Which one should I pick?

| You want | Pick |
|---|---|
| No spend at all | `none` |
| A safe default | `auto` |
| Lower cost when you often leave for more than 5 minutes (opt-in) | `1-hour-on-wait` |
| The 1-hour cache on the start of the prompt only, and no pings | `1-hour-head` (a small gain; read its section first) |
| The 1-hour cache on every request | `1-hour-always` (**does not pay off for most users**) |
| OpenAI Responses only | `30-min-ping` |

`1-ping` and `2-ping` are `auto` with a different base number of pings.

## Terms

- **Prompt cache.** The provider keeps the start of your prompt for a short time. A request that
  reuses it pays less.
- **Lifetime.** How long an entry stays in the cache after its last use. It is **5 minutes** or
  **1 hour**.
- **Prices**, as a multiple of the normal input price:

  | Action | Price |
  |---|---|
  | Read (cache hit) | 0.1x |
  | Write, 5-minute lifetime | 1.25x |
  | Write, 1-hour lifetime | 2x |

- **Rebuild.** The cache entry has expired. The next request writes the whole prompt again.
- **Ping.** A request that the proxy sends while you are away. It sends the whole prompt and asks for almost no output. It reads the cached prompt and
  so restarts the lifetime. It costs one read. The proxy sends the planned pings of
  one idle span only when they cost less than the one miss they protect (the value rule, #1420). An operator can also set a flat cap per ping
  with `keepalive_max_usd_per_ping`. No strategy sets it.
- **Main agent.** The thread you talk to. **Subagent.** A thread that the main agent starts.
- **Prefix.** The part of the prompt that the cache holds. The proxy pings only a prefix that is big
  enough (see the table below).

One ping (0.1x) pays for itself when it saves a rebuild (1.25x) more than 1 time in 12.

## Which wait is it?

When a turn ends, the proxy reads **what the thread waits for**. It uses the stop reason, the tool
names and the number of open background tasks and teammates. It finds open tasks and teammates
from marker strings in the messages and tool results. It does not read the meaning of the text.
The proxy stores the result as `waiting_for` on each request row. You can read it through the
dashboard API or the database.

The number of pings and the smallest prefix depend on this cause. "Base" is the base number of
pings of the strategy (2 for `auto`).

| `waiting_for` (stored value) | What it means | Main agent: pings, smallest prefix | Subagent: pings, smallest prefix |
|---|---|---|---|
| `own_tool` | The thread called a tool and waits for the result. | Until the result comes, at most 8. 20k tokens. | The same: at most 8. 20k tokens. |
| `human` | The turn ended and nothing else is open. Also a call to a tool that asks a person (`AskUserQuestion`, `ExitPlanMode`). | Base + 1. 80k tokens. | Base. 20k tokens. **It is a wait for the main agent, not for a person.** |
| `background` | Background work is open (a background command, a monitor, an async agent). | Base + 3. 20k tokens. | Base. 20k tokens. |
| `teammate` | An open message to a teammate waits for a reply. | Base. 20k tokens. | Base. 20k tokens. |
| `unknown` | The proxy cannot tell (the response had no stop reason and no usage). | Base. 80k tokens. | Base. 20k tokens. |
| `done` | The last tool call is a terminal tool (`StructuredOutput`). The thread has ended. | No ping. | No ping. |
| `retry` | The response failed (not a 2xx status). The agent will send the same request again. | No ping. | No ping. |

- A subagent ends a turn with `end_turn` or with the tool `SubagentHandback`. Both show as `human`.
  The subagent then waits for its main agent, which is working. The dashboard says `human`, but the
  proxy plans no wait for a person.
- A thread that has not called any tool yet gets **no ping** (it may be a side call, such as a title).
- A manager's time windows (working hours) do **not** apply to `own_tool`, `background`, `teammate`,
  or a subagent's `human` wait. They apply to every other cause: `human` of a main agent, `unknown`,
  `done` and `retry`.
- The numbers 8, +3, +1, 20k and 80k are the defaults. You can change them. See
  [the ping count follows the wait](cache-keepalive.md#the-ping-count-follows-what-the-thread-waits-for).
- A **long wait** is a main agent in `human`, `background`, `teammate` or `unknown`. A tool wait
  (`own_tool`) is short. A subagent never has a long wait. Only `1-hour-on-wait` uses this term.

## Pings and cache lifetime at a glance

- On a 5-minute entry, the first ping goes out after 4 minutes, and each next ping 4 minutes after
  the one before. With the base of 2 and no extra, the entry lives about 13 minutes.
- On a 1-hour entry (the client asked for it, and the provider granted it), the proxy sends 1 ping
  at 59 minutes. `2-ping` sends a second ping 59 minutes later. The wait cause does not change this.
  The rules for `done`, `retry` and "no tool call yet" still apply.

## Strategies for the main agent

### `none`

- **Does:** nothing. No ping, no spend.
- **Lifetime:** 5 minutes after your last request.
- **Cost trade:** every pause of more than 5 minutes ends in a rebuild (1.25x).
- **Pick it when:** you want no spend at all.

### `auto`

- **Does:** pings while you are away, as in the table above.
- **Pings:** base 2. A 5-minute entry gets 2 pings for `teammate`, `unknown` and a waiting
  subagent, 3 for `human`, 5 for `background`, and up to 8 for `own_tool`.
  A 1-hour entry gets 1 ping at 59 minutes.
- **Lifetime:** 5 minutes after each ping or request.
- **Cost trade:** each ping costs 0.1x. You pay it, and you pay a rebuild (1.25x) only when the pings
  end before you return.
- **Pick it when:** you want the safe default.

### `1-ping` and `2-ping`

They are `auto` with another base.

| | `1-ping` | `2-ping` |
|---|---|---|
| Base on a 5-minute entry | 1 | 2 (the same as `auto`) |
| `human` / `background` / `own_tool` | 2 / 4 / 8 | 3 / 5 / 8 |
| 1-hour entry | 1 ping at 59 min | 2 pings, 59 min apart |

**Pick `1-ping`** to spend less on pings. **Pick `2-ping`** when your sessions use the 1-hour cache
and you leave for more than 2 hours.

### `1-hour-on-wait`

- **Does:** it is `auto`, with one change for a **long wait** of the main agent. The first ping, at
  the normal time (after 4 minutes), asks for the 1-hour lifetime on every cache marker. It is the
  **only** ping of that wait. There is no ping at 59 minutes.
- **Tool waits:** the same as `auto`.
- **Lifetime:** if the provider grants it, the entry lives 1 hour after the ping.
  Your next request reads the entry and moves it back to 5 minutes.
- **Cost trade:** on the gateway we measured, this switch is a read, not a rewrite (15 of 15 runs,
  2026-11-19). So one read (0.1x) covers a full hour, and no request pays the 2x write.
- **Pick it when:** you often leave for more than 5 minutes.

The proxy checks the grant. It does not assume it. It checks per provider, endpoint, route and model.

**The test.** The first long wait sends the 1-hour ping as a test. Only one wait tests a route at a
time. The band shows `1h?` (1 hour, not yet proven) until the test ends. There are three outcomes.

| What the proxy sees | What it means | What the proxy does |
|---|---|---|
| The ping reads the prompt and writes almost nothing. You return 5 to 60 minutes later and the prompt is **still cached**. | The provider keeps the 1-hour lifetime. | The route is proven. Every long wait uses the 1-hour ping. The band shows the 1-hour clock without `1h?`. |
| The ping reads the prompt and writes almost nothing. You return 5 to 60 minutes later and the prompt is **gone** (a rebuild). | The provider **did not keep the 1-hour lifetime**. It quietly gave 5 minutes. | The route is switched off for the life of the proxy. The proxy uses plain `auto` there. It does **not** write at 1 hour: that write would pay more and still lose the prompt after 5 minutes. |
| The ping itself reads almost nothing and **writes the whole prompt** at 1 hour. | The provider **charged for the switch**: it rewrites the prompt. We expect this on the Anthropic first-party API. We have not tested it. | The route is marked costly. The proxy sends no more 1-hour pings there. The main agent follows the next section. |

A ping that failed, landed on an entry that had already expired, or whose usage was neither a free nor a costly switch decides
nothing. The proxy releases the test, and a later wait tests again.

A return inside 5 minutes, after more than 1 hour, on another model, or after a rewrite of the
prompt by the client (for example a compaction) proves nothing. The proxy tests again later.

If a summary or a sweep call takes the first ping, the 1-hour ping follows it.


#### On a route where the switch is costly

The provider charged for the switch, so 1-hour pings stop on that route. The main agent then does this:

- It runs as `auto` until its first **rebuild of a big prefix**: at least 150,000 tokens.
- That rebuild goes out with 1-hour markers. The proxy writes the whole prompt at 1 hour (2x).
- From then on, every request of this thread asks for 1 hour, and the thread gets **no pings**.
- The thread that found the rewrite does the same from the start.

The proxy decides this before it sends the request. Its last response, ping or summary call is
older than the entry lifetime, and its last prefix was at least 150,000 tokens. After a
compaction the real prefix can be smaller than the last one. The rebuild then costs a little more
than planned.

- **Change the size:** `keepalive_one_hour_rebuild_tokens` (default 150,000).
- **Turn it off:** `-1`. The route then uses plain `auto`.
- **Stop:** this mode has its own check. If a request with 1-hour markers writes tokens but the
  provider bills them at 5 minutes, the provider did not keep the 1-hour lifetime. The proxy stops
  this mode for that route and model, and the route uses plain `auto`. A request that wrote nothing
  proves nothing.

### `1-hour-always`

> **For most users it does not pay off.** It is an opt-in. It is never a default.

- **Does:** the main agent asks for 1 hour on every cache marker of every request, from the first
  request, on any route. It adds no marker. The main agent gets **no pings**.
- **Lifetime:** 1 hour after each request.
- **Cost trade:** every write costs 2x instead of 1.25x. Most sessions have few long waits, so the
  extra price is more than the savings. In the simulation below it costs **22% more** than `auto` on
  hosted traffic.
- **Pick it when:** your sessions nearly always idle for more than 5 minutes between turns. Test it
  first. A manager can also set it in a stored strategy (`one_hour_always`). It only turns the mode
  on, never off.

### `1-hour-head`

- **Does:** asks for 1 hour on the `tools` and `system` blocks only, and only on a request body of 200,000
  bytes or more (about 50,000 tokens at 4 bytes per token; `head_ttl_min_bytes` sets it). It sends **no ping** (the strategy does not spend).
- **Lifetime:** `tools` and `system`: 1 hour. The message history: 5 minutes, so it goes cold after
  5 minutes of idle time.
- **Cost trade:** the 1-hour part is a small share of a long prompt. Other sessions often keep it
  warm anyway.
- **Pick it when:** the head is most of your prompt. For a long session with idle gaps, use
  `1-hour-on-wait`.
- **Provider:** whether the provider grants the 1-hour lifetime depends on the model and the date.
  On 2026-11-19 the gateway granted it for `claude-sonnet-5`, `claude-sonnet-5-5`,
  `claude-opus-4-8`, `claude-opus-5-5` and `claude-haiku-5-5`. An earlier test refused
  `claude-sonnet-5` (0 of 48,212 tokens). See
  [#1243](https://github.com/rossoctl/context-guru/issues/1243).

### `30-min-ping`

OpenAI Responses only. The proxy pings 28 minutes after the last request, shortly before the 30-minute lifetime ends. On a proxy that
also serves Anthropic, `keepalive: true` also turns on the Anthropic pings with the defaults.

## Strategies for subagents

A subagent has its own cache entry.

| Strategy | What a subagent gets |
|---|---|
| `none` | Nothing. |
| `auto`, `1-ping`, `2-ping` | Pings on a 5-minute entry, as in the table above: up to 8 for `own_tool`, base for every other wait. The prefix must be 20k tokens or more. |
| `1-hour-on-wait` | The same as `auto`. The 1-hour ping and the 1-hour markers never apply to a subagent. |
| `1-hour-always` | The same as `auto`. The subagent keeps the 5-minute lifetime and the pings. |
| `1-hour-head` | The 1-hour head on a request body of 200,000 bytes or more (about 50,000 tokens). No ping. |
| `30-min-ping` | As for the main agent (OpenAI). |

- A subagent that ends its turn waits for its main agent. The dashboard shows `human`, but this wait
  is short. It gets the base number of pings and the 20k floor, not the 80k floor of a person.
- A subagent that waits for background work or a teammate gets the base number of pings. It gets no
  extra ping.
- A subagent in `done` gets no ping.
- A manager's time windows do not apply to a subagent's `human` wait, nor to its `own_tool`, `background` and `teammate` waits. They can apply to its `unknown`, `done` and `retry` causes.

### Caution: 1-hour writes for subagents

Today no strategy puts a subagent on the 1-hour cache, except the head rule of `1-hour-head`.
`1-hour-on-wait` and `1-hour-always` change the main agent only. A subagent keeps the 5-minute
cache and the pings of `auto`. In the code, `1-hour-head` has no check for the thread, so it also
changes the head of a subagent request.

A simulation shows why. It counts **subagent threads only** and assumes a provider where the switch
is a read (Bedrock). These are simulated results, not live measurements. Each is the change in cost
against `auto`.

| | Hosted traffic (2026-11-10 to 11-12; 387 subagent threads, 7 tenants) | Owner's laptop (2026-11-16 to 11-22; 43 subagent threads) |
|---|---|---|
| How long a subagent lives | Median 1.2 minutes. 90% live 14.2 minutes or less. | Median 24.7 minutes. 90% live 180 minutes or less. |
| Gaps of 5 to 60 minutes | 0.4% of all gaps | 7.9% of all gaps |
| 1-hour writes from the first request | **37.6% more expensive**. 382 of 387 threads worse. All 7 tenants worse. The 1-hour premium is $41.94 of $142.31. | **13.6% cheaper**, but 32 of 43 threads are worse. |
| One 1-hour ping on a long wait | 2.8% cheaper | 10.0% cheaper |
| 1-hour writes after a rebuild of 150,000 tokens or more | 0.6% more expensive (1 thread switched) | 13.7% cheaper |

- Details: [the subagent simulation in #1261](https://github.com/rossoctl/context-guru/issues/1261#issuecomment-7081747363). The table counts each subagent thread as one unit. The code applies none of these variants to subagents today.
- Most subagents are short. A 1-hour lifetime rarely pays back for them.
- 1-hour writes from the first request cost more for most users. Use them with caution.
- Only long-lived subagents gain.

## What the 1-hour options cost: simulations

These are **simulations on real traffic, not live measurements.** They count the main agent only.
They assume a provider where the switch is a read (Bedrock). Each result is the change in cost
against `auto`, at each model's input price.

| Strategy | Hosted traffic (97 main sessions, 16 tenants) | Owner's laptop (33 main sessions) |
|---|---|---|
| `1-hour-on-wait` | **24% cheaper**. 44 sessions better, 8 worse. No tenant is worse. | **36% cheaper**. 11 better, 0 worse. |
| `1-hour-on-wait` on a costly route (150,000-token rule) | **12% cheaper**. 12 better, 12 worse. | **25% cheaper**. 4 better, 2 worse. |
| `1-hour-always` | **22% more expensive**. 20 better, **77 worse**. | 25% cheaper. 7 better, 26 worse. |

- On the costly route, the result is 15% (hosted) to 18% (laptop) above `1-hour-on-wait` on a free
  switch. The reason: it pays the full rebuild until its first big rebuild.
- `1-hour-always` on hosted traffic: the 1-hour price premium is $359.02. That is 39% of the cost.
- **Anthropic first-party API: not tested.** One outside report (Sonnet 5.5 and Opus 5.5, one run each) says the switch rewrites the whole
  prompt there. We have not reproduced it. We have no first-party key to check this.
- Only aggregates are shown. Details: [#1243](https://github.com/rossoctl/context-guru/issues/1243#issuecomment-7037261430)
  and [#1261](https://github.com/rossoctl/context-guru/issues/1261). The `1-hour-on-wait` and `1-hour-always` rows come from [this table in #1261](https://github.com/rossoctl/context-guru/issues/1261#issuecomment-7076076690).

## What the band and the status line show

- **A 1-hour clock** after a 1-hour ping, a thread in the 1-hour mode, or a 1-hour request.
- **`1h?`** The clock is a 1-hour ping on a route that the proxy has not yet proven. Your return
  tests it.
- **No Ping button** while the clock is on a 1-hour ping. A manual ping uses 5-minute markers and
  would move the entry back to 5 minutes. The status line has no Ping button.
