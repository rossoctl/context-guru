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
  keepalive_max_usd_per_session: 0.25
```

`X = 280` and `K = 2` are the measured optimum, not defaults picked by feel:

| X | K | pings | ping cost | misses converted | saving | **net** |
|---:|---:|---:|---:|---:|---:|---:|
| 280 | 1 | 4,775 | $70.35 | 105 | $161.45 | +$91.10 |
| **280** | **2** | **8,967** | **$109.08** | **159** | **$243.09** | **+$134.01** |
| 280 | 3 | 12,191 | $122.22 | 175 | $256.64 | +$134.43 |
| 240 | 2 | 9,303 | $122.53 | 140 | $208.83 | +$86.29 |
| 280 | 12 | 34,064 | $147.60 | 185 | $264.26 | +$116.66 |

`X = 280` beats 240 because it wastes fewer pings on gaps that would have hit anyway. `K`
peaks at 2–3 and falls away after: a session that has *ended* looks exactly like one that
is thinking, so every additional ping is spent partly on sessions that will never come
back.

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

The saving is credited only where a ping of ours preceded the request, the gap exceeded the
provider's lifetime, the provider read more than it wrote, and the amount is capped at the
tokens that ping actually refreshed. It is a **ceiling**, not a floor: the provider's cache
is keyed on content, so another session sending the same prefix would have refreshed the
entry for free.

## The honest downside

The mechanism is a lottery with a strongly positive expectation and a losing median. Over
the production window at `X=280, K=2`:

- 3,812 sessions were pinged.
- **37 came out ahead**, and 3,775 came out behind.
- The 3,775 losing sessions lost **$13.10 between them** — an average of $0.0035 each.
- The **worst single session lost $0.95**: a large prefix, two pings, and a session that
  never resumed.

Sessions that lose are short ones and abandoned ones. Sessions that win are long ones with
large prefixes and human-paced gaps. An account whose work is many brief sessions should
leave this off; the per-tenant simulation found the keep-alive positive for 10 of 13
tenants and never worse than −$0.53 for the other three.

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

To ping a session the proxy keeps that session's last request body, and — where the
upstream is caller-pays, which is the default — the caller's own provider credential. There
is no alternative: the ping happens when no request is in flight, so nothing else can
authenticate it, and pinging on the operator's key would bill the wrong party.

It is bounded on every axis: opt-in per account, memory only, never logged or persisted,
dropped the moment the next request arrives, and retired by the policy itself after at most
`(K+1) × X` — about 14 minutes at the defaults. Total held bodies are capped at 128 MiB
across 512 sessions, and a single body over 8 MiB is not held at all.

## Related

- [Measuring savings](measure-savings.md) — how the ledger separates the provider's cache
  from ours.
- `apply/headttl.go` — the mixed 1h/5m TTL, which is implemented, off, and worth $0 on the
  models this service runs. Measured live: `aws/claude-haiku-4-5` grants the 1-hour tier
  (36,251 of 36,574 written tokens), `aws/claude-sonnet-5` silently downgrades it to 5
  minutes (0 of 48,212). The `ttl` field reaches the provider; Bedrock's 1h support covers
  the Claude 4.5 family, and this service's spend is Opus 5, Opus 4.8, Sonnet 5 and Opus
  4.6. `cache_write_1h` on each row is what says when that changes.
