# Choose a preset

A preset is a named, ordered pipeline. Set it once (`--preset` / `PRESET`, or `preset:` in
YAML) and every explicit field you add overrides it. This guide maps workloads to presets and
names the caveats before you commit.

!!! tip "Presets are just defaults"
    A preset expands to a default `pipeline:` list. Explicit `pipeline:` / `components:` blocks
    always win, so start from the closest preset and tune from there. See
    [Architecture](../design.md#config-registry).

## Which preset?

| Your workload | Preset |
|---|---|
| **Most agents — the default (SWE-bench-winning cache-aware config)** | **`codesmart`** |
| Same, but no LLM on the hot path (deterministic-only) | `codesafe` |
| Nothing — A/B baseline / passthrough control | `off` |
| Any traffic, want a guaranteed-safe win only | `safe` |
| Squeeze harder, tolerate LLM/structural offload | `aggressive` |
| Coding agent reading big source files | `coding` |
| MCP / list-endpoint JSON arrays | `mcp` |
| Long agentic sessions, age-based masking | `agent` / `general` |
| One long transcript to compress, run standalone | `summarize` |

## The presets

Every preset below is exactly the list in [`config/config.go`](../design.md#config-registry);
per-component behavior is in [Components](../components.md).

### `codesmart` — `[format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]`
**The shipped default** (`--preset codesmart`), and the SWE-bench study's winning cache-aware
config. Lossless repack first (`format`, `toon`), then the cheap structural offloaders
(`dedup`, `failed_run`, `cmdfilter`), then the cheap-model relevance trimmer with the free
deterministic `extract` behind it, and `cachesplit` last.

It is the one preset that ships **tuned per-component settings** rather than a bare name-list
(`presetConfigs` in `config/config.go`), carried verbatim from the study: `extract`'s floor at
`min_tokens: 400`, and `extract_llm` on `strategy: code`, `model.source: config`,
`min_tokens: 3000` with a matching `trigger.min_request_tokens: 3000`, and at most 4 calls per
request. Those thresholds are why most turns make no model call at all.

- **Fits:** most agents. Pick something else only for a reason listed below.
- **Caveat:** `extract_llm` needs a cheap model (`CHEAP_MODEL*`); with none configured it
  silently no-ops and the deterministic `extract` beside it does the cheap pass. That is a
  degradation, not an error, so it will not announce itself.
- **Caveat:** on a **prompt-caching** backend `extract_llm` declines to run at all unless
  `allow_on_caching_backend: true` — measured net-negative there. The preset still lists it; on
  caching traffic it makes zero calls and costs nothing.

### `codesafe` — `[format, dedup, failed_run, cmdfilter, extract, collapse, cachesplit]`
`codesmart` with the LLM pass removed and a blind `collapse` (pinned to `max_tokens: 3000`) as
the last-resort fallback in its place. **Deterministic-only, zero model calls by policy.**

- **Fits:** when an LLM on the hot path is not acceptable — a shared box, an air-gapped
  deployment, or a benchmark arm that must be reproducible byte for byte.
- **Caveat:** `collapse` is content-agnostic. It keeps a head/tail window and stashes the
  middle, so it always leaves *something* recoverable, but it has no idea what mattered.

### `off` — `[]`
No components. Passthrough. Use it as the A/B control when you measure savings — the baseline in
[Benchmarks](../RESULTS.md) is this preset.

### `safe` — `[format, cachesplit]`
Two lossless [Reformat](../components.md#reformat-lossless) components only: compact JSON
(`format`) and the Anthropic volatile-tail split (`cachesplit`). Nothing is ever dropped, so there
is nothing to expand.

- **Fits:** any traffic where you want a zero-risk win and no reversibility surface.
- **Caveat:** `cachesplit`'s savings are provider-side cache hits, invisible to `/stats` token
  counts — it will show up under `top_passthrough`. That's expected, not dead weight.
- **Note:** breakpoint *placement* (`cacheinject`) is deliberately **not** here — it is opt-in
  since [#36](https://github.com/rossoctl/context-guru/pull/36), and the one live reading since
  its marks began reaching the provider is mildly *negative* per step at n=1 with no mechanism
  established. `cachesplit` carries the part with measured savings.

### `balanced` — `[format, dedup, failed_run, cmdfilter, cachesplit]`
Adds three cheap, high-precision offloaders: exact-dup removal (`dedup`), superseded test/build
runs (`failed_run`), and DSL command-log filtering (`cmdfilter`).

- **Fits:** general non-agentic traffic where you want a conservative win.
- **Caveat:** **not** the choice for long agentic sessions — it omits `mask`, the biggest lever
  there, and delivered 6% against `general`'s 31% in the Terminal-Bench replay. Use `codesmart`
  (the actual default), `agent` or `general` for that.
- **Caveat:** `cmdfilter` only fires when ≥1 filter is loaded and the output's selector matches
  one. It ships **26** filters covering test runners, build tools, package managers, IaC plans and
  verbose network clients; author more with a [custom DSL filter](custom-dsl-filter.md).

### `aggressive` — `[format, dedup, failed_run, cmdfilter, smartcrush, extract, extract_llm, cachesplit]`
`balanced` plus JSON-array crushing (`smartcrush`), deterministic noise collapse (`extract`) and
the cheap-model relevance trimmer (`extract_llm`).

- **Fits:** you want more savings and accept structural/LLM offload with expand recovery.
- **Caveat:** `extract_llm` spends a model call (gated by its `trigger` and throttled per session);
  `extract` beside it is free and deterministic. With no cheap model configured `extract_llm`
  no-ops. Keep the [store](recover-context.md) on so the extra offloads stay recoverable.

### `coding` — `[format, skeleton, cmdfilter, cachesplit]`
Swaps in `skeleton`, which tree-sitter-parses fenced code blocks and replaces function bodies with
`{ … }`, keeping signatures/imports/types.

- **Fits:** a coding agent that reads large source files but mostly needs the shape.
- **Caveat:** **this preset needs a `cg_skeleton` build.** `skeleton` is the only cgo component,
  so without the tag it is not registered and the pipeline fails to build — the proxy exits with
  `components: unknown component "skeleton"`. `make build` does not pass the tag.
- **Caveat:** `skeleton` is inert on unfenced file reads, unknown languages, or when the skeleton
  isn't smaller than the body.

### `mcp` — `[format, smartcrush, cachesplit]`
Targets homogeneous JSON arrays (list endpoints, search hits): keep `keep_first` + `keep_last`
items plus any item carrying an error signal, drop the middle.

- **Fits:** MCP tools and REST list endpoints returning long uniform arrays.
- **Caveat:** inert on non-array output or arrays below `min_items`.

### `agent` — `[format, dedup, failed_run, mask, extract, extract_llm, cachesplit]`
Tuned for long agentic sessions (e.g. Claude Code on SWE-bench) where the dominant cost is the
transcript of old tool outputs re-sent every turn.

- **Fits:** long-running agents with a growing transcript.
- **Caveat:** **`mask` is the biggest lever here** — age-based GC of tool outputs older than
  `keep_recent`. In the SWE-bench sweep it delivered ~27% mean content-token savings (up to 93.5%
  on a long session) with no reward loss ([Benchmarks](../RESULTS.md)). Order matters: lossless
  first, then offload old-then-large, cache last.

### `general` — `[format, toon, dedup, failed_run, cmdfilter, mask, extract, extract_llm, collapse, cachesplit]`
The recommended all-round pipeline: the reward-neutral levers of `agent` plus the situational
shrinkers (`toon`, `cmdfilter`, `collapse`) that cost nothing when they don't fire.

- **Fits:** any agent or benchmark, when you don't want to pick per workload.
- **Caveat:** it stacks one LLM component (`extract_llm`) and one blind fallback (`collapse`). It
  deliberately does **not** stack `summarize` beside `mask` — they are overlapping old-context
  reducers, and `mask` is the one kept.

### `summarize` — `[summarize]`
One LLM component that collapses the middle of the transcript into a single
`=== History Summary ===` message, keeping the head + last few turns.

- **Fits:** long agentic sessions where the stale middle is the token cost.
- **Caveat:** **run it alone.** It changes the message count and restructures the whole transcript,
  so `apply.Body` rebuilds the body — no other component's in-place edits can race that rebuild.
  It needs a model; with none it no-ops.

!!! warning "LLM presets cost model calls"
    `codesmart`, `aggressive`, `agent` and `general` (all via `extract_llm`) and `summarize` call
    a model. Every call is gated by a `trigger` and throttled per session, and a prior compaction
    is reused byte-for-byte on later turns, so they don't fire every turn. Pick the model with
    `model.source` (`incoming` reuses the request's own model+key; `config` uses a dedicated cheap
    model set via `CHEAP_MODEL*`). With no model available they no-op. See
    [LLM components](../design.md#llm-components).

!!! info "Every caching preset carries `cachesplit`, not `cacheinject`"
    Breakpoint *placement* is in no preset — see the note under `safe` above and the
    [`cacheinject` page](../components/cacheinject.md#what-placement-is-actually-worth).
