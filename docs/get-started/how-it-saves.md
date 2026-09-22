# How it saves money

context-guru saves **5–15% of your API cost in four ways** — two from carrying less
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

    **Measured:** −0.5% of tool-output tokens, fleet-wide. Small per request, and it
    compounds — it runs first, so every later component sees an honestly-smaller number.

=== "Unused MCP tools & skills"

    **Carry less context.** An agent declares its whole toolbox — every MCP tool,
    every skill — on **every** request, whether or not it ever calls them. Those
    declarations sit in the cached prefix and get re-read on every turn of the
    session. `toolfilter` drops the ones you tell it are dead weight.

    ```mermaid
    flowchart LR
      A["Session tools array<br/>21,889 tokens declared"] --> B{toolfilter<br/>remove: [...]}
      B --> C["Sent to model<br/>only what gets called"]
    ```

    **Example:** measured across real Claude Code sessions, **82.7%** of a declared
    tool catalogue is never invoked in that session — 27 tools carried by every
    session and called by none. Naming those in `remove:` drops them from every
    request of that session, not just one.

    **Measured:** 2–4% fewer system-prompt tokens, depending on how tool-heavy your
    setup is. `toolfilter` never removes a name you didn't list, and keeps anything
    still described in your system prompt's own prose.

=== "Compaction when it matters (experimental)"

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

    **Measured:** ~2% lower input cost. Marked experimental because *when* to
    compact for the best cache behavior is still being tuned — see
    [`summarize`](../components/summarize.md).

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

    **Measured:** 5–10% lower input cost — the largest single lever of the four,
    and it needs no configuration to try: [Keep an idle prompt cache
    warm](../how-to/cache-keepalive.md).

---

*These are separate baselines measured independently — the four don't simply add
up, and your own mix will vary with how you use tools, how long your sessions run,
and how often they sit idle.*
