# Choose a preset

A preset is a named, ordered pipeline. Set it once and every explicit field you add
overrides it.

```sh
context-guru-proxy --preset codesmart      # or PRESET=codesmart, or preset: in YAML
```

## Which preset?

| Your workload | Preset |
|---|---|
| **Most agents — the default** | **`codesmart`** |
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
[`config/config.go`](../design.md#config-registry) and the exemption is noted in the
[preset reference](../reference/presets.md).

The last three rows are not options in the chooser above. `house` and `housellm` are the
**service** configs — what a hosted account runs unless it asks otherwise — and `agentdiet`
reproduces a published baseline for A/B comparison, not a recommendation. They are in the
table so it lists every preset that exists; pick from the table above this one.

## Notes on the ones people pick

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
[write a filter](custom-dsl-filter.md) for them.

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
savings. See the [`cacheinject` page](../components/cacheinject.md).

</details>

Presets are only defaults: an explicit `pipeline:` or `components:` block always wins, so
start from the closest preset and tune. Full expansions live in
[`config/config.go`](../design.md#config-registry); per-component behaviour is in
[Components](../components.md) and the [preset reference](../reference/presets.md).
