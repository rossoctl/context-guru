# Choose a cache TTL

The provider sells two prompt-cache lifetimes: five minutes (`1.25x` base input to create,
`0.1x` to read) or one hour (`2.0x` to create, `0.1x` to read). Both refresh for free on every
hit. Picking between them is a real cost decision, not a default to leave alone.

## The short version

- **Prompt caching itself is the win, already banked.** On real deployment traffic, caching
  takes off roughly three-quarters of the uncached bill regardless of which TTL you pick.
- **The five-minute tier is right almost all the time.** Most conversational gaps close well
  inside five minutes, so the one-hour tier's extra creation cost usually never pays for itself.
- **The one-hour tier earns back its premium only on long-idle, large-prefix conversations** —
  think a session someone leaves open across a coffee break or a meeting, not a quick back-and-
  forth. It breaks even once the entry survives long enough that the avoided re-creation outweighs
  the higher creation cost.
- **Keep-alive pings (see [below](#or-just-keep-it-warm)) usually beat switching to the 1-hour
  tier outright**, because they only spend when the session is actually still idle, at the cheap
  read rate.

## How to set it

Configure the TTL on the `cacheinject` component:

```yaml
components:
  cacheinject:
    ttl: "5m"   # default; also accepts "1h"
```

Set per pipeline/preset, not per request — this is a fleet-level policy, not something to flip
turn by turn.

## Which one should I pick?

| Your traffic looks like | Pick |
|---|---|
| Short, rapid-fire turns (most agent sessions) | `5m` (default) — cheapest, and hits almost as often as `1h` |
| Long-running sessions with multi-hour idle gaps and large prefixes | `1h`, or `5m` + keep-alive pings |
| You don't know / mixed traffic | Leave the default and turn on keep-alive (below) rather than switching the tier |

**One important caveat:** not every provider/model combination actually honors a requested
one-hour TTL — some silently downgrade it to five minutes. Check `cache_write_1h` in your
`/stats` output; if it's consistently zero on a model you're paying the 1-hour premium for,
you're not getting what you paid for.

## Or, just keep it warm

Instead of paying the 1-hour creation premium up front, you can keep a 5-minute entry alive
with periodic keep-alive pings while the session is idle — see
[Keep an idle prompt cache warm](cache-keepalive.md) for how to turn that on and what it costs.
For most traffic this is cheaper than committing to the 1-hour tier, because you only pay the
(cheap) read rate, and only while something might still come back.

## The research behind this

This page gives the practical recommendation. The full cost model, the offline TTL predictor,
and the measured breakdown of exactly where the savings headroom comes from (and why hit rate
alone is a misleading metric) live in Results:

- [KV-cache TTL predictor: summary](../results/kv-ttl-predictor-summary.md)
- [KV-cache TTL predictor: features](../results/kv-ttl-predictor-features.md)
- [KV-cache TTL predictor: arms](../results/kv-ttl-predictor-arms.md)
- [KV-cache TTL predictor: comparison](../results/kv-ttl-predictor-comparison.md)

## Related

- [Keep an idle prompt cache warm](cache-keepalive.md)
- [Budget keep-alive pings](kv-cache-keepalive-budget.md)
- [Measuring savings](measure-savings.md)
