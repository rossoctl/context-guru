# Dashboard

context-guru ships a **persistent observability dashboard**: an embedded single-page UI
plus a JSON/SSE API, backed by a durable per-request store. It exists to answer one
question honestly — **what value is context-guru providing?** — including when the
answer is "less than you hoped".

```sh
context-guru-proxy --preset general --dashboard
# open http://localhost:4000/dashboard/
```

`/dashboard/` opens the **Overview**. The older links `/dashboard/#home` and `/dashboard/#cache` open
Overview and Cache strategy, and `/dashboard/home.html` redirects to the dashboard.

The dashboard uses the words *preset* and *cache strategy*. The older names, compaction and keep-alive, mean the same things.

It is **off by default**. Turning it on adds `/dashboard/` and `/api/*`; every existing
route, including [`/stats`](reference/reference.md), is byte-for-byte unchanged.

!!! info "No CDN, no build step, no network"
    The whole UI is a handful of static files (`index.html`, `style.css`, `app.js`, `charts.js`,
    `tools.js`, `kvcache.js`, `campaigns.js` and one bundled font) embedded in the
    binary with `go:embed`. Charts are hand-drawn SVG — no chart library, no framework.
    The page fetches nothing off-origin, so it works in a VPC or fully air-gapped.

!!! tip "New to the dashboard?"
    [The dashboard, tab by tab](how-to/dashboard-guide.md) walks through every tab with a
    screenshot and a plain-words explanation. This page is the reference.

## What you'll see

- **Overview** — "What context-guru saved": one **total** in dollars, out of what the traffic would
  have cost without context-guru, with the percent saved and the input tokens removed. Below it, one
  card each for the **Cache** (with automatic vs manual pings, tokens read instead of rewritten,
  waiting time saved and cache writes prevented), the **Preset** and the **Tools and skills filter**.
  Each card shows its money and tokens out of the same whole, with a percent. An "i" explains the
  total and the waiting-time estimate. Below: "Tools and
  skills to remove" (a ranked list, with `/context-guru:remove-mcp-and-skills` and
  `/context-guru:insights tools`), "About the other tabs", and one closed "Detailed diagnostics"
  section with five plots (net saving over time; money saved and added, where an outlined bar is an
  upper bound; tokens removed; where input tokens went, as 100% columns of cache read, cache write
  and fresh; time added per request and requests with cache misses) and a live requests table.
  Every plot shows exact figures on hover.
- **Savings** — Cache strategy (also called keep-alive), Inventory (the [tool inventory](#tool-inventory-panel)), Cleanup (what to remove, with one command) and Usage over time.
- **Traffic** — Sessions (newest or costliest first; six columns, more with a checkbox) and
  Requests, including a git-style diff of exactly what was rewritten on the wire.
- **Admin** — Config, Cache strategies, Tenants, Setup, Settings, Archive and Feedback
  (some of these appear only in a [hosted service](hosted.md)).
- **Advanced** — Preset steps (per-component economics), [Cache detail](#kv-cache-panel) (the KV-cache view),
  and Strategy campaigns (per-account cache strategies).

Every tab starts with a title and one line saying what it shows. Dark mode, small
viewports, and empty states (a blank dashboard explains *why* each panel is blank) are
all handled.

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

## Savings source

Every saving the dashboard stores comes from the accountant library
([rossoctl/accountant](https://github.com/rossoctl/accountant)). It needs `--dashboard`: the
library's result is attached to the recorded event, so without a recorder nothing runs. The proxy
itself books no saving: `Event.Price` stores the cost, the provider's own cache saving and the
accounting fields, and nothing else.

What the library prices:

- **Components.** Each component's saved dollars, net of the cache break it caused, and the baseline
  cost. The component figures sum to baseline minus cost. Each row also stores the library's split:
  `saved_first_usd` (saved tokens priced at the write or input tier: content the provider had not
  cached), `saved_replay_usd` (at the read tier: content in the cached prefix), and the costs, which
  are `saved_usd` minus both. `priced_by` is `accountant`. On a request the library could not price (a
  fallback) it is `none`: the row books no saving, and the read views do not value it.
- **The envelope.** The proxy changes `tools` and `system` in seven places (toolschema, toolfilter,
  cachesplit, headttl, the tail mark, expand and adjudicate). It reports each change to the library
  from a diff of the two bodies. An injected tool declaration (expand, adjudicate) is a cost on a
  row of its own.
- **Keep-alive.** `keepalive_saved_usd` is the library's rescue. The proxy passes each ping's usage,
  and the library derives the extent the ping read from it. A prefix that another thread of the
  session kept warm inside the TTL is not credited (`EntryFacts.SharedWarm`, accountant R54).
  When the override applies, the request stores `requests.acc_priced = 1` (sent as `acc_priced`). The
  keep-alive views trust the stored `keepalive_saved_usd` of such a request as the library's own
  decision, with no ping probe and with no component row needed, so a keep-alive-only setup keeps its
  rescue. A row with `acc_priced = 0` (older rows, and requests the library could not price) keeps the
  read-side reachability test. The entry TTL given to the library follows the billed tier: if the
  request asked for 1h and the response billed 5m, the library judges the next gap against 5 minutes.
- **Tool-filter credit.** A request whose `toolfilter` row is `priced_by = accountant` adds no dollars
  to the measured filter credit (the net saving has them). It still counts in the request count, the
  tokens not sent and the first date, so the Realized block and the item list stay.
- **Cachesplit.** `cachesplit_saved_usd` is the library's split effect.
- **Head TTL.** The 1h premium and the rescue it earns are on the `headttl` row.
- **A break nobody owns.** A break that a change left behind when it ended is counted on the
  `ended_change` row. A break the client body has too (a client edit, an expiry) is not counted.

When the library cannot price an event, the event books **no saving**, and the proxy logs one
`acc_fallback` line with the reason: `no_result`, `ops_dropped`, `observe`, `unpriced`,
`check_sum`, `check_unique_change`, `check_spans`, `size_unknown`, `implausible_size`,
`figure_without_row`, `duplicate_name`, `acted_without_figure` or `unsupported_wire`. A saving nothing
measured is not shown.

For `no_result` the line also carries `why`: `no_record` (no session, or the pipeline did not run), `no_usage`,
`upstream_rounds=N` (an expand loop) or `compute_panicked`.

`unsupported_wire` is the OpenAI Responses API. It keeps the conversation in `input`, and the proxy hashes
`messages` and reports no operations there, so **every saving on that wire is $0**. Chat Completions and the
Anthropic Messages API are priced. Until Responses is read, a Responses account sees no saving and the
keep-alive tab on it can read negative: the pings still cost, and no rescue is credited.

Limits:

- A request with no history to classify a cache break against gets `penalty_unknown`. This is the
  first request of a thread, and a request after a gap longer than the cache lifetime when the
  provider's lifetime is unknown (no explicit breakpoints). The gross saving is right, and the net
  can be too high if a component broke the cache on that request (accountant design §6.4). The
  event carries an estimate flag, stored in `requests.price_estimated` and sent as `price_estimated`
  in the request JSON. The request detail marks the net saving with "≈" and says why in its tooltip.
  Overview counts them (`estimated_requests`, on the full and the lean path) and the Preset card says how many of
  the window's requests they are. The totals and charts add such a figure like any other: they do not mark it. A row written before
  the column existed reads as exact.
- The library keeps each thread's state in memory. The proxy saves it to `<dashboard-db>.accountant-state.json`
  every minute and at a clean shutdown, and restores it at start, so a restart does not drop the active
  changes of a live session. The file holds hashes, sizes and ids, no message text, and is mode 0600.
  A file older than one hour is ignored (no cache entry lives longer), and so is a missing or unreadable
  one: the proxy then starts empty, as before. A crash (kill -9) loses up to one minute of state. A
  request that follows a lost state is priced as a first request and carries the estimate flag. The
  library also drops the least recently used threads past its own bound; that is not saved or restored.
  Two proxies must not share one dashboard database: they would write the same state file and the last writer wins.
- A thread that continues another by content (a rewind, a fork) is priced as a first request. The
  proxy passes the parent thread to the library, but the library does not read it, so the new thread
  does not start from a copy of the parent's cache state (accountant design §4.1).
- Every request is priced at the model's base rates. A model whose provider charges more above a prompt size
  (LiteLLM's `*_above_100k_tokens` and `*_above_200k_tokens` fields) is under-priced on those requests: its
  cost and its saving both use the base rate, so their ratio looks right and the dollars are low. Among Claude
  models, `claude-haiku-5-5` carries a 100k tier (input at 5x) on every listing, and Sonnet 4 and 4.5 carry a
  200k tier (2x) on their Bedrock, Vertex and Azure listings but not on the Anthropic API ids; Sonnet 5, Opus and
  Fable carry none. Models of other families carry tiers too.
  `MODEL_PRICES` sets one flat rate for a model, so it does not fix a tiered price.
- The cache lifetime comes from the provider rule the cold gate uses (`apply.CacheLifetime`): an
  exact expiry for Anthropic, a minimum for OpenAI's 30-minute entries. Where it is unknown, the
  library does not credit a keep-alive rescue after a gap past the TTL.
- The proxy measures the operations against the body it forwards, and starts the request record from
  the body it received. When a repair runs before the pipeline (`repairExpandErrors`,
  `AnswerStrayCalls`, a forced model), the two differ, the sum check fails (`check_sum`) and the
  event books no saving. A request with no usage, or a model with no price, books no saving and logs
  `unpriced`.
- A change the pipeline made that no operation can describe drops every operation of the request
  (`ops_dropped`): a message whose only change is a `cache_control` mark, a cache anchor written
  after a count-changing splice, and a splice that moves the later messages in a request the equal-count
  writeback handled. Such a request books no saving.
- A request whose response called `context_guru_expand` (more than one upstream round) books no
  saving (`no_result`): the usage is billed on the expanded body, which the library did not see.
- The thread state lives in memory. After a restart, and for a thread dropped by the library's cap
  of 10,000 threads (the oldest go first), the next request of the thread is a first request again.
- The figures are estimates. The library converts removed bytes to tokens with the request's own
  billed-token ratio. A figure that implies more removed tokens than the request had is rejected
  (`implausible_size`). The bound counts message tokens, so it leaves out operations on the tool
  section (removed or rewritten declarations).
- The cost and the provider's cache saving are not the library's.
- Rows stored before the switch (`priced_by` empty) keep their legacy figures. The Components tab,
  the summarize ledger and the replay cap value those rows with the legacy rule at read time, and
  sum the stored split on the rows the library priced. A window that spans the switch mixes the two.
- The savings tracker runs only when a dashboard is on. The pipeline records what each component
  changed on every request, with or without a dashboard (a scan of the changed messages). That cost
  is accepted.
- A request that asks for the one-hour tier and is billed the five-minute tier (a gateway that
  downgrades it) keeps the one-hour TTL in the facts, because the facts are written before the
  response. The library can then credit a rescue after a gap of 5 to 60 minutes that the provider
  did not honour. The keeper uses the billed tier.

## Tool inventory panel

*How much of what your agent carries in every request does it actually use?*

An agent declares its whole toolbox — every tool, every MCP server, every skill — on
**every** request, inside the cached prefix. The panel scans the request's `tools` array
and system-prompt skills listing (never the conversation itself), records the token
weight of each declaration, and tracks which ones actually got called. On real Claude
Code traffic, roughly 80% of declared tokens are typically never invoked in a session.

Reading it: `GET /api/tools` for the rollup, `GET /api/prompt` for one session's actual
prefix text. See [Config & routes reference](reference/reference.md) for both routes.

Two read-only routes serve the plugin's cost commands (`/context-guru:insights` and
`/context-guru:status`): `GET /api/spend/explain?since=&tz=` prices the window at the configured gateway
rates by token type, classifies full-prefix cache rewrites from the token columns
(`cache_write >= 0.8 x context`; the cause is inferred from the idle gap and message count),
and reports live session cache state and Context Guru's own ledger; `GET /api/tools/last-used`
returns when each tool or skill was last called (names and times only).

Two more serve the status line. `GET /api/stats?lean=1&session=<id>` is one session's totals from a
single indexed read, including `keepalive_pings` and `keepalive_ping_usd`; `lean=1` with no session
and no `since`/`until` is answered by the full handler. `GET /api/cachestate?session=<id>` is the
session's live cache state per thread (`threads[]`, `a:<agent id>` for a sub-agent), and its money
in one read: `money.main` for the main thread and `money.others` summed over every other thread,
each `saved_usd` of `baseline_usd`. `baseline_usd` is what those requests would have cost without
context-guru (the uncompacted baseline plus the cache-split and keep-alive savings); `saved_usd` is
that minus what was paid, context-guru's own model calls and pings included, so it never exceeds
`baseline_usd`. Each listed thread carries its own pair. A thread the
keep-alive still plans to ping also carries `pings_left`, `held_until_ms` and `next_ping_ms`: when its
cache goes cold if nothing else is sent (the last planned ping's start plus the cache lifetime), and when
the next planned ping fires. The mod hides its Ping button while that ping fires before the cache expires.

The session and each thread also carry `next_warm_usd` and `next_cold_usd`: what the next request
costs for the cached prefix if the cache is still warm (a cache read) or already cold (a rewrite;
the same number as `rewrite_usd`). Both are left out when the model has no price.

`POST /api/me/cache/ping` with `{"session": "<id>", "thread": "<id, optional>"}` is the Ping
button. It sends **one** keep-alive ping on that thread now, with the same request and the same
price as a scheduled ping. It answers `200` with `cache_read`, `cache_write`, `ping_usd`,
`rewrite_usd`, `saved_usd` (the write it avoided minus the ping) and `warm_until_ms`. Other answers:
`409 cold` (nothing left to keep warm), `409 not held` (keep-alive is off, so the proxy keeps no
body or key to ping with), `404 unknown session`, and `429 too soon` (one press per thread every
30 seconds). It reaches only the caller's own sessions. The caller is the same as for
`/api/adapt/*`: the dashboard cookie, the account's own proxy token, or a direct loopback request
to a single-tenant proxy.

`pingable` and `ping_reason` on `/api/cachestate` say whether a press can work. `pingable` is true only
while the proxy holds the thread. When it is false, `ping_reason` is one of:
`not opted in` (no keep-alive for the account and the request did not carry the header),
`below limits` (keep-alive is on, but the thread is under its minimum prefix or fails its gates),
`over budget` (one ping would cost more than the per-ping cost ceiling),
`too large` (the body is over 8 MiB), `unsupported` (extended thinking, or a route that cannot be pinged cheaply),
`done` (the thread's last response ended with a terminal tool such as `StructuredOutput`, so the agent is finished and the proxy sends no pings),
`unavailable` (the route is failing in a row, or the provider refused this credential),
`pings used` (the planned pings ran out and the proxy let go of the thread),
`stopped` (the proxy stopped the thread after a miss, a refused ping or a failure, and has let go of its copy),
`expired` (a manual-only hold ran out) and
`no entry` (nothing is known about the thread: the proxy restarted, or has seen no request on it since).
While a thread whose planned pings ran out is still held and its cache can still be warm, a press is allowed,
under the same cost ceiling and 30-second gap. A press is also allowed after a miss or a refused ping, while the proxy
still holds the copy: the refusal may not repeat, and no automatic ping is left to keep the cache warm.

**Ping with keep-alive off.** A request that carries `x-context-guru-ping: 1` is held for the button
even when the account has no automatic keep-alive. Nothing is pinged for it. The proxy keeps the body and
the key from the start of the LAST REAL request for at most 300 seconds on a 5-minute cache, and for at most
59 minutes on a 1-hour cache (the proxy reads the tier from the traffic), then releases and zeroes them. Only a
new request starts that time again. A press never does, so pressing
cannot keep a credential held. The usual limits still apply: 8 MiB per body, 512 threads, 128 MiB in all, and
the per-ping cost ceiling. One account may hold at most 16 such threads and 32 MiB, and they are released
before any automatic keep-alive hold, so a client that sends the header cannot starve other accounts. Claude Code sends the
header when `ANTHROPIC_CUSTOM_HEADERS` in its settings `env` contains `x-context-guru-ping: 1` (one `Name: Value`
per line when there are several headers). The proxy never forwards it upstream.

Its row is a keep-alive row with `manual_ping` set. `GET /api/stats` reports these rows as
`manual_pings` (`count`, `ping_usd`, `saved_usd` net of the pings' cost, and `writes_avoided`: the pings that found the cache warm). They are a savings
category of their own: they join `total_saved_usd`, and no cache strategy's ledger counts them.
The figure is an upper bound. A press needs the proxy to hold the thread for keep-alive, so with
automatic pings on it can overlap the keep-alive credit for the same avoided write. `saved_usd` is
`avoided - ping` summed over all presses, so a ping that missed shows as a loss; the per-press
`saved_usd` in the reply is never below 0, so the two need not match. A press is refused with
`409 over budget` when one ping would cost more than the per-ping ceiling, and `?tenant=` is ignored:
it pings only the caller's own account.

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

The tab sits next to **Cache strategy** — both answer "what is the prompt cache doing to my
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
cache read             cache_read     × cache_read_rate      (default 0.1× input; 0.05× Opus/Sonnet 5.5, 0.025× Fable/Mythos 5.1)
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
