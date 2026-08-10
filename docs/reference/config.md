# Config & environment

One strict YAML struct serves both hosts (the proxy loads a file; the AuthBridge
plugin hands its `config:` subtree to the same loader). A `preset` expands to a
default pipeline; explicit fields override it.

## Config shape

The document has six top-level fields (from the `Config` struct in
`config/config.go`):

| Field | Type | Role |
|---|---|---|
| `preset` | string | Named default pipeline (see [Presets](presets.md)). |
| `pipeline` | `[]string` | Ordered component names — controls **order + enablement**. Overrides the preset's pipeline when present. |
| `components:<name>` | map | Each component's typed config block, handed to its constructor verbatim. |
| `store` | object | State store options (`enabled`, `ttl_seconds`, `max_entries`, …). |
| `mode` | string | Operating mode: `sync` (default) \| `async` \| `observe`. See [Operating modes](../how-to/operating-modes.md). |
| `async` | object | Async-mode tuning; ignored in the other two modes. |

### `mode`

| Value | Behavior |
|---|---|
| `sync` (default) | Compact inline; the caller waits. Byte-identical to the behavior before modes existed. |
| `async` | Compact off the request path; subsequent turns use the result. Protects cache-write economics by default. |
| `observe` | Forward the request untouched and report what compaction *would* have saved, under `potential_*` / `projected_*` keys. |

Always explicit — nothing infers it from the rest of the configuration.

### `async`

| Field | Default | Purpose |
|---|---|---|
| `cache_uncompacted_tail` | `false` | When false (the safe default), no prompt-cache breakpoint is placed at or beyond the tail a pending compaction will replace. A cache-write costs **11.5x** a cache-read, so caching that tail and then replacing it makes async strictly worse than `sync`. Set true only for a backend confirmed **not** to cache prompts. |
| `strip_caller_breakpoints` | `false` | Let the tail protection remove a cache breakpoint the **agent** placed inside the protected span. Required for the protection to do anything on an agent that sets its own — claude-code does — otherwise async declines to defer those turns (`async_tail_unprotected_turns`) and is effectively inert. Default false because it overrides a directive in someone else's request. |
| `max_queue` | `256` | Bound on the off-path job queue. A full queue **drops** (counted as `dropped`) and never blocks the request path. |
| `workers` | `1` | Drain goroutines. One keeps a single compaction LLM call in flight per process, which keeps cheap-model spend and gateway rate limits predictable. |

!!! warning "Strict: unknown keys are rejected"
    The YAML loader runs with `KnownFields(true)`, so a typo'd key fails loudly
    at load time rather than being silently ignored.

## Example

```yaml
preset: balanced
pipeline: [format, dedup, failed_run, cmdfilter, cacheinject]   # order + enable
components:
  collapse:   { max_tokens: 2000, head_lines: 20, tail_lines: 20 }
  smartcrush: { min_items: 5, keep_first: 3, keep_last: 2 }
store: { ttl_seconds: 1800, max_entries: 1000 }
mode: sync                          # sync | async | observe
async: { cache_uncompacted_tail: false, strip_caller_breakpoints: false, max_queue: 256, workers: 1 }
```

A component registers its constructor + config type via `init()`, so adding one
makes it YAML-configurable with no core edit. See [Components](../components.md)
for every component's config block.

## Flags & environment

| Flag / env | Default | Purpose |
|---|---|---|
| `--preset` / `PRESET` | `balanced` | Pipeline preset when no `--config`. |
| `--config` / `CONFIG` | — | YAML config file (overrides preset). |
| `LISTEN_ADDR` | `:4000` | Listen address. |
| `--openai-upstream` / `OPENAI_UPSTREAM` | `https://api.openai.com` | OpenAI upstream base. |
| `--anthropic-upstream` / `ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | Anthropic upstream base. |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | — | Real key injected on forward (gateway mode); empty = pass client auth through. |
| `FORCE_MODEL` | — | Overwrite the request `model` (eval-containers uses `EVAL_MODEL`). |
| `--store` / `STORE` | on | Enable/disable the state store; `--store=false` disables offload reversibility. Wins over the file's `store:` block. |
| `--mode` / `MODE` | `sync` | Operating mode: `sync` \| `async` \| `observe`. Wins over the file's `mode:`. |

## Diagnostics

| Env | Effect |
|---|---|
| `CONTEXT_GURU_DEBUG=1` | Logs each tool output's token count + first line. |
| `CONTEXT_GURU_DUMP=<file>` | Appends a before → after JSON record per rewritten message. |
