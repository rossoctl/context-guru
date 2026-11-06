# How it saves money

context-guru saves **15–30% of your API cost in four ways** — two from carrying less
context, two from paying less for what you still carry. Each is independent: none of
them need the others, and none of them touch what the model sees or does.

=== "Smaller tool outputs"

    **Carry less context.** Tool output is full of tokens the model never reads:
    pretty-printing whitespace, ANSI color codes, terminal redraws, repeated warning
    lines. `format`/`textclean`/`searchfold`/`linecap` strip exactly that — deterministically,
    with no model call — and leave every unique, informative byte untouched.

    ```mermaid
    flowchart LR
      A[Tool output<br/>2,000-line build log] --> B{format · textclean<br/>searchfold · linecap}
      B --> C[Same information<br/>fewer tokens]
    ```

    **Example:** a dependency install repeats "resolving dependency graph" 30 times
    before the real error. `linecap` collapses the repeats to one line + `(×30)`;
    `format` compacts any pretty-printed JSON in the same response. Nothing an agent
    would act on is ever touched — file paths, stack frames, and diffs always survive
    a cap intact.

    **Measured:** ~1% lower cost (`conservative` and up). Small per request, and it
    compounds — it runs first, so every later component sees an honestly-smaller number.

=== "Tool search for MCP & skills"

    **Carry less context.** An agent declares its whole toolbox — every MCP tool,
    every skill — on **every** request, whether or not it ever calls them. Those
    declarations sit in the cached prefix and get re-read on every turn of the
    session. With tool search (the `ultrahigh` preset, experimental), the tools your
    own history shows you do not use are sent as names only. The model loads one
    through search when it needs it.

    ```mermaid
    flowchart LR
      A["Session tools array<br/>21,889 tokens declared"] --> B{toolfilter<br/>defer: auto}
      B --> C["Sent to model<br/>used tools + names of the rest"]
      C -. search .-> D["Tool loaded<br/>when needed"]
    ```

    **Example:** measured across real Claude Code sessions, **82.7%** of a declared
    tool catalogue is never invoked in that session. Deferral sends the same set on
    every turn of a thread, so the cached prefix stays valid.

    **Expected:** 5–10% lower cost, depending on how many MCP tools and skills you
    declare. A replay of live sessions measured 5.9% ([#485](https://github.com/rossoctl/context-guru/issues/485)).
    See [Experimental: ultrahigh](https://github.com/rossoctl/context-guru#experimental-ultrahigh)
    for accuracy and limits. To drop a fixed list of tools instead, use
    `toolfilter: {remove: [...]}`.

=== "Compaction when it matters"

    **Pay less for what you still carry.** Left alone, a long session gets
    compacted only when it hits the context limit — a forced, disruptive cut, done
    at the worst possible time. `summarize` can instead fold the *middle* of a long
    trajectory into one LLM-written summary earlier, while there's still headroom
    and the cache is warm, keeping the transcript small on your terms.

    ```mermaid
    flowchart LR
      A["[system, 30 turns of<br/>tool calls + results]"] --> B{summarize}
      B --> C["[system, one summary,<br/>last 3 turns]"]
    ```

    **Example:** a 30-turn debugging session where the first 25 turns are dead
    ends. Summarizing them into one paragraph, grounded in the current task, keeps
    the last few turns verbatim and the transcript a fraction of the size — done
    once, then reused byte-for-byte on every later turn instead of re-summarizing
    each time.

    It works in two parts:

    - **Summarize early** (`medium`, `high`): fold older turns before the context
      limit, while the cache is still warm. See [`summarize`](../components/summarize.md).
    - **Sweep cold turns** (`xhigh`): after a turn's cache has expired, drop the old
      tool outputs that the task no longer needs.

    **Measured:** ~5% lower cost on `xhigh` (live, 2026-11). Most of it comes from the
    sweep, so `medium` and `high` save less.

=== "Smart cache management"

    **Pay less for what you still carry.** Providers charge far less to *read* a
    cached prompt than to rebuild it. Idle time is what breaks that: a session that
    sits untouched for a few minutes lets the provider's cache expire, and the next
    message repays the whole prefix at the expensive rate. Keep-alive sends a tiny
    ping just under that TTL so the cache never goes cold — bounded by a budget, so
    it never spends more than the rebuild it's avoiding.

    ```mermaid
    sequenceDiagram
      participant You
      participant Guru as context-guru
      participant Provider
      You->>Provider: turn 1 (cache created)
      Note over You,Provider: 4 minutes idle — cache close to expiring
      Guru->>Provider: keep-alive ping (cheap)
      Note over Provider: cache TTL renewed
      You->>Provider: turn 2 — cache READ, not rebuilt
    ```

    **Example:** a cache miss on a 150k-token prefix bills at roughly 8–12.5× the
    cache-read rate. A ping costing a few cents keeps that prefix warm instead of
    triggering a rebuild that costs dollars.

    **Expected:** 15–20% lower cost — the largest single lever of the four. It is on
    by default in the plugin. On the proxy binary, keep-alive is off until you turn it
    on: [Keep an idle prompt cache warm](../how-to/cache-keepalive.md).

---

*These are separate baselines measured independently — the four don't simply add
up, and your own mix will vary with how you use tools, how long your sessions run,
and how often they sit idle.*
