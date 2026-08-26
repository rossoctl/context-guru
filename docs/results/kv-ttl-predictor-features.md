# What could predict the next request's timing, and what actually can be built

This is the feature catalog for a predictor that answers the question a TTL decision
needs: will this conversation's *next* request land inside the 5-minute cache lifetime,
the 5m–1h band a keep-alive can rescue, or beyond an hour where nothing helps? It extends
[kv-cache-ttl.md](../how-to/kv-cache-ttl.md)'s prior study rather than restarting it — that page
already found the exact ceiling and the two arms it tried before this one. Read it first.

Every number below is from the live deployment, measured **read-only** via
`sudo -u cg python3 ... file:/var/lib/context-guru/cg.db?mode=ro` (no copy, no chown — the
DB's owner never changes, and no `&immutable=1`: that flag skips SQLite's own
change-detection and throws intermittent "database disk image is malformed" errors
against this live, actively-checkpointing WAL database) on 2026-08-26: 66,779 non-ping
requests, 17 tenants,
15,359 sessions, 51,420 observed inter-request gaps, spanning `ts_min`=2026-08-17 07:08 UTC
to `ts_max`=2026-08-26 09:20 UTC. Tenant identifiers are pseudonymized (`t01`..`t17`) in
everything that follows — the real ids never left the DB's own account.

## The headline feature: `stop_reason`

`requests.stop_reason` is populated on every row and nobody has used it as a ping-gating
feature yet. It splits cleanly into three behavioural clusters, not two:

| cluster | stop_reason values | n | mean gap | P(<5m) | **P(5m–1h band)** | P(>1h) |
|---|---|---:|---:|---:|---:|---:|
| still working | `tool_use`, `stop_sequence`, `tool_calls`, `length`, `content_filter` | 41,199 | 13–175s | 99.4–100% | **0.0–0.6%** | ~0% |
| looks done, isn't | `stop`, `` (unset) | 3,196 | 127–501s | 93.4–96.2% | **2.9–6.1%** | 0.4–0.9% |
| actually done | `end_turn`, `max_tokens`, `refusal` | 7,025 | 1,263–6,295s | 43.3–84.6% | **11.7–43.3%** | 3.7–13.3% |

Break-even for one 5-minute keep-alive ping is `p > (mult_w5m − mult_read) / (mult_w5m − 1)
= (1.25 − 0.1) / (1.25 − 1)` when comparing a ping against a bare miss recreate, or more
simply `p > read_rate/write_rate ≈ 0.1/1.25 = 8.0%` against the cost of the ping itself
being wasted (see `docs/how-to/cache-keepalive.md` for the exact derivation this page
already carries — this doc doesn't re-derive it, it applies it). Only the third cluster
clears that bar with real margin; the second cluster ("looks done, isn't") sits *below* it
despite superficially reading like end-of-turn. **A naive rule keying only on "did the
turn end" (folding `stop`/`""` in with `end_turn`) would ping on ~4,500 more requests a
year at negative expected value than a rule that excludes them.**

`max_tokens` is small (n=166) but has the highest band rate (35.5%) — a response that got
cut off is usually a task still in progress that will resume soon, but not *immediately*
the way a mid-tool-loop turn is. `refusal` is tiny (n=30, 43.3% band) — too little to act
on alone, but consistent with the same story: the agent stops, the human looks at it, comes
back within the hour more often than not.

This alone is the strongest rule-based arm candidate and should be built and scored before
anything ML-shaped (§ predictor exploration, separate doc).

## Per-tenant heterogeneity is extreme — a global rule undersells or oversells almost everyone

Conditioned on `stop_reason = end_turn` alone (removing the biggest confound), the 5m–1h
band rate ranges from **0.76% to 41.4%** across the 15 tenants with ≥10 `end_turn` events:

| tenant | n (all) | n (end_turn) | P(band \| end_turn) |
|---|---:|---:|---:|
| t12 | 231 | 29 | 41.4% |
| t01 | 5,290 | 982 | 37.8% |
| t06 | 4,518 | 335 | 29.3% |
| t13 | 408 | 33 | 27.3% |
| t08 | 2,008 | 179 | 23.5% |
| t09 | 324 | 19 | 21.1% |
| t07 | 1,857 | 242 | 19.0% |
| t10 | 2,708 | 70 | 14.3% |
| t04 | 1,803 | 276 | 11.6% |
| t15 | 4,010 | 501 | 11.0% |
| t16 | 81 | 20 | 10.0% |
| t17 | 4,408 | 701 | 3.1% |
| t02 | 6,754 | 1,048 | 0.86% |
| t11 | 11,970 | 2,112 | 0.76% |

A rule tuned on the pooled population (11.7%) is right for almost nobody: it overpings t11
and t02 (16x too aggressive relative to their real rate) and underpings t12/t01/t06 (2-3.5x
too conservative). **Any predictor that ships must be per-tenant, or per-tenant-adjusted
via a fallback chain** — which is exactly what `kvcache.Stats`/`History` already does
(`LevelUserBucket → LevelUserModel → LevelUser → LevelModel → LevelGlobal`, gated on
`minCell = 6` observations before trusting a cell over its parent). Reuse it; don't
reinvent a second fallback ladder.

The two heaviest tenants by volume (t11: 11,970 rows, t02: 6,754 rows) are also the two
with the *lowest* band rates — so a volume-weighted global average is dragged toward "never
ping" by exactly the accounts that would benefit least from a ping in the first place, and
a manager reading only the pooled number would conclude keep-alive barely matters when for
t12/t01/t06 it matters a great deal.

## Time-of-day: real structure, not what the two shipped strategies assumed

Restricted to `end_turn` rows (removing the "still working" noise), band rate by UTC hour:

| UTC hour | n | P(band) | | UTC hour | n | P(band) |
|---:|---:|---:|---|---:|---:|---:|
| 00 | 183 | 2.7% | | 12 | 389 | 13.1% |
| 01 | 181 | 1.1% | | 13 | 358 | 17.3% |
| 02 | 130 | 2.3% | | 14 | 440 | 9.3% |
| 03 | 79 | 2.5% | | 15 | 520 | 16.3% |
| **04** | **156** | **52.6%** | | 16 | 260 | 22.3% |
| 05 | 221 | 9.5% | | 17 | 123 | 16.3% |
| 06 | 165 | 20.6% | | 18 | 299 | 4.7% |
| **07** | **226** | **38.5%** | | 19 | 531 | 3.2% |
| 08 | 268 | 22.0% | | 20 | 332 | 3.0% |
| 09 | 387 | 15.8% | | 21 | 345 | 1.4% |
| 10 | 459 | 6.1% | | 22 | 254 | 2.0% |
| 11 | 324 | 12.3% | | 23 | 199 | 3.0% |

Two real peaks — 04:00 and 07:00 UTC — and a trough from 18:00–23:00 UTC. The manager's own
strategy windows target `Asia/Jerusalem` (UTC+2 in winter, UTC+3 in summer per DST). At
UTC+3, 04:00 UTC = 07:00 local (pre-workday, genuinely idle-then-resuming) and 07:00 UTC =
10:00 local (mid-morning) — roughly consistent with the shipped "09:00–12:00" window, but
the shipped "14:00–18:00" window (11:00–15:00 UTC) sits in a *middling* band (6–17%), not a
peak, and the two lowest-value hours (19:00–23:00 UTC = 22:00–02:00 local) are outside both
shipped windows already — so those two windows aren't wrong, they just aren't tuned to
where the actual peaks are.

**Caveat, stated once because it applies everywhere on this page**: the store carries no
per-tenant timezone. `docs/how-to/kv-cache-ttl.md` already flags this — "time of day" here
is UTC, full stop. The Jerusalem comparison above is illustrative (it's the design doc's
own stated default TZ, `tenant/keepalivestrategy.go:45`), not a per-tenant fact. A feature
genuinely keyed to "this tenant's local morning" needs a tenant-timezone field that does
not exist today.

## Requested-but-not-honoured 1h TTL — a finding worth its own line

`cache_ttl = 'ephemeral_1h'` on 11,822 of 66,779 requests (17.7%) — the client (or a
component) explicitly asked for the hourly tier on nearly one in five requests — and
`cache_write_1h > 0` on **zero** of them. Every one of those requests either had its `ttl`
silently downgraded by the gateway or hit a warm entry and read rather than wrote (a read
doesn't distinguish tiers in the wire response the way a write does). This confirms and
sharpens the prior finding (`docs/how-to/kv-cache-ttl.md`'s "honest downside" section,
which inferred it from a smaller window): **on this deployment's traffic mix, the 1-hour
tier has never once been billed at its own creation rate.** It is not obviously a cost bug
(a silently-downgraded write still bills at the cheaper 1.25x, not the 2.0x it asked for),
but it is a coverage lie: any code path assuming a `ttl:"1h"` request is protected for an
hour is wrong on this gateway/model combination. See the whole-session 1h/5m predictor doc
for what this means for that workstream specifically — its savings are simulated for
sonnet-5 traffic and only get a real live number against `claude-haiku-4-5`.

## Full feature list

Grouped by what's already usable today vs. what's genuinely missing.

### Available now, and where it lives

| feature | source | population | notes |
|---|---|---:|---|
| `stop_reason` (3-cluster) | `requests.stop_reason` | 66,779/66,779 | the headline finding above |
| tenant identity | `requests.tenant_id` | 17 tenants | extreme heterogeneity — see above |
| hour of day (UTC) | `requests.ts` | 100% | `kvcache.BucketOf`/`Observation.HourUTC` already expose this |
| day of week | `requests.ts` | 100% | not yet on `Observation`; cheap to derive from `Now` |
| `Turn` (request ordinal in conversation) | already on `Observation` | 100% | |
| `SinceLastMs` (the just-closed gap) | already on `Observation` | 100% except turn 1 | "the single most useful past fact a strategy has" per the code's own comment |
| cached prefix size (`CachedTokens`) | already on `Observation` | wherever a prefix exists | |
| per-(tenant,model,bucket) historical reuse rate | `kvcache.History`/`Stats.ReuseWithin` | accumulates during replay | leak-free by construction; reuse, don't reinvent |
| median/EWMA of past gaps | `deploy/harbor/kv_ttl_survival_predictor.py`'s feature engineering | per user, backwards-only | already built, cite it |
| `agent` (claude-cli vs openai vs litellm vs anthropic vs curl/python-* clients) | `requests.agent` | 100%; claude-cli=49,297, openai=12,535, anthropic=2,758, others long-tail | a genuinely distinct population, not just a label — see caveat below |
| `model` | `requests.model` | 100% | cache does not transfer across models — a trajectory that switches model is two conversations, per the existing convention |
| `reasoning_effort`, `thinking_mode`, `thinking_budget` | `requests.*` | sparse — most rows are `''`/`0` on non-Claude-Code traffic | worth trying, low expected signal given sparsity |
| declared tool count (`tools`), `system_blocks` | `requests.*` | 100% | proxy for task complexity, unvalidated as a feature yet |
| cache breakpoint placement (`cache_bp_system/tools/messages/blocks`) | `requests.*` | 100% | breakpoints only ever land in system+blocks on this traffic (per prior exploration) — likely low marginal signal over `model`/`agent` alone, worth a quick check, not a priority |
| requested TTL tier (`cache_ttl`) | `requests.*` | 66,779 | see the 1h finding above — what was *asked for*, not what was honoured |
| `cache_miss_reason` | `requests.*` | 100% | `hit`/`cold_start`/`ttl_expiry`/`prefix_change`/`unknown` — a forced-miss reason overrides any TTL choice; already load-bearing in the simulator's semantics |

### Requested, and genuinely not instrumentable today

| requested feature | why it's missing | what it would take |
|---|---|---|
| number of this tenant's other sessions currently open | not tracked anywhere as a live count | a session registry the proxy already half-has in `proxy/keepalive.go`'s `kaEntry` map (per-conversation, in-memory) — extending it to a per-tenant open-session gauge is plausible, but it's new state, not a query over history |
| last tool call name, per-request | `tool_uses` is keyed by (session, tool name), not by request — only session-level call counts and first/last-seen timestamps exist | would need a request-level tool-call log; does not exist |
| per-tool-call duration | not recorded anywhere | same — no per-invocation timing table exists (`request_components.duration_ms` exists but is per-*component*, i.e. context-guru's own pipeline stages, not the agent's tool calls) |
| subagent/sidechain marker | no parent/child link recorded; `Agent` shows up in `tool_uses` as a tool name (27 calls in an earlier snapshot) but that's a count, not a relationship | would need a session hierarchy column |
| "was this a Claude Code `Monitor` tool call" marker | same limitation — `tool_uses` has session-level counts (`Monitor`: 58 calls in an earlier snapshot) but not per-request | same |
| long-running-tool-call marker | nothing tracks call duration at the tool level (see above) | same |

The pattern across the missing group is the same: everything genuinely about "what
happened inside this specific request's tool use" would need new per-request
instrumentation, and the schema-bump policy (a version bump discards `requests` history
outright — `dash/schema.go`) is a real reason not to add speculative new columns without a
concrete predictor that needs them and a migration plan. None of the missing features are
needed for the two strongest signals found so far (`stop_reason`, tenant identity) — they'd
be a second round, not a blocker for this one.

### A caveat on `agent`

`agent = openai` is 12,535 rows and `anthropic` is 2,758 — meaningful volume, but these are
different *client dialects* hitting the same proxy, not necessarily the same population of
human behaviour as `claude-cli`'s 49,297. A predictor trained across all of them without an
`agent` feature (or fit separately per agent) risks learning an artifact of which client
happens to send more `end_turn`-shaped traffic rather than a real timing signal. Include
`agent` as a categorical feature, and check per-agent stop_reason distributions before
trusting a pooled fit.

## What this means for the predictor work

1. Build and score the `stop_reason`-gated rule first — it's free (no model, no
   training), and the cluster split above is the whole rule.
2. Any predictor, rule-based or learned, must be evaluated **per tenant** as well as
   pooled — the heterogeneity above means a single pooled number can hide a strategy that
   is bad for most tenants and good for a couple of large ones, or vice versa.
3. `HourUTC`/day-of-week carries real signal but is UTC-only; don't claim "morning" or
   "working hours" per tenant without a timezone field that doesn't exist.
4. The 1h-tier finding is a hard constraint, not a modeling choice: on this gateway/model
   mix, a "will 1h TTL pay off" predictor's savings are simulated, not live-verifiable,
   except on `claude-haiku-4-5` traffic.
5. Everything the user asked for that isn't in this list (per-tool timing, subagent
   markers, monitoring-command markers) is a real, stated gap — not a modeling failure.

## Related

- [Choose a cache TTL, and know what it is worth](../how-to/kv-cache-ttl.md) — the prior study this
  extends: the exact ceiling, the arm registry, and the "93% of the ceiling is 3.7% of
  requests" finding.
- [Keep an idle prompt cache warm](../how-to/cache-keepalive.md) — the shipped keep-alive mechanism
  and the break-even derivation this doc applies rather than re-derives.
- `deploy/harbor/kv_ttl_survival_predictor.py` — the existing offline survival model; its
  docstring carries the exact extraction SQL and seven numbered traps.
