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
route, including [`/stats`](reference/routes.md), is byte-for-byte unchanged.

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
  (which ones earn their place), the [tool inventory](tool-inventory.md), keep-alive,
  and KV-cache behavior.
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

## Verifying it yourself

```sh
CGO_ENABLED=1 go test ./dash/ ./proxy/           # unit + integration
curl -s localhost:4000/api/stats | jq '.denominators[] | {label, percent, available}'
```

See [Config & environment](reference/config.md) for every dashboard flag, and
[Routes & headers](reference/routes.md) for the full `/api/*` surface.
