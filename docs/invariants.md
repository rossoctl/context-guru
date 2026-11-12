# Component invariants

> **Status: draft for review.** Keep-alive and the sweep have proposed invariants and measured
> baselines. The other components are listed, with gaps, and come next. Every threshold here is a
> proposal until the owner confirms it.

## Summary

- **What this is:** for each component, the rules (invariants) that must hold on real traffic, and
  how to check each one from metadata.
- **Why:** our counters measure "did the component act". None measures "did it cause a cost
  somewhere else". #716 found that ping 2 re-wrote the whole conversation in 26% of cases ($97.97 in
  3.9 days, 68% of all that Context Guru saved on that laptop). The cause was the sweep question
  taking ping 1 (#776). No counter could see it.
- **How a monitor uses it:** it runs each query on a copy of a dashboard database, every day or
  every week. It compares the result with the threshold, and reports each breach to the owner.
- **3 kinds of invariant:**
  1. **Intent:** the component does what it is for.
  2. **Side effect:** it causes no cost or harm that is not documented (a cache miss, a prefix
     re-write, a slower response, a failed request, a wrong dashboard figure, an unplanned request).
  3. **Contract:** the Hard boundaries in `CLAUDE.md` hold: fail open, every lossy change is
     reversible, metadata first, and keep-alive never shortens the cache strategy.

## Rules for every check

- **Metadata only.** A check reads counts, tokens, times, ids, roles and flags. It never reads text,
  bodies or summaries. It never reads `request_content`, `extraction_calls.summary`,
  `extraction_calls.before_gz`, `extraction_calls.after_gz` or `declaration_text`.
- **Query a copy.** `sqlite3 <db> ".backup $D/dash.db"`, run the check, then delete the copy.
- **A request "wrote instead of read"** when `cache_write > cache_read`. In this document that is a
  **miss**.
- **`cache_saved_usd` is the provider's saving, not ours.** Compare a cost with `total_saved_usd`
  from `/api/stats`, never with `cache_saved_usd`.
- **`/stats` counters reset when the proxy restarts.** A weekly check uses the database, or
  `/api/stats?since=<ms>`.

## Where the baselines come from

- **Source:** the laptop proxy (port 8789), on a copy of its dashboard database, on 2026-11-01.
  It holds 13,575 requests from 2026-10-23 to 2026-11-01.
- **All traffic is preset `xhigh`.** Components that `xhigh` does not run have no baseline here.
  They are listed as gaps (see [Components with no baseline](#components-with-no-baseline)).
- **2 windows:**
  - **W1:** 2026-10-26 00:00 UTC to 2026-11-01 08:57 UTC. Versions v0.6.0 to v0.6.3, before the
    fixes #777 and #779.
  - **W2:** from 2026-11-01 08:57 UTC, when the laptop proxy started v0.6.4. **Only about 7 hours:
    1,099 requests, 94 pings.** W2 numbers show the direction, not a stable baseline. #778 asks
    for 3 or more working days.

## The components

| Group | Components | Where the intent is written |
|---|---|---|
| Keep-alive (`keepalive/`) | planned pings, the substitutes (sweep question, summarizer call), the extra slot, the manual Ping, the hold limit, the breaker | `docs/design.md` "Keep-alive: the cache strategy comes first"; #716, #776, #778, #779 |
| The sweep | `extract_llm_sweep` | `docs/components/extract_llm.md`, `docs/components/advanced-offload.md`; #508, #695 |
| Pipeline, runs on the laptop | `extract_llm`, `cache_aware_summarizer`, `extract`, `dedup`, `cmdfilter`, `format`, `toon`, `searchfold`, `textclean`, `toolfilter`, `cachesplit` | `docs/components/*.md`, `docs/data-dependence.md` |
| Pipeline, not on the laptop | `mask`, `failed_run`, `collapse`, `readlifecycle`, `linecap`, `smartcrush`, `skeleton`, `coref`, `summarize`, `agentdiet`, `cacheinject`, `toolschema` | `docs/components/*.md`, `docs/data-dependence.md` |
| `apply/` | the cache split, the tail breakpoint (#796), the in-band placer (#786), the anchors (#727), the restart grace (#759, #767) | `docs/components/caching.md`, `docs/design.md` "The cached-prefix boundary" |
| State | `store/`, session state (#770), `expand` and the stash | `docs/design.md` "State: the Store", "Reversibility: marker + expand loop" |
| Dashboard (`dash/`) | the savings, the cache-miss causes, the attribution | `docs/dashboard.md` "Honest-metrics rules" |

## Keep-alive

**Purpose:** keep the provider's prompt cache warm while the user is idle, so the next turn reads
the cache instead of writing it again. The cache strategy sets how many pings go out and when. No
other request may shorten it.

**The plan since #867 (v0.6.6).** The ping count depends on what the real turn waits for
(`requests.waiting_for`). K is `keepalive_max_pings` (2 by default).

| `waiting_for` | Planned pings | The cache stays warm for about |
|---|---|---|
| `own_tool` | until the result, at most 8 | up to 37 min |
| `background` (main thread) | K + 3 = 5 | 25 min |
| `human` (main thread) | K + 1 = 3 | 17 min |
| `teammate`, or a subagent at end of turn | K = 2 | 13 min |
| `done` (#859), or no tool call yet on the thread | 0 | — |

- The warm time is pings × 240 s + 300 s.
- The plan covers the 5-minute Anthropic tier only. The 1-hour tier, OpenAI, session overrides and
  campaigns keep a fixed count.
- No sweep question while the thread waits for its own tool.
- `/stats` has no new `keepalive_*` key for the plan. The database has no new keep-alive column.

**Windows.** W1 and W2 are as in
[Where the baselines come from](#where-the-baselines-come-from). **W3** starts at the v0.6.6 start,
2026-11-03 21:27 UTC, and ends 2026-11-04 17:37 UTC. The proxy ran preset `xhigh` until 09:38 UTC
and `ultrahigh` after that. W3 has 1,339 real requests and 257 pings.

| # | Kind | Statement | Signal | Threshold (proposed) | W1 | W2 | W3 |
|---|---|---|---|---|---|---|---|
| K1 | side effect | A planned ping reads the prefix and does not write it again. | `keepalive = 1` rows. Split by `request_lifecycle.ping_n`, the held turn's `waiting_for`, and `model`. A miss is `cache_write > cache_read`. | over 2% in any group | **ping 2: 90 of 377 (23.9%)** | 0 of 79 | 1 of 257 (one `own_tool` ping 2, $0.29) |
| K2 | intent | A real turn that comes back while the cache should still be warm reads the cache. | The #716 walk ([Q1](#q1-pings-and-the-turn-after-them)), with the gap compared to pings sent × 240 s + 300 s. Split by `waiting_for`. | over 5% missed | — | — | `own_tool` 0 of 45. `teammate` 0 of 16. `human` 1 of 8. **`background` 10 of 21 ($5.98, B5).** |
| K3 | contract | After a turn that does not end at a tool result, no substitute takes a planned ping (#779). | Ping kinds per idle span ([Q2](#q2-the-ping-sequence-after-each-real-turn)). P = plain ping, Q = sweep question (`max_tokens = 0`). The turn's shape is the held turn's `wake_cause`: `tool_result`, or not. | any span that starts with Q when `wake_cause` is not `tool_result` | 103 of 199 | 0 of 10 | **1 of 37 (B6)** |
| K4 | intent | Pings do not fail or time out. | ping rows with `status >= 400` or `upstream_ms >= 45000` | over 1% | 3 of 905 | 0 of 84 | **3 of 257 (1.2%)** |
| K5 | intent | Keep-alive saves more than its pings cost. | `/api/stats?since=<ms>`: `keepalive_net_usd` | below $0 over 7 days | +$66.50 | +$17.94 | restart the baseline after the release with #768 |
| K6 | contract | The extra slot after the last planned ping sends at most one request per idle span. | rows with `max_tokens = 0` and `ping_n` = planned count + 1 for that `waiting_for` | more than 1 per span | — | 5 of 5 spans with 1 request | 4 spans (`teammate` 3, `human` 1), 1 request each |
| K7 | contract | No sweep question while the thread waits for its own tool (#867). | spans where the held turn's `waiting_for = 'own_tool'` and a ping has `max_tokens = 0` | any | — | — | **1 of 45 (B6)** |
| K8 | contract | A thread that is done gets no ping (#859, #867). | spans where the held turn's `waiting_for = 'done'` and a ping follows | any | — | — | 0 |
| K9 | intent | A release does not cut the pings per long wait. | per day, by thread type (main or subagent) and `waiting_for`: pings per real-turn gap longer than 300 s | a drop of more than 30% against the week before, at the same request volume | see [the #1040 case](#case-a-default-change-hidden-by-a-config-layer-1040) |  |  |
| K10 | side effect | A long wait that the plan covers does not miss for lack of a ping. | per day, by thread type and prefix band (under 20k, 20k to 80k, 80k or more): gaps longer than 300 s with no ping between and `cache_write > cache_read` on the turn after | a rise of more than 2× against the week before, in any band | see the #1040 case |  |  |
| K11 | contract | A real turn that comes back warm after a ping gets keep-alive credit. | A real request (`keepalive = 0`) with a `keepalive = 1` row before it on the same `session_id` and `thread_id`, in the same idle gap. The gap since the thread's previous real request is 300 s or more (5-min tier). The turn read its cache (`cache_read > cache_write`). The check is `keepalive_saved_usd > 0`. Report the share with credit, per day and per host ([Q4](#q4-credit-for-warm-turns-after-a-ping)). | a share below 80% on a day with 10 or more such turns, or any day at 0% with 10 or more | — | — | Laptop (v0.7.1): **about 100 of 100** in the last 3 days. Hosted deployment, before its v0.7.2 deploy: 31 of 33. Hosted deployment, after the deploy: **0 of 48 (B10)**. Measured 2026-11-11. |
| K12 | contract | A large thread is not refused for its bytes alone (#1100, #1102). | `cg_keepalive_refused_total{reason="too_large"}` per day (#1137) | any rise against the week before; a non-zero count after #1102 is a body over 32 MiB | — | — | not shipped yet (#1137) |
| K13 | side effect | The per-tenant cap does not evict a manual hold that a user is waiting on. | `cg_keepalive_manual_hold_evicted_total{reason="bytes"\|"count"}` per day; the WARN line when a tenant holds 2 bodies of 8 MiB or more (#1137) | any `reason="bytes"` eviction; report the WARN count | — | — | not shipped yet (#1137) |
| K14 | intent | Held bodies stay well inside the hold cap. | the `cg_keepalive_manual_hold_body_bytes` histogram (buckets at 1, 4, 8, 16 and 32 MiB); `cg_tenant_keepalive_manual_hold_bytes` aggregated over tenants (#1137) | a share above 8 MiB that rises week over week | — | — | not shipped yet (#1137) |

- **A miss after the warm time ends is not a breach.** The strategy accepts that cost. The monitor
  reports it but does not alert on it.
- **Do not compare K5 across releases.** #855 lowered the cache-read prices. #768 (on main, not
  released) changes how `keepalive_saved_usd` is computed.
- **Breach meaning, K11:** the host does not book the ping saving. The pings still work. On a correct host, the share is about 100%. First check: does the host call `proxy.PingSavings` and set `proxy.Options.Pings` (see `cmd/context-guru-proxy/main.go`, around `:725` and `:1033-1043` on v0.7.2)? Then check whether `acc_priced` changed on the day the share dropped.
- **Breach meaning, K1:** a ping or a substitute did not renew the cache entry. First check: what
  the ping before it was (Q2), and the model.
- **Owner:** David Amid (proposed).

## The sweep (`extract_llm_sweep`)

**Purpose:** at a keep-alive ping, ask the agent's model which old tool outputs are spent. Hold the
answer. On the next cold request, remove those outputs, with a `<<cg:HASH>>` marker so `expand` can
give each one back.

| # | Kind | Statement | Signal | Threshold (proposed) | W1 | W2 |
|---|---|---|---|---|---|---|
| S1 | contract | The sweep removes outputs only on a cold request. | `request_components.events.sweep_applied_cold > 0` joined with `requests.cache_miss_reason` | any apply on a request labelled `hit` | **4 requests** (see [B2](#suspected-breaches)) | 0 of 12 |
| S2 | intent | The sweep saves more than its questions cost. | sweep `request_components.saved_usd` minus `extraction_calls.cost_usd` where `component = 'extract_llm_sweep'` | below $0 over 7 days | $110.70 − $28.42 = **+$82.28** | $10.09 − $3.62 = **+$6.47** |
| S3 | contract | `expand` gives back each removed output byte for byte. | **Gap G2.** No event records it. | — | — | — |
| S4 | side effect | The sweep rarely removes an output that the agent needs later. | `/stats` `keepalive.sweep_drops.false_drop_rate`; database `events.sweep_apply_dropped_referenced_later` | over 15% | — | **`/stats`: 28.7% (39 of 136). Database: 6.7% (9 of 135).** See [B1](#suspected-breaches). |
| S6 | contract | The sweep never cuts a live prefix. No sweep apply on a turn that a ping read less than the cache lifetime before. | rows with `events.sweep_applied_cold > 0` where the last `keepalive = 1` row of the same session and thread read the prefix (`cache_read > cache_write`) less than 300 s before (5-minute tier) | any | — | — | **`ultrahigh`: 11 of 11 such applies broke the cache (#902).** `xhigh`: 0 applies |
| S5 | side effect | The sweep question renews the cache entry it reads. | substitute rows (`keepalive = 1`, `max_tokens = 0`): miss rate, and the miss rate of the next ping | over 2% | 0 of 353 wrote; the next ping missed 90 times (#776) | 0 of 36 |

- **Owner:** David Amid (proposed; the sweep row in `docs/data-dependence.md`).

## Expand rate, by component

**Why:** an `expand` call means the agent needed an output that a component removed. Each one
costs a turn and the tokens it returns. A rising rate means a component removes too much.

| # | Kind | Statement | Signal | Threshold (proposed) | Laptop baseline | cg2, scan 1 (11-03 to 11-04) | cg2, scan 2 (11-10 to 11-11) |
|---|---|---|---|---|---|---|---|
| E1 | side effect | The agent rarely needs a removed output back from the sweep. | `sum(sweep_expands)` per 100 sweep removals (`events.sweep_applied_cold`) | over 5 per 100 removals over 7 days | 11-03 to 11-11: **0 expands for 460 removals** | **1.78 per 100** (6 of 337); false drops 1.48 per 100; 22,564 tokens back | **0.66 per 100** (2 of 301); false drops 0.33 per 100; 8,818 tokens back |
| E1b | side effect | The same, weighted by tokens. | `sum(sweep_expand_tokens) / sum(saved_unique)` of the sweep | over 5% over 7 days | 0% (0 of 1,720,868 tokens) | not measured | **1.75%** (8,818 of 503,425 tokens) |
| E1c | side effect | The sweep rarely drops an output that the agent names again later. | `events.sweep_apply_dropped_referenced_later` per 100 removals | over 15 per 100 over 7 days | **28.3 per 100** (130 of 460) | not measured | 9.97 per 100 (30 of 301) |
| E2 | side effect | The agent rarely needs a removed output back from `extract_llm`. | **Gap G9.** No expand can be tied to `extract_llm`. The upper bound is `expands − sweep_expands`, which also holds `extract`, `dedup`, `searchfold`, `textclean`, `cmdfilter` and the other offloads. | none until G9 is fixed: the upper bound is too loose | at most 3 for 8 fresh removals | at most 7 for 8 fresh removals | at most 9 for 5 fresh removals |
| E3 | contract | A sweep expand is counted as a false drop at most once. | rows with `sweep_false_expands > sweep_expands`, and rows with `sweep_expands > expands` | any row | 0 and 0 | 0 and 0 | 0 and 0 |
| E4 | side effect | Expands stay rare in total. | `sum(expands)` per 1,000 real requests | over 3 per 1,000 over 7 days | 0.37 per 1,000 (3 of 8,118) | **0.97 per 1,000** (13 of 13,338; 0.07% of requests) | **0.97 per 1,000** (11 of 11,376; 0.10% of requests) |

- **The cg2 window is always short.** The database holds about 2 days, because of its size cap.
  Each scan covers one busy day: 12,200 of 13,338 requests on 11-04, and 11,119 of 11,376 on
  11-10. About 98% of requests are on preset `xhigh`. So one scan is one day of fleet traffic.
- **Scan 2:** 82 sessions and 1,574 threads. The laptop test of the same script covers
  2026-11-03 to 11-11: 8,118 real requests, 35 sessions, 169 threads.
- **The fleet is stable at 0.97 expands per 1,000 requests in both scans.** The sweep's expand
  rate fell from 1.78 to 0.66 per 100. With 6 and 2 expands, that is within the noise.
- **The laptop's referenced-later rate (28.3 per 100) is 3× the fleet's (9.97).** The laptop runs
  `ultrahigh`. The fleet runs `xhigh`.
- **Run the scan once a week,** and keep each result. The T1..Tn labels are per scan: the same
  label is a different tenant in another scan.
- **Why the `extract_llm` number is weak:** `expand` gives back the stored bytes for a hash, and it
  records no component (`expand/expand.go`, `Resolve`). Only the sweep keeps its own ledger of
  removals, so only the sweep's expands can be counted apart
  (`components/offload/extract_sweep_apply.go`, `NoteSweepExpand`).
- **A fresh `extract_llm` removal** is a request where it acted and saved tokens. A replay of an
  earlier removal saves 0 tokens and is not counted.
- **The per-tenant numbers stay private.** They go into the internal repo only, with tenants as
  T1 to Tn.
- **The script:** read-only, metadata only. It makes a backup copy of the cg2 database, reads the
  copy, prints aggregates, and deletes the copy.

## Other components that run on the laptop

Not reviewed yet. Activity on all windows (laptop, 12,265 non-ping requests):

| Component | `acted` | `mutated` | `reverted` | `err` |
|---|---|---|---|---|
| `searchfold` | 8,733 | 8,734 | 0 | 0 |
| `extract` | 3,714 | 3,714 | 0 | 0 |
| `textclean` | 3,133 | 3,133 | 0 | 0 |
| `extract_llm_sweep` | 2,537 | 2,537 | 0 | 0 |
| `dedup` | 469 | 469 | 0 | 0 |
| `cmdfilter` | 328 | 328 | 0 | 0 |
| `cache_aware_summarizer` | 280 | 280 | 0 | 0 |
| `extract_llm` | 224 | 224 | 0 | 0 |
| `format` | 22 | 22 | 0 | 0 |
| **`toolfilter`** | **0** | **3,204** | 0 | 0 |
| **`cachesplit`** | **0** | **0** | 0 | 0 |
| `toon` | 0 | 0 | 0 | 0 |

- One invariant fits all of them: **`acted = mutated`, and `reverted` and `err` stay near 0.**
  `toolfilter` already breaks the first part (B3).

## Components with no baseline

The laptop runs only `xhigh`, so these components never ran there. **The owner decided (2026-11-01):
mark them as gaps.** Their invariants get written, but no baseline is measured until real traffic
runs them.

`mask`, `failed_run`, `collapse`, `readlifecycle`, `linecap`, `smartcrush`, `skeleton`, `coref`,
`summarize`, `agentdiet`, `cacheinject`, `toolschema`.

## `apply/`, state, `expand`, the dashboard

Not written yet. Inputs, from the release checks done by hand (RELEASING.md Part B):

- After a restart, session state survives (`session_state` row count, key prefixes only), and the
  next turn has no `window_not_exact`.
- The restart grace protects a live thread's first request after a restart, and not a new thread.
- `expand` gives back a removed output byte for byte (sha256 and length).
- Every read-only `cg.py` command exits 0. This check found #781.
- The default release binary links no OpenTelemetry, gRPC or protobuf.

## Tool deferral (`toolfilter` defer, preset `ultrahigh`)

**Purpose:** cut the weight of tool definitions that the user does not use. It never removes a
tool, and it never rewrites the cached prefix. A deferred tool is sent with `defer_loading: true`.
The model sees only its name, and loads its definition through a search when it needs it.

- **The rule:** the user's last 3 sessions declared the tool and did not use it. The project used it
  in none of its last 10 sessions. It is not on the never-defer list. It weighs at least 200 tokens.
  An MCP server counts as one unit. A load counts as a use.
- **The deferred set is fixed per thread,** also across a restart (the `cg:tdf:` record).
- **The search tool:** a native search when one exists, ours only when none exists.
  - Claude Code with ToolSearch on: the server-side `tool_search_tool_bm25` today. After #900,
    Claude Code's own ToolSearch.
  - Codex, which has its own `tool_search`: nothing is deferred (#897).
  - GPT, GLM, Kimi: our `context_guru_tool_search`, which removes the tool and adds it back in band
    (#875).
- **Opt-in and experimental.** Source: #861 and its follow-ups, `docs/tool-search-matrix.md` (#900).

**Signals that exist on main (verified 2026-11-04):**
- `/stats` `tool_defer`: `requests`, `decisions`, `rebuilds`, `rebuilds_unproven`, `history_live`,
  `accounts`, `probes_pass`, `probes_fail`, `turned_off`, `retries_400`, `add_backs`,
  `wrong_guesses`, `dropped_searches`, `mixed_searches`, and
  `ledger{gross_usd, cost_usd, net_usd, requests, searches, deferred_tokens, unpriced_requests}`.
- `requests` columns: `defer_tokens`, `defer_list_tokens`, `defer_searches`,
  `defer_dropped_searches`, `defer_loaded`, `defer_wrong_guesses`, `defer_gross_usd`,
  `defer_net_usd`. The table `tool_defer_history`.
- **No `cg_*` metric** for tool deferral on main. The first is `cg_tooldefer_client_disabled_total`
  in #900. Gap G7.
- **The net saving** (#891): `defer_net_usd` = the gross weight of the deferred definitions, at the
  tier each request paid, times the billed-token factor 1.6857, minus the names list, the searches
  and the loaded definitions. It equals `/stats` `ledger.net_usd`.

| # | Kind | Statement | Signal | Threshold (proposed) | Baseline |
|---|---|---|---|---|---|
| D1 | intent | Tool deferral saves more than it costs. | `ledger.net_usd`; `sum(defer_net_usd)` | below $0 over 7 days | laptop 2026-11-04, about 8 h: net **+$1.07** (gross $1.09, cost $0.017), 361 requests |
| D2 | side effect | It does not rewrite the cache in the middle of a thread. | `rebuilds`, `rebuilds_unproven`; misses on threads with `defer_tokens > 0` against threads without | any `rebuilds_unproven`; miss rate over the no-deferral rate + 2 points | rebuilds 0 |
| D3 | side effect | A deferred tool is rarely needed. | `wrong_guesses / decisions` (deferred, then loaded) | to set after 7 days | 0 searches, 0 wrong guesses |
| D4 | side effect | No user loses a tool. | `break` disables (T1); the #871 accuracy test | any `break`; #871 below its baseline | #871 Opus: 66 of 80 with deferral, 72 of 80 without |
| D5 | contract | A 400 caused by deferral never reaches the user. | `retries_400`; `turned_off` per model | any `turned_off`; report each one | 0 and 0 |
| D6 | contract | A request never carries 2 search tools. | `retries_400` with an "Only one tool_search" error | any | 0 |
| D7 | side effect | Deferral does not make answers slower to start. | `ttfb_ms` on rows with `defer_tokens > 0` against rows without, same model | p50 over +20% | to measure |
| D8 | intent | The probe passes before deferral starts. | `probes_pass`, `probes_fail` | any `probes_fail` | 1 and 0 |
| D9 | contract | A purge deletes the tenant's deferral data. | **Gap G8.** `PurgeToolDefer` deletes the rows, but no counter shows it. | — | — |

- **Not monitored, enforced in code and tests:** no `defer_loading` on Chat Completions; no deferral
  when the user set `ENABLE_TOOL_SEARCH=false`. Neither has a counter.
- **Baseline scope:** one user's laptop, from the switch to `ultrahigh` on 2026-11-04. It is not a
  fleet baseline. cg2 runs `xhigh`, so it has no deferral traffic.
- **Other figures:** #825 (option B on GPT, GLM, Kimi) measured a net saving of 9.68%.
- **Owner:** David Amid (proposed).

### Option B: Claude Code's own search (#898)

**Purpose:** on preset `ultrahigh`, with Claude Code's own `ToolSearch` on, the proxy adds its own
deferred tools to Claude Code's search results. It answers only the exact `select:Name1,Name2`
form, and only for names it deferred. Source: the
[#898 design update](https://github.com/rossoctl/context-guru/issues/898#issuecomment-6635851339).

**On any fault, the proxy disables, and never switches.** It marks the `ToolSearch` description
hash disabled, per tenant. It defers nothing for that hash until the mark expires. The live thread
pays one cache rewrite. 2 reasons (since #934):

1. `unknown_description`: the description hash is unknown. A new Claude Code version is likely.
2. `break`: the model called a deferred tool that was never loaded, or got a "not loaded" error.

| # | Kind | Statement | Signal | Threshold (proposed) | Baseline |
|---|---|---|---|---|---|
| T1 | side effect | No user loses a tool. | `cg_tooldefer_client_disabled_total{reason="break"}`; `/stats` `tool_defer.client_search_disabled.break` | **any** | not shipped yet |
| T2 | intent | A Claude Code release does not silently stop option B. | `cg_tooldefer_client_disabled_total{reason="unknown_description"}`, with the `desc_hash` from the log line and the Claude Code version | any; report the version and the hash | not shipped yet |
| T4 | side effect | A disable costs at most one cache rewrite on the live thread. `break` stops deferral on the live thread and costs exactly one. `unknown_description` is decided at thread start and costs none. | for each `break` log line: misses (`cache_write > cache_read`) on the later requests of that `session` and `thread` | more than 1 miss per disable | not shipped yet |

- **What counts as a `break` (#900):** only a deferred tool's own error result: "not loaded",
  "no such tool" or "does not exist". A direct call that worked is a use, not a break.
- **Signals (from PR #900, merged to main as 44f8318d on 2026-11-04; not released yet):**
  - `/stats` `tool_defer.client_loads`: the loads the proxy added to Claude Code's ToolSearch
    results. `tool_defer.direct_calls`: deferred tools that the model called with no load.
  - `add_backs` now counts the #825 path only.
  - The probe log line `cg.tooldefer_probe` carries `probe=server|client`.
  - Metric: `cg_tooldefer_client_disabled_total{reason}`, with `reason` one of
    `unknown_description`, `break`.
  - `/stats`: `tool_defer.client_search_disabled`, an object with the same 2 reasons.
  - **#934 (merged as 442fe9a0, in the next release) removed the reason
    `non_select_query_for_our_tool`.** Claude Code's own ToolSearch indexes every tool, also the
    ones the proxy defers (the #871 controlled test). So a keyword query for one of our tools
    works, and it no longer disables anything. Releases up to v0.6.7 still send this third label.
    The monitor ignores it.
  - Log (warn): `cg.tooldefer_client_search_disabled`, with `reason`, `desc_hash` (8 characters),
    `session` and `thread`. It holds no query text.
- **How it counts:** `unknown_description` counts once per tenant and hash per 24 h. `break`
  counts every time. So T2 counts disables, not events.
- **Disable scope:** per tenant and description hash, for 24 h. A proxy restart clears it.
- **Known-good hash today:** `88033eceb421f1c2`. It was the only `ToolSearch` description in 109
  captures. A T2 alert with any other hash means a new description.
- **T4 needs a join** between the log line and the `requests` table on `session` and `thread`.
  The dashboard database does not store the disable today, so T4 reads the log.
- **Report:** every disable event, with its reason, and the Claude Code version if it is known.
  **A rise in `break` is the most urgent,** because a user lost a tool for one turn.
- **No laptop baseline:** the laptop runs `xhigh`, not `ultrahigh`.
- **Owner:** David Amid (proposed).

## Case A: a default change hidden by a config layer (#1040)

**Why it is here:** this is the kind of defect the monitor must catch. No test failed and nothing
errored. Only the metrics showed it. Source: the fix-subagents session, on the owner's laptop
proxy, 2026-11-01 to 11-10. It is not fleet data.

- **What happened:**
  - #867 lowered the default `keepalive_min_prefix_tokens` from 80k to 20k, so small subagents
    get pings.
  - The cache-strategy table (`config/strategy.go`, and the plugin's `settings.py`) still wrote
    80000 into the user's config. An explicit value wins over the default. So no plugin user on a
    strategy got the 20k floor.
  - #867 also stopped the sweep question during a tool wait, because pings were to keep the cache
    warm there. With the 80k floor, those pings never went out. Subagents lost both the sweep cut
    and the pings.
- **What the metrics showed** (fix-subagents):
  - Subagent savings fell from 9.3% to 4.0% of baseline cost, from one week to the next.
  - Subagent keep-alive credit fell from about $65 a day to $2–4 a day.
  - Subagent pings fell from about 140 to 17 a day, at the same request volume.
  - On 11-08 to 11-10, 109 subagent tool waits longer than 5 min got no ping. All 109 missed,
    and all had a prefix of 20k to 80k tokens.
- **The fix:** #1040 (merged 2026-11-10, after v0.7.1). The floor follows the wait cause: 20k
  (`keepalive_tool_wait_min_prefix_tokens`) for a tool wait, background work, a teammate and every
  subagent wait; 80k for a main thread that waits for a person or for an unknown cause. #1061 adds
  a test. #1044 makes Pi render its config again.
- **Baseline for K9, K10 and X6** (laptop, measured 2026-11-11 with
  [Q3](#q3-long-waits-by-thread-type)). The laptop runs v0.7.1, which does not have #1040, and its
  config holds `keepalive_min_prefix_tokens: 80000`. So 11-08 onward is the broken state, and the
  next release must move these numbers:

| Day | Subagent long waits | Subagent pings per wait | Subagent no-ping misses (all 20k–80k) | Subagent saved | Main pings per wait |
|---|---|---|---|---|---|
| 11-03 | 19 | 1.37 | 3 | 8.8% | 2.00 |
| 11-04 | 70 | 1.64 | 7 | 6.0% | 2.23 |
| 11-08 | 46 | 0.87 | 22 | 2.6% | 2.03 |
| 11-09 | 66 | 0.39 | 48 | 2.1% | 2.53 |
| 11-10 | 182 | 0.48 | 133 | 4.1% | 2.16 |
| 11-11 (part) | 48 | 0.46 | 36 | 7.5% | 2.17 |

- **Main threads did not move** (2.0 to 2.5 pings per wait). The drop is in subagents only. So the
  monitor must split by thread type, or the main threads hide the drop.
- **After the release with #1040:** subagent pings per wait should rise to about 1.4 or more, and
  the 20k–80k no-ping misses should fall to near 0.
- **The lesson for the monitor:** compare each metric with the week before, and split it by thread
  type and wait cause. A sharp drop on the day a release goes in, at the same request volume, is
  the alert.

## Cross-component invariants

| # | Statement | Signal | Threshold (proposed) | Baseline |
|---|---|---|---|---|
| X1 | No request that Context Guru sends by itself re-writes the prefix. | every `keepalive = 1` row; miss = `cache_write > cache_read` | over 2% | see K1, S5 |
| X2 | The total saving is not negative. | `/api/stats?since=`: `total_saved_usd` | below $0 over 7 days | W1 $183.20; W2 $28.99 |
| X3 | The share of requests that miss does not rise after a release. | non-ping rows with `cache_write > cache_read`, per version window | over +3 points against the last release | to measure |
| X4 | A request labelled `hit` read its cache. | `cache_miss_reason = 'hit'` and `cache_write > cache_read + 10000` | 0, except a first request of a thread | **34 later requests; 3 in W2** (B2) |
| X5 | A component's own action is not followed by another component's cost. | Q2 generalized: for each request kind, the miss rate of the next request in the thread | to define | to measure |
| X6 | The savings share of each thread type does not fall after a release. | per week, by thread type (main or subagent): `1 − sum(cost_usd) / sum(baseline_cost_usd)` on real requests | a drop of more than 3 points against the week before the release | see the #1040 case |

## Suspected breaches

| # | What | Found by | Next step |
|---|---|---|---|
| B1 | **Two counters give different false-drop rates for the sweep.** `/stats` says 28.7% (39 of 136) since the restart. The database says 6.7% (9 of 135) in the same window. 3 of 6 threads are at 33% to 56% in `/stats`. | S4 | Find what each counter counts. If 28.7% is right, the sweep removes too much. |
| B2 | **Requests labelled `hit` that wrote the prefix again.** 260 non-ping rows have `cache_miss_reason = 'hit'` and `cache_write > cache_read + 10000`. 226 are a thread's first request. 34 are later, and 3 of those are in W2. 4 of them had a sweep apply. #759 meant to label a partial read as a miss. **cache-strategies confirms 3 cases on 2026-11-03 and 11-04:** 2 turns wrote 225k and 297k after a gap longer than 5 min and are labelled `hit`; one first request of a new subagent thread is labelled `hit`, where `cold_start` fits. | X4, S1 | Check how `cache_miss_reason` is set for a first request and for a partial read. Then open an issue. |
| B3 | **`toolfilter` changes requests but reports `acted = 0`.** `mutated` is 3,204, the same as `/stats` `decl_filter_requests`. | the activity table | Check which flag `toolfilter` sets. The dashboard may undercount it. |
| B5 | **After a wait for background work, half the real turns write the conversation again while it should still be warm.** In W3, 10 of 21 missed. Every one reads about 19k tokens and writes the rest. The last ping just before it read the whole prefix, and the model, tools and cache lifetime did not change. **Confirmed: issue #902 (cache-strategies, 2026-11-04).** The cold check of a request and the touch of a ping share one clock, `cg:seen:<alias>`. The alias is a hash of the first system message and the first user message. The request computes it before tool deferral. The keeper records the body after deferral, where `messages[1]` holds the deferred-names list. So the ping renews a clock that the request never reads. The turn looks cold, and the sweep cuts a live prefix, right after system + tools. Laptop, main thread, turns that a ping kept warm less than 5 min before: `ultrahigh` 11 sweep applies in 25 turns, 11 of 11 broke at about 20k; `xhigh` 0 applies in 15 turns. Only `ultrahigh` shows it. | K2, S6 | Fix in progress (cache-strategies). After the merge, S6 must be 0. The 5 `xhigh` reads at about 20k are not #902, and each has a known cause (cache-strategies): 2 proxy restarts dropped the planned pings (rows 1 to 3), Claude Code rewrote a subagent's system prompt (rows 1 and 2), a new subagent thread (row 4), and a ping plan that ended 3 s before the turn came back (row 5, the approved base count for a subagent). See B8. |
| B8 | **A restart of the proxy drops the planned pings.** The keeper holds them in memory. After a restart, the proxy recovers session recency and the grace clock, but no pending ping. Each restart during a wait costs one cache rewrite per active thread. On 2026-11-03 at 21:27 and 21:30 UTC, 2 restarts did this to 3 laptop turns ($4.16). | K2 | Small on a laptop. Larger on a hosted proxy that deploys often. A new issue only if the owner wants it. |
| B9 | **A 20k read does not mean that a ping failed.** Sibling threads in a session share a head (system + tools, about 20k tokens), and another thread keeps it warm (#853). So a thread that lost its own entry still reads 20k. | X5 | A check for "early breaks" must split the cases. A shared-head read on a thread with no ping in the cache lifetime is expected. A shared-head read after a ping that read the full prefix is a breach. |
| B6 | **A sweep question during a tool wait, 3 s after the turn.** One span: `waiting_for = 'own_tool'`, `wake_cause = 'teammate'`. The question went out 3 s after the turn, not after about 240 s. Both K3 and K7 count it. | K3, K7 | Check how the keeper starts a question when the wait cause is a tool and the turn woke from a teammate. |
| B7 | **Ping failures rose to 1.2%** (3 of 257) in W3, from 0 of 84 in W2. | K4 | Check the 3 rows: status, or slow. |
| B4 | **`cachesplit` never acted** on 12,265 requests. `split_requests` and `cachesplit_saved_usd` are 0. | the activity table | Check if Claude Code requests have anything to split, or if the component is broken. |
| B10 | **Keep-alive credit is $0 on a host that embeds the library with its own `main`.** That host does not wire `PingSavings`. Since #930 (v0.7.0), a host must do it, or the accountant books no rescue. It showed on the day the host first ran an accountant build: credit went from about $5/h to $0 on every preset. Hosted deployment, after its v0.7.2 deploy: 0 of 48 warm turns had credit (31 of 33 before). Reported to that host's owners. Fix pending. | K11 | Wire `proxy.PingSavings` and set `proxy.Options.Pings` in the host's `main`. After the fix, K11 must be near 100%. |

## Gaps

| # | What we cannot measure | What would fix it | Needs content? |
|---|---|---|---|
| G1 | ~~Whether the held request ended at a text turn.~~ **Closed:** `requests.wake_cause` gives the shape of the last message. | — | No |
| G2 | `expand` gives back the original byte for byte. | A short hash of the original and of what `expand` returned, per expand event. | No (a hash is metadata) |
| G3 | The kind of a keep-alive request. Today `max_tokens = 0` means "sweep question". This is a convention, not a field. | A `ping_kind` column: plain, sweep, summary, extra, manual. | No |
| G4 | `/stats` counters reset on each restart. | Write the counters that the monitor needs into the database. | No |
| G5 | The 12 components with no laptop baseline. | Real traffic on other presets. | No |
| G7 | A `cg_*` metric for tool deferral. Only `/stats` and the database show it today. | Export the `/stats` `tool_defer` counters as `cg_tooldefer_*`. | No |
| G9 | The expand rate of `extract_llm` alone (E2). The database counts expands per request, not per component that made the marker. Only the sweep has its own column. | Record the component that made each restored marker, as a count per component (for example `expands_by_component` in `request_components`). | No |
| G10 | The keep-alive settings in effect for each request. A config layer can hide a new default (the #1040 case), and no counter shows it. | Record the effective prefix floor (and the plan's ping count) on the ping row, or show the effective keep-alive config in `/stats`, with where each value came from (default, strategy table, user). | No |
| G8 | A tool the model needed but never searched for (D4), and a purge that ran (D9). | D4: only the #871 test sees it. D9: a purge counter. | No |
| G6 | Tool deferral disables, by reason (T1, T2, T4). The signals are on main (#900, 44f8318d). This gap closes when the next release ships. T4 needs the log, because the dashboard database does not store a disable. | Ship the next release. Optional: a disable flag on the `requests` row, so T4 is one query. | No |
| G11 | The proxy has no counter of pings that went out but were never priced. | An upstream counter, in a separate PR. | No |
| G12 | **Partly closed by #1137** (the `too_large` refusals and the manual-hold evictions get counters). Still open: the other skip reasons. The keeper's skip reason (`too large`, `below limits`, `over budget`, `no entry`, and others). It lives only in the keeper's memory and the cache-state endpoint. `/stats` and the database do not show it by reason, so a thread that is never pinged shows only an empty `keepalive_strategy_id`. On a hosted deployment, one 875k-token thread went 2 days with no ping (`too large`, #1100), and it took 3 investigations to find the reason. | Count skips by reason in `/stats` and as `cg_keepalive_skipped_total{reason}`. Also write the reason on the real request row (for example `keepalive_skip_reason`), so K2 and K10 can split misses by reason. | No |

## Monitor spec (draft)

- **Daily:** K1, K3, K4, K7, K8, K9, K10, K11, K12, K13, S1, S6, X1, X4, T1, T2, D2, D5, D6, D8. These catch a new defect fast, and each costs little to run.
- **Weekly:** K2, K5, K14, S2, S4, X2, X3, X6, D1, D3, D7, E1, E1b, E1c, E3, E4, and the per-component activity table.
- **Before each release:** the #778 tables, with 3 or more working days of data.
- **The report:** one table per run. For each invariant: value, threshold, the change since the
  last run, and `OK` or `BREACH`.
- **Alert:** a `BREACH`, or the same invariant within 20% of its threshold on 3 runs in a row.
- **Where the production part goes:** the internal repo (cg2, tenants, real thresholds, who gets
  the report).

## Technical notes

### Q1: pings and the turn after them

The #716 walk. It counts pings since the last real turn, per `(session_id, thread_id)`.

```python
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
LO, HI = int(sys.argv[2]), int(sys.argv[3])  # window, ms
rows = db.execute("""select r.session_id, coalesce(r.thread_id,''), r.ts, r.keepalive,
  coalesce(r.cache_read,0), coalesce(r.cache_write,0), coalesce(r.cost_usd,0), l.ping_n, r.max_tokens,
  coalesce(r.status,0), coalesce(r.upstream_ms,0)
  from requests r left join request_lifecycle l on l.request_id = r.id
  where r.ts >= ? and r.ts < ? order by 1, 2, 3""", (LO - 3600000, HI)).fetchall()
last, n, lastreal, agg = None, 0, None, {}
for s, th, ts, ka, cr, cw, cost, pn, mt, st, ms in rows:
    if (s, th) != last: last, n, lastreal = (s, th), 0, None
    if ka == 1: n += 1; continue
    if n > 0 and LO <= ts < HI and lastreal:
        gap = (ts - lastreal) / 60000
        b = "<5" if gap < 5 else "5-9" if gap < 9 else "9-13" if gap < 13 else "13-60" if gap < 60 else ">60"
        a = agg.setdefault((min(n, 3), b), [0, 0, 0.0]); a[0] += 1
        if cw > cr: a[1] += 1; a[2] += cost
    n, lastreal = 0, ts
for k in sorted(agg): print(k, agg[k])
```

K1 and K4, by ping number and kind:

```sql
select l.ping_n, r.max_tokens = 0 as sweep_question, r.model, count(*) as sent,
  sum(r.cache_write > r.cache_read) as missed,
  sum(r.status >= 400) as failed, sum(r.upstream_ms >= 45000) as slow
from requests r join request_lifecycle l on l.request_id = r.id
where r.keepalive = 1 and r.ts >= :lo and r.ts < :hi
group by 1, 2, 3;
```

### Q2: the ping sequence after each real turn

For each idle span, the kinds of the pings in order (P or Q). "Text turn" is the proxy from G1:
the real request before the held one did not stop with `tool_use`.

```python
# rows: requests in (session_id, thread_id, ts) order: keepalive, max_tokens, stop_reason
# For each span of keepalive rows after a real request:
#   kinds = ''.join('Q' if max_tokens == 0 else 'P' for each ping)
#   text  = reals[-2].stop_reason != 'tool_use'
# K3 breach: text and kinds.startswith('Q')
```

### S1, S2

```sql
-- S1: sweep applies by cache label
select coalesce(r.cache_miss_reason, '') as reason, count(*)
from request_components c join requests r on r.id = c.request_id
where c.component = 'extract_llm_sweep'
  and json_extract(c.events, '$.sweep_applied_cold') > 0
  and r.ts >= :lo and r.ts < :hi
group by 1;

-- S2: the sweep's saving and its cost
select sum(c.saved_usd) from request_components c join requests r on r.id = c.request_id
where c.component = 'extract_llm_sweep' and r.ts >= :lo and r.ts < :hi;
select sum(cost_usd) from extraction_calls
where component = 'extract_llm_sweep' and ts >= :lo and ts < :hi;
```

### X4

```sql
select count(*) from requests
where cache_miss_reason = 'hit' and keepalive = 0
  and cache_write > cache_read and cache_write > 10000
  and ts >= :lo and ts < :hi;
```

### Q3: long waits by thread type

```python
# rows: requests in (session_id, thread_id, ts) order.
# thread type: 'sub' when thread_id starts with 'a:', else 'main'.
# A long wait: two real requests on one thread more than 300 s apart.
# For each long wait: the pings between them; the turn after missed when cache_write > cache_read;
# the prefix band from the held turn's cache_read + cache_write + fresh_input.
# Per day and thread type: long waits, pings per wait, no-ping misses by band,
# and saved = 1 - sum(cost_usd) / sum(baseline_cost_usd) on real requests.
```

### Q4: credit for warm turns after a ping

```python
# rows: requests in (session_id, thread_id, ts) order.
# For each real request R (keepalive = 0) with a previous real request P on the same thread:
#   gap    = R.ts - P.ts
#   pinged = any keepalive = 1 row on the thread with P.ts < ts < R.ts
#   warm   = R.cache_read > R.cache_write
# Count R when gap >= 300 s and pinged and warm.
# Per day and host: counted turns, turns with keepalive_saved_usd > 0, and the share.
# K11 breach: share < 80% with 10 or more turns, or share = 0% with 10 or more turns.
```

- **Reading #1137's counters on a hosted deployment:** `/metrics` is open only to loopback, or with the `METRICS_TOKEN` bearer. A monitor on the box reads it from `127.0.0.1` with no credential. `cg_tenant_keepalive_manual_hold_bytes` carries a tenant id and label, and the WARN line carries a tenant id. The monitor must aggregate them over tenants, and report no tenant id.
