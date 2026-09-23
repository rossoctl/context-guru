# Budget keep-alive pings

[Choose a cache TTL](kv-cache-ttl.md) covers the *tier* — five minutes or an hour.
[Keep an idle prompt cache warm](cache-keepalive.md) covers turning keep-alive pings on at all.
This page is about the knob in between: **how many pings** a conversation is worth spending
while it's idle, rather than a single fleet-wide number.

## The short version

- A ping pays for itself once the chance it rescues the entry from expiring clears roughly
  **8.7%** — that number comes straight out of the provider's cache-read vs. cache-write rates,
  not a tunable constant.
- A wrong skip (letting the entry expire when a ping would have saved it) costs about **11.5x**
  what a wasted ping costs. That asymmetry is why the default is conservative.
- A late ping — one that arrives after the entry has already lapsed — isn't a cheap refresh, it's
  a full re-creation at the write rate. Keep your ping interval safely inside the tier's lifetime
  (under 300s for a 5-minute entry).
- **For most deployments, a flat ping cap is good enough.** A per-conversation adaptive budget
  (predicting how likely a specific conversation is to come back) helps mainly when you're
  constrained on *number of pings* (e.g. a rate limit) rather than on money — see
  [the research](#the-research-behind-this) for the measured trade-off.

## How to configure it

The practical knobs live under the `cache` keep-alive config (see
[Keep an idle prompt cache warm](cache-keepalive.md#turning-it-on) for the full picture):

```yaml
cache:
  keepalive: true
  keepalive_idle_seconds: 280          # ping interval — keep this under the tier's TTL
  keepalive_max_pings: 2               # flat cap per idle span
  keepalive_min_prefix_tokens: 20000   # skip small prefixes — a ping's fixed cost isn't worth it below this
  keepalive_max_usd_per_ping: 0.25     # per-ping cost ceiling
```

`keepalive_max_pings` is a **ceiling** other policies (including any predictor-driven budget)
can only tighten, never raise.

## Tuning guidance

- **Raise `keepalive_max_pings` if pings are effectively free to you** (no rate-limit pressure)
  — a flat cap of 5-6 outperforms a fancier per-conversation model on pure dollar savings in the
  measured data.
- **Lower it, or add a per-tenant strategy, if you're bumping into rate limits** — an adaptive
  per-conversation budget spends far fewer pings for a similar fraction of the reachable savings.
- **Don't set `keepalive_idle_seconds` at or above the tier's lifetime.** A ping that lands after
  expiry pays the re-creation rate, not the read rate — it stops being a keep-alive and starts
  being an expensive accident.
- **Skip small prefixes.** `keepalive_min_prefix_tokens` exists because a ping has a small fixed
  token overhead of its own; below a few thousand tokens of cached prefix that overhead can erase
  the saving.

## The research behind this

The break-even derivation, the backward-induction budget (why a ping's value depends on the pings
after it, not just the next window), the feature set an adaptive predictor would need, and the
measured comparison between a flat cap and a learned per-conversation budget all live in Results:

- [KV-cache TTL predictor: summary](../results/kv-ttl-predictor-summary.md)
- [KV-cache TTL predictor: features](../results/kv-ttl-predictor-features.md)
- [KV-cache TTL predictor: arms](../results/kv-ttl-predictor-arms.md)
- [KV-cache TTL predictor: comparison](../results/kv-ttl-predictor-comparison.md)

## Related

- [Choose a cache TTL](kv-cache-ttl.md)
- [Keep an idle prompt cache warm](cache-keepalive.md)
- [Measuring savings](measure-savings.md)
