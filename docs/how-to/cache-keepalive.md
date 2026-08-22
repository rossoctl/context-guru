# Keep an idle prompt cache warm

The provider's prompt cache has a five-minute lifetime. A session idle longer than that
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
proxy re-sends that session's last request with `max_tokens: 1` — a pure cache read, which
refreshes the entry. **One ping buys back about 11.5 of itself.**

Nothing else about the request changes. The prefix hash is cumulative over
`tools` → `system` → `messages`, so a ping that altered any of it would *create* an entry
at `1.25x` instead of refreshing one at `0.1x` — the one way this mechanism costs money
instead of saving it. Only `max_tokens` and `stream` are touched, and neither is inside the
hashed prefix.

## Turning it on

It is **off by default**, because it spends the caller's own money without the caller
asking. On the Settings page: *"Keep my prompt cache warm while I am away."* In a
configuration document:

```yaml
cache:
  keepalive: true
  keepalive_idle_seconds: 280          # X — must be under 300
  keepalive_max_pings: 2               # K
  keepalive_min_prefix_tokens: 20000   # the gate
  keepalive_max_usd_per_ping: 0.25
```

These are the measured optimum, not defaults picked by feel. Replayed through the shipped
decision function over the production window:

| policy | pings | ping cost | converted | saving | **net** |
|---|---:|---:|---:|---:|---:|
| **X=280 K=2, gated (shipped)** | **912** | **$90.76** | **148** | **$215.84** | **+$125.08** |
| X=280 K=2, blanket (no gate) | 8,915 | $93.79 | 153 | $221.05 | +$127.26 |
| X=280 K=1, gated | 536 | $53.51 | 96 | $138.80 | +$85.28 |
| X=280 K=3, gated | 1,226 | $121.33 | 174 | $252.13 | +$130.80 |
| X=240 K=2, gated | 1,012 | $102.19 | 134 | $197.03 | +$94.85 |
| X=280 K=12, gated | 3,307 | $311.78 | 253 | $370.12 | +$58.35 |

**The gate is the whole reason this is deployable: 9.8× fewer pings for 1.7% less money.**
What it drops is the near-free pings on tiny prefixes — ping cost is bimodal, p50 $0.0004
against a p99 of $0.2275 — so 8,000 requests of real gateway load disappear and almost none
of the value does. On a path that already returned 180 HTTP 429s in the same window, that
matters as much as the dollars.

`X = 280` beats 240 because it wastes fewer pings on gaps that would have hit anyway. `K`
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

## What is held in memory, and for how long

To ping a session the proxy keeps that session's last request body, and — where the upstream
is caller-pays, which is the default — the caller's own provider credential. There is no
alternative: the ping happens when no request is in flight, so nothing else can authenticate
it, and pinging on the operator's key would bill the wrong party.

Seven controls, because this is the one place the service holds a caller's credential beyond
the life of a request:

| control | what it does |
|---|---|
| **Hard deadline** | `time.AfterFunc` at `(K+1) × X` ≈ 14 min. A *scheduled* deadline, not a check inside a loop: a process with no traffic still releases on time. |
| **Eager release** | dropped the moment the next request arrives, the gate refuses, the span is exhausted, or the process shuts down — whichever comes first. |
| **Zeroized** | the bytes are overwritten, not just dereferenced. A dropped `[]byte` sits in the heap until a collection that may never come. |
| **Masked at rest** | XORed under a random per-process key, so the idle hold contains no working credential. |
| **Consent per request** | re-read from the tenancy on every request. Turning the setting off retires what is already held on the account's next request; a session that goes quiet is dropped by the deadline. |
| **Audit row per ping** | every use is a durable `requests` row: tenant, session, timestamp, cost, and whether it read or wrote. |
| **Kill switch** | `CONTEXT_GURU_KEEPALIVE=off` stops *retention*, not merely pinging — nothing is held at all. |

Bounded on volume too: 128 MiB across at most 512 sessions, and a single body over 8 MiB is
not held.

**What the masking does and does not buy.** A heap or core dump, a crash report, a
`/proc/<pid>/mem` read or a `strings` pass over a snapshot yields masked bytes rather than a
working key — the accidental-capture class, which is the realistic one. It does **not** stop
an attacker with code execution in this process: the mask is in the same address space. It is
obfuscation at rest, not encryption. And the credential is necessarily plaintext for the
duration of the ping itself, because `net/http`'s header map holds strings and a Go string
cannot be overwritten — so the window is one request instead of the whole idle hold, which is
the part that was worth closing.

## Related

- [Measuring savings](measure-savings.md) — how the ledger separates the provider's cache
  from ours.
- `apply/headttl.go` — the mixed 1h/5m TTL, which is implemented, off, and worth $0 on the
  models this service runs. Measured live: `aws/claude-haiku-4-5` grants the 1-hour tier
  (36,251 of 36,574 written tokens), `aws/claude-sonnet-5` silently downgrades it to 5
  minutes (0 of 48,212). The `ttl` field reaches the provider; Bedrock's 1h support covers
  the Claude 4.5 family, and this service's spend is Opus 5, Opus 4.8, Sonnet 5 and Opus
  4.6. `cache_write_1h` on each row is what says when that changes.
