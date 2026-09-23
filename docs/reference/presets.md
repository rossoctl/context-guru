# Presets

A preset is a named, ordered pipeline. Set it once and every explicit field you add overrides it.

```sh
context-guru-proxy --preset codesmart      # or PRESET=codesmart, or preset: in YAML
```

## Which preset?

| Your workload | Preset |
|---|---|
| **Trying context-guru for the first time** | **`cache`** |
| **Most agents — the recommended pipeline** | **`codesmart`** (pass `--preset codesmart`; the binary defaults to `house`) |
| Same, but no LLM on the hot path | `codesafe` |
| A guaranteed-safe, lossless win only | `safe` |
| General non-agentic traffic | `balanced` |
| Long agentic sessions | `agent` or `general` |
| Coding agent reading big source files | `coding` |
| MCP / list-endpoint JSON arrays | `mcp` |
| One long transcript to compress, standalone | `summarize` |
| Squeeze harder, tolerate LLM/structural offload | `aggressive` |
| Nothing — A/B baseline, passthrough control | `off` |

## What each one runs

| Preset | Pipeline |
|---|---|
| `codesmart` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, extract_llm, extract, linecap, cachesplit` |
| `codesafe` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, extract, collapse, linecap, cachesplit` |
| `cache` | `cachesplit` |
| `safe` | `format, textclean, searchfold, cachesplit` |
| `balanced` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, linecap, cachesplit` |
| `aggressive` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, smartcrush, extract, extract_llm, linecap, cachesplit` |
| `coding` | `format, textclean, searchfold, dedup, cmdfilter, extract, linecap, cachesplit` |
| `mcp` | `format, textclean, smartcrush, cachesplit` |
| `agent` | `format, textclean, searchfold, dedup, failed_run, mask, extract, extract_llm, cachesplit` |
| `general` | `format, textclean, searchfold, dedup, failed_run, cmdfilter, mask, extract, extract_llm, collapse, linecap, cachesplit` |
| `summarize` | `summarize` |
| `off` | *(empty)* |
| `agentdiet` | `format, agentdiet, cachesplit` |
| `house` | `format, dedup, toon, cmdfilter, searchfold, textclean, extract, cachesplit, toolfilter` |
| `housellm` | `format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm, extract_llm_sweep, extract, cachesplit, toolfilter` |

Order is deliberate: lossless repack first, then the cheap structural offloaders, then
anything that costs a model call, cache directives last — except in `house` and `housellm`,
whose order is the operator's on purpose: `dedup` and `cmdfilter` run ahead of the lossless
pair and `toolfilter` sits after `cachesplit`. That costs per-component attribution in
`/stats`, never content; the reasons are recorded in
[`config/config.go`](../design.md#config-registry) and the exemption is noted below.

The last three rows are not options in the chooser above. `house` and `housellm` are the
**service** configs — what a hosted account runs unless it asks otherwise — and `agentdiet`
reproduces a published baseline for A/B comparison, not a recommendation. They are in the
table so it lists every preset that exists; pick from the table above this one.

## Notes on the ones people pick

### `cache` — start here

`cachesplit` and nothing else. Pick it when what you want is to find out whether this thing
helps you, with the smallest possible claim to check:

- **Nothing is dropped, summarised, or replaced.** No `<<cg:HASH>>` markers, no
  `context_guru_expand` tool added to your requests, no model calls. It splits one oversized
  system block into two adjacent text blocks whose concatenation is byte-identical, so the
  model sees exactly the prompt your agent sent — and moves the cache breakpoint onto the
  half that does not churn.
- **What it is worth is regime-dependent, and the funnel's regime is the weak one.** The
  headline **−34.1% cost / 0% → 96.7% hit** comes from a benchmark harness running tasks
  back-to-back inside the provider's 5-minute cache TTL
  ([cacheinject](../components/caching.md#what-placement-is-actually-worth)), which is
  precisely the regime where the split pays — and is *one task measured three times, not a
  fleet average*. On this project's own interactive traffic the figure is **$0.0298 across
  1,127 sessions / 11,361 requests**
  ([dashboard](../dashboard.md)): Claude
  Code captures the environment snapshot once per session, and 1,105 of 1,127 session starts
  read zero tokens from cache because the previous prefix had already expired. It is also
  **exactly zero** outside a git repository, on a system prompt under the 1,024-token
  `minSplitTokens` floor, and on any implicit prefix-cache backend (vLLM, llm-d). Neither
  figure is wrong; they differ by three orders of magnitude because the mechanism needs a
  second session inside five minutes.

- **Anthropic-family only.** `cachesplit` is a no-op against implicit prefix-cache backends
  (vLLM, llm-d), which match to the divergence on their own — so on those it costs nothing
  and buys nothing.

Move to `codesmart` once you want the offloaders too. `safe` is the next step up and is still
lossless in meaning, but it does rewrite JSON, so `cache` is the one whose promise you can
confirm by reading a single line of `config/config.go`.

**`codesmart`** is the shipped default and the cheapest arm in the
[benchmarks](../RESULTS.md) at the highest reward. It is the one preset that ships tuned
per-component settings rather than a bare name-list, which is why most turns make no model
call at all.

**`codesafe`** is `codesmart` with the LLM pass swapped for a blind `collapse`. Zero model
calls by policy — the choice for a shared box, an air-gapped deployment, or a benchmark arm
that must be reproducible byte for byte. `collapse` keeps a head/tail window and stashes the
middle, so nothing is unrecoverable, but it has no idea what mattered.

**`safe`** is two lossless components. Nothing is ever dropped, so there is nothing to
expand and no reversibility surface at all.

**`agent`** and **`general`** are the long-session choices, and `mask` — age-based GC of tool
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

**`cachesplit` shows up under `top_passthrough`.** Expected. Its saving is a provider-side
cache hit, invisible to content-token counts.

**`summarize` produced nothing.** It needs a model; with none configured it no-ops.

**A preset spent money I did not expect.** `codesmart`, `aggressive`, `agent` and `general`
(all via `extract_llm`) and `summarize` call a model. Every call is gated by a `trigger`,
throttled per session, and a prior compaction is replayed byte for byte on later turns, so
they do not fire every turn. Choose the model with `model.source`: `incoming` reuses the
request's own model and key, `config` uses a dedicated cheap model from `CHEAP_MODEL*`. See
[LLM components](../design.md#llm-components).

**I want breakpoint placement (`cacheinject`).** It is in no preset and must be opted into
explicitly. Every caching preset carries `cachesplit`, which is the part with measured
savings. See the [`cacheinject` page](../components/caching.md#cacheinject).

</details>

Presets are only defaults: an explicit `pipeline:` or `components:` block always wins, so
start from the closest preset and tune. Full expansions live in
[`config/config.go`](../design.md#config-registry); per-component behaviour is in
[Components](../components.md).

## Exact pipelines, verbatim

Pipelines below are taken exactly from the `presets` map in `config/config.go`.

| Preset | Ordered pipeline | When to use |
|---|---|---|
| `house` | `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract` → `cachesplit` → `toolfilter` | **The default**, for the standalone proxy and for every hosted tenant that has not chosen otherwise. Deterministic all the way through: no model call, so it adds no upstream spend and no latency to anyone else's agent on a shared box. `toolfilter` carries an empty removal list until an account names something. The order is the operator's, not this file's lossless-first rule — see the note below the table. |
| `housellm` | `format` → `dedup` → `toon` → `cmdfilter` → `searchfold` → `textclean` → `extract_llm` → `extract_llm_sweep` → `extract` → `cachesplit` → `toolfilter` | `house` plus **both** compaction-model passes, applied per account on request. `extract_llm` works the uncached tail on any turn; `extract_llm_sweep` adjudicates at depth **only on a turn whose prompt cache has expired**. They were one component with a `per_output` / `cold_cache` pair of switches until the cold-sweep split — the sweep is now its own component, so "is it on" is its presence in the pipeline like everything else. The bounds that apply: the sweep's `max_calls` (default one concurrency round, each call adjudicating up to 12 outputs), `extract_llm`'s `llm_max_per_request: 8`, the economic gate on both, and a 3,000-token request trigger. |
| `codesmart` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `extract_llm` → `extract` → `linecap` → `cachesplit` | The SWE-bench-winning cache-aware config: structural offloaders + a cheap-model relevance-trimmer (`extract_llm`, routed to `CHEAP_MODEL`, gated so most turns make no model call) + deterministic `extract`. `extract_llm` no-ops (→ deterministic) when no cheap model is configured. **Changed 2026-08:** the lossless trio replaced `toon`, which acted 0 times on 5,752 production requests, and `linecap` was added. Re-measure before quoting the published SWE-bench numbers against it. |
| `codesafe` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `extract` → `collapse` → `linecap` → `cachesplit` | `codesmart` minus the LLM pass — **deterministic-only, zero model calls by policy**. The safe control / the choice when you don't want an LLM on the hot path. |
| `off` | *(empty)* | Passthrough — no components. The baseline / A-B control, and what the [Claude Code plugin](../how-to/install-plugin.md) installs by default; the plugin's actual default saving mechanism is the `5-min-ping` cache strategy (keep-alive), not a pipeline component. |
| `cache` | `cachesplit` | The volatile-tail split and nothing else: no content dropped, no `<<cg:HASH>>` markers, no `context_guru_expand` tool added to requests, no model calls. Chosen so a stranger deciding whether to route their agent through a local proxy can verify the claim by reading one line of `config/config.go` rather than trusting four components. The savings claim is regime-dependent and the funnel's regime is the weak one: **−34.1% cost / 0% → 96.7% hit** is a benchmark harness running tasks back-to-back inside the provider's 5-minute TTL (and is one task measured three times), while this project's own interactive traffic yields **$0.0298 across 1,127 sessions** — 1,105 of 1,127 session starts read zero from cache. Zero outside a git repo, under the 1,024-token `minSplitTokens` floor, or on an implicit prefix-cache backend (vLLM, llm-d). See [dashboard](../dashboard.md) and [cacheinject](../components/caching.md#cacheinject). |
| `safe` | `format` → `textclean` → `searchfold` → `cachesplit` | Lossless only: repack JSON compactly and split the volatile system tail so the shared prefix stays cacheable. Zero risk of dropping content. |
| `balanced` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `linecap` → `cachesplit` | Lossless repack + conservative offloads (dedupe, drop superseded/failed runs, filter command noise) + the cache split. **Not recommended for agentic traffic** — it omits `mask`, the biggest lever there. |
| `aggressive` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `smartcrush` → `extract` → `extract_llm` → `linecap` → `cachesplit` | `balanced` plus `smartcrush` (crush long homogeneous arrays), deterministic `extract` (noise collapse), and `extract_llm` (cheap-model relevance trim) for deeper savings. |
| `coding` | `format` → `textclean` → `searchfold` → `dedup` → `cmdfilter` → `extract` → `linecap` → `cachesplit` | Coding agents, deterministic only: the components measured to actually act on real Claude Code traffic. **Changed 2026-08** — it previously named `skeleton` and therefore could not start on a normal build; see below. |
| `mcp` | `format` → `textclean` → `smartcrush` → `cachesplit` | Tool/MCP servers returning long homogeneous JSON arrays (list endpoints, search hits). |
| `agent` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `mask` → `extract` → `extract_llm` → `cachesplit` | Long agentic sessions (e.g. Claude Code on SWE-bench) where re-sent tool outputs dominate cost. `mask` is the biggest lever — ~27% content-token savings with no task-reward loss (see [Benchmarks](../RESULTS.md)). |
| `general` | `format` → `textclean` → `searchfold` → `dedup` → `failed_run` → `cmdfilter` → `mask` → `extract` → `extract_llm` → `collapse` → `linecap` → `cachesplit` | The recommended all-round pipeline: the reward-neutral levers of `agent` plus the situational shrinkers (`cmdfilter` / `linecap` / `collapse`) that cost nothing when they don't fire. |
| `summarize` | `summarize` | Long trajectories where the transcript itself is the cost. **Runs alone** — it restructures the whole transcript (changes the message count), so no other component's in-place edits race the rebuild. |
| `agentdiet` | `format` → `agentdiet` → `cachesplit` | A **comparable baseline**, not a recommendation: the published [AgentDiet](../components/advanced-offload.md#agentdiet) method ([arXiv:2509.23586](https://arxiv.org/abs/2509.23586)) at its tuned hyperparameters, for A/B'ing against our own reducers. One cheap-model reflection per turn on the step that just aged past `delay_steps`. Carries no other offloader on purpose — they would reduce the same tool outputs first and leave nothing to attribute. |

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
    Every preset that touches caching carries [`cachesplit`](../components/caching.md#cachesplit),
    which enables the measured volatile-tail split. Breakpoint *placement*
    ([`cacheinject`](../components/caching.md#cacheinject)) is in **no** preset, because it has never
    been shown to help. Add it by hand if you want to run the placement study.

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
