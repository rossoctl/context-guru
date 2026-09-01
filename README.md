<div align="center">

<img src="docs/img/context-guru.png" alt="context-guru" width="320" />

# context-guru

**Provider-agnostic context engineering for LLM agents.**
Shrink the tokens every request carries — losslessly, or lossy-but-reversibly — **without touching the agent.**

[![Docs](https://img.shields.io/badge/docs-online-009688.svg)](https://rossoctl.github.io/context-guru/)
[![Go Reference](https://img.shields.io/badge/pkg.go.dev-reference-007d9c.svg)](https://pkg.go.dev/github.com/rossoctl/context-guru)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)

[Quickstart](#quickstart-60-seconds) · [Why it wins](#benchmark-the-cheapest--highest-reward-arm-on-swe-bench-verified) · [Components](#the-pipeline) · [Docs](https://rossoctl.github.io/context-guru/) · [Reproduce](docs/results/REPRODUCE.md)

</div>

---

context-guru is a single Go core that reduces the token cost of LLM-agent traffic. The **same core**
runs as an **HTTP proxy/gateway** (drop-in, any language, zero agent changes) or as an **in-process
plugin**. It operates on the messages array — dropping redundant tool output, collapsing superseded
runs, projecting large reads down to what's relevant — and every reduction is **safe by construction**:

- **Fail open, always** — any component error or panic reverts *that component only*; the original
  request is always a valid fallback.
- **Never worse** — a component that would grow a message is reverted. You never pay to compact.
- **Reversible** — every lossy drop leaves a `<<cg:HASH>>` marker and stashes the original,
  recoverable via a model-callable `context_guru_expand` tool or `GET /expand`.

## Benchmark: the cheapest & highest-reward arm on SWE-bench Verified

Evaluated **live, end-to-end**, with the **claude-code** agent on **`aws/claude-sonnet-5`**, against a
no-compaction baseline, against the [**headroom**](https://pypi.org/project/headroom-ai/) request-stream
proxy, and against [**rtk**](https://github.com/rtk-ai/rtk) (Rust Token Killer, a shell-level Bash-output
hook). All 50 tasks scored under all **four** arms.

| dimension | baseline | **context-guru** | headroom | rtk |
|---|--:|--:|--:|--:|
| tasks solved | 86% | **88%** | 80% | 86% |
| **total billed cost** vs baseline | — | **−13.2%** | −5.3% | −9.0% |
| cache-read tokens vs baseline | — | **−17.8%** | −6.3% | −10.8% |
| cache-write tokens vs baseline | — | −0.4% | −0.9% | −1.1% |
| mean steps / task vs baseline | — | **−13.9%** | −2.8% | −8.0% |
| added latency / req | — | 117 ms | 63 ms | **0 ms** |
| tool's own LLM cost | — | $0.31 | $0 | $0 |

**context-guru is the cheapest arm and solves the most tasks** — it cuts billed cost **13.2%** vs no
compaction, driven by an **17.8%** cache-read reduction, while keeping cache-write within **1%** of baseline
(it never busts the cache). It does this by *freezing each compaction and replaying it byte-identically every
turn*, so the saving compounds across the whole session. The surprise is **rtk**: a simple deterministic
shell filter is the **2nd-cheapest** arm (**−9.0%**), **reward-neutral** (86% = baseline), at **zero
request-path latency and $0 tool cost** — it **beats the headroom proxy on both cost and reward**. rtk's
ceiling is that it only compresses **Bash-tool** output (Claude Code's built-in `Read`/`Grep`/`Glob` bypass
its hook), which is why the whole-request proxy goes deeper. Full four-way study, per-task/per-component
breakdowns, real before→after examples, and how to reproduce: **[docs/RESULTS.md](docs/RESULTS.md)**.

## Architecture

```mermaid
flowchart LR
  A[Agent] -->|chat request| H{Host adapter}
  H -->|proxy: proxy.Handler| P[apply.Body]
  H -->|in-process: AuthBridge plugin| P
  P -->|messages array| PIPE[Pipeline<br/>ordered components]
  PIPE --> P
  P -->|byte-lossless splice| UP[Upstream provider]
  UP -->|response| EX[expand loop]
  EX -->|resolve markers from Store| UP
  EX --> A
  PIPE -.per-component Report.-> M[Emitter / Aggregator]
  PIPE -.stash originals.-> S[(Store<br/>TTL+LRU)]
  EX -.resolve.-> S
```

Components implement one of two lossiness-typed interfaces and are stacked in config order:

```mermaid
flowchart TD
  C["Component — Name() · Enabled(ctx)"]
  C --> R["Reformat: lossless repack<br/>format · toon · cachesplit · cacheinject"]
  C --> O["Offload: drop + stash, returns cache_keys<br/>skeleton · dedup · collapse · failed_run<br/>cmdfilter · extract · extract_llm · smartcrush · mask · summarize"]
```

## Install

Requires **Go 1.26**; no C toolchain — `make build` builds with cgo off and produces a statically
linked binary. (A C compiler is needed only for `make test`'s race detector and the optional
`cg_skeleton` tag.) Build from the repo root:

```sh
CGO_ENABLED=1 go build -tags cg_skeleton -o bin/context-guru-proxy ./cmd/context-guru-proxy
```

The `cg_skeleton` build tag pulls in tree-sitter (via cgo) so the `skeleton` component can parse code.
It is **optional** — omit the tag and the tree-sitter dependency for a pure-Go build; everything else
works. Note that without the tag `skeleton` is not *inert*, it is **not registered**: a config or
preset naming it fails at pipeline build with `components: unknown component "skeleton"` and the
proxy exits rather than starting without it. The `coding` preset no longer names it (it could
not start on a normal build while it did), so `skeleton` now needs an explicit pipeline entry and a `cg_skeleton` binary,
and `make build` does not pass the tag. Or build the gateway image
(see [docs/setup.md](docs/setup.md)):

```sh
docker build -t context-guru:local .
```

## Quickstart (60 seconds)

Download a release binary — statically linked, **no Go and no C compiler needed** — or build
from source:

```sh
# 1 — run the proxy (ships with the SWE-bench-winning cache-aware config by default)
./bin/context-guru-proxy                          # --preset house (the default); listens on :4000

# 2 — point any agent at it (one port serves both dialects)
export ANTHROPIC_BASE_URL=http://localhost:4000/anthropic
export OPENAI_BASE_URL=http://localhost:4000/openai/v1
claude                                            # e.g. Claude Code

# 3 — watch the savings add up
curl -s localhost:4000/stats | jq                 # token-weighted savings rollup
```

Or drive it directly with an Anthropic-style request (this is exactly how the quickstart is tested — see
[docs/get-started/quickstart-proxy.md](docs/get-started/quickstart-proxy.md)):

```sh
curl -s localhost:4000/anthropic/v1/messages \
  -H 'content-type: application/json' \
  -H "Authorization: Bearer $YOUR_KEY" \
  -d '{"model":"...","max_tokens":64,"messages":[ ... ]}'
```

Presets: **`house`** is the binary's default. **`codesmart`** is the SWE-bench-winning
cache-aware config, `[format, textclean, searchfold, dedup, failed_run, cmdfilter, extract_llm, extract, linecap, cachesplit]`, and is what the
published benchmark numbers describe — pass `--preset codesmart` to run it. **`codesafe`** (the same
minus the LLM pass — deterministic-only `[format, textclean, searchfold, dedup, failed_run, cmdfilter, extract, collapse, linecap, cachesplit]`,
zero model calls by policy), plus `general`, `agent`, `aggressive`, `coding`, `mcp`, `balanced`, `safe`,
`summarize`, `off`.
`codesmart`'s LLM relevance-trimmer (`extract_llm`) engages only when a cheap model is configured
(`CHEAP_MODEL*`); without one it safely no-ops and behaves like `codesafe`.
See [docs/components.md](docs/components.md) and [docs/reference/presets.md](docs/reference/presets.md).

| Flag / env | Default | Purpose |
|---|---|---|
| `--preset` / `PRESET` | `house` | pipeline preset when no `--config` |
| `--idle-exit` / `IDLE_EXIT` | `0` (never) | exit after this long unused; floor `max(2 × store.ttl_seconds, 1h)`, refused with `--upstreams` |
| `--version` | — | print version and commit, then exit |
| `--config` / `CONFIG` | — | YAML config (overrides preset) |
| `LISTEN_ADDR` | `:4000` | listen address |
| `--anthropic-upstream` / `ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | Anthropic upstream base |
| `--openai-upstream` / `OPENAI_UPSTREAM` | `https://api.openai.com` | OpenAI upstream base |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | — | real key injected on forward (gateway mode); empty = pass client auth through |
| `CHEAP_MODEL` (+ `CHEAP_MODEL_*`) | — | dedicated cheap model for the LLM components (`extract_llm`, `summarize`) |
| `FORCE_MODEL` | — | overwrite the request `model` (eval-containers `EVAL_MODEL`) |

Routes: `POST /openai/v1/chat/completions`, `POST /anthropic/v1/messages`,
`POST /compact` (stateless — pipeline in, rewritten body out, no upstream call), `GET /healthz`,
`GET /stats` (savings rollups), `GET /metrics` (the same counters as Prometheus text),
`GET /expand?id=` (recover an offloaded original), and — with
`--dashboard` — `GET /dashboard/` plus `/api/*`. Per-request: header
`x-context-guru-session` sets the session key; `x-context-guru-bypass: true` skips the pipeline.

## Dashboard

`--dashboard` adds a persistent observability UI at `/dashboard/` plus a JSON/SSE API at
`/api/*`. It exists to answer the question the product exists to answer — **what value is
context-guru providing?** — and to make the answer falsifiable.

```sh
context-guru-proxy --preset codesmart --dashboard
# open http://localhost:4000/dashboard/
```

[![The context-guru dashboard](docs/img/dashboard/01-overview.jpg)](docs/dashboard.md)

- **Four labelled savings denominators**, because a single "savings %" is a lie of
  omission: of what we tried to compact · of new provider-billed input · of the whole
  request (diluted) · unique-of-whole. Each one states what it divides by, and reports
  **n/a** rather than a number it cannot compute.
- **Baseline vs actual cumulative cost**, with the saved area shaded, plus an honest
  savings **waterfall** that will show a negative net if we spent more than we saved.
- **The cost of our own safety mechanisms beside their benefit** — cache-frozen tokens,
  restorations, reverts, and context-guru's own latency and LLM spend.
- **Per-component economics**: unique vs gross savings, `overcount_ratio`, own latency, and
  a verdict — so a component that burns wall time for nothing is obvious without a doc.
- **Sessions, requests, and the before/after Git-style diff** of exactly what was removed.
- **Benchmark ingestion** straight from `summary.json` + `rows-*.json`, with cost-vs-reward
  per arm and per-task drill-down.

Embedded via `go:embed` — no CDN, no npm, no build step, so it works air-gapped. Capture is
off the hot path (**~175 ns** per request, drops rather than blocks) and redaction happens
before anything reaches disk. `/stats` is unchanged.

Full guide: **[docs/dashboard.md](docs/dashboard.md)**.

## The pipeline

Every component operates on tool-output messages. **Reformat** = lossless repack; **Offload** = drop
bytes, stash the original, leave a recoverable marker. Real, live-captured before→after examples for each
are in **[docs/components.md](docs/components.md)** and **[docs/results/components.md](docs/results/components.md)**.

| Component | Kind | What it does |
|---|---|---|
| `format` | Reformat | re-encodes pretty JSON tool output as compact JSON |
| `toon` | Reformat | re-encodes a uniform JSON array as TOON (header once, one row per item) |
| `cachesplit` | Reformat | splits the volatile tail off the `system` prompt so the shared prefix stays cacheable (in the default presets) |
| `cacheinject` | Reformat | places Anthropic `cache_control` breakpoints — **opt-in, in no preset**; placement is unmeasured |
| `dedup` | Offload | replaces a byte-identical earlier tool output with a pointer |
| `failed_run` | Offload | collapses superseded test/build runs, keeps the latest in full |
| `cmdfilter` | Offload | shrinks structured command output via declarative DSL filters |
| `extract` | Offload | deterministic noise collapse (repeated lines, blank runs, progress bars) |
| `extract_llm` | Offload (LLM) | a cheap model writes a sandboxed filter that trims to what's relevant |
| `collapse` | Offload | head/tail window on any oversized output (last-resort fallback) |
| `mask` | Offload | age-based GC — keep the newest N tool outputs, stash older ones |
| `skeleton` | Offload | replaces code-block function bodies with signatures (needs `cg_skeleton`) |
| `smartcrush` | Offload | keeps anchor items of a long JSON array, drops the middle |
| `summarize` | Offload (LLM) | compresses the middle of the trajectory into one summary (run alone) |

## Operating modes

`sync` (the default) compacts inline and the caller waits. One other mode changes that:

| Mode | The request path | Use it when |
|---|---|---|
| **`sync`** *(default)* | Compacts inline; the caller waits. | You want the savings. |
| **`observe`** | Forwards the request **untouched, byte for byte**, and reports what compaction *would* have saved. | You want to evaluate context-guru on your own traffic without enforcing it. |

```yaml
mode: observe
```

Byte-identity is **structural**: in observe mode the request path never runs the pipeline
at all (and never injects the expand tool), so no code path could alter a forwarded body.
Measured cost to the enforced path: **0.062 ms/req**, against 1,599 ms for `sync` on the
same benchmark.

Observe-mode numbers are reported under their own `potential_*` / `projected_*` keys that
share no name with an enforced metric, so a hypothetical can never be read as a realized
saving. On identical traffic its projection matches what `sync` actually achieved exactly
(23.06% both sides), and on traffic with nothing to save it correctly projects 0%.

This is a genuine differentiator, not a port: headroom has no observe/shadow/dry-run mode
at all — its `token` and `cache` modes are both enforcing.

Details in [docs/how-to/operating-modes.md](docs/how-to/operating-modes.md).

## Integrate

| Option | What | Where |
|---|---|---|
| **Proxy / gateway** | `context-guru-proxy` in front of the provider; the eval-containers gateway image | `proxy/`, `cmd/context-guru-proxy/` |
| **In-process plugin** | AuthBridge (Rossoctl sidecar) plugin importing this module, running the same pipeline on `pctx.Body` | plugin lives in `cortex`; reuses `apply.Body` + `expand/` |
| _(also)_ **bifrost LLMPlugin** | run the pipeline as a `PreRequestHook` inside any bifrost deployment | `adapters/bifrost/` |

Details in [docs/integrations.md](docs/integrations.md).

## Docs

- [docs/design.md](docs/design.md) — architecture: component model, fail-open pipeline, store, session, expand loop, metrics, operating modes, the dashboard's capture/store layer.
- [docs/how-to/operating-modes.md](docs/how-to/operating-modes.md) — sync vs observe: when to use each, and how to read observe's projections.
- [docs/dashboard.md](docs/dashboard.md) — the persistent observability dashboard: metrics semantics, the diff view, storage, access gating, API.
- [docs/components.md](docs/components.md) — every registered component: how it works, live before→after, lossiness, config, best use.
- [docs/integrations.md](docs/integrations.md) — proxy gateway vs AuthBridge plugin, with request paths.
- [docs/setup.md](docs/setup.md) — setup + a concrete SWE-bench run through the eval-containers gateway.
- [docs/RESULTS.md](docs/RESULTS.md) — the live four-way SWE-bench Verified benchmark (Claude Code, `aws/claude-sonnet-5`): context-guru is the cheapest arm (−13.2% billed cost vs baseline) and solves the most tasks (88%); headroom −5.3%/80%; rtk (shell-level Bash-output hook) −9.0%/86% at $0 tool cost.

## License

Apache-2.0. See [LICENSE](LICENSE). A [Rossoctl](https://github.com/rossoctl) platform component.
