# Config & routes reference

## Routes & headers

The proxy serves both provider dialects on one port (default `:4000`).

### Routes

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

#### Request-size ceiling — 32 MiB by default, 128 MiB for compaction

`/openai/v1/chat/completions` and `/anthropic/v1/messages` refuse a request body over 32 MiB
with `413 Request Entity Too Large` — buffering more than that would risk exhausting proxy
memory on a single request. Two exceptions, both because the whole point of the request is
carrying a transcript that is, by definition, already this large:

- **The agent's own compaction request** (see [agent-compaction.md](../how-to/agent-compaction.md))
  — detected the same way it already is for the pipeline bypass — may read up to 128 MiB. It
  must be forwarded byte-identical regardless of size, so relaxing the *read* ceiling for it
  costs nothing extra: no pipeline component runs on it either way.
- **`/compact`** always reads up to 128 MiB, unconditionally — every caller of this endpoint
  wants exactly this: a body already larger than 32 MiB is the normal, intended input, never a
  reason to refuse up front.

128 MiB is still a hard ceiling, not "no limit" — a request larger than that is refused
regardless of content. `/anthropic/v1/messages/count_tokens` is NOT exempt (no pipeline stage
would benefit from a higher ceiling — it forwards verbatim either way); it still refuses
anything over 32 MiB, now with an honest `413` rather than a generic `400`.

#### `count_tokens` answers about the ORIGINAL body, and that is deliberate

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

#### Bob (BobShell) gateway routes

Mounted only when `--bob-upstream` / `BOB_UPSTREAM` is set, or in hosted mode. Without
either, an unknown path still 404s exactly as before.

| Route | Purpose |
|---|---|
| `POST /inference/v1/chat/completions` | Bob's model call. OpenAI-compatible, so it runs the pipeline like any OpenAI chat request. |
| `/` (catch-all) | Verbatim passthrough for Bob's control-plane calls (`/admin/v1/profile`, `/inference/v1/model/info`, …), which must not be rewritten or the CLI cannot boot. Less specific than every route above, so it only receives what nothing else matched. |

!!! note "`POST /compact` (compaction-service mode)"
    The [llm-d compaction service example](../examples/llm-d-service.md) uses this route with
    the store disabled and `marker_mode: off`, so the returned body is clean, marker-free and
    directly usable. See [llm-d compaction service example](../examples/llm-d-service.md).

### `GET /stats`

Fields are only ever **added** to this payload, so a consumer that reads by key keeps working.
Two different consumers make that a hard rule: `deploy/harbor/*.py` reads `runs`, `acted` and
`saved_tokens` off the per-component objects (`measure.py` by direct index, so a removal is a
`KeyError`), and `/metrics` re-publishes much of the rest as `cg_*` series that off-repo alert
rules and dashboards are written against — where a rename breaks monitoring *silently*. The tables below group the `Snapshot` struct in
`metrics/metrics.go`; that struct is the authority, and it grows.

#### Savings

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
| `adjudicate_stray` | Times the **agent** called `context_guru_adjudicate`, the verdict tool the proxy injects for [`extract_llm_sweep`](../components/advanced-offload.md#extract_llm_sweep) and tells the model not to call. Counted whether the call was answered on the response path or repaired on the next request. Most cost the agent nothing, because the response path answers them in band before the client is written to; one co-called with a *client* tool in the same assistant turn costs one turn, because the loop hands that round over whole and the next request's repair fixes it. Either way this is a *description-health* number rather than an error: non-zero means the "do not call this yourself" wording has stopped working. Measured 0 over three benchmark passes. Also exported as `cg_adjudicate_stray_total`. |
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

#### Extraction economics (`extract`)

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

#### Context-guru's own cost

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

#### Provider-billed token tiers, and the honest ratios

Summed from response `usage` blocks, so all four are zero against an upstream that
reports no usage — **or against one whose usage this proxy could not read**, which is a different
thing and is counted separately. See [when the tiers read zero](#when-the-tiers-read-zero).

| Field | Meaning |
|---|---|
| `fresh_input_tokens` · `cache_read_tokens` · `cache_write_tokens` · `output_tokens` | The four billed tiers. |
| `usage_unparsed` | Responses that carried a usage block in a **spelling this proxy does not recognise**, so the four tiers above were recorded as 0 on an otherwise healthy request. Non-zero means token accounting is offline for some route or provider. Counts **responses**, not requests — one request can drive several upstream rounds through the expand loop — so do not read it as a fraction of `requests`. |
| `usage_unreadable` | Responses whose examined bytes would not parse at all, so usage could not be sought — a spliced head+tail sniffer window rather than an unrecognised dialect. Different fix; see below. |
| `attempted_tokens` | What compaction was **allowed** to touch this window — the uncached tail when cache-aware. |
| `frozen_tokens` | What cache-awareness deliberately left alone. Its benefit is the cache reads that stayed cheap; this is its cost. |
| `savings_pct_attempted` | `saved ÷ attempted`. The ratio to quote: `savings_pct`'s whole-request denominator recounts the transcript every turn and trends to ~0% on a long session. `0` when nothing was attempted. |
| `savings_pct_new_input` | `saved ÷ (fresh + cache_write + saved)` — savings as a fraction of what would newly have entered the provider. Reported as `0`, **never 100**, when the provider reported no usage: savings must not be divided by themselves. |

##### When the tiers read zero

Four token fields at 0 on a 200 with correct savings, correct latency and correct everything else
used to mean **five** different things, with nothing to tell them apart:

| Cause | `usage_miss` in the log | Counted | What to do |
|---|---|---|---|
| The provider genuinely reported no usage (an OpenAI response without `stream_options`) | `absent` | — | nothing |
| A recognised block whose every tier is zero | `all_zero` | — | nothing; legitimate on some responses |
| Empty response body | `no_body` | — | nothing |
| A usage block in an **unrecognised dialect** (Bedrock Converse camelCase `usage.inputTokens`, or a nested `response.usage`) | `unparsed_dialect` | `usage_unparsed` | **alert.** Add the dialect — the DEBUG record below names it |
| The examined bytes were not a whole document, so the block was hidden | `unreadable_body` | `usage_unreadable` | **alert.** Raise `sniffMax` or buffer the body; do *not* go hunting for a missing field name |

Only the last two are failures, and only they are counted — the number an operator watches to
confirm accounting is healthy must not be incremented by it being broken. The reason for every
miss, benign ones included, rides on the `cg.request` log line as `usage_miss`.

**Which of the two you can even get depends on the response path**, and this has already misled
two readers, so: `unreadable_body` can arise **only** on the sniffed path — the one taken when
neither proxy-injected tool is advertised on the request (`proxy.go`, `if !advertised`), where usage
is read from a bounded head+tail window instead of the whole body. When either tool *is* advertised
— which `inject_expand: always` guarantees from the first turn — a non-streamed response is read
whole and the window never applies, so a miss there is a dialect or a genuine absence and nothing
else. `valid_json` in the record below settles it either way without needing to know the path.

**The shape record.** On the first unaccounted response per process, `cg.usage_unaccounted` is
logged at DEBUG with the response's **key names** — top-level keys, where a usage block was found,
and the key names inside it — and nothing else. No values and no body: a body dump on agent traffic
writes kilobytes of transcript per response, and this record cannot, by construction. It is what
turns "usage_reported is false" into "the provider is sending camelCase", from any deployment that
hits the gap, without a capture rig or an extra request.

This ran undetected for **4,015 of 4,015 requests** in one benchmark iteration and was found two
iterations later, in a post-mortem chasing a different question.

#### SSE streaming health

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

#### Freeze-replay health

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

#### Rewind reserve (reversibility health)

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
| `stash_missing` | Marker replays that found **no payload** behind them: a dangling `<<cg:HASH>>` went upstream. This one *is* a broken promise. A replay re-stashes the payload it re-derived, so this fires only when that write was **also** refused — raise the reserve, and `stash_ttl_seconds` if `stash_expired` is what is taking them. |
| `stash_expired` | Payloads reclaimed by their own TTL (`stash_ttl_seconds`, shorter than `ttl_seconds`). **Not an alert on its own** — read it against `stash_revived`. |
| `stash_revived` | Reclaimed payloads written again by a later replay, which re-derives them from the transcript **before** the marker goes upstream: reclamation absorbed at no cost. Tracking `stash_expired` means the shorter payload TTL is working as designed; see [why payloads expire sooner](#why-payloads-expire-sooner-than-decisions). |

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

#### What an expand costs

| Field | Meaning |
|---|---|
| `expand_prefix_flips` | Turns where an **established** compaction was abandoned because the agent had expanded that content, so the original went upstream in full at its cached position — a suffix cache-write attributable to expansion, at ~11.5× a read. |

Deliberate rather than a defect: re-compacting would loop the agent into another expand, and one
cache-write is cheaper than an unbounded loop. Counting it makes the trade visible, since no other
counter distinguishes an expand-induced cache-write from any other.

It grows **per turn per message**, not per distinct content: every later turn re-sends the same
original and the same abandonment is observed again, while only the first is a real cache-write. Read
it as "expansion is churning cached prefixes here".

It also counts **one event**, not every expand-induced cache-write: a replay declined because the
content was expanded. At least one sibling is uncounted — protecting expanded content shortens
`summarize`'s span, which can invalidate a checkpoint and force a re-summary, and different summary
text at a fixed prefix position is another suffix cache-write. So a zero here does not mean expansion
cost nothing. The per-component gate
`kept_verbatim_after_expand` is the per-message view, and `already_marked` is its benign sibling —
the two were one label (`marker_or_kept_verbatim`) until they were split. Full picture in
[what an expand costs](../how-to/recover-context.md#what-an-expand-costs-across-turns).

#### cmdfilter attribution

| Field | Meaning |
|---|---|
| `cmdfilter_families` | Per command family (`builds` / `tests` / `iac` / `pkg` / `net` / `other`): `acts`, `saved_tokens`, `saved_tokens_unique`. |
| `cmdfilter_filters` | The same, per individual filter — which filters actually earn their place. |
| `cmdfilter_selector_misses` | Output shapes that matched **no** filter, frequency-ranked. The backlog of filters worth writing. Bounded at 200 distinct selectors, first-seen wins. |

#### Operating mode

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
### `GET /metrics` — the two families do not agree

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

#### Reading `acted: 0`

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

### Dashboard routes (`--dashboard`)

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
| `GET /api/sessions/{session}/transcript` | One session's before/after content for the compaction-diff view, oldest first, with the component rows per request. Reports one of nine transcript states (see [dashboard](../dashboard.md)), plus [`content_captured` and `capture_blocked_by`](#why-a-transcript-panel-is-empty). **Lazy**: it touches the local database only and reports `cold` unless `?fetch=1`, so opening the view never costs an rclone round trip. |
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

#### The config route serves the SERVER's configuration, not yours

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

#### Why a transcript panel is empty

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

#### Filter parameters

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

#### Access gating

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

### Control-plane routes (hosted mode only)

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

#### The tenant view's configuration fields

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

### Per-request headers

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

See [Config & environment](#config-environment) for flags and env vars, and
[Presets](presets.md) for the built-in pipelines.

## Config & environment

One strict YAML struct serves both hosts (the proxy loads a file; the external plugin
plugin hands its `config:` subtree to the same loader). A `preset` expands to a
default pipeline; explicit fields override it.

### Config shape

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

#### `store`

| Field | Default | Purpose |
|---|---|---|
| `enabled` | `true` | Toggles the state store. `false` wires a `store.Nop`: nothing is stashed, so offloads become **one-way** and must run `marker_mode: off`. |
| `ttl_seconds` | `10000` | Entry lifetime, and it **slides** — a `Get` refreshes the deadline, so an entry replayed every turn never ages out. Raised from 1800 because Terminal-Bench tasks average ~1975 s of wall clock and run to 4 h, so the old default expired live frozen decisions mid-task. |
| `max_entries` | `5000` | LRU cap. Two groups of keys are **exempt** from LRU eviction, and they behave differently when full — see below. Eviction reclaims **expired** entries first, exempt ones included. Raised from 1,000: one process-wide store serves every concurrent session, and a single reversible removal writes five entries (the payload, `cg:own:`, `cg:xseen:`, and the two pinned decision records), so 1,000 was an order of magnitude under the observed volume. |
| `stash_ttl_seconds` | `1800` | The **rewind payloads'** own entry lifetime, shorter than `ttl_seconds` and capped by it (the cap is silent, so `/config` reports the **effective** value, not the configured one). A payload is re-derivable from the transcript, a frozen decision is not — see [the two horizons](#why-payloads-expire-sooner-than-decisions) below. |
| `stash_max_bytes` | `268435456` (256 MiB) | What the **rewind reserve** may cost in memory. Entries are a poor proxy for it in this one namespace: every other exempt entry is a marker line or an integer, a rewind payload is a whole tool output. Whichever of `max_entries` and this binds first, binds. |
| `max_sessions` | `100` | Cap on per-session sticky-id sets. |

##### The two exemptions, and the floor under them

**Pinned decisions** (`cg:frz:`, `cg:res:`, `cg:xres:`, `cg:len:`, `cg:ttl:`, `cg:seen:`) are
exempt because losing one is cache-destructive rather than merely a miss: the replacement bytes
for an already-cached message stop being reproducible, so the message flips and the provider
re-writes the whole suffix at ~11.5x the read price. Over its cap a pin simply becomes an
ordinary evictable entry.

**The rewind reserve** holds the payloads behind `<<cg:HASH>>` markers. It cannot be a pin
prefix — a payload key *is* the marker id, a bare content hash the model reads out of the
request — so it is claimed explicitly and, unlike a pin, a payload that cannot be admitted is
**refused**: the component declines the removal and leaves the content verbatim rather than
stamping a marker nothing can resolve. Only the TTL releases a slot — which is why payloads get a
shorter one than everything else.

Each exemption is capped at half `max_entries`, and **a quarter of `max_entries` is held back
from both** so something is always evictable. Without that floor the two could occupy the whole
cap, and a cache with nothing evictable does not fail loudly: the next write to an unpinned
namespace is evicted by its own insert, silently turning `cg:keep:` (the flag that stops the
expand loop), `cg:sum:` (summarize checkpoints) and `cg:own:` (which gates `GET /expand`) into
no-ops.

**When `stash_refused` rises**, raise `max_entries` or `stash_max_bytes` — read `stash_live`
against `stash_capacity` and `stash_bytes` against `stash_max_bytes` at
[`/stats`](#get-stats) to see which budget bound. Nothing became irreversible: the
removals did not happen. `stash_missing` is the different, worse number — a marker replayed with
no payload behind it. A replay re-stashes the payload it re-derived, so `stash_missing` only fires
when that write was **also** refused: raise the reserve first, and `stash_ttl_seconds` if
`stash_expired` is what is taking the payloads.

##### Why payloads expire sooner than decisions

`ttl_seconds` is sized for a **frozen decision** — the replacement bytes the provider already
cached. Nothing else holds them, so losing one flips an already-cached message and re-writes the
whole suffix at ~11.5x the read price; it has to survive a long-horizon task, idle gaps included.

A **payload** is a copy of content the agent re-sends every turn, so it needs a much shorter
horizon. Every offloader replays its frozen decision on **every turn**, regardless of the
cache-tail gate (it must, or the message reverts full→compacted→full and churns the KV cache), and
that replay re-stashes the payload from the message text it just read. So a live marker's payload
has its deadline slid every turn, and one the TTL already took is **re-created on the request
path** — before the request goes upstream, and therefore before any `expand` call in the response
could ask for it. `stash_revived` counts exactly that.

Giving both namespaces `ttl_seconds` meant one busy period could hold the reserve saturated for
~2.8 h after the load that filled it, refusing every removal process-wide the whole time. The
split shrinks that hangover to `stash_ttl_seconds` without making any removal irreversible.

**Which offloaders re-derive, because it is not all of them:**

| Offloader | Per-turn re-stash | If its payload is reclaimed |
|---|---|---|
| `mask`, `cmdfilter`, `collapse`, `failed_run`, `skeleton`, `readlifecycle`, `agentdiet` | `reapplyFrozen` → `commitRefresh`, every turn regardless of the tail gate | re-created on the request path; `stash_revived` |
| `summarize` | **every turn once a checkpoint exists**, regardless of its trigger or whether a model is available — `replayStale`/`tryReuse` → `commitRefresh` | re-created on the request path; `stash_missing` if the payload has already gone, and the replay proceeds anyway because the summary text must stay byte-identical |
| `extract_llm` | only past its own gate — `no_goal_keywords` | a skipped turn refreshes nothing, but it splices nothing either, so no marker of its own dangles while the skip lasts |
| `dedup`, `extract`, `linecap`, `smartcrush` | **none** — no replay path at all; they redo the transformation from the re-sent original through the *refusable* `commitMark` | once reclaimed it is a new stash, so a saturated reserve **refuses** and the message goes upstream verbatim after earlier turns sent it compacted: `stash_refused` **plus a representation flip** |

That last row is worth reading twice, because `stash_refused`'s own description promises "nothing
became irreversible" — true about reversibility, and silent about the cache-write actually paid. It
is reachable at `ttl_seconds` too, so it is not new; a shorter payload horizon shortens the distance
to it.

`summarize`'s trigger skip is a **fill** skip: `trigger.min_request_frac` defaults to `0.9`, so a turn
fires only once the provider has billed past 90% of **C** — the point where the client's own compaction
would act (see `Ctx.FillDenominator`). A long session therefore skips its early turns and then fires.

**`trigger.cache_state` defaults to `any`, and that is a retraction.** For one release it defaulted to
`pre_expiry_or_cold`: fire only when the prompt cache was within a minute of expiring, or had already
expired past a clock-skew allowance. The reasoning was that compaction should wait until invalidating
the cached prefix was free. It does not survive its own measurements.

**Measured, on a real Claude Code session:** 0 of 53 turns fired under that default, with
`cache_state_declined_warm` on 51 of them. The fill gate was not the obstacle — the session reached
0.996 of the window and 12 turns were over the 0.9 threshold — the **idle time** was. `pre_expiry`
needs roughly 240s of idle on a 5-minute entry, and the largest gap in that session was 44s.

Three things retired it:

1. **The downside it avoided is −$0.84.** Across the whole corpus, that is the total cost of every
   session that crossed 90% and then simply ended — which is the entire population a warm-cache fire
   can waste money on. Against a $531 upside, the gate was insuring a rounding error.
2. **Firing warm pays back in 2-3 turns.** At 0.9 of 200k a 180k prefix compacts to roughly 40k. The
   compacting turn writes 40k at 1.25x instead of reading 180k at 0.1x — about 32k base-equivalents
   more — and every later turn then reads 40k instead of 180k, saving about 14k. The ratio is
   rate-independent, so it is the same arithmetic on haiku and opus.
3. **A cold gate cannot prevent the first cold rewrite.** Compaction is two-turn: the turn that fires
   commissions a summary and forwards the transcript **untouched**, and a later turn splices. So the
   turn that first sees a cold cache is carrying the full prefix and pays that rewrite in full.
   Waiting for cold pays the first of the median 7 rewrites and prevents the rest; compacting while
   warm prevents all 7.

**What the cache-aware work was worth stands.** `Fires` resolves the fraction against the provider's
billed input rather than our own token count, and against C rather than the raw context window. Those
are what made "0.9" denote anything at all. The gate on top of them did not.

**`pre_expiry` survives, for a different question — and nothing asks it yet.** A summarizer whose model
call reuses the conversation's own prefix needs that prefix **live**, which is genuinely
phase-dependent. `summarize` is not such a summarizer: it flattens its prompt into a single string and
shares no prefix with anything, so setting `pre_expiry` here is honoured but only makes it fire less.
The prefix-reusing component is
[`cache_aware_summarizer`](../components/advanced-offload.md#cache_aware_summarizer), and it consults neither
`CacheAllows` nor `CachePhase` — so the key is **silently inert** there today (the same defect as #247).
The value is kept for when that is wired up, because no other value can express "is there a live prefix
to hit". `cold` and `pre_expiry_or_cold` are **withdrawn**: a config carrying either is refused at
startup with the replacement named, rather than silently migrated to a value that fires at a different
moment.

(The agent's own compaction is a second route to the same skip — it shrinks the incoming request and
can drop it back under `min_request_tokens` for several consecutive turns.)

That is only safe because a skipped turn still **splices**. Once a checkpoint exists, every later turn
re-emits the same summary bytes from it, with no model call, whichever gate declined and whether or
not a summarizer is configured — see the first row of the table above. A skip that forwarded the full
transcript instead would diverge from the cached prefix at the first summarized message and force a
full-suffix cache-write, so under a rarely-firing trigger the component would cost money on nearly
every turn to save it on a few.

The exposure this leaves, stated plainly: a turn that runs **no pipeline** performs no refresh (an
`x-context-guru-bypass` request, or the agent-compaction bypass), so an unbroken run of bypassed
turns longer than `stash_ttl_seconds` could outlive a payload whose marker is still live. Both are
single-request events in practice, and on the first row above the outcome is the reported one —
`stash_missing` — not a silent loss.

**`stash_expired` and `stash_revived` both at zero means the reserve never bound**, not that the
horizon is working: the sweep runs only once a budget is already binding, and an expired-but-unswept
payload is resurrected in place by the next write. What tells the two apart is `stash_refused` and
`stash_live` against `stash_capacity`.

The pinned prefixes are a code-level property of the key layout, supplied by their owners via
`store.Options.PinPrefixes` — not a YAML knob.

#### `mode`

| Value | Behavior |
|---|---|
| `sync` (default) | Compact inline; the caller waits. Byte-identical to the behavior before modes existed. |
| `observe` | Forward the request untouched and report what compaction *would* have saved, under `potential_*` / `projected_*` keys. The request path never runs the pipeline and skips `expand.Inject` too (a tool declaration is a modification), so byte-identity is **structural**. |

Always explicit — nothing infers it from the rest of the configuration.

!!! note "An `async` mode is designed but not shipped"
    A third mode deferring compaction off the request path is implemented on a separate
    branch and deliberately held pending a benchmark arm establishing a benefit. `sync` and
    `observe` are the only values the loader accepts.

#### `observe`

| Field | Default | Purpose |
|---|---|---|
| `max_queue` | `256` | Bound on the off-path measurement queue. A full queue **drops** (counted as `dropped`) and never blocks the request path. |
| `workers` | `1` | Drain goroutines. One keeps a single measurement's cheap-model call in flight per process, which keeps that spend and gateway rate limits predictable. |

!!! warning "Strict: unknown keys are rejected"
    The YAML loader runs with `KnownFields(true)`, so a typo'd key fails loudly
    at load time rather than being silently ignored.

### Example

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

### Flags & environment

| Flag / env | Default | Purpose |
|---|---|---|
| `--preset` / `PRESET` | `house` | Pipeline preset when no `--config`. `codesmart` is the SWE-bench arm and must be asked for by name. |
| `--config` / `CONFIG` | — | YAML config file (overrides preset). |
| `--listen` / `LISTEN_ADDR` | `:4000` | Listen address. The flag exists so the port is visible in `ps` and to a supervisor; before it, the address reached the process only through the environment. |
| `--version` | — | Print version and commit, then exit. |
| `--idle-exit` / `IDLE_EXIT` | `0` (never) | Exit after this long with **no requests and no keep-alive ping pending**, so a proxy started on demand does not outlive its use. Refused at startup below `max(2 × store.ttl_seconds, 1h)` — 5h33m20s at the default TTL — because exiting clears the in-memory store, and losing a frozen decision re-bills its whole prefix as cache creation. Also refused together with `--upstreams`: a gateway serving other people's agents must not self-terminate. Liveness probes (`/healthz`, `/metrics`) deliberately do **not** count as activity; anything else does, including the dashboard's own polling. |
| `--openai-upstream` / `OPENAI_UPSTREAM` | `https://api.openai.com` | OpenAI upstream base. |
| `--anthropic-upstream` / `ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | Anthropic upstream base. |
| `--bob-upstream` / `BOB_UPSTREAM` | — | Bob (BobShell) backend base. Setting it mounts the [Bob gateway routes](#bob-bobshell-gateway-routes); unset, an unknown path 404s as before. |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | — | Real key injected on forward (gateway mode); empty = pass client auth through. |
| `CHEAP_MODEL` (+ `CHEAP_MODEL_BASE` / `_KEY` / `_AUTH` / `_PROVIDER`) | — | Dedicated cheap model for the LLM components (`extract_llm`, `summarize`) — the `model.source: config` client. Without it they no-op. |
| `FORCE_MODEL` | — | Overwrite the request `model` (eval-containers uses `EVAL_MODEL`). |
| `INJECT_EXPAND` | `auto` | Whether the `context_guru_expand` tool is advertised: `auto` (whenever the request declares tools, the store persists, **and the pipeline contains at least one Offload** — all three session-stable, so the `tools` array never changes shape mid-session and the prompt-cache prefix survives) \| `always` (unconditional, including when the request declares no tools) \| `never`. The pipeline condition exists because a pipeline that mints no markers would be advertising a tool whose every call must fail. |
| `CACHE_MODE` | `auto` | Cache-aware compaction: `auto` (on when the agent sets its own breakpoints) \| `on` \| `off`. |
| `MODEL_INFO_URL` / `MODEL_INFO` | LiteLLM map | Source for context-window sizes (used by the fractional triggers). `MODEL_INFO=off` disables the lookup; fractions are then ignored and absolutes apply. |
| `MODEL_PRICES` | — | Path to an **operator price list**, consulted before the public map. See below. A file that fails to load is fatal. |
| `--store` / `STORE` | on | Enable/disable the state store; `--store=false` disables offload reversibility. Wins over the file's `store:` block. |
| `--mode` / `MODE` | `sync` | Operating mode: `sync` \| `observe`. Wins over the file's `mode:`. |

#### Per-model prices, and why the public map is not enough

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

#### Extraction-model pricing

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
### Dashboard

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

#### Example (container)

```sh
DASHBOARD=true \
DASHBOARD_DB=/var/lib/context-guru/dashboard.db \
DASHBOARD_RETENTION=720h \
DASHBOARD_MAX_BYTES=2147483648 \
DASHBOARD_TRUSTED_CIDRS=10.0.0.0/8,192.168.0.0/16 \
context-guru-proxy --preset codesmart
```

### Hosted mode, cold storage, and disk pressure

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

#### Cold storage (Box via rclone)

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

#### Disk-pressure eviction

`--dashboard-max-bytes` bounds *this database*; these bound the **filesystem**, which on a
shared box is mostly filled by other things.

| Flag / env | Default | Purpose |
|---|---|---|
| `--dashboard-disk-high` / `DASHBOARD_DISK_HIGH` | `0.90` | Evict the oldest **sessions** while this fraction of the filesystem is in use. Negative = disable. |
| `--dashboard-disk-low` / `DASHBOARD_DISK_LOW` | `0.85` | Stop evicting once usage falls to this. The gap from the high watermark is what stops the janitor grinding when the host is full for other reasons. |
| `--dashboard-min-keep-bytes` / `DASHBOARD_MIN_KEEP_BYTES` | `1073741824` (1 GiB) | Never shrink the dashboard database below this under disk pressure — below it the pressure is not ours to relieve, and a blank dashboard would hide the real problem. |

### Logging

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

### Diagnostics

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
