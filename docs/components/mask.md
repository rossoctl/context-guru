# mask

!!! info "Offload — lossy, reversible"
    Age-based garbage collection: keep the newest few tool outputs verbatim, replace older ones with a short marker + stash.

## How it works

`mask` is age-based garbage collection: it keeps the newest `keep_recent` tool outputs verbatim and
replaces older ones (≥ `min_tokens`) with a short `<<cg:HASH>>` marker + stash. It is complementary
to the content-based offloaders.

## Before → After

```
after (older):  [older tool output masked; starts: 700 701 def __rmul__(self, m): 702 …] <<cg:…>> [full output: call context_guru_expand]
```

The head-peek comes from `keep_head_chars` (default 96); with `keep_head_chars: 0` the marker
is the bare `[older tool output masked]`.

## Lossiness

Lossy but reversible — masked outputs are stashed and recovered via `context_guru_expand` /
`GET /expand`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `keep_recent` | 3 | Newest tool outputs kept verbatim. |
| `min_tokens` | 100 | Only mask older outputs at least this large. |
| `keep_head_chars` | 96 | Characters of the hidden output left inside the marker as a one-line head-peek, so the model knows *what* was masked without a blind `expand` round trip — evidence showed a bare marker on a masked source-file read forces needless expands. Set `0` for the opaque marker (≈2pp more savings). |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |
| `cold_cache` | `false` | On a turn whose prompt cache has **provably expired** (idle past the provider TTL), act at any depth instead of only in the uncached tail. Free when the cache really is gone — the whole transcript is re-billed anyway — and the decision is frozen so later warm turns replay it. Off by default because a *wrong* cold reading costs a cache-write of the whole suffix. See [Freeze / cold_cache](../design.md#the-one-turn-where-depth-is-free-cold_cache). |

## When it's inert

`mask` used to report `acted: 0` with no reason at all, which is indistinguishable from a broken
component. It now names one of these on every message it passes over, visible per component in
`/stats` and on the dashboard:

| gate | meaning |
|---|---|
| `within_keep_recent` | the request holds no more tool outputs than `keep_recent`, so nothing is "older" |
| `below_min_tokens` | the output is smaller than `min_tokens` |
| `marker_or_kept_verbatim` | already offloaded by an earlier component, or expanded by the agent |
| `cached_prefix` | the output is inside the provider's cached prefix, where a new mask would flip already-cached content and force a cache-write of the suffix. This is the gate `cold_cache` lifts on a provably-expired cache |
| `non_text_blocks` | a text rewrite would drop the message's non-text blocks |
| `marker_no_win` | marker + head-peek would not be smaller than the output itself |

On a prompt-cached backend `cached_prefix` is the dominant one by construction: an agent adds one
tool result per turn, so by the turn an output is older than `keep_recent` it is already in the
prefix. That is why `mask` looks inert on cache-aware traffic even though it is the biggest lever
on non-caching traffic, and why the `cold_cache` option matters most here.

## When it shines

Long agent trajectories where old tool results are unlikely to matter. In the `agent` preset it is
the biggest lever (~27% content-token savings, no reward loss — see
[RESULTS.md](../RESULTS.md)).

## When it's inert

≤ `keep_recent` tool outputs, small outputs.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
