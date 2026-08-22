# Presets

A preset is a named default pipeline — an ordered list of component names.
Selecting one (`--preset <name>` / `PRESET`, or `preset:` in YAML) expands to
its pipeline; explicit config fields always override it. Pipelines below are
taken exactly from the `presets` map in `config/config.go`.

| Preset | Ordered pipeline | When to use |
|---|---|---|
| `codesmart` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` → `extract_llm` → `extract` → `cachesplit` | **The default.** The SWE-bench-winning cache-aware config: structural offloaders + a cheap-model relevance-trimmer (`extract_llm`, routed to `CHEAP_MODEL`, gated so most turns make no model call) + deterministic `extract`. `extract_llm` no-ops (→ deterministic) when no cheap model is configured. **Changed 2026-08:** the lossless trio replaced `toon`, which acted 0 times on 5,752 production requests, and `linecap` was added. Re-measure before quoting the published SWE-bench numbers against it. |
| `codesafe` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` → `extract` → `collapse` → `cachesplit` | `codesmart` minus the LLM pass — **deterministic-only, zero model calls by policy**. The safe control / the choice when you don't want an LLM on the hot path. |
| `off` | *(empty)* | Passthrough — no components. The baseline / A-B control. |
| `safe` | `format` → `textclean` → `searchfold` → `cachesplit` | Lossless only: repack JSON compactly and split the volatile system tail so the shared prefix stays cacheable. Zero risk of dropping content. |
| `balanced` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` → `cachesplit` | Lossless repack + conservative offloads (dedupe, drop superseded/failed runs, filter command noise) + the cache split. **Not recommended for agentic traffic** — it omits `mask`, the biggest lever there. |
| `aggressive` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` → `smartcrush` → `extract` → `extract_llm` → `cachesplit` | `balanced` plus `smartcrush` (crush long homogeneous arrays), deterministic `extract` (noise collapse), and `extract_llm` (cheap-model relevance trim) for deeper savings. |
| `coding` | `format` → `textclean` → `searchfold` → `dedup` → `cmdfilter` → `linecap` → `extract` → `cachesplit` | Coding agents, deterministic only: the components measured to actually act on real Claude Code traffic. **Changed 2026-08** — it previously named `skeleton` and therefore could not start on a normal build; see below. |
| `mcp` | `format` → `textclean` → `smartcrush` → `cachesplit` | Tool/MCP servers returning long homogeneous JSON arrays (list endpoints, search hits). |
| `agent` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `mask` → `extract` → `extract_llm` → `cachesplit` | Long agentic sessions (e.g. Claude Code on SWE-bench) where re-sent tool outputs dominate cost. `mask` is the biggest lever — ~27% content-token savings with no task-reward loss (see [Benchmarks](../RESULTS.md)). |
| `general` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` → `mask` → `extract` → `extract_llm` → `collapse` → `cachesplit` | The recommended all-round pipeline: the reward-neutral levers of `agent` plus the situational shrinkers (`cmdfilter` / `linecap` / `collapse`) that cost nothing when they don't fire. |
| `summarize` | `summarize` | Long trajectories where the transcript itself is the cost. **Runs alone** — it restructures the whole transcript (changes the message count), so no other component's in-place edits race the rebuild. |
| `agentdiet` | `format` → `agentdiet` → `cachesplit` | A **comparable baseline**, not a recommendation: the published [AgentDiet](../components/agentdiet.md) method ([arXiv:2509.23586](https://arxiv.org/abs/2509.23586)) at its tuned hyperparameters, for A/B'ing against our own reducers. One cheap-model reflection per turn on the step that just aged past `delay_steps`. Carries no other offloader on purpose — they would reduce the same tool outputs first and leave nothing to attribute. |

!!! info "The lossless trio, `toon`'s retirement, and `linecap` (August 2026)"
    `format` → `textclean` → `searchfold` now leads every preset that does deterministic work.
    All three verify-then-adopt — `format` re-parses, `textclean` compares informative lines,
    `searchfold` checks its own inverse byte-for-byte — so there is no risk argument for omitting
    one, and running them first makes every downstream token count honest. Two were missing:
    `textclean` shipped in `general` alone while 49.6% of corpus messages carry ANSI, and
    `searchfold` shipped in **no preset at all**, fully written and round-trip verified, folding
    nothing.

    [`toon`](../components/toon.md) is retired from every preset. Production: 0 acts in 5,752
    requests, `not_uniform_object_array` 234,437, and 0 convertible candidates in 11.67M measured
    tokens — at 1.53 ms and one `TextTokens` call per tool message. The component and its tests
    stay, so tabular traffic can enable it by hand.

    [`linecap`](../components.md#linecap) is new and runs after `cmdfilter`: a 500-char per-line
    cap with a never-truncate allow-list, plus a non-adjacent duplicate-line collapse. It is the
    answer to why 939 lines of per-command filters have matched two filters in production — the
    value in tool output is not per-command.

!!! note "`coding` changed in August 2026"
    It used to be `format → skeleton → cmdfilter → cachesplit`, and because
    [`skeleton`](../components/skeleton.md) is behind the `cg_skeleton` build tag (it is the
    only cgo component) it was **not registered in a normal binary** — so `--preset coding`
    failed at pipeline build with `components: unknown component "skeleton"` and the proxy
    exited. A preset is a promise that one word of config works, so a preset nobody could
    start was not worth keeping: it now names the deterministic components measured to act on
    this traffic. `TestEveryPresetBuilds` now FAILS on a preset that depends on a tag-gated
    component, instead of skipping it as it did while this shipped.

    `skeleton` itself remains available in a `cg_skeleton` build and remains **not
    recommended** — see its page for the measured 67.5% reduction on source reads and the
    correctness argument against enabling it.

!!! tip "Order matters"
    Components run in pipeline order: lossless repack first, then offloads
    (old-then-large), with `cachesplit` last because it edits the top-level
    `system` array rather than `messages`.

!!! info "`cachesplit`, not `cacheinject`"
    Every preset that touches caching carries [`cachesplit`](../components/cachesplit.md),
    which enables the measured volatile-tail split. Breakpoint *placement*
    ([`cacheinject`](../components/cacheinject.md)) is in **no** preset, because it has never
    been shown to help. Add it by hand if you want to run the placement study.

!!! warning "`extract_llm` is off by default on prompt-caching backends"
    `extract_llm` is the only component that spends money to save money, and on a
    prompt-caching backend it was measured **~8× underwater**: a token removed from a cached
    region saves the cache-read rate (`$0.30/MTok`), not the fresh-input rate (`$3/MTok`), so
    break-even is **~30,500 tokens per output** at the measured compression ratio — far above a
    typical tool output (the largest in one capture was 2,053).

    Since #28 the component **declines to run at all on caching backends** unless
    `allow_on_caching_backend: true` is set. This is enforced in code rather than documented as
    advice, because every caching workload measured came out net-negative even with the
    [economic gate](../components/extract_llm.md#economics) working correctly. So `codesmart`
    and `aggressive` still list `extract_llm`, but on caching traffic it makes **zero calls**
    and costs nothing; the deterministic passes do the work.

    On **non-caching** traffic it runs, and the gate decides per call — a strict improvement
    over the old behavior in every arm measured (waste cut 68% while saving more tokens on one
    capture; 26 calls reduced to 1 on another). Even there the honest result on those captures
    is break-even rather than profit: it earns its place when outputs are genuinely large.
    See [the component's measured tables](../components/extract_llm.md#measured-with-the-gate-on-replay-of-real-captures-awsclaude-haiku-4-5).

    `codesmart`'s pinned `min_tokens: 3000` still governs its per-output floor, unchanged.

Not sure which to pick? See [Choose a preset](../how-to/choose-a-preset.md).
Every component's config lives in [Components](../components.md).
