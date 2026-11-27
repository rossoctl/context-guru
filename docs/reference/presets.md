# Presets

A preset is a named, ordered pipeline. Set it once and every explicit field you add overrides it.

```sh
context-guru-proxy --preset conservative      # or PRESET=conservative, or preset: in YAML
```

Six presets are current. They form one ladder. Each step adds to the step below it.

## Which preset?

| Your workload | Preset |
|---|---|
| Nothing: passthrough, A/B baseline | `off` |
| **Start here.** No model calls. | **`conservative`** |
| Long sessions. Adds a summary for a cold return. | `medium` |
| Long sessions. Summary at 90% of the window, warm or cold. | `high` |
| Deepest cut. Adds the model sweep of old tool outputs. | `xhigh` |
| Deepest cut and tool deferral. Opt-in, experimental. | `ultrahigh` |

`conservative` is the default of the proxy binary and of new hosted accounts. The old default
name, `house`, runs the same pipeline. It is [obsolete](#obsolete-presets) as a name, and still works.

## What each one runs

| Preset | What it adds | Pipeline |
|---|---|---|
| `off` | Nothing. | *(empty)* |
| `conservative` | Lossless repack, dedup, command filters, `extract`. No model calls. | `format, dedup, toon, cmdfilter, searchfold, textclean, extract, toolfilter` |
| `medium` | `conservative` plus `extract_llm` plus `cache_aware_summarizer` (`cache_state: pre_expiry`). The summary stays in reserve. The proxy uses it only when the cache has expired. | `cache_aware_summarizer, format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm, extract, toolfilter` |
| `high` | `medium`, with the summarizer at `cache_state: any`. The summary is made at 90% of the window and used on the next request. | same pipeline as `medium` |
| `xhigh` | `high` plus `extract_llm_sweep`. It asks the model at the keep-alive ping which old tool outputs are spent. It removes content only on a cold request. | `cache_aware_summarizer, format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm, extract_llm_sweep, extract, toolfilter` |
| `ultrahigh` | `xhigh` plus tool deferral (`toolfilter` with `defer: auto`). **Opt-in. Experimental.** | same pipeline as `xhigh` |

- `medium` and `high` differ in the settings of `cache_aware_summarizer`, not in the pipeline.
- `xhigh` and `ultrahigh` differ in the setting `defer: auto` of `toolfilter`, not in the pipeline.
- `medium`, `high`, `xhigh` and `ultrahigh` call a model. The component gates limit the spend.
- Full detail for each tier, with the exact rules, is in [Exact pipelines, verbatim](#exact-pipelines-verbatim).

Order is deliberate. The proxy repacks lossless first, then runs the cheap structural
offloaders, then anything that costs a model call. The `house` family (`house`, `housellm`,
`conservative` and up) keeps the operator's order: `dedup` and `cmdfilter` run ahead of the
lossless pair, and `toolfilter` runs last. That costs per-component attribution
in `/stats`, never content. See [`config/config.go`](../design.md#config-registry).

## Obsolete presets

!!! warning "Obsolete (#1345)"
    These 14 presets are **obsolete**: `safe`, `balanced`, `aggressive`, `coding`,
    `mcp`, `agent`, `general`, `summarize`, `summarizer1509`, `agentdiet`, `codesmart`,
    `codesafe`, `house`, `housellm`. Do not pick one for a new setup. Use one of the six
    current presets above.

    They still load and run as before. The proxy keeps them so that old configs work and
    so that published benchmark numbers stay reproducible. A later release may remove them.

| Preset | Use a current preset instead |
|---|---|
| `house` | `conservative` (same pipeline). `house` was the binary default before `conservative`. |
| `housellm` | `xhigh` (adds the summarizer to it) or `medium`. |
| `safe`, `balanced`, `coding`, `mcp`, `codesafe` | `conservative` (no model calls). |
| `codesmart`, `aggressive`, `agent`, `general` | `medium` or `high`. |
| `summarize`, `summarizer1509`, `agentdiet` | None. These stay by name for baselines and A/B runs. |

| Preset | Pipeline |
|---|---|
| `codesmart` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, extract_llm, extract, linecap` |
| `codesafe` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, extract, collapse, linecap` |
| `safe` | `format, textclean, searchfold` |
| `balanced` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, linecap` |
| `aggressive` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, smartcrush, extract, extract_llm, linecap` |
| `coding` | `format, textclean, searchfold, dedup, cmdfilter, extract, linecap` |
| `mcp` | `format, textclean, smartcrush` |
| `agent` | `format, textclean, searchfold, dedup, failed_run, mask, extract, extract_llm` |
| `general` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, mask, extract, extract_llm, collapse, linecap` |
| `summarize` | `summarize` |
| `summarizer1509` | `summarize` |
| `agentdiet` | `format, agentdiet` |
| `house` | `format, dedup, toon, cmdfilter, searchfold, textclean, extract, toolfilter` |
| `housellm` | `format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm, extract_llm_sweep, extract, toolfilter` |

`house` and `housellm` are the old **service** configs. `agentdiet` reproduces a published
baseline for A/B comparison. Neither is a recommendation.

## Notes on the obsolete presets

These notes describe how the obsolete presets behave. They stay so that old configs make sense.

**`codesmart`** was the cheapest arm in the
[benchmarks](../RESULTS.md) at the highest reward. It is the one preset that ships tuned
per-component settings rather than a bare name-list, which is why most turns make no model
call at all.

**`codesafe`** is `codesmart` with the LLM pass swapped for a blind `collapse`. Zero model
calls by policy — the choice for a shared box, an air-gapped deployment, or a benchmark arm
that must be reproducible byte for byte. `collapse` keeps a head/tail window and stashes the
middle, so nothing is unrecoverable, but it has no idea what mattered.

**`safe`** is two lossless components. Nothing is ever dropped, so there is nothing to
expand and no reversibility surface at all.

**`agent`** and **`general`** were the long-session choices, and `mask` — age-based GC of tool
outputs older than `keep_recent` — is the biggest lever in both. In the SWE-bench sweep it
delivered ~27% mean content-token savings (up to 93.5% on a long session) with no reward
loss. `balanced` omits `mask` and is *not* the choice for a long agentic session: it
delivered 6% against `general`'s 31% on the Terminal-Bench replay.

**`coding`** is deterministic only: the components measured to actually act on real Claude
Code traffic. Until August 2026 it named `skeleton` instead, which is behind a build tag and
so is absent from a normal binary — meaning the preset could not start at all. `skeleton`
still exists in a `cg_skeleton` build and is still not recommended; its page has the numbers
and the reason.

**`summarize`** must run alone. It restructures the whole transcript, so no other component's
in-place edits can share the request with it.

<details markdown="1">
<summary>Troubleshooting</summary>

**The proxy exits with `components: unknown component "skeleton"`.** You are naming
`skeleton` in a pipeline built without the `cg_skeleton` tag — it is the only cgo component
and `make build` does not pass the tag, so it is not registered. This used to happen from
`--preset coding`; that preset no longer names it. Either drop `skeleton` from your pipeline
or build with `CGO_ENABLED=1 go build -tags cg_skeleton ./cmd/context-guru-proxy`.

**`skeleton` is loaded but never fires.** It is inert on unfenced file reads, unknown
languages, and whenever the skeleton would not be smaller than the body.

**`extract_llm` makes no calls.** Three normal reasons: no cheap model is configured
(`CHEAP_MODEL*`), so it silently no-ops and the deterministic `extract` beside it does the
cheap pass; the request is below its `min_tokens` / `trigger.min_request_tokens` floor; or
you are on a prompt-caching backend, where it declines unless
`allow_on_caching_backend: true` because it measured net-negative there. None of these is an
error, and none of them announces itself.

**`cmdfilter` never fires.** It is only enabled when at least one filter is loaded, and it
only acts when the output's selector matches one. It ships 26 filters covering test runners,
build tools, package managers, IaC plans and verbose network clients — check
`cmdfilter_selector_misses` in `/stats` to see which shapes went unclaimed, and
[write a filter](../components/dsl.md#write-a-custom-filter) for them.

**`summarize` produced nothing.** It needs a model; with none configured it no-ops.

**A preset spent money I did not expect.** `codesmart`, `aggressive`, `agent` and `general`
(all via `extract_llm`) and `summarize` call a model. Every call is gated by a `trigger`,
throttled per session, and a prior compaction is replayed byte for byte on later turns, so
they do not fire every turn. Choose the model with `model.source`: `incoming` reuses the
request's own model and key, `config` uses a dedicated cheap model from `CHEAP_MODEL*`. See
[LLM components](../design.md#llm-components).

**I want breakpoint placement (`cacheinject`).** It is in no preset and must be opted into
explicitly. See the [`cacheinject` page](../components/caching.md#cacheinject).

</details>

Presets are only defaults: an explicit `pipeline:` or `components:` block always wins, so
start from the closest preset and tune. Full expansions live in
[`config/config.go`](../design.md#config-registry); per-component behaviour is in
[Components](../components.md).

## Exact pipelines, verbatim

Pipelines below are taken exactly from the `presets` map in `config/config.go`. Rows for the 15 obsolete presets are marked in the [list above](#obsolete-presets).

| Preset | Ordered pipeline | When to use |
|---|---|---|
| `house` | `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract` → `toolfilter` | **Obsolete name** of the old default. It is now an alias of `conservative`, which is the default for the standalone proxy and for every hosted tenant that has not chosen otherwise. Deterministic all the way through: no model call, so it adds no upstream spend and no latency to anyone else's agent on a shared box. `toolfilter` carries an empty removal list until an account names something. The order is the operator's, not this file's lossless-first rule — see the note below the table. |
| `housellm` | `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract_llm` → `extract_llm_sweep` → `extract` → `toolfilter` | `house` plus **both** compaction-model passes, applied per account on request. `extract_llm` works the uncached tail on any turn; `extract_llm_sweep` asks the request's own model **at the keep-alive ping**, in place of the ping, which old tool outputs are spent. It holds the answer and **removes content only on a cold request** (or, after a changed prefix, only after the change point). Each removal leaves a `<<cg:HASH>>` marker, and `expand` gives the original back. With no keep-alive there is no question. The bounds that apply: the sweep asks about at most 12 outputs per question, `extract_llm`'s `llm_max_per_request: 8` with its economic gate, and a 3,000-token request trigger. |
| `conservative` | `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract` → `toolfilter` | The default, and the Claude Code plugin picker's tier 1. `house` is its obsolete alias. Deterministic, no model calls. |
| `medium` | `cache_aware_summarizer` → `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract_llm` → `extract` → `toolfilter` | The picker's tier 2: `conservative` plus `extract_llm` (`housellm` minus its cold-cache sweep), plus [`cache_aware_summarizer`](../components/advanced-offload.md#two-ways-to-use-cache_state) with `cache_state: pre_expiry`. The summary is prepared in the background, in place of one keep-alive ping, and kept in reserve. It is used only when the user comes back after the cache expired, so the history never changes while the user works. Spends on its own, gated by `extract_llm`'s economic gate and 3,000-token trigger, and by the summarizer's 90%-of-window trigger. Needs keep-alive to prepare a summary; with cache strategy `none` the summarizer almost never acts. |
| `high` | `cache_aware_summarizer` → `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract_llm` → `extract` → `toolfilter` | The picker's tier 3: `medium` with `cache_aware_summarizer` at its default `cache_state: any`. The summary is made at 90% of the window and used on the next request, warm or cold. `summarize` is no longer in any picker tier; it stays available by name (see [Two ways to use `cache_state`](../components/advanced-offload.md#two-ways-to-use-cache_state) for when it is the better choice). |
| `xhigh` | `cache_aware_summarizer` → `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract_llm` → `extract_llm_sweep` → `extract` → `toolfilter` | The picker's tier 4, and the deepest cut it offers: `high` plus `extract_llm_sweep` (it asks at the keep-alive ping and removes content only on a cold request), i.e. `housellm` plus [`cache_aware_summarizer`](../components/advanced-offload.md#two-ways-to-use-cache_state) at its default trigger (`min_request_frac: 0.9`, `cache_state: any`). The summarizer sends the conversation itself plus one appended instruction, so its call reads the provider's cached prefix instead of paying full price. On Anthropic traffic it depends on #402, #406, #413 and #431. |
| `ultrahigh` | `cache_aware_summarizer` → `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract_llm` → `extract_llm_sweep` → `extract` → `toolfilter` | **Experimental** (see the README's "Experimental: ultrahigh"). The picker's tier 5, **opt-in only**: `xhigh` plus **tool deferral** (`toolfilter` with `defer: auto`, #485). A tool that the user's last 3 sessions declared and never used, that this project did not use in its last 10 sessions, and that weighs at least 200 tokens (counted with the proxy's own o200k tokenizer, about 340 Claude tokens) is deferred. An MCP server is one unit: all its tools are deferred together when no tool of it was used, and the 200-token minimum applies to the server's total. The set is decided once per conversation thread and never changes within it. Without 3 sessions of history it defers nothing. **On Claude models** the tool is sent with `defer_loading: true` and is never removed; the model sees its name in a short list and loads it through the API's search tool (`tool_search_tool_bm25`). **When the request has Claude Code's own `ToolSearch`** (#898), the proxy adds no `tool_search_tool_bm25`: the model loads through `ToolSearch`, and the proxy adds `tool_reference` blocks for its own deferred tools to a `select:Name1,Name2` result, on the next request. Any other query passes through. A non-`select:` query passes through untouched and disables nothing. An unknown `ToolSearch` description, or a call to a deferred tool whose own result is a "not loaded", "no such tool" or "does not exist" error, disables the mode for that description (24 h): nothing is deferred. See [the search matrix](../tool-search-matrix.md). A thread that started with the server-side search keeps it. This needs a provider check: picking the preset in the preset picker starts it, and the first session after that is not deferred. **On GPT, GLM and Kimi** (#825), which ignore `defer_loading`, the tool is removed from `tools` and listed by name; the model loads it through Context Guru's own search tool, `context_guru_tools`, which adds it back for the rest of the thread. No provider check runs there, so the first request of a thread can already defer. Off with `toolfilter: {defer: off}`, or in Claude Code with `ENABLE_TOOL_SEARCH=false`. Two account lists change the rule: `keep` (never defer these) and `always_defer` (#878: defer these in every new thread, without the history rule and the project veto; empty by default). The keep list wins over `always_defer`, and a tool on `always_defer` still stays loaded when the thread uses it, when `tool_choice` forces it, when it is a core tool, or when it weighs under 200 tokens. Each list takes a tool name, or `mcp__<server>` for a whole server. The per-thread record is kept on disk in every setup (the session-state table with `--dashboard`, else `context-guru-tooldefer.db` next to the dashboard database path), so a restart never moves a tool; with no durable place, it defers nothing. A Responses request that declares its own `tool_search` (Codex) keeps it, and nothing is deferred on it (#876). |
| `codesmart` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `extract_llm` → `extract` → `linecap` | The SWE-bench-winning cache-aware config: structural offloaders + a cheap-model relevance-trimmer (`extract_llm`, routed to `CHEAP_MODEL`, gated so most turns make no model call) + deterministic `extract`. `extract_llm` no-ops (→ deterministic) when no cheap model is configured. **Changed 2026-08:** the lossless trio replaced `toon`, which acted 0 times on 5,752 production requests, and `linecap` was added. Re-measure before quoting the published SWE-bench numbers against it. |
| `codesafe` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `extract` → `collapse` → `linecap` | `codesmart` minus the LLM pass — **deterministic-only, zero model calls by policy**. The safe control / the choice when you don't want an LLM on the hot path. |
| `off` | *(empty)* | Passthrough — no components. The baseline / A-B control, and what the [Claude Code plugin](../how-to/install-plugin.md) installs by default; the plugin's actual default saving mechanism is the `auto` cache strategy (keep-alive), not a pipeline component. |
| `safe` | `format` → `textclean` → `searchfold` | Lossless only: repack JSON compactly and split the volatile system tail so the shared prefix stays cacheable. Zero risk of dropping content. |
| `balanced` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` | Lossless repack + conservative offloads (dedupe, drop superseded/failed runs, filter command noise) + the cache split. **Not recommended for agentic traffic** — it omits `mask`, the biggest lever there. |
| `aggressive` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `smartcrush` → `extract` → `extract_llm` → `linecap` | `balanced` plus `smartcrush` (crush long homogeneous arrays), deterministic `extract` (noise collapse), and `extract_llm` (cheap-model relevance trim) for deeper savings. |
| `coding` | `format` → `textclean` → `searchfold` → `dedup` → `cmdfilter` → `extract` → `linecap` | Coding agents, deterministic only: the components measured to actually act on real Claude Code traffic. **Changed 2026-08** — it previously named `skeleton` and therefore could not start on a normal build; see below. |
| `mcp` | `format` → `textclean` → `smartcrush` | Tool/MCP servers returning long homogeneous JSON arrays (list endpoints, search hits). |
| `agent` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `mask` → `extract` → `extract_llm` | Long agentic sessions (e.g. Claude Code on SWE-bench) where re-sent tool outputs dominate cost. `mask` is the biggest lever — ~27% content-token savings with no task-reward loss (see [Benchmarks](../RESULTS.md)). |
| `general` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `mask` → `extract` → `extract_llm` → `collapse` → `linecap` | An all-round pipeline: the reward-neutral levers of `agent` plus the situational shrinkers (`cmdfilter` / `linecap` / `collapse`) that cost nothing when they don't fire. |
| `summarize` | `summarize` | Long trajectories where the transcript itself is the cost. **Runs alone** — it restructures the whole transcript (changes the message count), so no other component's in-place edits race the rebuild. |
| `summarizer1509` | `summarize` | The same component configured as a **cache-unaware, no-reuse** summarizer — `cache_state: any`, `resummarize_tokens: 0`, `keep_first: 2` — i.e. the reference shape the llm-d compaction work calls [P0], and the control arm the cache-aware trigger is measured against. Also the right starting point on a backend whose prompt-cache lifetime this repo cannot derive. **Runs alone**, for the same reason as `summarize`. |
| `agentdiet` | `format` → `agentdiet` | A **comparable baseline**, not a recommendation: the published [AgentDiet](../components/advanced-offload.md#agentdiet) method ([arXiv:2509.23586](https://arxiv.org/abs/2509.23586)) at its tuned hyperparameters, for A/B'ing against our own reducers. One cheap-model reflection per turn on the step that just aged past `delay_steps`. Carries no other offloader on purpose — they would reduce the same tool outputs first and leave nothing to attribute. |

!!! info "The lossless trio, `toon`'s retirement, and `linecap` (August 2026)"
    `format` → `textclean` → `searchfold` now leads every preset that does deterministic work
    (`mcp` takes only the first two — it serves JSON list endpoints with no search output to fold).
    All three verify-then-adopt — `format` re-parses, `textclean` compares informative lines,
    `searchfold` checks its own inverse byte-for-byte — so there is no risk argument for omitting
    one, and running them first makes every downstream token count honest. Two were missing:
    `textclean` shipped in `general` alone while 49.6% of corpus messages carry ANSI, and
    `searchfold` shipped in **no preset at all**, fully written and round-trip verified, folding
    nothing.

    [`toon`](../components/reformat.md#toon) is retired from every preset. Production: 0 acts in 5,752
    requests, `not_uniform_object_array` 234,437, and 0 convertible candidates in 11.67M measured
    tokens — at 1.53 ms and one `TextTokens` call per tool message. The component and its tests
    stay, so tabular traffic can enable it by hand.

    [`linecap`](../components/offload-reducers.md#linecap) is new: a 500-char per-line cap with a never-truncate
    allow-list, plus a non-adjacent duplicate-line collapse. It is the answer to why 939 lines of
    per-command filters have matched two filters in production — the value in tool output is not
    per-command.

    It runs **last** among the offloaders, and that position is measured. Every offload leaves a
    marker and every offload skips marker-bearing content, so a *modest* reducer ahead of a
    *drastic* one steals its candidates. On `general` over 1,795 real captured requests: linecap
    7th saved 5,524,476 tokens — **worse than the 5,556,801 with no linecap at all**, because it
    took 39,335 tokens off messages `collapse` would have taken 76,554 off. Placed last it saves
    **5,811,621 (+1.33 pp)**.

!!! warning "`house` / `housellm` depart from two rules above, on purpose"
    Both are the operator's ordering, and the two deviations are recorded here so nobody
    "corrects" them by reading the section above:

    **The lossless trio does not lead.** `dedup` and `cmdfilter` run before `searchfold` and
    `textclean`, so two offloaders have already edited the messages by the time the folds count
    theirs. The consequence is confined to **attribution** — the per-component split in `/stats`
    is less honest than `general`'s. Totals are unaffected, and nothing can lose content: all
    three folds verify-then-adopt whatever order they run in.

    **`toon` runs**, against its own retirement measurement (0 acts in 5,752 requests, 0
    convertible candidates in 11.67M tokens). It is a `Reformat`, so the cost is latency and
    never content: 1.53 ms and one `TextTokens` call per tool message. If tabular traffic ever
    arrives, it is already in the path.

    **`linecap` is absent.** Its **20.3%** is GROSS reach — the share of shipped tokens its
    rules touch — not what adding it to this pipeline buys. Measured incrementally on the same
    1,795-request corpus, `house` against `house`+`linecap`: **+152,615 tokens, +0.797 pp**,
    which is 14.5% of what was reachable once the other offloaders had taken their share. Worth
    adding; not the headline the gross number implies.

!!! note "`coding` changed in August 2026"
    It used to be `format → skeleton → cmdfilter`, and because
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
    (old-then-large).

!!! info "`cacheinject` is in no preset"
    Breakpoint *placement* ([`cacheinject`](../components/caching.md#cacheinject)) is in **no**
    preset, because it has never been shown to help. Add it by hand if you want to run the
    placement study.

!!! warning "`extract_llm` is off by default on prompt-caching backends"
    `extract_llm` is the only component that spends money to save money, and on a
    prompt-caching backend it was measured **~8× underwater**: a token removed from a cached
    region saves the cache-read rate (`$0.30/MTok`), not the fresh-input rate (`$3/MTok`), so
    break-even is **~40,300 tokens per output** at the measured compression ratio and amortization — far above a
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

Every component's config lives in [Components](../components.md).
