# The KV-cache page

*The dashboard's **KV-cache** tab: how long your conversations actually stay idle, what the
prompt cache is costing you at that idle profile, and what a different TTL policy would have
cost on the same history.*

This page documents the **UI and the read layer**. The policies, the cost model and the replay
are package `kvcache`, which is a plain domain package the request path can import — nothing on
this page decides or prices anything.

Files: `dash/ui/kvcache.js`, `dash/ui/kvcache.css`, `dash/kvcache.go` (the derived dataset and
its aggregates), `dash/kvcachesim.go` (name → strategy, and the comparison),
`dash/kvcacheapi.go` (four routes). The view mounts itself — its tab, its `<section>`, its
stylesheet and its loader registration are all in `kvcache.js`, so the shared page carries one
line about it (`<script src="kvcache.js"></script>` in `dash/ui/index.html`). No edit to
`app.js`, and every helper it draws with — `el()`, `tile()`, `tileGroup()`, `barRows()`,
`lineChart()`, `emptyState()` — is `app.js`'s.

## Opening it

The tab sits next to **Keep-alive** — both answer "what is the prompt cache doing to my bill" —
at `/dashboard/#/behaviour/kvcache` (and at `/dashboard/#kvcache`, which is rewritten to it). It respects the shared filter bar above it: every figure is "over this
window", so the time range, the model, the agent and the account selector all narrow it. Three
narrowings exist only here (time-of-day band, observed TTL tier, and whether a request has a
successor at all) because all three are derived rather than columns. The tier one is deliberately
**not** a shared `Filter` dimension as well: it briefly was both, and since `Filter` matched the raw
`cache_ttl` column while this page matches the *reconstructed* tier, the two were silently
intersected — so the groups that exist only by reconstruction drilled down to an empty table.

## What is on the page, top to bottom

| Section | Answers |
|---|---|
| **Coverage statement** | what this analysis could NOT answer: rows that recorded no TTL, rows with no cost, conversations with a single request, and whether the read was capped |
| **Summary tiles** | requests, conversations, median and mean idle, 5-minute and 1-hour reuse probability, cache-hit rate as the provider reported it, cost as billed, requests with no successor, median cached prompt |
| **How long until the next request** | the idle histogram, fixed edges, with everything past five minutes in the de-emphasis gray — a default cached prefix is already gone by then |
| **Has it come back yet** | the survival curve: the cumulative share of conversations that had returned by each elapsed time. The question a TTL is actually chosen against, which a histogram does not answer |
| **Who waits how long** | the same measurements grouped by observed TTL, by user, by model and by time-of-day band (UTC) |
| **Prices** | every rate the simulation uses, editable, with what each comes to on this window's own median prefix — medianed over the requests that **cached something**, because a request with no prefix has none to price, and omitted entirely when nothing in the window cached rather than shown as `$0.00` |
| **Strategies** | the arm picker, built from the server's registry, and the comparison table with each arm's cost against one baseline |
| **Every request in the analysis** | the derived dataset, sortable on thirteen columns and paged on the server, each row linking back to the request drawer and the session diff |
| **What every figure rests on** | the formulas and the caveats, printed from the payload |

## The three things this page refuses to do

**It never treats a missing measurement as a zero.** Three of them are routinely present on real
history and each gets its own answer rather than a default:

- A request with **no next request** in the same conversation has no idle time. `idle_ms` is
  `null` on the wire, the row renders as *"no next request"*, and it is excluded from every
  average. On the production snapshot that is 1,772 of 14,407 requests, and 1,551 of 1,772
  conversations hold a single request — so folding them in as zero-second returns would move
  every figure on the page.
- `cache_ttl` arrived as an **additive column** defaulting to `''`, so a blank on an older row
  means *not recorded*, and a request that genuinely carried no `cache_control` also reads blank.
  The two are separated by evidence (see below) and what is left is reported as **unknown** and
  counted, never assumed.
- A row whose token accounting was incomplete has an **unknown** cost. It is counted everywhere
  and valued nowhere. An unpriced request is not a free one.

**It does not present the hit rate as the objective.** It is on the table because an operator
asks for it, with a banner saying so: on this service's own traffic holding every prefix for an
hour gives the best hit rate and costs 38% more than the five-minute tier, because it pays 2.0×
input to protect prompts a 1.25× write already covered. Nothing sorts or colours by it.

**It never clamps a comparison, and it says which way it points in words.** The column is headed
*vs baseline* and each cell reads `$X cheaper` or `$X MORE`. A column of signed dollars headed
"saving" was read as a saving when it was the opposite, and a reader scanning for the biggest
number picked the worst arm.

## How the observed TTL is reconstructed

Three states, in `observedTTL` (`dash/kvcache.go`), with the SQL predicate that filters on them
directly beneath it and a test asserting the two agree:

| The row says | Billed at the 1h tier | Anything cached | Tier | Source |
|---|---|---|---|---|
| `ephemeral_5m` / `ephemeral_1h` | — | — | as recorded | **configured** |
| blank | yes | — | 1h | **observed** — the provider's own 1h write counter is the only proof a requested `ttl:"1h"` was honoured rather than silently downgraded |
| blank | no | yes | 5m (assumed) | **unknown** — it cached something at a tier nobody wrote down |
| blank | no | no | none | **observed** — it really did cache nothing |

An unrecognised value (`ephemeral_10m` from a future build) is treated as *not recorded* rather
than coerced into the tier it resembles. The **observed-policy** arm reports how much of itself
rested on the assumed default (`observed_coverage`: recorded against assumed) — on a snapshot
that predates the column it equals the fixed-5m arm exactly, and that is not a coincidence.

## The cost model

Every rate is configurable per provider/model and editable in the page; nothing is hardcoded in
a UI component, and `dash/uikvcache_test.go` asserts that no per-token rate literal and no
arithmetic on a `*_usd` field appears in `kvcache.js`. Rates are entered in **USD per million
tokens** — the unit every vendor's price page uses — and converted once at the HTTP boundary.

```text
uncached input        input_tokens   × input_rate
cache read           cache_read     × cache_read_rate          (default 0.1× input)
cache write, 5m      written_tokens × write_5m_rate             (default 1.25× input)
cache write, 1h      written_tokens × write_1h_rate             (default 2.0× input)
keep-alive ping      cached_tokens  × cache_read_rate + ping_input × input_rate
                                                      + ping_output × output_rate
keep-alive, too late cached_tokens  × write_rate(tier) + the same overhead
request cost         uncached_input + cache_read + cache_write + output
total cost           Σ request_cost + Σ ping_cost
cache premium        total_cost − uncached_cost      (NEGATIVE = the cache paid for itself)
absolute savings     baseline_cost − strategy_cost   (never clamped)
percentage savings   absolute_savings ÷ baseline_cost × 100   (undefined, not 0%, at a zero baseline)
```

Three assumptions worth stating out loud, all of them editable:

- **The 1-hour write rate is derived**, because no gateway publishes one. It is a multiplier
  against base input, and both the multiplier and the resulting rate are on the page.
- **A ping is a cache read**, so a five-minute and a one-hour keep-alive cost the same *per
  ping*. What differs is the creation tier that put the entry there and how **often** a ping is
  needed — a five-minute entry has to be touched about twelve times as often. A page that
  charged two different per-ping rates would be inventing a price no provider publishes.
- **A ping cannot generate nothing** where the provider requires `max_tokens ≥ 1`, so its
  minimum output is priced and stated rather than rounded away. `zero_gen=1` models a provider
  that accepts a zero-generation request.

Cache semantics are configurable because they are not universal. The shipped defaults are
Anthropic's documented behaviour: a cache hit refreshes the entry's lifetime for free, a
keep-alive read is just a use and refreshes it the same way, and a refresh buys another lifetime
*of the tier the entry was created at* — it does not upgrade a tier.

## What a strategy is allowed to know

A strategy sees a `kvcache.Observation`, and the whole design of that struct is what is
**absent** from it: no next-request timestamp, no idle duration, and no field derived from
either. The historical statistics attached to it are accumulated as gaps **close**, so a
decision taken at time *T* can only see gaps that had ended by *T*. The real next-request time
is used exactly once — to score the decision after it has been made.

`optimal` is the exception and it is labelled one. It reads the true next-request time and
computes the cheapest plan that exists for a history, so it is a **ceiling** on what any policy
could achieve: it is drawn in the de-emphasis gray with the word *ceiling* beside it, and it can
never be selected as a baseline, because a percentage measured against it would be a share of a
number no policy can reach.

## Routes

Four, all `GET`, all tenant-scoped, all returning numbers, enum labels and ids only — no prompt
text, no transcript. They are `GET` rather than `POST` for two reasons worth more than tidiness:
both scoping tests walk the mounted route table with a `GET`, so a `POST` route would be one
neither could check; and a simulation is a **view**, so its whole input belongs in a URL that can
be bookmarked and pasted into an issue. See [Routes & headers](reference/routes.md).

## Telemetry that is estimated, or absent

- **The 1-hour write rate** is derived from a multiplier, not published. Stated above.
- **Latency avoided** is derived from *this window's own* means — the mean upstream time of
  requests that really missed, minus that of requests that really hit — and is reported as
  *not measured* unless both populations reach 20 rows.
- **`upstream_ms = 0` means not recorded**, and such rows are skipped by that differential
  rather than averaged in as instant responses.
- **Pings on open idle spans** are charged into a span whose end is not in the window, bounded by
  the window's own last request, and counted apart (`pings_on_open_spans`) because their number
  depends on where the window ends.
- **A miss recorded as `prefix_change` or `cold_start`** cannot be rescued by any TTL. Those rows
  force a miss in every arm and are reported as `forced_misses` — the ceiling on all of it.
- **There is no per-user timezone anywhere in the store**, so every hour and every time-of-day
  band on this page is **UTC** and says so.
- **A conversation is `(account, session, model)`.** The account is in the key because a session
  id is client-supplied and can collide between accounts. The **model** is in it because a cache
  entry does not transfer between models — an opus request cannot read a sonnet request's entry —
  so two requests in one session on different models are not each other's successor. Leaving the
  model out linked them, which granted the second a hit at the read rate on an entry it could never
  have matched; the bias was one-directional, making every arm look cheaper and hit more often than
  it can. On this deployment's corpus the model adds 124 trajectories (1,896 against 1,772) and
  touches the 12,035 requests that sit in a session using more than one model.
