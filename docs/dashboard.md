# Dashboard

context-guru ships a **persistent observability dashboard**: an embedded single-page UI
plus a JSON/SSE API, backed by a durable per-request store. It exists to answer one
question honestly — **what value is context-guru providing?** — including when the
answer is "less than you hoped".

```sh
context-guru-proxy --preset general --dashboard
# open http://localhost:4000/dashboard/
```

It is **off by default**. Turning it on adds `/dashboard/` and `/api/*`; every existing
route, including [`/stats`](reference/reference.md), is byte-for-byte unchanged.

!!! info "No CDN, no build step, no network"
    The whole UI is three files (`index.html`, `style.css`, `app.js`) embedded in the
    binary with `go:embed`. Charts are hand-drawn SVG — no chart library, no framework.
    The page fetches nothing off-origin, so it works in a VPC or fully air-gapped.

!!! note "Screenshots"
    The UI was recently redesigned. Updated screenshots are coming soon.

## What you'll see

- **Overview** — the headline numbers: tokens before/after, dollars saved, and the
  honesty machinery behind them (gross vs. unique savings, what got frozen for cache
  safety, what our own compaction calls cost). One view, no sub-nav.
- **Savings** — where the money went: usage over time, per-account campaigns, and the
  benchmark evidence behind the headline numbers.
- **Behaviour** — what the proxy actually did to your traffic: per-component economics
  (which ones earn their place), the [tool inventory](#tool-inventory-panel), keep-alive,
  and [KV-cache behavior](#kv-cache-panel).
- **Traffic** — per-session and per-request detail, including a git-style diff of
  exactly what was rewritten on the wire.
- **Admin** — configuration, tenants, and the capture pipeline's own health.

Each group opens on the view a manager would read first; depth is further in. Dark mode,
small viewports, and empty states (a blank dashboard explains *why* each panel is
blank) are all handled.

## Honest-metrics rules

The dashboard follows a few rules that are the difference between a dashboard and a
marketing surface:

1. **Every ratio names its denominator.** "Saved" is ambiguous on its own — gross
   (re-sent history counted every turn) and unique (content that genuinely never
   reached the provider) are shown separately, never blended into one number.
2. **The cost of our own safety mechanisms sits beside their benefit**, as a number —
   not a promise in prose.
3. **A component that ran and did nothing is distinguishable from a component that
   never ran at all.** Only components actually in your pipeline appear in the table.
4. **Cache misses are attributed**, and a cold start is not treated as a failure.
5. **"Why didn't you compact this?" is a first-class answer**, not an absence of data.

## Access

| Surface | Who can see it |
|---|---|
| Aggregates, series, per-component and per-session rollups | anyone who can reach the port |
| Per-request **content** (the diff view) and prompt text | loopback, or an explicit trusted CIDR |
| Effective **configuration** | loopback, or a trusted CIDR |

Aggregates are open on purpose — a proxy bound to `0.0.0.0` should still show its own
numbers. Content and prompt text are gated because a transcript can carry a user's
source code. There is no "disable observability in production" switch, for a tool
whose value *is* observability.

Running as a [hosted service](hosted.md) adds a sign-in gate and a few tenant-scoped
tabs (Setup, Settings, Archive, and — for a manager — Tenants), on top of the same UI.

## Storage & retention

SQLite (pure Go, no C toolchain), in WAL mode. Retention is bounded by both **age**
(`--dashboard-retention`) and **size** (`--dashboard-max-bytes`), so a burst can't
blow past the limit and a quiet week can't erase itself. `--dashboard-db :memory:`
runs with no persistence at all.

## Tool inventory panel

*How much of what your agent carries in every request does it actually use?*

An agent declares its whole toolbox — every tool, every MCP server, every skill — on
**every** request, inside the cached prefix. The panel scans the request's `tools` array
and system-prompt skills listing (never the conversation itself), records the token
weight of each declaration, and tracks which ones actually got called. On real Claude
Code traffic, roughly 80% of declared tokens are typically never invoked in a session.

Reading it: `GET /api/tools` for the rollup, `GET /api/prompt` for one session's actual
prefix text. See [Config & routes reference](reference/reference.md) for both routes.

Top to bottom, the panel shows:

- **Headline tiles and gauge** — declared vs. invoked tokens, over the *controllable*
  set only (your MCP tools and skills); the agent's own built-in tools are shown
  separately at the bottom and are never included in this ratio, since removing them
  isn't an option.
- **Who owns your system prompt** — a part-to-whole bar of the whole prefix, colored by
  whether that region is yours to change.
- **Your system prompt, and what shares it** — the prompt broken into its own sections
  (by markdown heading), each measured and readable.
- **Carried by every request, never once called** — the actionable list, grouped by
  what one action removes (a whole MCP server, or a single tool/skill), each group with
  the exact command to run.
- **Realized by your removals** — what a removal *actually* avoided on requests really
  sent, kept separate from the projected savings above it.
- **Full declaration table and MCP/skills rollups** — sortable, per-server and
  per-skill detail.

To act on it: add the dead weight to [`toolfilter`](components/reformat.md#toolfilter)'s
`remove:` list — it never removes a name you didn't list. The opt-out checkbox in the
UI does the same thing without editing config, and it's reversible, but toggling it has
a one-time cost: declarations sit at the very front of the cached prompt, so changing
the set changes the prefix, and every live session's next turn pays a full cache-miss
write instead of a cache read. Switch off everything you mean to switch off in one pass.

Declaration **names and token weights** are gated on tenant scoping only. The **text**
of a declaration (the schema itself, the skill listing, the system prompt) is
transcript-class data and needs the same operator + tenant consent that gates any other
captured content — see [Access](#access) above. Without that consent the measurement
still works; only the text is withheld.

## KV-cache panel

*How long your conversations actually stay idle, what the prompt cache is costing you
at that idle profile, and what a different TTL policy would have cost on the same
history.*

The tab sits next to **Keep-alive** — both answer "what is the prompt cache doing to my
bill." It respects the shared filter bar (time range, model, agent, account), plus
three narrowings that exist only here because they're derived rather than stored
columns: time-of-day band, observed TTL tier, and whether a request has a successor at
all.

Top to bottom:

- **Coverage statement** — what the analysis could *not* answer: rows with no recorded
  TTL, rows with no cost, conversations with a single request.
- **Summary tiles** — requests, conversations, median/mean idle time, 5-minute and
  1-hour reuse probability, cache-hit rate as billed, requests with no successor.
- **Idle histogram and survival curve** — how long until the next request, and the
  cumulative share of conversations that had returned by each elapsed time.
- **Breakdowns** — the same measurements grouped by observed TTL, user, model, and
  time-of-day (UTC only — there is no per-user timezone in the store).
- **Prices** — every rate the simulation uses, editable, priced against this window's
  own median cached prefix.
- **Strategies** — pick a TTL policy from the server's registry and compare its cost
  against a baseline. Each cell reads `$X cheaper` or `$X MORE` rather than a bare
  signed number, and `optimal` (the ceiling — reads the true next-request time in
  hindsight) can never be selected as a baseline.
- **Every request in the analysis** — the derived dataset, sortable, linking back to
  the request drawer.

A few gotchas worth knowing when reading this page:

- A request with **no next request** in its conversation has no idle time and is
  excluded from every average rather than counted as an instant return.
- A blank `cache_ttl` on an older row means *not recorded*, not "no cache used" — the
  two are distinguished by whether the request billed at the 1-hour tier or cached
  anything at all, and reported as **unknown** rather than assumed.
- The hit rate is shown because operators ask for it, but the page doesn't sort or
  color by it: on real traffic, holding every prefix for an hour gives the best hit
  rate while costing more overall than a 5-minute TTL, because it pays a larger write
  cost than the reads it saves.
- A conversation is keyed by `(account, session, model)` — a cache entry doesn't
  transfer between models, so two requests in one session on different models are
  never treated as each other's successor.

Cost model (every rate is per-provider/model and editable in the page):

```text
uncached input        input_tokens   × input_rate
cache read             cache_read     × cache_read_rate      (default 0.1× input)
cache write, 5m        written_tokens × write_5m_rate         (default 1.25× input)
cache write, 1h        written_tokens × write_1h_rate         (default 2.0× input)
cache premium          total_cost − uncached_cost   (negative = the cache paid for itself)
```

The 1-hour write rate is derived (no provider publishes one) as a multiplier over base
input; both the multiplier and the resulting rate are shown on the page. Routes: four,
all `GET`, all tenant-scoped — see [Config & routes reference](reference/reference.md).

## Verifying it yourself

```sh
CGO_ENABLED=1 go test ./dash/ ./proxy/           # unit + integration
curl -s localhost:4000/api/stats | jq '.denominators[] | {label, percent, available}'
```

See [Config & routes reference](reference/reference.md) for every dashboard flag and
the full `/api/*` surface.
