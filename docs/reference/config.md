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
| `components:<name>` | map | Each component's typed config block, handed to its constructor verbatim. Decoded **strictly** — an unknown key inside a block is an error, not silence — and every key is declared for the dashboard's field form: see [the settings form](../how-to/settings-form.md). |
| `store` | object | State store options — see [`store`](#store) below. |
| `mode` | string | Operating mode: `sync` (default) \| `observe`. See [Operating modes](../how-to/operating-modes.md). |
| `observe` | object | Observe-mode tuning; ignored in sync mode. |

### `store`

| Field | Default | Purpose |
|---|---|---|
| `enabled` | `true` | Toggles the state store. `false` wires a `store.Nop`: nothing is stashed, so offloads become **one-way** and must run `marker_mode: off`. |
| `ttl_seconds` | `10000` | Entry lifetime, and it **slides** — a `Get` refreshes the deadline, so an entry replayed every turn never ages out. Raised from 1800 because Terminal-Bench tasks average ~1975 s of wall clock and run to 4 h, so the old default expired live frozen decisions mid-task. |
| `max_entries` | `5000` | LRU cap. Two groups of keys are **exempt** from LRU eviction, and they behave differently when full — see below. Eviction reclaims **expired** entries first, exempt ones included. Raised from 1,000: one process-wide store serves every concurrent session, and a single reversible removal writes five entries (the payload, `cg:own:`, `cg:xseen:`, and the two pinned decision records), so 1,000 was an order of magnitude under the observed volume. |
| `stash_max_bytes` | `268435456` (256 MiB) | What the **rewind reserve** may cost in memory. Entries are a poor proxy for it in this one namespace: every other exempt entry is a marker line or an integer, a rewind payload is a whole tool output. Whichever of `max_entries` and this binds first, binds. |
| `max_sessions` | `100` | Cap on per-session sticky-id sets. |

#### The two exemptions, and the floor under them

**Pinned decisions** (`cg:frz:`, `cg:res:`, `cg:xres:`, `cg:len:`, `cg:ttl:`, `cg:seen:`) are
exempt because losing one is cache-destructive rather than merely a miss: the replacement bytes
for an already-cached message stop being reproducible, so the message flips and the provider
re-writes the whole suffix at ~11.5x the read price. Over its cap a pin simply becomes an
ordinary evictable entry.

**The rewind reserve** holds the payloads behind `<<cg:HASH>>` markers. It cannot be a pin
prefix — a payload key *is* the marker id, a bare content hash the model reads out of the
request — so it is claimed explicitly and, unlike a pin, a payload that cannot be admitted is
**refused**: the component declines the removal and leaves the content verbatim rather than
stamping a marker nothing can resolve. Only the TTL releases a slot.

Each exemption is capped at half `max_entries`, and **a quarter of `max_entries` is held back
from both** so something is always evictable. Without that floor the two could occupy the whole
cap, and a cache with nothing evictable does not fail loudly: the next write to an unpinned
namespace is evicted by its own insert, silently turning `cg:keep:` (the flag that stops the
expand loop), `cg:sum:` (summarize checkpoints) and `cg:own:` (which gates `GET /expand`) into
no-ops.

**When `stash_refused` rises**, raise `max_entries` or `stash_max_bytes` — read `stash_live`
against `stash_capacity` and `stash_bytes` against `stash_max_bytes` at
[`/stats`](routes.md#get-stats) to see which budget bound. Nothing became irreversible: the
removals did not happen. `stash_missing` is the different, worse number — a marker replayed with
no payload behind it — and its fix is `ttl_seconds`.

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
store: { ttl_seconds: 10000, max_entries: 5000 }
mode: sync                          # sync | observe
```

A component registers its constructor + config type via `init()`, so adding one
makes it YAML-configurable with no core edit. See [Components](../components.md)
for every component's config block.

## Flags & environment

| Flag / env | Default | Purpose |
|---|---|---|
| `--preset` / `PRESET` | `house` | Pipeline preset when no `--config`. `codesmart` is the SWE-bench arm and must be asked for by name. |
| `--config` / `CONFIG` | — | YAML config file (overrides preset). |
| `LISTEN_ADDR` | `:4000` | Listen address. |
| `--openai-upstream` / `OPENAI_UPSTREAM` | `https://api.openai.com` | OpenAI upstream base. |
| `--anthropic-upstream` / `ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | Anthropic upstream base. |
| `--bob-upstream` / `BOB_UPSTREAM` | — | Bob (BobShell) backend base. Setting it mounts the [Bob gateway routes](routes.md#bob-bobshell-gateway-routes); unset, an unknown path 404s as before. |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | — | Real key injected on forward (gateway mode); empty = pass client auth through. |
| `CHEAP_MODEL` (+ `CHEAP_MODEL_BASE` / `_KEY` / `_AUTH` / `_PROVIDER`) | — | Dedicated cheap model for the LLM components (`extract_llm`, `summarize`) — the `model.source: config` client. Without it they no-op. |
| `FORCE_MODEL` | — | Overwrite the request `model` (eval-containers uses `EVAL_MODEL`). |
| `INJECT_EXPAND` | `auto` | Whether the `context_guru_expand` tool is advertised: `auto` (whenever the request declares tools, the store persists, **and the pipeline contains at least one Offload** — all three session-stable, so the `tools` array never changes shape mid-session and the prompt-cache prefix survives) \| `always` (unconditional, including when the request declares no tools) \| `never`. The pipeline condition exists because a pipeline that mints no markers would be advertising a tool whose every call must fail. |
| `CACHE_MODE` | `auto` | Cache-aware compaction: `auto` (on when the agent sets its own breakpoints) \| `on` \| `off`. |
| `MODEL_INFO_URL` / `MODEL_INFO` | LiteLLM map | Source for context-window sizes (used by the fractional triggers). `MODEL_INFO=off` disables the lookup; fractions are then ignored and absolutes apply. |
| `MODEL_PRICES` | — | Path to an **operator price list**, consulted before the public map. See below. A file that fails to load is fatal. |
| `--store` / `STORE` | on | Enable/disable the state store; `--store=false` disables offload reversibility. Wins over the file's `store:` block. |
| `--mode` / `MODE` | `sync` | Operating mode: `sync` \| `observe`. Wins over the file's `mode:`. |

### Per-model prices, and why the public map is not enough

Every dollar figure on the dashboard is `observed tokens × per-model rates`. Those rates come
from LiteLLM's public `model_prices_and_context_window.json`, which prices the **public API**.
A gateway does not have to charge that, and two things go wrong when it does not:

* **Wrong rates.** IBM's `ete-litellm` bills `aws/claude-sonnet-5` at $1.52/MTok in where
  anthropic.com bills $3.00 — so every cost, every baseline and every saving for that gateway
  read about twice the truth.
* **No rates at all.** A model the public map has never heard of — a preview id, an internal
  route name, or a server-resolved **tier** like Bob's `premium`, which is not a model id —
  resolves to nothing, the row is marked `partial`, and its cost reads *unknown*. That is why
  a Bob session showed tokens and latency but no money.

`MODEL_PRICES=/etc/context-guru/prices.yaml` fixes both. Start from
[`deploy/service/prices.example.yaml`](https://github.com/rossoctl/context-guru/blob/main/deploy/service/prices.example.yaml),
which carries the whole ete-litellm table:

```yaml
cache_read_frac: 0.1     # fills cache_read where an entry omits it
cache_write_frac: 1.25   # …and cache_write
models:
  - {match: "aws/claude-sonnet-5", in: 1.52, out: 7.60}
  - {match: "claude-sonnet*",      in: 2.28, out: 11.40}   # a family; longest match wins
  - {match: "premium*",            in: 3.00, out: 15.00, note: "Bob tier — an ESTIMATE"}
```

Rates are **dollars per million tokens**, matching every vendor's price page; they are
converted once on load. Ids are matched case-insensitively: an exact id wins, and otherwise
the **longest** entry wins whether it matched as a family prefix or by containment — so a
specific entry always beats the family it belongs to and order in the file decides nothing.
Containment applies only to entries that look like a model id (one containing a `/` or a
`.`), which is what stops a short word-like entry such as Bob's `fast` tier from claiming
`azure/gpt-5.2-fast` and pricing it ten times under with `ok=true`, where a miss would have
let the public map answer. `window:` optionally supplies a context window for a model the
public map does not list either.

These are list prices, not credentials, and the file holds no secret. A malformed one is a
**startup error** rather than a fallback: a price list that silently failed to load is
indistinguishable from "every model is free".

!!! warning "A tier is an estimate, and the file should say so"
    Bob puts `premium` / `premium-ide` / `standard` / `fast` on the wire — a tier, resolved
    server-side, not a model. Any rate for it is a guess. The shipped entry is fitted against
    the `session_costs` Bob prints for itself and lands within about a third on real runs;
    the example file shows both measurements. That error bar carries into every **absolute**
    dollar figure for those rows — baseline, compaction savings, prefix-cache savings and the
    "total avoided" headline. It does not touch the before/after **ratio**, where a uniform
    rate error cancels.

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
| `--dashboard-content` / `DASHBOARD_CONTENT` | `false` | Capture before/after message text for the diff view. It stores arbitrary agent output on disk, scrubbed of known credential shapes and size-capped **before** storage — but content cannot be allowlisted the way headers and config keys are, so the safe default is off. In hosted mode this is only the **operator's** half of the decision: a tenant is registered with its own `capture_content` already **on**, so this flag is what keeps a new account's transcripts off disk. |
| `--dashboard-content-cap` / `DASHBOARD_CONTENT_CAP` | `16384` | Maximum bytes stored per captured before/after blob. |
| `--dashboard-queue` / `DASHBOARD_QUEUE` | `4096` | Capture-channel depth. A full channel **drops** events (counted, and shown in the UI) rather than delaying a request. |
| `--dashboard-trusted-cidrs` / `DASHBOARD_TRUSTED_CIDRS` | — | Comma-separated CIDRs allowed to view per-request **content**, prompt text (tool schemas and the system prompt) and the effective config. Loopback always is; aggregates and token weights are open to everyone. |
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
| `--upstreams` / `UPSTREAMS` | — | Path to the upstream allow-list YAML. **Setting it enables hosted multi-tenant mode.** `key_env` is optional (omit it and the caller’s own provider key is forwarded); if an entry names one, the loader refuses to start while it is unset. |
| `--control-db` / `CONTROL_DB` | `./context-guru-control.db` | Tenants, tokens, per-tenant config. Kept separate from the dashboard DB, which is a derived view that may be rebuilt or pruned. |
| `--manager-email` / `MANAGER_EMAIL` | — | The email that becomes the manager account **at registration**, matched case-insensitively. Must be set *before* the first account registers. |
| `--register-domains` / `REGISTER_DOMAINS` | — | Comma-separated email domains allowed to self-register (exact-domain or a subdomain of it). Applies only when `CG_REGISTER` is `open` or `invite`; the address itself is **unverified**. |
| `CG_REGISTER` | `closed` | Registration mode: `closed` \| `invite` \| `open`. Re-read **per request**, so switching needs no restart. Anything unrecognised normalises to `closed`. |
| `CG_REGISTER_CODE` | — | The invite code `invite` mode compares against. Empty in `invite` mode refuses everyone rather than falling through to open. |
| `--max-tenancies` / `MAX_TENANCIES` | `256` | How many tenants keep live pipelines and compaction state in memory. Evicting one costs it a cold cache on its next turn. |
| `--tenant-rpm` / `TENANT_RPM` | `0` (unlimited) | Requests per minute, per tenant. |
| `--tenant-concurrent` / `TENANT_CONCURRENT` | `0` (unlimited) | In-flight requests, per tenant. |
| `--metrics-token` / `METRICS_TOKEN` | — | Bearer token letting a remote Prometheus scrape `/metrics`. Loopback never needs one; `/metrics` carries per-tenant cost. |
| `--dashboard-max-rows-per-tenant` / `DASHBOARD_MAX_ROWS_PER_TENANT` | `100000` | Server-wide cap on one tenant's retained request rows. The janitor runs **quota first**, then the age and byte rules, then the disk rule — so a heavy user is trimmed to its own quota before anyone else's history is touched. Set `0` and one tenant can fill the database, after which the byte rule deletes the oldest rows in the whole table: the offender keeps its recent history and the quiet tenants lose theirs. A per-tenant value a manager sets overrides it. |

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

## Logging

Levels mean the same thing everywhere: **ERROR** we failed, **WARN** we degraded but kept
serving (fell open, reverted, refused, evicted), **INFO** the request lifecycle — one line
per request — plus startup facts, **DEBUG** per-component decisions: which gate declined a
component and on what numbers.

| Env | Default | Effect |
|---|---|---|
| `CG_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. `debug` is about 8x the volume of `info` (measured: 337 lines against 40 on the same agent traffic, 8-component pipeline); it is the level to investigate at, not to leave on. |
| `CG_LOG_FORMAT` | `text`, or `json` when `CG_LOG_FILE` is set | `text` for a human, `json` for a log shipper. |
| `CG_LOG_FILE` | unset | Also write every record as JSON to this file, for promtail/Loki. **This is the only thing that lets logs leave the box**, and even then only once promtail exists — the proxy never talks to Loki itself. |
| `CG_LOG_PLAIN` | unset | Opt out: a plain `log/slog` text handler on stderr, no file sink. Also **disables credential scrubbing**, which is what "use the standard logger instead" means. |

Every line of one request's lifecycle carries the tenant id — never its token — and, from
the point the session is resolved, the session id. See
[deploy/grafana/README.md](https://github.com/rossoctl/context-guru/blob/main/deploy/grafana/README.md)
for the Loki setup, the LogQL recipes, and the exact command to turn DEBUG on for a
deployed service.

Credentials are scrubbed on the way **out**, in the handler, rather than at each call site:
a rule that every caller must remember holds only until someone who has not read it adds a
line. Attribute values (including those baked in by `Logger.With`), attribute keys that
*name* a credential, and the message itself all go through the same patterns `dash` uses
before writing a captured request to disk.

## Diagnostics

| Env | Effect |
|---|---|
| `CONTEXT_GURU_DEBUG=1` | Legacy alias for `CG_LOG_LEVEL=debug`. Turns on the per-tool-output and per-candidate DEBUG lines it always did, now at DEBUG rather than INFO. |
| `CONTEXT_GURU_DUMP=<file>` | Appends a before → after JSON record per rewritten message. The [dashboard](../dashboard.md) captures the same material into a queryable store with a diff view. |
| `CONTEXT_GURU_CAPTURE=<file>` | Appends each pristine inbound request as one JSONL record, for offline replay through `/compact`. |

!!! warning "Both are refused in hosted mode"
    With `--upstreams` set, either variable makes the process **exit at startup**, naming
    the one it found. Both append to a single process-wide file with no tenant column and
    without running the redactor, so on a shared instance they are a plaintext transcript of
    every tenant's source code, written whether or not that tenant consented to capture.
    Unset it, or drop `--upstreams` to run single-tenant.
