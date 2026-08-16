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
| `store` | object | State store options — see [`store`](#store) below. |
| `mode` | string | Operating mode: `sync` (default) \| `observe`. See [Operating modes](../how-to/operating-modes.md). |
| `observe` | object | Observe-mode tuning; ignored in sync mode. |

### `store`

| Field | Default | Purpose |
|---|---|---|
| `enabled` | `true` | Toggles the state store. `false` wires a `store.Nop`: nothing is stashed, so offloads become **one-way** and must run `marker_mode: off`. |
| `ttl_seconds` | `10000` | Entry lifetime, and it **slides** — a `Get` refreshes the deadline, so an entry replayed every turn never ages out. Raised from 1800 because Terminal-Bench tasks average ~1975 s of wall clock and run to 4 h, so the old default expired live frozen decisions mid-task. |
| `max_entries` | `1000` | LRU cap. Frozen-decision keys (`cg:frz:`, `cg:res:`, `cg:len:`) are **pinned** — exempt from LRU eviction, because losing one is cache-destructive rather than merely a miss. The pin is capped at half `max_entries`, and eviction reclaims **expired** entries first (pinned included). |
| `max_sessions` | `100` | Cap on per-session sticky-id sets. |

The pinned prefixes are a code-level property of the key layout, supplied by their owners via
`store.Options.PinPrefixes` — not a YAML knob.

### `mode`

| Value | Behavior |
|---|---|
| `sync` (default) | Compact inline; the caller waits. Byte-identical to the behavior before modes existed. |
| `observe` | Forward the request untouched and report what compaction *would* have saved, under `potential_*` / `projected_*` keys. The request path never runs the pipeline and skips `expand.Inject` too (a tool declaration is a modification), so byte-identity is **structural**. |

Always explicit — nothing infers it from the rest of the configuration.

!!! note "An `async` mode is designed but not shipped"
    A third mode deferring compaction off the request path is implemented on a separate
    branch and deliberately held pending a benchmark arm establishing a benefit. `sync` and
    `observe` are the only values the loader accepts.

### `observe`

| Field | Default | Purpose |
|---|---|---|
| `max_queue` | `256` | Bound on the off-path measurement queue. A full queue **drops** (counted as `dropped`) and never blocks the request path. |
| `workers` | `1` | Drain goroutines. One keeps a single measurement's cheap-model call in flight per process, which keeps that spend and gateway rate limits predictable. |

!!! warning "Strict: unknown keys are rejected"
    The YAML loader runs with `KnownFields(true)`, so a typo'd key fails loudly
    at load time rather than being silently ignored.

## Example

```yaml
preset: balanced
pipeline: [format, dedup, failed_run, cmdfilter, cachesplit]   # order + enable
components:
  collapse:   { max_tokens: 2000, head_lines: 20, tail_lines: 20 }
  smartcrush: { min_items: 5, keep_first: 3, keep_last: 2 }
  cmdfilter:  { min_size: 400 }   # the default; measured, see components/cmdfilter.md
store: { ttl_seconds: 10000, max_entries: 1000 }
mode: sync                          # sync | observe
```

A component registers its constructor + config type via `init()`, so adding one
makes it YAML-configurable with no core edit. See [Components](../components.md)
for every component's config block.

## Flags & environment

| Flag / env | Default | Purpose |
|---|---|---|
| `--preset` / `PRESET` | `codesmart` | Pipeline preset when no `--config`. |
| `--config` / `CONFIG` | — | YAML config file (overrides preset). |
| `LISTEN_ADDR` | `:4000` | Listen address. |
| `--openai-upstream` / `OPENAI_UPSTREAM` | `https://api.openai.com` | OpenAI upstream base. |
| `--anthropic-upstream` / `ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | Anthropic upstream base. |
| `--bob-upstream` / `BOB_UPSTREAM` | — | Bob (BobShell) backend base. Setting it mounts the [Bob gateway routes](routes.md#bob-bobshell-gateway-routes); unset, an unknown path 404s as before. |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | — | Real key injected on forward (gateway mode); empty = pass client auth through. |
| `CHEAP_MODEL` (+ `CHEAP_MODEL_BASE` / `_KEY` / `_AUTH` / `_PROVIDER`) | — | Dedicated cheap model for the LLM components (`extract_llm`, `summarize`) — the `model.source: config` client. Without it they no-op. |
| `FORCE_MODEL` | — | Overwrite the request `model` (eval-containers uses `EVAL_MODEL`). |
| `INJECT_EXPAND` | `auto` | Whether the `context_guru_expand` tool is advertised: `auto` (only when the request already declares tools, carries a `<<cg:HASH>>` marker, and the store persists) \| `always` \| `never`. |
| `CACHE_MODE` | `auto` | Cache-aware compaction: `auto` (on when the agent sets its own breakpoints) \| `on` \| `off`. |
| `MODEL_INFO_URL` / `MODEL_INFO` | LiteLLM map | Source for context-window sizes (used by the fractional triggers). `MODEL_INFO=off` disables the lookup; fractions are then ignored and absolutes apply. |
| `--store` / `STORE` | on | Enable/disable the state store; `--store=false` disables offload reversibility. Wins over the file's `store:` block. |
| `--mode` / `MODE` | `sync` | Operating mode: `sync` \| `observe`. Wins over the file's `mode:`. |

### Extraction-model pricing

[`extract_llm`](../components/extract_llm.md)'s economic gate only calls the LLM when the
expected saving exceeds the expected cost, so it needs the real price of a call. The cost is
computed from **observed token usage × these rates** — never a hard-coded per-call constant.
Defaults are `claude-haiku-4-5` list rates; override them to match your contract.

| Env | Default | Purpose |
|---|---|---|
| `CHEAP_MODEL_PRICE_IN` | `1.00` | Extraction-model input price, **dollars per million tokens**. |
| `CHEAP_MODEL_PRICE_OUT` | `5.00` | Output price per MTok. |
| `CHEAP_MODEL_PRICE_CACHE_WRITE` | `1.25` | Cache-write price per MTok (1.25× input). |
| `CHEAP_MODEL_PRICE_CACHE_READ` | `0.10` | Cache-read price per MTok (0.1× input). |

An unparseable or absent value silently keeps the default — pricing must never fail a request.

| Env | Default | Purpose |
|---|---|---|
| `CONTEXT_GURU_LLM_TIMEOUT` | `90s` | Per-call deadline for a compaction-model call. Accepts a Go duration or a bare number of seconds. An abandoned call leaves the output verbatim, so **watch `llm_timeouts` in `/stats`**: a non-zero count means that run's savings are an undercount rather than a measurement. |
| `--cheap-model-concurrent` / `CHEAP_MODEL_CONCURRENT` | `4` | Process-wide cap on concurrent compaction-model calls, so one tenant's `extract_llm` cannot stall everyone's agents. `0` = unlimited. |

!!! note "`extract_llm` is off by default on caching backends"
    Independently of pricing, the component declines to run on prompt-caching traffic unless
    `allow_on_caching_backend: true` is set — measured net-negative there. See
    [extract_llm](../components/extract_llm.md#the-honest-verdict).
## Dashboard

The [dashboard](../dashboard.md) is **off by default**. Enabling it adds `/dashboard/` and
`/api/*`; nothing else about the proxy changes.

| Flag / env | Default | Purpose |
|---|---|---|
| `--dashboard` / `DASHBOARD` | off | Enable the persistent dashboard (embedded UI + JSON/SSE API). |
| `--dashboard-db` / `DASHBOARD_DB` | `./context-guru-dashboard.db` | SQLite path. `:memory:` keeps history in RAM only (the no-persistence mode). An unwritable path falls back to in-memory with a warning rather than failing to start. |
| `--dashboard-retention` / `DASHBOARD_RETENTION` | `168h` (7 days) | Drop rows older than this. `0` disables the age rule. |
| `--dashboard-max-bytes` / `DASHBOARD_MAX_BYTES` | `536870912` (512 MiB) | Cap the database size, dropping the oldest requests first. `0` disables the size rule. |
| `--dashboard-content` / `DASHBOARD_CONTENT` | `false` | Capture before/after message text for the diff view. **Opt-in**: it stores arbitrary agent output on disk, scrubbed of known credential shapes and size-capped **before** storage — but content cannot be allowlisted the way headers and config keys are, so the safe default is off. |
| `--dashboard-content-cap` / `DASHBOARD_CONTENT_CAP` | `16384` | Maximum bytes stored per captured before/after blob. |
| `--dashboard-queue` / `DASHBOARD_QUEUE` | `4096` | Capture-channel depth. A full channel **drops** events (counted, and shown in the UI) rather than delaying a request. |
| `--dashboard-trusted-cidrs` / `DASHBOARD_TRUSTED_CIDRS` | — | Comma-separated CIDRs allowed to view per-request **content** and the effective config. Loopback always is; aggregates are open to everyone. |
| `--dashboard-bench-dirs` / `DASHBOARD_BENCH_DIRS` | — | Comma-separated directories of benchmark runs (each with `summary.json` + `rows-*.json`) to ingest at startup. Re-ingesting replaces a run rather than duplicating it. |

!!! note "Retention is bounded by age AND size"
    Age alone cannot bound a burst of traffic; size alone silently erases a quiet week.
    The age rule runs first, then the size rule drops the oldest remaining requests until
    the file fits.

!!! warning "There is deliberately no 'disable observability in production' switch"
    For a tool whose value *is* observability, that would be backwards. What is gated is
    per-request **content** and the effective **configuration** — not the metrics.

### Example (container)

```sh
DASHBOARD=true \
DASHBOARD_DB=/var/lib/context-guru/dashboard.db \
DASHBOARD_RETENTION=720h \
DASHBOARD_MAX_BYTES=2147483648 \
DASHBOARD_TRUSTED_CIDRS=10.0.0.0/8,192.168.0.0/16 \
context-guru-proxy --preset codesmart
```

## Hosted mode, cold storage, and disk pressure

Every one of these has an environment alias, which is how the
[systemd deployment](../hosted.md#2-install-the-service) sets them — the shipped unit is
replaced on every install, so site settings live in a drop-in as `Environment=` lines
rather than as flags. Hosted mode is off unless `--upstreams` is set; the rest are inert
without it, except the disk and cold-storage rules, which apply to any dashboard.

| Flag / env | Default | Purpose |
|---|---|---|
| `--upstreams` / `UPSTREAMS` | — | Path to the upstream allow-list YAML. **Setting it enables hosted multi-tenant mode.** The loader refuses to start if any named `key_env` is unset. |
| `--control-db` / `CONTROL_DB` | `./context-guru-control.db` | Tenants, tokens, per-tenant config. Kept separate from the dashboard DB, which is a derived view that may be rebuilt or pruned. |
| `--manager-email` / `MANAGER_EMAIL` | — | The email that becomes the manager account **at registration**, matched case-insensitively. Must be set *before* the first account registers. |
| `--register-domains` / `REGISTER_DOMAINS` | — | Comma-separated email domains allowed to self-register (exact-domain or a subdomain of it). Applies only when `CG_REGISTER` is `open` or `invite`; the address itself is **unverified**. |
| `CG_REGISTER` | `closed` | Registration mode: `closed` \| `invite` \| `open`. Re-read **per request**, so switching needs no restart. Anything unrecognised normalises to `closed`. |
| `CG_REGISTER_CODE` | — | The invite code `invite` mode compares against. Empty in `invite` mode refuses everyone rather than falling through to open. |
| `--max-tenancies` / `MAX_TENANCIES` | `256` | How many tenants keep live pipelines and compaction state in memory. Evicting one costs it a cold cache on its next turn. |
| `--tenant-monthly-cap-usd` / `TENANT_MONTHLY_CAP_USD` | `50` | Default monthly spend cap per tenant against the shared credential. Over it returns **402**, not 429. |
| `--tenant-rpm` / `TENANT_RPM` | `0` (unlimited) | Requests per minute, per tenant. |
| `--tenant-concurrent` / `TENANT_CONCURRENT` | `0` (unlimited) | In-flight requests, per tenant. |
| `--metrics-token` / `METRICS_TOKEN` | — | Bearer token letting a remote Prometheus scrape `/metrics`. Loopback never needs one; `/metrics` carries per-tenant cost. |
| `--dashboard-max-rows-per-tenant` / `DASHBOARD_MAX_ROWS_PER_TENANT` | `0` (no cap) | Server-wide cap on one tenant's retained request rows, trimmed **before** the disk rule so a heavy user cannot evict everyone else. A per-tenant value a manager sets overrides it. |

### Cold storage (Box via rclone)

Setting `--archive-remote` is what makes eviction **migrate** instead of delete: a session
is uploaded and its size verified before its local rows go. The remote is probed at boot,
and an unreachable one logs an error and disables archiving rather than refusing to serve
traffic. See [Eviction is migration, not deletion](../hosted.md#eviction-is-migration-not-deletion).

| Flag / env | Default | Purpose |
|---|---|---|
| `--archive-remote` / `ARCHIVE_REMOTE` | — | rclone remote path, e.g. `box:context-guru`. **Unset, eviction deletes.** |
| `--rclone` / `RCLONE` | `rclone` | Path to the rclone binary. |
| `--rclone-config` / `RCLONE_CONFIG` | — | The rclone config file holding the remote's OAuth token. Set it explicitly for a service: under systemd `$HOME` is not the shell's. |
| `--archive-bwlimit` / `ARCHIVE_BWLIMIT` | — | rclone `--bwlimit` for archiving (e.g. `8M`). Empty = unlimited, which will use all the upload bandwidth the box has. Note `--bwlimit` is **bytes** per second while speed tests report bits. |
| `--archive-content-after` / `ARCHIVE_CONTENT_AFTER` | `24h` | Move a session's **transcripts** to cold storage once idle this long. This is where the bytes are. `0` = never. |
| `--archive-session-after` / `ARCHIVE_SESSION_AFTER` | `720h` (30 days) | Move a **whole** session once idle this long. `0` = never. |
| `--archive-interval` / `ARCHIVE_INTERVAL` | `15m` | How often the archiver runs. It runs on its own goroutine, never the writer's. |
| `--archive-batch` / `ARCHIVE_BATCH` | `50` | Maximum sessions archived per pass, so one catch-up cycle cannot exhaust the remote's API quota. |
| `--archive-required` / `ARCHIVE_REQUIRED` | `false` | Under disk pressure, refuse to delete a session that could not be archived. Safer for data; lets the filesystem fill if the remote is down, which takes every user's agent with it. Pick it deliberately. |

### Disk-pressure eviction

`--dashboard-max-bytes` bounds *this database*; these bound the **filesystem**, which on a
shared box is mostly filled by other things.

| Flag / env | Default | Purpose |
|---|---|---|
| `--dashboard-disk-high` / `DASHBOARD_DISK_HIGH` | `0.90` | Evict the oldest **sessions** while this fraction of the filesystem is in use. Negative = disable. |
| `--dashboard-disk-low` / `DASHBOARD_DISK_LOW` | `0.85` | Stop evicting once usage falls to this. The gap from the high watermark is what stops the janitor grinding when the host is full for other reasons. |
| `--dashboard-min-keep-bytes` / `DASHBOARD_MIN_KEEP_BYTES` | `1073741824` (1 GiB) | Never shrink the dashboard database below this under disk pressure — below it the pressure is not ours to relieve, and a blank dashboard would hide the real problem. |

## Diagnostics

| Env | Effect |
|---|---|
| `CONTEXT_GURU_DEBUG=1` | Logs each tool output's token count + first line. |
| `CONTEXT_GURU_DUMP=<file>` | Appends a before → after JSON record per rewritten message. The [dashboard](../dashboard.md) captures the same material into a queryable store with a diff view. |
| `CONTEXT_GURU_CAPTURE=<file>` | Appends each pristine inbound request as one JSONL record, for offline replay through `/compact`. |
