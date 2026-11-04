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

| # | Kind | Statement | Signal | Threshold (proposed) | W1 | W2 |
|---|---|---|---|---|---|---|
| K1 | side effect | A planned ping reads the prefix and does not write it again. | `requests.keepalive = 1`, `request_lifecycle.ping_n`; miss = `cache_write > cache_read`. Split by `ping_n` and `model`. | over 2% for any `ping_n` or model | ping 1: 0 of 528. **Ping 2: 90 of 377 (23.9%, $113.66).** | ping 1: 0 of 44. Ping 2: 0 of 35. |
| K2 | intent | A real turn that comes back inside the plan's warm period reads the cache. | The #716 walk ([Query Q1](#q1-pings-and-the-turn-after-them)). Gap from the last real turn, in buckets. On the 2-ping plan, warm is under 13 min. | over 5% missed | after 2 pings, 9 to 13 min: 12 of 76 missed ($34.95). After 1 ping, 5 to 9 min: 17 of 117 ($21.87). | 0 of 20 |
| K3 | contract | After a text turn, no substitute takes a planned ping (#779). | Ping sequence per idle span ([Query Q2](#q2-the-ping-sequence-after-each-real-turn)). P = plain ping, Q = sweep question (`max_tokens = 0`). | any span that starts with Q after a text turn | 103 of 199 text-turn spans started with Q | **0 of 10** |
| K4 | intent | Pings do not fail or time out. | ping rows with `status >= 400` or `upstream_ms >= 45000` | over 1% | 3 of 905 | 0 of 84 |
| K5 | intent | Keep-alive saves more than its pings cost. | `/api/stats?since=<ms>`: `keepalive_net_usd` | below $0 over 7 days | +$66.50 | +$17.94 |
| K6 | contract | The extra slot after the last planned ping sends at most one request per idle span, inside the hold limit. | rows with `ping_n = 3` and `max_tokens = 0`; `/stats` `keepalive_sweep_extra_asked` | more than 1 per span | — | 5 spans, 1 request each, wrote 0 to 6 tokens; `keepalive_sweep_extra_asked` = 5 |

- **K2 on 13 to 60 min is not a breach.** A user who comes back after the plan ends pays the
  re-write that the strategy accepts. The monitor reports these numbers but does not alert on them.
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
| S5 | side effect | The sweep question renews the cache entry it reads. | substitute rows (`keepalive = 1`, `max_tokens = 0`): miss rate, and the miss rate of the next ping | over 2% | 0 of 353 wrote; the next ping missed 90 times (#776) | 0 of 36 |

- **Owner:** David Amid (proposed; the sweep row in `docs/data-dependence.md`).

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
pays one cache rewrite. 3 reasons:

1. `unknown_description`: the description hash is unknown. A new Claude Code version is likely.
2. `non_select_query_for_our_tool`: a query names a tool the proxy deferred, but not in the
   `select:` form. A format change is likely.
3. `break`: the model called a deferred tool that was never loaded, or got a "not loaded" error.

| # | Kind | Statement | Signal | Threshold (proposed) | Baseline |
|---|---|---|---|---|---|
| T1 | side effect | No user loses a tool. | `cg_tooldefer_client_disabled_total{reason="break"}`; `/stats` `tool_defer.client_search_disabled.break` | **any** | not shipped yet |
| T2 | intent | A Claude Code release does not silently stop option B. | `cg_tooldefer_client_disabled_total{reason="unknown_description"}`, with the `desc_hash` from the log line and the Claude Code version | any; report the version and the hash | not shipped yet |
| T3 | intent | The `select:` form is the only form used for the proxy's tools. | `cg_tooldefer_client_disabled_total{reason="non_select_query_for_our_tool"}` | any | not shipped yet |
| T4 | side effect | A disable costs at most one cache rewrite on the live thread. `break` costs exactly one. `non_select_query_for_our_tool` happens on a live thread and costs at most one (the #900 review must confirm this). `unknown_description` is decided at thread start and costs none. | for each `break` or `non_select_query_for_our_tool` log line: misses (`cache_write > cache_read`) on the later requests of that `session` and `thread` | more than 1 miss per disable | not shipped yet |

- **Signals (from PR #900, open and not reviewed; names can still change):**
  - Metric: `cg_tooldefer_client_disabled_total{reason}`, with `reason` one of
    `unknown_description`, `non_select_query_for_our_tool`, `break`.
  - `/stats`: `tool_defer.client_search_disabled`, an object with the same 3 reasons.
  - Log (warn): `cg.tooldefer_client_search_disabled`, with `reason`, `desc_hash` (8 characters),
    `session` and `thread`. It holds no query text.
- **How it counts:** `unknown_description` and `non_select_query_for_our_tool` count once per
  tenant and hash per 24 h. `break` counts every time. So T2 and T3 count disables, not events.
- **Disable scope:** per tenant and description hash, for 24 h. A proxy restart clears it.
- **Known-good hash today:** `88033eceb421f1c2`. It was the only `ToolSearch` description in 109
  captures. A T2 alert with any other hash means a new description.
- **T4 needs a join** between the log line and the `requests` table on `session` and `thread`.
  The dashboard database does not store the disable today, so T4 reads the log.
- **Report:** every disable event, with its reason, and the Claude Code version if it is known.
  **A rise in `break` is the most urgent,** because a user lost a tool for one turn.
- **No laptop baseline:** the laptop runs `xhigh`, not `ultrahigh`.
- **Owner:** David Amid (proposed).

## Cross-component invariants

| # | Statement | Signal | Threshold (proposed) | Baseline |
|---|---|---|---|---|
| X1 | No request that Context Guru sends by itself re-writes the prefix. | every `keepalive = 1` row; miss = `cache_write > cache_read` | over 2% | see K1, S5 |
| X2 | The total saving is not negative. | `/api/stats?since=`: `total_saved_usd` | below $0 over 7 days | W1 $183.20; W2 $28.99 |
| X3 | The share of requests that miss does not rise after a release. | non-ping rows with `cache_write > cache_read`, per version window | over +3 points against the last release | to measure |
| X4 | A request labelled `hit` read its cache. | `cache_miss_reason = 'hit'` and `cache_write > cache_read + 10000` | 0, except a first request of a thread | **34 later requests; 3 in W2** (B2) |
| X5 | A component's own action is not followed by another component's cost. | Q2 generalized: for each request kind, the miss rate of the next request in the thread | to define | to measure |

## Suspected breaches

| # | What | Found by | Next step |
|---|---|---|---|
| B1 | **Two counters give different false-drop rates for the sweep.** `/stats` says 28.7% (39 of 136) since the restart. The database says 6.7% (9 of 135) in the same window. 3 of 6 threads are at 33% to 56% in `/stats`. | S4 | Find what each counter counts. If 28.7% is right, the sweep removes too much. |
| B2 | **Requests labelled `hit` that wrote the prefix again.** 260 non-ping rows have `cache_miss_reason = 'hit'` and `cache_write > cache_read + 10000`. 226 are a thread's first request. 34 are later, and 3 of those are in W2. 4 of them had a sweep apply. #759 meant to label a partial read as a miss. | X4, S1 | Check how `cache_miss_reason` is set for a first request and for a partial read. |
| B3 | **`toolfilter` changes requests but reports `acted = 0`.** `mutated` is 3,204, the same as `/stats` `decl_filter_requests`. | the activity table | Check which flag `toolfilter` sets. The dashboard may undercount it. |
| B4 | **`cachesplit` never acted** on 12,265 requests. `split_requests` and `cachesplit_saved_usd` are 0. | the activity table | Check if Claude Code requests have anything to split, or if the component is broken. |

## Gaps

| # | What we cannot measure | What would fix it | Needs content? |
|---|---|---|---|
| G1 | Whether the held request ended at a text turn. K3 uses a proxy: the `stop_reason` of the real request before it. | Store the keeper's `endsAtText` flag on each ping row. | No |
| G2 | `expand` gives back the original byte for byte. | A short hash of the original and of what `expand` returned, per expand event. | No (a hash is metadata) |
| G3 | The kind of a keep-alive request. Today `max_tokens = 0` means "sweep question". This is a convention, not a field. | A `ping_kind` column: plain, sweep, summary, extra, manual. | No |
| G4 | `/stats` counters reset on each restart. | Write the counters that the monitor needs into the database. | No |
| G5 | The 12 components with no laptop baseline. | Real traffic on other presets. | No |
| G7 | A `cg_*` metric for tool deferral. Only `/stats` and the database show it today. | Export the `/stats` `tool_defer` counters as `cg_tooldefer_*`. | No |
| G8 | A tool the model needed but never searched for (D4), and a purge that ran (D9). | D4: only the #871 test sees it. D9: a purge counter. | No |
| G6 | Tool deferral disables, by reason (T1 to T4). The names exist in PR #900, which is not merged. T4 needs the log, because the dashboard database does not store a disable. | Merge #900. Optional: a disable flag on the `requests` row, so T4 is one query. | No |

## Monitor spec (draft)

- **Daily:** K1, K3, K4, S1, X1, X4, T1, T2, T3, D2, D5, D6, D8. These catch a new defect fast, and each costs little to run.
- **Weekly:** K2, K5, S2, S4, X2, X3, D1, D3, D7, and the per-component activity table.
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
