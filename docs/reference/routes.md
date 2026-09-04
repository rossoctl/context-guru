# Routes & headers

The proxy serves both provider dialects on one port (default `:4000`).

## Routes

| Route | Purpose |
|---|---|
| `POST /openai/v1/chat/completions` | OpenAI chat dialect — runs the pipeline, forwards to the OpenAI upstream. |
| `POST /anthropic/v1/messages` | Anthropic Messages dialect — runs the pipeline, forwards to the Anthropic upstream. |
| `POST /anthropic/v1/messages/count_tokens` | Token counting, forwarded **verbatim** — the pipeline does not run. Absent this route a client falls back to counting context with *inference* requests, which a proxy sold on reducing spend must not cause. See the note below on what it costs. |
| `POST /compact` | Stateless compaction: run the pipeline and return the rewritten body, no upstream call. `?provider=anthropic` switches dialect; `?preset=` / `x-context-guru-pipeline` override the pipeline; `?cache=on\|off\|auto` overrides cache-awareness. |
| `GET /healthz` | Liveness check. |
| `GET /stats` | Savings rollups and health counters — see below. |
| `GET /metrics` | The same counters as Prometheus text, hand-serialised (`proxy/promexport.go`). Two families: in-process `cg_*` and database-backed `cg_tenant_*` — they answer different questions and [do not agree](#get-metrics-the-two-families-do-not-agree). In hosted mode it is gated exactly like `/stats`: loopback needs nothing, anything else needs the `METRICS_TOKEN` bearer. |
| `GET /expand?id=` | Recover an offloaded original by its `<<cg:HASH>>` id. Scoped to the caller's session. |
| `GET /favicon.ico` | `204`. Present so a browser's unprompted request does not fall through to the Bob catch-all below and answer `401`. |


### `count_tokens` answers about the ORIGINAL body, and that is deliberate

The route forwards the client's body unchanged, so the count it returns describes what the client
sent — not what context-guru will forward. That is the safe direction and it is the literal API
answer, but it has a cost worth stating plainly.

Returning the *compacted* count would be smaller and would look better. It would also be wrong in
the dangerous direction: the client would believe it has more room than it does, and because every
component fails open (a reverted component forwards the full body), the very next request could
send the uncompacted body and take a `400`. Over-reporting is recoverable; under-reporting is a
failed turn.

The cost: Claude Code uses this number to decide when to run **its own** compaction, so a routed
session self-compacts earlier than it needs to — paying for a summarization call and discarding
transcript the proxy was already handling. On a measured body: `115,933` tokens reported,
`32,802` actually forwarded. The count also excludes any tool declaration the proxy adds.

Two upstream caveats: on an implicit prefix-cache backend the numbers are unaffected because
`cachesplit` is a no-op there, and at least one LiteLLM-fronted gateway answers this endpoint with
an implausible count (`13` for a body whose system prompt alone is ~7,929 tokens) — the same answer
it gives when called directly, so the undercount is upstream's, but it means the route's
cheap-budgeting justification does not hold on that upstream.

### Bob (BobShell) gateway routes

Mounted only when `--bob-upstream` / `BOB_UPSTREAM` is set, or in hosted mode. Without
either, an unknown path still 404s exactly as before.

| Route | Purpose |
|---|---|
| `POST /inference/v1/chat/completions` | Bob's model call. OpenAI-compatible, so it runs the pipeline like any OpenAI chat request. |
| `/` (catch-all) | Verbatim passthrough for Bob's control-plane calls (`/admin/v1/profile`, `/inference/v1/model/info`, …), which must not be rewritten or the CLI cannot boot. Less specific than every route above, so it only receives what nothing else matched. |

!!! note "`POST /compact` (compaction-service mode)"
    The [llm-d compaction service example](../examples/llm-d-service.md) uses this route with
    the store disabled and `marker_mode: off`, so the returned body is clean, marker-free and
    directly usable. See [Quickstart: Compaction service](../get-started/quickstart-compaction.md).

## `GET /stats`

Fields are only ever **added** to this payload, so a consumer that reads by key keeps working.
Two different consumers make that a hard rule: `deploy/harbor/*.py` reads `runs`, `acted` and
`saved_tokens` off the per-component objects (`measure.py` by direct index, so a removal is a
`KeyError`), and `/metrics` re-publishes much of the rest as `cg_*` series that off-repo alert
rules and dashboards are written against — where a rename breaks monitoring *silently*. The tables below group the `Snapshot` struct in
`metrics/metrics.go`; that struct is the authority, and it grows.

### Savings

| Field | Meaning |
|---|---|
| `requests` | Enforced requests aggregated. |
| `tokens_before` / `tokens_after` | Content-token totals before and after the pipeline. |
| `saved_tokens` / `savings_pct` | `before − after`, and the **token-weighted** ratio (`Σ saved / Σ before`), not a mean of per-request percentages. |
| `wasted_tokens` | Content offloaded then re-served via `expand` — a premature offload. |
| `bounces` | How many expand events produced that waste. |
| `adjusted_saved` | `saved − wasted`. May be negative. |
| `components` | Per-component rollup (see below). |
| `top_passthrough` | Components that ran but never *changed* a request — dead weight. A component that mutated without saving content tokens (`cachesplit`, `cacheinject`) is **not** listed here. |
| `adjudicate_stray` | Times the **agent** called `context_guru_adjudicate`, the verdict tool the proxy injects for [`extract_llm_sweep`](../components/extract_llm_sweep.md) and tells the model not to call. Counted whether the call was answered on the response path or repaired on the next request. Most cost the agent nothing, because the response path answers them in band before the client is written to; one co-called with a *client* tool in the same assistant turn costs one turn, because the loop hands that round over whole and the next request's repair fixes it. Either way this is a *description-health* number rather than an error: non-zero means the "do not call this yourself" wording has stopped working. Measured 0 over three benchmark passes. Also exported as `cg_adjudicate_stray_total`. |
| `top_discarded` | Components whose changes the **writeback layer threw away** at least once. Any entry needs investigating: the component ran, mutated, and had no effect on the wire. |

Per-component (`components.<name>` and `potential_components.<name>`):

| Field | Meaning |
|---|---|
| `runs` | Times the component ran. |
| `acted` | Runs that actually saved tokens. **Includes free replays** — see the two fields below before reading this as work the component paid for. |
| `acted_fresh` | Runs that saved tokens by doing new work. Every deterministic component's `acted` is entirely this. |
| `acted_replay` | Runs whose whole saving came from **replaying a decision already frozen** — the same bytes re-spliced, no model call, no spend. `acted_fresh + acted_replay == acted`. A component with `acted: 239` and `acted_replay: 239` made no calls at all; one with the same `acted` and `acted_replay: 0` paid for every one. |
| `mutated` | Runs that changed the request at all — may save 0 content tokens. |
| `reverted` | Runs the pipeline rolled back (error, panic, or grew the request). |
| `saved_tokens` | **Cumulative** — re-counted every turn the compaction re-appears. |
| `saved_tokens_unique` | **Unique** — each distinct compaction counted once, deduped by content key. |
| `overcount_ratio` | `saved_tokens / saved_tokens_unique`. ~1.0 is honest; a large value means the cumulative figure is inflated by the agent re-sending history verbatim. |
| `duration_ms` | Cumulative wall time this component spent on the hot path. |
| `discarded_changes` | Changes the writeback layer threw away, attributed back to this component. |
| `gates` | Rejection histogram, gate name → candidates that gate declined. Omitted when empty. It is what turns `acted: 0` into a diagnosis: the component saw no candidates, or saw them and a named guard refused. |

### Extraction economics (`extract`)

Present only when an extraction component has recorded something. See
[`extract_llm`](../components/extract_llm.md) for the field meanings.

!!! warning "The `extract` block is a SUM across components, not one component's figures"
    `extract_llm` and `extract_llm_sweep` both write these counters, and the enclosing block is
    their **total**. It reads like one component's numbers and is not: a measured run attributed
    101 calls at 59,009 ms and a net value of −$1.162 to `extract_llm` when that component had
    made no call at all and the figures were the sweep's. **Read `extract.by_component`** for any
    per-component cost, latency or call claim. The enclosing keys are kept for `/metrics`
    compatibility (`cg_extract_*`), not because they are the figure to quote.

| Field | Meaning |
|---|---|
| `by_component` | The same fields again, keyed by the component that recorded them. The only per-component-safe figures here. Omitted when nothing recorded. |
| `cost_source` | Where `extraction_cost_usd` came from, because `$0` and *no evidence* are the same number and the opposite claim. `component` = every call priced itself at the rates of the model it called (trust it). `host_total` = the host's process-global cheap-model spend, a **superset** that also carries `summarize` and `agentdiet` and prices everything through one card. `partial` = some calls priced themselves and some did not, so the total is a **floor**. `unpriced` = this row made calls and priced none, so nothing is known about what it spent. `none` = no calls, no spend; `0` is true. |
| `net_value_usd` | `null` when the spend behind it is not known (`cost_source: unpriced`). The aggregate is never null. |
| `unpriced_components` | Which components' calls priced nothing, so `partial` and `host_total` say what the total is **short of** rather than only that it is incomplete. Aggregate only; omitted when every call priced itself. |

!!! warning "Cumulative is not unique"
    `saved_tokens` counts the same compaction again on every later turn that carries it. A
    figure like "4.8M tokens saved" is a *cumulative* total; the unique figures behind the
    Terminal-Bench and SWE-bench runs are **234,119 tokens behind 103 markers** and
    **15,457 behind 29** — 21× and 8× smaller respectively. Quote
    `saved_tokens_unique`, and check `overcount_ratio` before citing either.

### Context-guru's own cost

| Field | Meaning |
|---|---|
| `llm_calls`, `llm_input_tokens`, `llm_output_tokens` | The cheap-model spend context-guru's *own* components incurred (`extract_llm`, `summarize`). Separate from the agent's spend; priced externally. |
| `llm_timeouts` | Model calls abandoned on the deadline. **Read this before citing any savings figure**: an abandoned call leaves the output verbatim, which is correct fail-open behaviour but reports nothing — so an arm that quietly stops compacting under load looks like an arm that got faster. A non-zero count means that arm's savings are an *undercount*, not a measurement. |
| `llm_errors` | Model calls that failed for any other reason. Same reading. |
| `llm_call_timeout_ms` | The per-call budget actually in force (`CONTEXT_GURU_LLM_TIMEOUT`, default 90 s). Read it beside `llm_timeouts`: timeouts against a small budget mean the budget is too small for the server's current load. |
| `extract` | `extract_llm`'s own economics, when it ran: calls, calls avoided/suppressed, extraction cost, gross and **net** value in dollars, and a `reasons` histogram with its `top_reason`. Omitted when the component never ran. The honest headline for the one component that spends money to save money. |
| `cg_added_ms_avg` | Mean ms context-guru added per request (normalize + pipeline + writeback). |
| `upstream_ms_avg` | Mean provider round-trip on the active path. |
| `upstream_ms_avg_bypassed` | Same on `x-context-guru-bypass` requests — the baseline for a with/without latency comparison. |

### Provider-billed token tiers, and the honest ratios

Summed from response `usage` blocks, so all four are zero against an upstream that
reports no usage.

| Field | Meaning |
|---|---|
| `fresh_input_tokens` · `cache_read_tokens` · `cache_write_tokens` · `output_tokens` | The four billed tiers. |
| `attempted_tokens` | What compaction was **allowed** to touch this window — the uncached tail when cache-aware. |
| `frozen_tokens` | What cache-awareness deliberately left alone. Its benefit is the cache reads that stayed cheap; this is its cost. |
| `savings_pct_attempted` | `saved ÷ attempted`. The ratio to quote: `savings_pct`'s whole-request denominator recounts the transcript every turn and trends to ~0% on a long session. `0` when nothing was attempted. |
| `savings_pct_new_input` | `saved ÷ (fresh + cache_write + saved)` — savings as a fraction of what would newly have entered the provider. Reported as `0`, **never 100**, when the provider reported no usage: savings must not be divided by themselves. |

### SSE streaming health

Buffering a stream is the one thing that stops it being a stream, so it is counted. All four
fields count **once per client request**, not per upstream round: a request that drove several
expand rounds waited for all of them.

| Field | Meaning |
|---|---|
| `sse_streamed` | Streaming responses passed straight through — the fast path. |
| `sse_buffered` | Streaming responses read in full before the client saw a byte, because the response **opened with a call to the expand tool** and had to be intercepted. |
| `sse_buffered_pct` | `buffered / (streamed + buffered) × 100`. |
| `sse_ttfb_ms_avg` | Real time-to-first-byte, streamed-through requests only. |
| `sse_ttfb_ms_avg_buffered` | Time-to-**last**-byte by construction — a buffered response is read in full before the client is written to, so its first byte cannot precede the buffer completing. Read it as "what buffering cost these requests", **not** as a latency comparable to `sse_ttfb_ms_avg`. |
| `sse_expand_after_stream` | Streamed responses that named the expand tool anyway — a call the loop would have intercepted had the whole stream been buffered. The bounded peek's price, measured. An **upper** bound: a model that writes the tool's name in prose is counted too. |

A high `sse_buffered_pct` on traffic that never expands is the regression to watch. It has had
two causes. The marker check once matched the expand tool's own description, so **every**
stream was buffered (issue #26). After that, buffering was chosen whenever the expand tool was
*advertised* — which is from the first offload onward — so a third of all responses were
buffered and waited ~21 s extra for a first byte. Both are fixed: a streaming response is now
inspected with a **bounded peek** at its first content block, and only a response that opens
with the expand call is withheld.

`sse_expand_after_stream` is the counter that keeps that trade honest. If it is a meaningful
fraction of `sse_streamed`, the peek is letting real expand calls through and the fix is to
splice the continuation into the live stream instead (see `proxy/ssepeek.go`).

### Freeze-replay health

The cache-**write** cost line. A frozen decision replayed (`frozen_hits`) keeps an
already-cached message byte-identical. One the store **drops** would flip that message's
representation inside the provider's cached prefix and force the whole suffix to be re-written
at 11.5× the read price — unless it is re-derived.

| Field | Meaning |
|---|---|
| `frozen_hits` | Replay lookups that found a stored decision and re-sent the same bytes. |
| `frozen_misses` | Replay lookups that found nothing. **Dominated by the ordinary "never frozen yet" case** — it is a lookup counter, not an error counter. Read it beside `frozen_dropped`, not instead of it. |
| `frozen_dropped` | Stored decisions the store actually **lost** (TTL expiry or the pin cap). Counted per drop *event*. |
| `frozen_repaired` | Dropped decisions re-derived, so a replay lands again. Only `mask` and `failed_run` qualify — `extract_llm` is deliberately excluded (its replacement is a *sampled* model output, so re-deriving could splice different bytes into the cached prefix). |
| `frozen_flips` | `frozen_dropped − frozen_repaired` — outstanding losses, i.e. drops that plausibly cost a suffix cache-write. **Should be 0.** |

A healthy long-horizon run shows `frozen_hits` climbing with turn count and `frozen_dropped` at
0; a rising `frozen_dropped` means decisions are dying mid-session (TTL too short for the task,
or the entry cap too small for the session's working set).

### Rewind reserve (reversibility health)

The store holds the **originals** behind `<<cg:HASH>>` markers in a reserve of its own that LRU
pressure cannot evict, bounded by two budgets: half the entry cap, and `stash_max_bytes`. Before
#187 those payloads shared the cache's evictable half with per-removal bookkeeping while the
*decisions* naming them were pinned, so the more a configuration removed the more of its own
reversibility it destroyed, silently.

| Field | Meaning |
|---|---|
| `stash_live` / `stash_capacity` | Payloads held now, and the reserve's entry cap (`max_entries / 2`). `live` approaching `capacity` is the warning. |
| `stash_bytes` / `stash_max_bytes` | What those payloads cost, and the byte budget (`stash_max_bytes`). Entries are a poor proxy for memory here — a payload is a whole tool output, every other exempt entry is a marker line — so read both pairs to see **which** budget bound. |
| `stash_refused` | Removals **declined** because the reserve was full. The content was left verbatim and nothing became irreversible — raise `max_entries` or `stash_max_bytes`. |
| `stash_missing` | Marker replays that found **no payload** behind them: a dangling `<<cg:HASH>>` went upstream. This one *is* a broken promise — raise `ttl_seconds`. |
| `stash_expired` | Payloads reclaimed by the TTL. With `stash_missing`, the remaining way an outstanding marker stops resolving — raise `ttl_seconds`. |

| Field | Meaning |
|---|---|
| `expand_prefix_flips` | Turns where an **established** compaction was abandoned because the agent had expanded that content, so the original went upstream in full at its cached position — a suffix cache-write attributable to expansion, at ~11.5× a read. Deliberate: re-compacting would loop the agent into another expand, and one cache-write is cheaper than an unbounded loop. **Per turn per message**, not per distinct content — only the first is a real cache-write. See [what an expand costs](../how-to/recover-context.md#what-an-expand-costs-across-turns). |

`stash_refused` is the **leading** indicator for `expand_unresolved_missing`: that counter cannot
move until the agent happens to call `expand`, so a proxy that had stopped being able to promise
reversibility read as perfectly healthy until one did. This one moves when the budget binds.

**Do not read `stash_refused` and `stash_missing` as the same thing.** They are opposite outcomes,
and they were one counter until the #188 review pointed out that made the safe case
indistinguishable from the dangerous one. A refusal means a removal did **not** happen — the
content went upstream verbatim, and the cost is tokens. A missing payload means a marker went out
with nothing behind it, which is the failure #187 was about. Alert on `stash_missing`; watch
`stash_refused`. `stash_missing` also grows with **turn count** rather than with distinct dangling
markers: a payload that has gone cannot be restored (the replayed bytes must stay byte-identical to
the turn that created them), so every later turn re-reports it for every affected message.

### cmdfilter attribution

| Field | Meaning |
|---|---|
| `cmdfilter_families` | Per command family (`builds` / `tests` / `iac` / `pkg` / `net` / `other`): `acts`, `saved_tokens`, `saved_tokens_unique`. |
| `cmdfilter_filters` | The same, per individual filter — which filters actually earn their place. |
| `cmdfilter_selector_misses` | Output shapes that matched **no** filter, frequency-ranked. The backlog of filters worth writing. Bounded at 200 distinct selectors, first-seen wins. |

### Operating mode

| Field | Meaning |
|---|---|
| `mode` | The configured operating mode: `sync` \| `observe`. |
| `sync_enforced` | Requests whose forwarded body context-guru actually shaped. **0 in observe mode by construction** — the machine-readable form of "context-guru did not modify requests". |

Observe mode's results are **hypotheticals** and live under keys that never collide with an
enforced metric, so a consumer cannot sum one into a real saving even by accident. All zero
outside observe mode.

| Field | Meaning |
|---|---|
| `observe_notice` | The banner. Present whenever hypotheticals are reported. |
| `observe_hypothetical_requests` | Requests observed off-path. |
| `actual_baseline_tokens` | What the agent really sent. Actual, not hypothetical. |
| `projected_optimized_tokens` | What it would have sent under this pipeline. |
| `potential_saved_tokens` / `potential_savings_pct` | The difference, and its ratio. |
| `potential_components` | Per-component hypothetical contributions (same shape as `components`). |
| `potential_overhead_ms_avg` | What compaction *would* have added per request — measured off-path, so it is what `sync` would cost, not what `observe` costs. |
| `observe_llm_notice` | Warns that `llm_calls` / `llm_*_tokens` in observe mode are the cost of **measuring** off-path, not of enforcing. The spend is real (not hypothetical), so it stays where cost tooling reads it, labelled. |
| `observe_queue` | The off-path pool's own health: `queued` · `pending` · `processed` · `dropped` · `errors`. Omitted when no pool is running, so a sync-only deployment shows no phantom queue. `dropped` is the field that changes a reader's conclusion — a drop is an observation given up, so a rising count means the `potential_*` figures **understate** what compaction would have saved. |

`cg_added_ms_avg` and the `llm_*` fields deliberately **do** accumulate in observe mode: they
are real measurements and real spend. Zeroing them would hide a true number rather than protect
anyone. See [Operating modes](../how-to/operating-modes.md).
## `GET /metrics` — the two families do not agree

`/metrics` serves two families, and a reader comparing either against the dashboard needs
to know which one they are holding. The HELP text of every in-process series now says so
itself, because HELP travels with the metric into every scraper, explorer and panel
tooltip — a note only in the docs is a note the person reading the panel does not see:

- **`cg_*`** comes from the in-memory aggregator (`metrics.Aggregator`) — the same snapshot
  `/stats` reports. It is **counted in this process since it started** and **summed over
  every tenant**. It restarts from 0 with the process, which `rate()` handles.
- **`cg_tenant_*`** comes from the dashboard database — **persistent** across restarts and
  **scoped to one tenant**. Cached for just under a scrape interval, because a SQL query
  per scrape per tenant would make observability the load.

So `cg_*` **will not equal the dashboard's figure for the same window**, and that is
correct behaviour on both sides rather than a bug in either. Observed live: `/metrics` read
24 requests / 28,644 tokens-before while the dashboard read 26 / 28,656 — a restart and a
tenant scope, not a counting error. **For the persistent, per-tenant numbers, use
`cg_tenant_*`.** The `cg_*` series are deliberately not re-sourced from the database: their
meaning is "what this process did", and `/stats` — which the harnesses read — reports the
same snapshot.

!!! warning "Four `cg_tenant_*` names end in `_total` and are GAUGES"
    `cg_tenant_requests_total`, `cg_tenant_tokens_total`,
    `cg_tenant_saved_tokens_unique_total` and `cg_tenant_billed_tokens_total` are re-read
    from the store for the **current calendar month**. They reset at the month boundary and
    they *fall* mid-month as request rows migrate to cold storage, so they are declared
    `# TYPE gauge` despite the suffix (the name is kept because dashboards and scrapes
    already reference it). **Never wrap them in `rate()` or `increase()`** — both read a
    fall as a counter reset and extrapolate a spike at exactly the moment the value went
    down. For a per-second rate use the process-wide `cg_*` counters, which are monotonic.

### Reading `acted: 0`

Three families answer "this component ran and did nothing — is it broken?":

- `cg_component_runs_total{outcome="ran|mutated|acted|discarded|reverted"}`. The outcomes
  are **nested, not disjoint**: `acted` ⊆ `mutated` ⊆ `ran`. `acted` means content tokens
  were removed; `mutated` means the request changed at all. A component can be
  mutated-never-acted **by design** — `cachesplit` moves tokens out of the hashed prefix
  rather than removing any, so `acted/ran` reads 0% for the component with the largest
  measured cost effect in the pipeline. Any "is it doing anything?" query wants `mutated`.
  `discarded` is the odd one out: it counts *changes* the writeback layer threw away, not
  runs.
- `cg_component_gate_declines_total{component,gate}` — how many candidates each named gate
  turned away. This is what separates "the guard is misfiring" from "there was genuinely
  nothing to do": `toon` declining 14,675 of 18,288 candidates as
  `not_uniform_object_array` is the component working. Cardinality is bounded by code
  (gate names are constants), not by traffic. **Declines only** — see the next entry.
- `cg_component_events_total{component,event}` — things a component *did*, as against
  candidates it turned away: a replay served (`reapplied_same_session`), an output removed
  (`sweep_dropped`), an inventory offered (`sweep_offered`), a candidate reached past the
  cached boundary (`sweep_candidate_at_depth`). Split out because these used to land in
  `..._gate_declines_total`, so that series **rose as a component worked better** and summing
  it to gauge whether the pipeline was doing anything gave the wrong sign. A name appears in
  one series or the other, never both — the component has to say which happened.
- `cg_extract_*` — extraction economics, the one component that spends money:
  `cg_extract_calls_total{outcome="made|avoided|suppressed"}`, `cg_extract_cost_usd`,
  `cg_extract_net_value_usd`, `cg_extract_latency_ms`, and
  `cg_extract_gate_declines_total{reason}`. **`cg_extract_net_value_usd` going negative is
  the alert**: it means the component's own saved tokens are worth less than its own model
  spend, which is a state gross token counts cannot show (measured live at −$0.7085).

## Dashboard routes (`--dashboard`)

Present only when the [dashboard](../dashboard.md) is enabled. Without the flag the route
table above is unchanged and every path below returns 404.

| Route | Purpose |
|---|---|
| `GET /dashboard/` | The embedded single-page UI (HTML + CSS + JS from `go:embed`; no CDN, no build step). `/dashboard` redirects here. |
| `GET /api/stats` | Overview aggregates: token tiers, costs, the four labelled savings denominators, the honest-savings waterfall, safety-mechanism costs, and the accounting / cache-miss / uncompressed-reason distributions. Accepts every filter parameter below. |
| `GET /api/series?bucket=<ms>` | Time series, bucketed **at query time** (no rollup tables). One object per bucket with tokens, the four billed tiers, costs, mean latencies, restorations and cache misses. |
| `GET /api/requests` | Paginated request list. Server-side filters + **keyset** pagination (`before=<id>`, not an offset). Returns `{requests, next_cursor, total}`. |
| `GET /api/requests/{id}` | One request with its per-component rows and, for a permitted caller, the before/after content the diff view renders. Carries [`content_captured` and `capture_blocked_by`](#why-a-transcript-panel-is-empty) alongside `content_visible`, `content_cap_bytes` and `content_archived`. |
| `GET /api/sessions` | Session list with per-session rollups (`limit` / `offset`). |
| `GET /api/sessions/{session}/transcript` | One session's before/after content for the compaction-diff view, oldest first, with the component rows per request. Reports one of the [nine transcript states](../dashboard.md#nine-states-because-empty-is-nine-different-facts), plus [`content_captured` and `capture_blocked_by`](#why-a-transcript-panel-is-empty). **Lazy**: it touches the local database only and reports `cold` unless `?fetch=1`, so opening the view never costs an rclone round trip. |
| `GET /api/whoami` | The UI's mode probe: `{hosted, authenticated}`, plus the account, tokens, base URL and registration mode when signed in. Answers **200 in every case** — including "not signed in" — so a question with a legitimate negative answer is not asked with an error. Mounted whenever the dashboard is. |
| `GET /api/archive` | The local, permanent cold-storage index (`limit`). Also reports `remote` (the **configured** destination name, `""` when none) and `reachable` as separate fields, so "not configured" and "configured but down right now" cannot be rendered as the same thing. |
| `GET /api/archive/{session}` | Fetches one session back out of cold storage. The only route that does a network round trip, and **read-only** — it does not reinsert the rows. `404` for "never archived", `503` for "the remote is down". |
| `GET /api/components` | Per-component economics: runs, acted, reverted, unique/gross savings, `overcount_ratio`, total and mean own-latency, errors. |
| `GET /api/breakdown?dim=<name>` | Requests, tokens and **spent vs saved** grouped by one dimension: `model`, `provider`, `agent`, `preset`, `mode`, `reasoning_effort`, `thinking_mode`, `stop_reason`, `tool_choice`, `cache_miss_reason`, `cache_breakpoints`, `stream`. `spent_usd` is billed cost plus context-guru's own model spend; `saved_usd` is the baseline counterfactual minus that. `incomplete_rows` counts rows the provider reported no usage for, so a group with no priced rows renders as **unknown** rather than as zero. The dimension is an **allowlist**: an unknown one is a `400` naming the valid set, never a chart of some other dimension's numbers. Defaults to `model`. Per-**day** usage bars need no route of their own — they are `/api/series?bucket=86400000`, since bucketing happens in SQL at query time. |
| `GET /api/facets` | The distinct values present for each filter dimension, so a UI shows only what the data contains. |
| `GET /api/config` | **This proxy process's** effective (resolved, key-allowlisted) configuration, wrapped in a scope envelope — see [below](#the-config-route-serves-the-servers-configuration-not-yours). Access-gated; **manager-only** in hosted mode. |
| `GET /api/benchmarks` | Ingested harness runs with per-arm aggregates. `?refresh=1` re-scans the configured run directories. **Manager-only** in hosted mode — the runs are the operator's own eval history, and `?refresh=1` walks the filesystem and inserts rows. |
| `GET /api/benchmarks/{id}/tasks` | Per-task rows for a run (`?arm=<name>` to restrict). **Manager-only** in hosted mode. |
| `GET /api/capture` | The capture pipeline's own health, **including its drop count**. The counters are process-wide, so in hosted mode a non-manager gets `mode` alone (with a `description` that says so, rather than one describing counters that are not in the payload). |
| `GET /api/events` | SSE stream of captured requests (summary rows only — never content). Honors `Last-Event-ID` (or `?last_event_id=`) so a reconnect backfills the gap. |
| `GET /api/kvcache` | The KV-cache analysis: summary cards, the idle histogram, the survival curve, the grouped views (observed TTL, user, model, time-of-day band), the price list and the **coverage statement**. One payload, so every figure shares its denominator. `scanned` / `total` / `truncated` say whether the read was capped (`kvCacheMaxRows`, 200,000) — a silent cap would read as "this is your whole history". |
| `GET /api/kvcache/rows` | Accepts `ttl=` — **page-local, not a shared filter**: `ephemeral_5m` \| `ephemeral_1h` \| `none` (carried no `cache_control`) \| `unrecorded` (cached something at a tier nothing recorded). It selects on the RECONSTRUCTED tier, so it matches a 1-hour request whose tier was deduced from the provider's own 1h write counter as well as one that recorded it. It was briefly a `Filter` dimension too, matching the raw column; the two vocabularies were silently intersected and the reconstructed-only groups drilled down to nothing. One reader now. One page of the derived per-request dataset: the billed tiers, the reconstructed TTL and how well it is known, the next request in the same conversation, the idle time (**`null`**, never `0`, on a conversation's last request), and the links back to the request drawer and the session diff. Sortable on thirteen columns and paged **on the server** (`sort`, `dir`, `limit`, `offset`) — the derivation runs over the whole scoped window inside the query, so a row on page 7 carries the same successor it would on page 1. |
| `GET /api/kvcache/simulate` | Replays the window under every requested strategy (`strategy=` repeated or comma-joined) and scores each against one `baseline=`. Returns a result and a saving per arm, the arm registry the picker is built from, and the assumptions. Savings are **never clamped** and a percentage against a zero baseline is `percent_known:false`, not `0`. An unknown arm is named in `unknown` rather than silently replaced; an unknown baseline is a `400`, because every percentage is divided by it. |
| `GET /api/kvcache/pricing` | The editable rate table for the models in this window, plus what each rate comes to on the window's own **median** billed prefix. Its own route so editing a rate re-prices without re-running the replay. |

### The config route serves the SERVER's configuration, not yours

**Breaking change.** The route used to return the redacted configuration map as the whole
body. It now returns that map inside a scope envelope, so a reader can tell *whose*
configuration is on screen:

```json
{
  "scope": "server",
  "config": { "preset": "codesmart", "pipeline": ["format", "…"], "…": "…" },
  "description": "The configuration this proxy resolved at startup. …"
}
```

`scope` is always `"server"`; `config` is the payload that used to be the entire response.
A client that read the old shape must move down one level into `config`.

`description` differs by deployment mode, because the sentence a reader needs differs:

| Mode | What the description says |
|---|---|
| Single-tenant | The configuration this proxy resolved at startup, and — since there are no accounts here — also the one every request ran through, subject to per-request overrides (`/compact?preset=`, `x-context-guru-pipeline`). |
| Hosted | The server-wide default this process resolved at startup, and that it is **not necessarily what compacted the caller's traffic**: an account that stores its own configuration runs that one instead. |

That distinction is the reason the envelope exists. A live hosted dashboard reported preset
`codesmart` with `extract_llm` in the pipeline while every request that day ran preset
`custom` and `extract_llm` never fired once — the tenant was following its own
configuration, and nothing on the page said whose configuration was being shown.

Where to look instead, in hosted mode:

- **A tenant's own configuration** — [`GET /api/me`](#control-plane-routes-hosted-mode-only),
  which reports both the stored and the effective document.
- **The default a tracking tenant inherits** — `/api/options`'s `default_config`. This is
  the `tenant.DefaultConfigYAML` constant, *not* the process's `--preset` / `--config`, so
  in hosted mode it is a different document from the one `/api/config` shows.
- **What actually ran on one request** — the request row itself: its preset, its mode, and
  its components in the order they ran.

### Why a transcript panel is empty

`/api/requests/{id}` and `/api/sessions/{session}/transcript` both report two fields about
content capture, and they answer different questions:

| Field | Meaning |
|---|---|
| `content_captured` | The **effective** decision for the tenant whose rows these are: the operator's service-wide gate **and** that tenant's own consent, both read per request. It is not the process flag. |
| `capture_blocked_by` | `"operator"` \| `"tenant"` \| `""` — which party's gate is the closed one, so a message can name someone who can actually act. `""` when nothing is blocking, and `""` for a manager viewing the whole service, who is not a party whose consent there is to report. |

Capture therefore needs **both** yeses, and only the operator's is off by default — a hosted
account is registered with its own `capture_content` already on. So the consequence bites in
exactly one direction: **a tenant whose own switch is on still gets nothing until the operator
sets `--dashboard-content` / `DASHBOARD_CONTENT`.** In that state `content_captured` is `false` and
`capture_blocked_by` is `"operator"` — which is the whole point of the field, because the
dashboard used to read the process flag and tell such a user to switch on a setting they had
already switched on. In single-tenant mode there is no second gate, so the operator flag is
the entire decision and `capture_blocked_by` is `"operator"` or `""`.

### Filter parameters

Accepted by `/api/stats`, `/api/series`, `/api/requests`, `/api/sessions`,
`/api/components`, `/api/breakdown` and `/api/facets`. All filtering happens in SQL,
server-side.

| Parameter | Matches |
|---|---|
| `since` / `until` | Epoch-**millisecond** bounds (`since` inclusive, `until` exclusive). |
| `session` · `model` · `provider` · `agent` · `preset` · `mode` | Exact match. |
| `component` | Requests on which that component ran. |
| `reason` | The uncompressed-reason bucket (`bypassed`, `below_trigger`, `cache_frozen`, `found_nothing`, `reverted`, `no_messages`), or `compacted` for requests we did compact. |
| `accounting` | `complete` \| `partial` \| `missing`. |
| `effort` · `thinking` · `stop_reason` | Captured request metadata: the reasoning effort (`low`…`max`) and thinking mode (`adaptive` \| `enabled` \| `disabled`) the client asked for, and the provider's terminal stop reason. Exact match; the drill-down from a `/api/breakdown` bar into the rows behind it. |
| `q` | Free-text match against session id, model and agent. |
| `limit` · `before` · `offset` | Page size; keyset cursor (`/api/requests`); offset (`/api/sessions`). |

### Access gating

| Surface | Who can see it |
|---|---|
| Aggregates, series, session/component rollups, per-request **metrics** | anyone who can reach the port |
| Per-request **content** (`/api/requests/{id}` content, the diff view) | loopback, or a `--dashboard-trusted-cidrs` entry |
| **Prompt text** — each declaration's schema and the system prompt (`/api/prompt` `regions[].text`) | loopback, or a `--dashboard-trusted-cidrs` entry |
| The **server's** effective configuration (`/api/config`) | loopback, or a trusted CIDR |

Aggregates stay open on purpose: a proxy bound to `0.0.0.0` should still report its own
numbers. Content is gated because a transcript can carry a user's source code. An
untrusted caller still gets the metrics row, plus `content_visible: false` so the UI can
say *why* the panel is empty rather than implying nothing changed. `/api/prompt` behaves the
same way: an untrusted caller keeps every region's token weight, share and the coverage
count, and loses only `text` / `has_text`.

`TestNoRouteServesContentTextFromAnUntrustedAddress` walks this mounted table and asserts
the rule for every route, so a new content surface cannot be added on the aggregates' side
of the line — which is exactly how `/api/prompt` first shipped.

In **hosted** mode the gate is identity rather than address, and it is declared per route
in the mounted table itself (`dash/api.go`) so a newly added route cannot skip the
question — a test walks the table and asserts every route's declared scope. Three classes:

| Scope | Routes |
|---|---|
| Public — no principal at all | `/dashboard/`, `/api/whoami` |
| Tenant — that principal's data only, and a manager may widen with `?tenant=<id>` or `?tenant=*` | every other `/api/*` data route |
| Manager — server-wide or process-wide facts that are nobody's tenant data | `/api/config`, `/api/benchmarks`, `/api/benchmarks/{id}/tasks`, and `/api/capture`'s counters |

A manager reading another tenant sees their **metrics and their transcripts** — an
explicit owner decision. `/api/requests/{id}`, `/api/sessions/{session}/transcript` and
`/api/archive/{session}` all serve content to a manager and to the row's owner, and to
nobody else.

## Control-plane routes (hosted mode only)

Mounted only when `--upstreams` is set. Without it the tenancy layer is nil and none of
these exist — `/api/me` 404s, which is how the UI detects a single-tenant proxy.
See [Hosted service](../hosted.md).

| Route | Purpose |
|---|---|
| `POST /api/register` | **The only route that creates an account.** Gated by `CG_REGISTER` (`closed` default / `invite` + `CG_REGISTER_CODE` / `open`). Returns the token once. |
| `POST /api/login` · `POST /api/logout` | Exchange a token for a session cookie, and drop it. |
| `GET /api/me` · `PUT /api/me` | The caller's own account and configuration. |
| `POST /api/me/tokens` · `DELETE /api/me/tokens/{prefix}` | Mint and revoke the caller's own tokens. |
| `POST /api/me/agent-key` · `DELETE /api/me/agent-key` | Bind (and unbind) the **sha256** of the caller's own provider key, so an agent that can send no `x-context-guru-token` header is still identified. The key is sent in `Authorization` / `x-api-key` — the same slot the agent uses — and is hashed on arrival: never stored, echoed or logged. Two refusals, both the caller's to fix: a key under **20 characters** is a `400`, because identity here *is* the digest and a guessable key is a guessable account; a digest already bound to **another** account is a `403` and is never moved, because binding a digest someone else had bound used to transfer their traffic — and, with `capture_content` on, their captured transcripts — to whoever bound it. A real transfer is the owner's `DELETE` followed by a fresh bind. `DELETE` drops all of the caller's, because a digest is not displayable and "which one" is not answerable. |
| `GET /api/me/audit` | The caller's configuration-change history. |
| `GET /api/options` | Which upstreams the operator allows, and which presets and components are registered — so the settings page cannot offer something the server would reject. Names no base URL and no credential env var. |
| `POST /api/me/password` | Change the caller's own password. The **current** one is required — a stolen session cookie must not convert into permanent ownership of the account — and every *other* signed-in machine is signed out. |
| `POST /api/password-reset` · `POST /api/password-reset/verify` | Self-service recovery, unauthenticated by necessity: the person who needs it cannot sign in. Phase one mails a code and answers **identically** whether or not the address has an account; phase two spends the code together with the new password. The code's purpose is fixed by the route, so a login code cannot be spent here and a reset code is not a second factor. |
| `GET /api/tenants` | Manager only: the roster. |
| `PATCH /api/tenants/{id}` | Manager only: the whole account — label, role, variant, quota, upstreams, capture consent, `disabled` + `disabled_reason`, and `config_yaml`, which is validated by the same strict loader the proxy builds with (a typo is a `400` naming the key, and nothing is partially applied). There is **no spend cap** to set — each account bills its own provider credential. |
| `POST /api/tenants/{id}/tokens` | Manager only: reissue a token for a tenant that **already exists**. There is no manager-side create. |
| `POST /api/tenants/{id}/password-reset` | Manager only: mail that account a reset code. The manager **cannot see the code and cannot set the password** — a manager who could set one could sign in AS the user and act in their name. The account's current password keeps working until its owner finishes. |
| `POST /api/tenants/{id}/purge` | Manager only, **irreversible**: erase that tenant's requests, component rows, stored transcripts, monthly spend rollup and archived objects. The account keeps working. Requires `{"confirm": "<their email or id>"}` and writes an audit row. |
| `DELETE /api/tenants/{id}` | Manager only, **irreversible**: the purge above, then the account — tokens, sessions, agent keys and pending codes go with it by cascade. Same confirmation; the audit row outlives the account. A manager cannot delete themselves. |
| `GET /api/variants` | Manager only: per-variant rollup of the metrics that already exist, folded from each account's own aggregates, plus the `caveats` that say what the comparison cannot show. Accepts `since`/`until`. |
| `POST /api/feedback` | One feedback submission from the signed-in account: which agent it is about (`claude-code` or `bob`, mandatory — a third value is refused), 1–5 stars for every one of the seven questions, and a comment of **at least 50 characters of real text**, enforced here and not only in the form (whitespace is collapsed before counting, so 50 spaces is not 50 characters). The tenant is taken from the cookie, never from the body. `422` names the rule that was broken. Stored in the **control** database, then mailed to the manager off the request path — a slow or unconfigured relay cannot fail or delay a submission, and `mailed_at = 0` records a notification that never got out. |
| `GET /api/authz/grafana` | **Manager only**, and not a route a browser calls: it is nginx's `auth_request` target for `/grafana/`. `204` for a manager's `cg_dash` session, `401`/`403` for anyone else. The `204` carries **`X-Cg-Grafana-User: <that manager's email>`** and nothing else: nginx copies it onto the request it proxies and Grafana's auth-proxy signs the manager in as an Admin, so the gate is also the sign-in and there is no second password. That header is a complete authentication, so it is named **only** from the validated session — a client's own copy is ignored here and unconditionally overwritten by nginx (`proxy/manager_test.go`, `TestGrafanaAuthzNamesTheSessionOwnerNotTheRequestsHeader`, and `deploy/grafana/README.md`, "Proving the header cannot be forged"). There is no body: nginx reads the status and that header, and a body would describe what is behind the gate to somebody who did not get through it. Cookie only, like every route here, so a proxy token cannot open the dashboards. It ships alongside `deploy/service/nginx.conf` but only takes effect when the proxy restarts; until then the front end refuses `/grafana/` for everyone, managers included. |
| `GET /api/feedback` | **Manager only** — including the aggregate. Returns every submission with the agent it is about, per-question mean and 1–5 distribution, the NPS split on the recommend question, the daily trend, and `by_agent` — the same arithmetic per agent. The question keys and their wording are served alongside, so the form and this view hold no copy of either. A plain account gets `403` for every form of this, its own rows included: "you said 2, the average is 4.4" is a disclosure about other people's answers. A manager may narrow with `?tenant=<id>`. |

### The tenant view's configuration fields

Every route that returns an account — `/api/whoami`, `GET`/`PUT /api/me`, `/api/login`,
`/api/register`, `GET /api/tenants`, `PATCH /api/tenants/{id}` — returns the same tenant
view, and three of its fields describe the configuration:

| Field | Meaning |
|---|---|
| `config_yaml` | **Changed meaning.** The document this tenant has **stored**, and **empty while they track the server default**. It used to be the resolved value. It is also the field `PUT /api/me` and `PATCH /api/tenants/{id}` write back, so a round trip through a settings form cannot turn tracking into a frozen copy of today's default by accident. |
| `effective_config_yaml` | What their traffic actually runs under, resolved by the same `Registry.Config` the proxy uses — the stored document when there is one, the server default otherwise. |
| `config_inherited` | `true` when they are tracking the default (i.e. `config_yaml` is empty or whitespace). |

Tracking is not a flag; it is the **absence of a stored document**. So the two transitions
are one call each, both audited:

- **Start customising:** `PUT /api/me` with `config_yaml` set to a document (the UI seeds it
  with the current effective one, so nothing changes at the moment of taking ownership).
- **Go back to following the default:** `PUT /api/me` with `config_yaml: ""`.

`Register` no longer copies the default into the new row, so a new account tracks it from
the first request. The trade this exposes: **a tenant who customises stops receiving
improvements to the server default** until they choose to follow it again. There is
deliberately no data migration for rows the old `Register` stamped with a copy of the
default — recognising a byte-identical *previous* default is not possible here (none were
ever recorded), and a guess that hits would delete a configuration somebody chose.

## Per-request headers

| Header | Effect |
|---|---|
| `x-context-guru-token: cg_live_…` | **Hosted mode:** identifies the calling account. Read from this header first; an auth slot is accepted as a fallback, but only for a value shaped like one of our tokens (`cg_live_` + 26 characters), so a provider key is never looked up as a token. Prefer the header — the auth slot has to stay free for the caller's own provider credential, and it cannot hold both. |
| `x-context-guru-session: <id>` | Sets the session key explicitly. Otherwise a stable content hash (`sha256(system + firstUser)`) keys the session. |
| `x-context-guru-bypass: true` | Skips the pipeline entirely for this request (tokens unchanged). |
| `x-context-guru-pipeline: <a,b,c>` | Runs exactly these components, in order, for this request. |

Every `x-context-guru-*` header is stripped before the request is forwarded, so none of them
can reach an upstream — which is what makes the token safe to carry in one.

The auth slots (`Authorization`, `x-api-key`, `x-goog-api-key`) are **not** ours: they carry the
caller's own provider key and are forwarded unchanged, unless the operator configured a
server-held key for that upstream or the slot turned out to hold one of our tokens, which is
scrubbed. A request with no provider credential of its own is refused **401** and is never
served on the operator's credential.

See [Config & environment](config.md) for flags and env vars, and
[Presets](presets.md) for the built-in pipelines.
