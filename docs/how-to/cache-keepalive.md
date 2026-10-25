# Keep an idle prompt cache warm

Anthropic's prompt cache has a five-minute lifetime. A session idle longer than that
loses its whole cached prefix, and the next turn re-bills every token of it at the
cache-creation rate.

On this service, measured over 19,805 requests and $3,139.97 of spend:

| miss reason | requests | share | cost | share of spend | $/request |
|---|---:|---:|---:|---:|---:|
| hit | 16,368 | 82.6% | $1,928.65 | 61.4% | $0.1178 |
| **ttl_expiry** | **742** | **3.7%** | **$741.07** | **23.6%** | **$0.9987** |
| cold_start | 1,626 | 8.2% | $253.82 | 8.1% | $0.1561 |
| prefix_change | 292 | 1.5% | $205.05 | 6.5% | $0.7022 |

A TTL-expired request costs **8.5x** a request that hit, and against each row's own
counterfactual the penalty is **11.35x**, of which 91.2% is pure re-write. $584.83 —
18.6% of all spend — sits in misses whose idle gap was under an hour.

## What the keep-alive does

Two sentences from the provider's documentation make this fixable:

> By default, the cache has a 5-minute lifetime. **The cache is refreshed for no
> additional cost each time the cached content is used.**

> **The lifetime is measured from the start of the request that writes or reads the cache
> entry, not from the end of its response.**

So a cache *read* refreshes the lifetime, and a read costs `0.1x` base input where
re-creating the prefix costs `1.25x`. After a session has been idle for `X` seconds, the
proxy re-sends that session's last Anthropic request with `max_tokens: 1` — a pure cache read, which
refreshes the entry. **One ping buys back about 11.5 of itself.**

Nothing else about the request changes. The prefix hash is cumulative over
`tools` → `system` → `messages`, so a ping that altered any of it would *create* an entry
at `1.25x` instead of refreshing one at `0.1x` — the one way this mechanism costs money
instead of saving it. Only `max_tokens` and `stream` are touched, and neither is inside the
hashed prefix.

For GPT-5.6+ **OpenAI Responses**, the documented default is a **30-minute
minimum** after the latest write or reuse, not a five-minute tier or an exact
expiry. The proxy schedules a separate ping at 28 minutes, leaving two minutes
for dispatch and upstream latency. It replays the same input with
`max_output_tokens: 16` (the minimum accepted by the tested IBM LiteLLM Azure
route) and `stream: false`. It never adds `prompt_cache_options.ttl`. A ping is
credited only when usage confirms a cache read; a cache write stops further
pings for that session. Earlier OpenAI models and OpenAI Chat Completions are
not enabled until their retention and ping shapes are separately validated.

## Turning it on

It is **off by default**, because it spends the caller's own money without the caller
asking. On the Settings page: *"Keep my prompt cache warm while I am away."* In a
configuration document:

```yaml
cache:
  keepalive: true
  keepalive_idle_seconds: 240          # X — must be under 300
  keepalive_openai_idle_seconds: 1680  # GPT-5.6+ Responses; must be under 1730
  keepalive_max_pings: 2               # K
  keepalive_min_prefix_tokens: 80000   # the gate
  keepalive_max_usd_per_ping: 0.50
  keepalive_idle_seconds_1h: 3540      # X for a 1-hour cache (59 min); must be under 3600
  keepalive_max_pings_1h: 1            # K for a 1-hour cache
```

### Each session follows its own cache lifetime

The proxy reads the lifetime from the traffic, per session (per thread). It does not use one
proxy-wide setting.

- A thread is a **1-hour** thread when every cache breakpoint in its request asks `ttl: "1h"` **and**
  the response billed the 1-hour tier (`ephemeral_1h_input_tokens` equals the whole cache write). A
  turn that only reads the cache bills no write, so it keeps the thread's last billed tier.
- The billed tier decides. A gateway can downgrade a requested `1h` to `5m` (#330), and the proxy then
  uses the 5-minute schedule.
- Anything else is a **5-minute** thread. That includes a mixed request (1-hour head, 5-minute tail).

| Cache lifetime | `auto` (default) | `1-ping` | `2-ping` |
|---|---|---|---|
| 5 minutes | first ping at 240 s, at most 2 | 1 ping at 240 s | same as `auto` |
| 1 hour | 1 ping at 59 minutes | 1 ping at 59 minutes | 2 pings, 59 minutes apart |

One proxy can serve a 1-hour subscription session and a 5-minute subagent or API-key session at once.
Each follows its own row. The 59-minute time is one constant, `AnthropicDefaultKeepAliveIdle1h` in
`internal/modelinfo/cachefrac.go`, or `keepalive_idle_seconds_1h` in the config. The credential hold for
a 1-hour thread is `(K+1) x 59 min`: 118 min at K=1, and at most 236 min, because `keepalive_max_pings_1h` is capped at 3. A 5-minute thread holds about 12 min. An armed session override does not change a 1-hour thread's schedule; it enables the session and sets its prefix floor. If the installed proxy binary is older than the plugin and rejects the `_1h` keys, `start-proxy.sh` re-renders the config without them and a 1-hour cache is pinged on the 5-minute schedule until the proxy is updated. The re-render is tied to the installed proxy version, so the keys come back after an update.

The 240-second Anthropic interval leaves room for a ping and its whole 60-second timeout inside the
5-minute cache: 240 + 60 = 300 s. On a live proxy, 92 of 95 failed pings were 60-second timeouts, and at
the earlier 270 s a ping that timed out came back after the cache had expired (#707). 240 s lowers that
risk; it does not remove it, because a ping that runs to its timeout still leaves no time to retry. It is
being measured (#716). The earlier 270 s was chosen for robustness to where the cache cliff sits (#435); the 1680-second
OpenAI interval is a safety margin below the documented minimum, not an
optimized value from a production replay. Replayed through the shipped
decision function over the production window, the earlier gated policy (X=280, K=2, 20k floor; replaced by the defaults above, #435) nets **+$125.08**
against $90.76 of ping cost — within 1.7% of an ungated blanket policy while sending **9.8x
fewer pings** (912 vs 8,915). What the gate drops is the near-free pings on tiny prefixes —
ping cost is bimodal, p50 $0.0004 against a p99 of $0.2275 — so most of the real gateway load
disappears and almost none of the value does, which matters on a path that already returned
HTTP 429s in the same window.

In that replay `X = 280` beat 240 because it wastes fewer pings on gaps that would have hit anyway. `K`
peaks at 2–3 and falls away after: a session that has *ended* looks exactly like one that
is thinking, so every additional ping is spent partly on sessions that will never come
back. K=2 ships because K=2 and K=3 are within noise here while K=2 sends 26% fewer
requests.

**Never use a per-session ping cap.** It truncates exactly the long large-prefix sessions
holding the value: capping the window's pings per session at 20 drops the net to $92.34,
and at 10 to $54.04. The guard that works is the per-*ping* cost budget above.

## What it will not do

- **It will not ping a session with `thinking.type: enabled`.** Such a request requires
  `max_tokens` above the thinking budget (32,000 on this traffic), so the ping cannot be
  made cheap, and altering the thinking parameters invalidates the message-level prefix.
  `adaptive` is unaffected, and is 81% of the addressable traffic.
- **It will not crowd out a real request.** A ping only ever uses a tenant's *slack* — a
  quarter of the rate and concurrency budget is reserved — and is skipped rather than
  queued when there is none.
- **It will not retry.** A failed ping is logged and forgotten. A `4xx`, or a ping whose
  usage reports more written than read, stops that session being pinged again.

## Where the money shows up

Every ping is a dashboard row marked `keepalive`, with its own tokens, its own cost, and
its own session — so an account can audit what was spent on its behalf while nobody was at
the keyboard. The Overview ledger carries both halves:

| line | meaning |
|---|---|
| Keep-alive pings | what the pings cost |
| Keep-alive savings | the prefix re-creations they avoided |
| `keepalive_misses_avoided` | how many requests resumed past the lifetime and hit anyway |

Ping rows are **excluded from every other aggregate** (`Filter.WithKeepAlive`). A ping is a
request we made on the user's behalf, not traffic their agent sent, so counting it would
inflate the request count and drag every per-request average towards a one-token response.
They are still listed on the Requests tab, flagged, because that list is the audit trail.
Spend figures do include them: the money was really spent on the caller's key.

The saving is credited only where a ping of ours preceded the request, the gap exceeded the
provider's lifetime, the provider read more than it wrote, and the amount is capped at the
tokens that ping actually refreshed. It is a **ceiling**, not a floor: the provider's cache
is keyed on content, so another session sending the same prefix would have refreshed the
entry for free.

## The honest downside

**The policy is a very small tax on almost every session it touches, funding a large rebate
for a few dozen.** Over the production window:

| policy | sessions touched | losing money | worst session | total losses | winners |
|---|--:|--:|--:|--:|--:|
| blanket X=280 K=2 | 3,809 | **3,773 (99.1%)** | −$0.95 | −$13.10 | 36 |
| **gated (shipped)** | **119** | **85 (71.4%)** | **−$2.42** | **−$13.59** | **34** |

The gate does not make the tax rarer per session touched — it makes it fall on 119 sessions
instead of 3,809, leaving the other 3,690 untouched entirely. That is the fairness argument,
and it is independent of the dollars: a session that is never pinged cannot lose.

Sessions that lose are short ones and abandoned ones. Sessions that win are long ones with
large prefixes and human-paced gaps. An account whose work is many brief sessions should
leave this off. Given this shape, the opt-in default and the per-account ledger are not
optional: a user has to be able to see that they are one of the ~70% paying the tax rather
than one of the few collecting the rebate, and turn it off.

## Operator kill switch

```
CONTEXT_GURU_KEEPALIVE=off
```

Set in the environment, read at startup. An environment variable rather than a control-plane
route on purpose: the thing an operator needs at 3am is one that works when the control
plane is what is broken. `/stats` publishes `keepalive` with `pings`, `skipped`, `failed`,
`wrote_instead_of_read` and `spend_usd`; `wrote_instead_of_read` above zero is a bug, not an
expense.

## Client lifecycle signals

The proxy cannot see a user leave: a session that ended looks like one that is idle. Clients
that run hooks tell it, on `POST /signal`, with a content-free event: the plugins and the hosted
setup snippet send only the session id, the event name and its reason (the hook picks those out
of Claude Code's payload, which also holds the working directory and the transcript path). This is on whenever the keep-alive runs; `CONTEXT_GURU_CLIENT_SIGNALS=off` turns it off.

| event | sent when | what the keep-alive does |
|---|---|---|
| `SessionEnd` | `/exit`, Ctrl-C twice, Ctrl-D, closing the terminal, `/clear`, logout | releases the session's held entries; no further pings |
| `SessionStart compact` | after `/compact` or auto-compaction | releases the entries that cache the pre-compaction prefix |
| `SessionStart resume` / `clear`, `Notification` | | recorded only |

A resumed session (`claude --resume`, same id) is tracked again from its first request after the
end; the provider's cache entry is not touched by any signal, so a prompt resume still reads it.
Every effect only removes spend: a lost or late event leaves the keep-alive as it is without
signals. The Claude Code plugin sends these by default to its own local proxy and to a hosted
service (`client_signals` option: `auto`, `on`, `off`); the hosted setup page includes the hooks;
the Codex plugin sends `SessionEnd` and `SessionStart` once you trust its hooks (`/hooks`).

## What is held in memory, and for how long

To ping a session the proxy keeps that session's last request body, and — where the upstream
is caller-pays, which is the default — the caller's own provider credential. There is no
alternative: the ping happens when no request is in flight, so nothing else can authenticate
it or supply the byte-identical prefix a cache read requires, and pinging on the operator's
key would bill the wrong party.

**The body is conversation content, and enabling the keep-alive is what consents to holding
it.** It is up to 8 MiB of that session's last request — system prompt, tools, and the
message history — held for the length of the idle gap. This is deliberately *not* covered by
the transcript-storage consent, which is a different promise about a different thing: that
one governs writing content to **disk**, where a manager can read it and a retention window
applies. This is memory only, masked, overwritten on release, never persisted, and gone
within about 12 minutes. It cannot be reduced further — the prefix hash covers the whole
`tools` → `system` → `messages` sequence, so a ping that trimmed the body would miss the
cache and cost 12.5x instead of saving 11.5x, which is the mechanism inverted.

Controls, because this is the one place the service holds a caller's credential beyond the life
of a request:

| control | what it does |
|---|---|
| **Hard deadline** | released at `(K+1) × X` ≈ 12 min regardless of traffic. |
| **Eager release** | dropped the moment the next request arrives, the gate refuses, or the process shuts down — whichever comes first. |
| **Zeroized** | the bytes are overwritten, not just dereferenced. |
| **Masked at rest** | the credential and the body are XORed under a random per-process key, so a leaked snapshot doesn't leak a working credential or a readable transcript. |
| **Consent per request** | re-read from the tenancy on every request; turning the setting off retires what's already held. |
| **Audit row per ping** | every use is a durable `requests` row: tenant, session, timestamp, cost, read-or-wrote. |
| **Purged on revocation** | account deletion, token revocation and disablement release held material immediately. |
| **Kill switch** | `CONTEXT_GURU_KEEPALIVE=off` stops *retention*, not merely pinging. |
| **No ping mid-request** | a due ping waits while a request of the same thread is in flight. Other threads are not held back: a subagent's slow request never delays the main thread's ping. |
| **Failed pings stop** | a 401 is about the key: it stops every session pinging with that credential, and only those (a teammate on the same account pings on), for an hour; a 403 (often a per-model permission or a gateway policy) does the same for that credential on that model only. A ping refused with any other non-retryable 4xx or answered 2xx with no usage at all (a translating gateway that hid an error) is counted `failed` and stops that session; 3 in a row for the same account, upstream and model switch the keep-alive off for that route, for that account only, for an hour (`route_trips` in `/stats`). A 429 or 5xx (an overload) is transient: counted `skipped`, and the next ping slot retries. A ping that lands after expiry and reads nothing is a real miss (`missed`): it re-wrote the prefix, so pinging goes on; a second miss in a row is a prefix that does not match and stops the session. |
| **Claude models only through a relay** | native Anthropic, Bedrock and Vertex upstreams are eligible by type, whatever the deployment is called. The Anthropic route forwarded to anything else (LiteLLM, a corporate gateway) may serve another family (an OpenAI or Azure model, with no explicit cache control), so there the model name must contain `claude`, `opus`, `sonnet` or `haiku`; `CONTEXT_GURU_KEEPALIVE_MODELS=alias1,alias2` adds names for aliases that do not. |

Bounded on volume too: 128 MiB across at most 512 sessions, and a single body over 8 MiB is not
held. The masking is obfuscation at rest against an accidental-capture read (a crash dump, a
`/proc` read) — it does not stop an attacker with code execution in this process.

## Gating pings on stop_reason (opt-in, per strategy)

Pings above fire unconditionally regardless of why the previous turn ended. A manager-controlled
strategy can instead name a predictor — `predictor_id: "stop-reason-gated"` plus a
`predictor_threshold` — so a ping only fires when the request it would refresh ended on
`end_turn`, `max_tokens`, or `refusal`. Measured at +1.54% vs `fixed-5m` pooled, because
`tool_use`/`stop_sequence`-preceded gaps almost never land in the addressable band while
`end_turn`/`max_tokens`/`refusal`-preceded ones do far more often — see
[KV-cache TTL predictor: features](../results/kv-ttl-predictor-features.md) for the measurement.

This is opt-in per strategy; a strategy with no `predictor_id` behaves exactly as before. Gains
are concentrated in a few tenants, so scope it to the tenants it actually helps rather than
applying it fleet-wide.

## Client lifecycle signals (opt-in)

The proxy cannot see from the request stream that a user has quit or that Claude Code has
compacted the conversation, so by default it keeps pinging a cache nobody will read. With
signals on, the Claude Code plugin tells the proxy, and the proxy stops holding that cache.

Turn it on in two places:

1. On the proxy: `CONTEXT_GURU_CLIENT_SIGNALS=on`. This enables `POST /signal`; without it the
   route returns 404.
2. In the plugin: set the `client_signals` option to `on` (`/plugin configure`), or export
   `CONTEXT_GURU_CLIENT_SIGNALS=on` in the shell that runs Claude Code.

| Event (hook) | What the proxy does |
|---|---|
| `SessionEnd` (any reason) | Retires every entry held for that session and remembers the end for up to 12 h, so a request that was still in flight when the user left is not held afterwards. A resumed session's next request starts after the mark and is held as usual. |
| `SessionStart` with `source=compact` | Retires the entries holding the pre-compaction prefix (those whose last request started at least 2 s before the event). The session goes on and its next request is held as usual. |
| `SessionStart` resume / clear / startup / fork, `Notification` | Recorded only. Offline replay found that acting on these earns little or loses money. |

The event carries no content: the session id, the event name and a reason from a closed set.
The hook reads the hook payload only to pick those three fields, sends them with the same
credential Claude Code already sends to `ANTHROPIC_BASE_URL`, and never blocks or fails the
session (2 s timeout, exit 0 on every path).

**How it fails safe.** A signal can only remove spend: it retires a held entry, it never
extends a hold or schedules a ping. A lost or late event leaves the keep-alive as it is without
signals; a forged one can only stop pings for the sender's own sessions, because the session key
is scoped to the authenticated tenant. End marks are capped per tenant (2,000), so one tenant
cannot evict another's.

If `ANTHROPIC_BASE_URL` points at something other than a context-guru proxy (a gateway, or a
proxy in front of one), that host receives `POST /signal` with the token it already sees on
every request. Leave the option off in that setup.

## Predictive campaigns (opt-in, per tenant)

Instead of one fixed ping count for everyone, a manager can build a **campaign** for a tenant from
that tenant's own history: either a learned model that decides at the end of each response how
many pings pay for themselves, or (`builder: "auto"`) the best fixed plan for that tenant, rebuilt
daily. A campaign runs in `shadow` (scores live decisions, changes nothing) or `active` (sets the
ping count of a keep-alive the tenant already has on). `active` is refused unless the build's own
held-out comparison recommended it. Routes, settings and safety properties:
`predictive/README.md` in the repository.

## Related

- [Measuring savings](measure-savings.md) — how the ledger separates the provider's cache
  from ours.
- `apply/headttl.go` — the mixed 1h/5m TTL, which is implemented, off, and worth $0 on the
  models this service runs. Measured live: `aws/claude-haiku-4-5` grants the 1-hour tier
  (36,251 of 36,574 written tokens), `aws/claude-sonnet-5` silently downgrades it to 5
  minutes (0 of 48,212). The `ttl` field reaches the provider; Bedrock's 1h support covers
  the Claude 4.5 family, and this service's spend is Opus 5, Opus 4.8, Sonnet 5 and Opus
  4.6. `cache_write_1h` on each row is what says when that changes.
