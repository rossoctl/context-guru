---
name: insights-idle
description: How long this machine's sessions actually sit idle, how many prompt-cache entries that lets expire, what those expiries cost, and what a keep-alive strategy would have saved over the same window - with the exact setting that turns it on. Use when the user asks about idle time, cache misses, cache expiry, why the cache went cold, keep-alive pings, whether keep-alive is worth it, which cache strategy to use, or what walking away from the keyboard costs them. Accepts --json.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/insights.py)
---

# Idle time, cache misses, and what a ping is worth

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/insights.py" idle
```

**With `--json`**, add `--json` and print the output verbatim.

## The chain, in this order, because each link is a separate measurement

Present it as the chain. Jumping to the dollar figure skips the two facts that make it believable.

**1. How long they actually go idle.** `idle_gap_p10_hours` / `idle_gap_p50_hours` /
`idle_gap_p90_hours`, and the `gap_band.*` histogram with a count and a cost per band. Give the
median and the p90, not a mean — a mean over gaps that range from seconds to days describes none of
them.

**This is not a criticism and must not be phrased as one.** An idle gap is a developer reading a
diff, thinking, or in a meeting. The gap is the input to the problem, never the problem.

**2. How many of the resulting expiries are ADDRESSABLE.** `addressable_misses` and
`addressable_usd`. Addressable means a ping placed inside the entry's own lifetime could have
refreshed it. A gap longer than the pings can reach is not addressable at any price, and counting
those in a saving is the single easiest way to inflate this whole report — so quote
`addressable_misses`, never a total miss count, as the size of the opportunity.

`coverage_seconds` is how far the current policy actually reaches: **K pings x the interval, plus the
entry lifetime.** Reach is what more pings buy, not a deeper discount. Two pings at 280s reach 860s;
one reaches 580s.

**3. What the pings DID**, if any ran. `keepalive_pings`, `keepalive_ping_usd` (what they cost),
`keepalive_saved_usd` (the prefix re-creations they avoided), `keepalive_net_usd` (the difference,
and **the only one of the three a saving may be reported from**). Plus `keepalive_winners` /
`keepalive_losers` / `keepalive_sessions_touched` — a positive total over a losing majority is a
different fact from a broadly positive one, and the split is how a reader tells them apart.

`keepalive_misses_avoided` is prefixed **"at most"** wherever you quote it, and say why once: the
provider's cache is keyed on content, so another session sending a byte-identical prefix would have
refreshed the same entry anyway. That confound cannot be measured from our side, so the count is a
ceiling on coverage rather than an attribution.

**4. What they WOULD have done**, if they did not. This is `recommend_*`, and it has two shapes.

## The recommendation, or the refusal

**`recommend_refused` non-empty is the answer, not a gap in it.** Report the reason verbatim and
stop. Below its floor the interval the bootstrap produces is an artefact of the resample rather than
a property of the traffic. The `service_lo_usd`/`service_hi_usd` pair in that finding is
**service-wide scale, explicitly not the user's figure** — say which it is or do not say it.

Where it is not refused: `recommend_idle_seconds`, `recommend_max_pings`, and `recommend_lo_usd` to
`recommend_hi_usd` over `recommend_n` addressable expiries in `recommend_sessions` sessions.

**Quote both ends of that range. Always. Never average them into one number.** There is deliberately
no point-estimate field on the wire: every account measured has a 90% interval whose relative
half-width is at least 62%, and some cross zero. A midpoint would be a number nobody measured, and
the field was left out precisely so nobody could render one.

## The K ladder

`calc.k1..kN.{convertible_misses,net_usd,coverage_seconds}`, with `calc_current_k` marking the
policy in force. This is where "what would have saved me money" becomes a table rather than an
assertion. Alongside it:

- `calc_prefix_tokens` with **`calc_prefix_source`** — `session`, `account_median` or `given`. Per-ping
  cost is bimodal (p50 $0.0004, p99 $0.2275), so there is no honest average of it; the source field
  exists so a typed number is never read as a measured one. If it says `none`, there was no prefix to
  measure and the ladder's dollars are absent rather than assumed.
- `calc_ping_usd_each` — one ping on that prefix. `calc_avoided_usd_each` — what one converted miss
  is worth, which is the avoidable **write premium** `(cache_write - cache_read) x prefix`, not the
  whole cost of the miss. Quoting the whole miss would overstate every figure here by roughly a fifth.
- `calc_priced=false` means the model has no rate. Every dollar in the ladder is then absent. It does
  **not** fall back to a blended rate.

## What is at risk right now

`live_sessions_expiring_soon`, `live_soon_usd`, `live_potential_usd` — the only part of this report
about the present rather than the window, and the most persuasive thing in it. A session whose cache
entry lapses in ninety seconds has a named dollar cost attached to walking away. Mention it when it
is non-zero; skip it silently when it is not.

## Turning it on

```bash
/plugin configure context-guru
```

`cache strategy: 5-min-ping` — and **say the cost in the same breath, once**: it spends the user's
own credential on pings while nobody is at the keyboard, and it applies to every session routed
through this proxy rather than just this one. The range in the finding is already net of that spend.
`/context-guru:cache-strategy-picker` explains `none` and `1-hour-head` and does the switching.

**A restart is required and saying otherwise is the failure mode.** The strategy is picked up when a
proxy starts, and a running proxy keeps its current one until it is stopped and started again. A new
session does that; do not restart the proxy from inside a session routed through it.

## The two findings that are problems, not savings

- **`keepalive-wrote`** — pings that CREATED a cache entry instead of refreshing one. That pays the
  premium write rate to cache something nothing will re-read: the one keep-alive outcome with no
  upside at all. Report it as a bug, never fold it into the saving, and say that switching to `none`
  stops the spend meanwhile.
- **`keepalive-underwater`** — running, and net negative. **A negative net is a real outcome and is
  shown as one.** Do not clamp it to zero, do not lead with the ping count as though firing were the
  achievement, and do say that a few long sessions with large prefixes can flip the sign — so
  "check again in a week" is honest advice rather than a hedge.

`phantom_ttl_rows` above zero is a data-quality count, not a saving: the expiry attribution behind
every figure in this section is weaker than it looks. Mention it if it is large relative to
`addressable_misses`.

## On a non-Anthropic backend

Keep-alive still spends, and a chosen pipeline preset may save nothing — vLLM, llm-d and similar
match an implicit longest prefix on their own. Zero pipeline saving there is correct behaviour rather
than a failure, and it does not make the keep-alive figures wrong.
