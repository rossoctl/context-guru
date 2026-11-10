# Home and status

**Question: is it working, how am I doing, and what should I do next?**

`/context-guru:status` is the home screen. It first checks that this session is routed through the proxy and that the proxy answers, then prints:

1. one line with the dollars saved over the last 7 days and the share of what the same work would have cost;
2. today against yesterday, saved and spent, with a trend for the week;
3. the top three things you could do, each with the dollars at stake.

![/context-guru:status](img/status-dark.svg#only-dark)
![/context-guru:status](img/status-light.svg#only-light)

```text
Saved $41.20 in 7 days (4.6% of what the same work would have cost).

            saved     spent
Today        $6.35   $141.20
Yesterday    $5.10   $118.40
7 days     $41.20   $856.85   saved ▇▁▁█▇▇█  spent ▇▂▁██▇█

≈ Estimated   (spend you could still avoid; rows overlap, do not add)
  1.  $107.79  You paused longer than the cache lasts, 214 times: run /compact before a long break
  2.   $37.70  Add-ons you never use: remove them, /context-guru:insights tools lists them
  3.   $23.31  Settings or tools changed mid-session: pick MCP servers and edit CLAUDE.md before you start

/context-guru:insights  /context-guru:session  /context-guru:doctor  /context-guru:save-more  /context-guru:remove-mcp-and-skills
/context-guru:watch  /context-guru:timeline  /context-guru:chart  /context-guru:heatmap  /context-guru:fill
```

## On a Claude subscription

A subscription pays in usage limits, not dollars. So on a subscription the screen shows everything in tokens:

```text
This session:       saved 42.0k wr · 610.0k rd
This 5-hour window: saved 85.0k wr · 1.2M rd · cost 9.0k rd
                    ≈ 3% of the 5-hour limit (estimate at API-price weights)

Today:              saved 31.0k wr · 240.0k rd · cost 15.0k rd
Yesterday:          saved 26.0k wr · 190.0k rd
7 days:             saved 214.0k wr · 1.6M rd · 12.0k in · cost 3.0k wr · 96.0k rd
                    3 requests not measured in tokens

≈ Estimated   (waste you could still avoid; rows overlap)
  1. You paused longer than the cache lasts, 214 times: run /compact before a long break
  2. Add-ons you never use: remove them, /context-guru:insights tools lists them
  3. Settings or tools changed mid-session: pick MCP servers and edit CLAUDE.md before you start

/context-guru:insights  /context-guru:session  /context-guru:doctor  /context-guru:save-more  /context-guru:remove-mcp-and-skills
/context-guru:watch  /context-guru:timeline  /context-guru:chart  /context-guru:heatmap  /context-guru:fill
```

- Every figure is in tokens, by tier. `wr` is a 5-minute cache write, `wr1h` a 1-hour cache write, `rd` a cache read and `in` fresh input. The screen never adds the tiers into one number: Anthropic does not publish how the 5-hour limit weights them.
- `saved` is tokens the requests did not send or write. `cost` is tokens a change made the requests pay. A feature can save in one tier and pay in another. For example, the keep-alive rescue saves a cache write and pays cache reads.
- **This session**, **This 5-hour window**, **Today**, **Yesterday** and **7 days** are the totals of every feature: compaction, cache split, keep-alive and the 1-hour cache.
- **≈ Estimated** lists what you could still avoid, in the same order as the dollar view, without the dollars.
- The line that starts with `≈` and ends with `(estimate at API-price weights)` is an estimate. It says what share of the 5-hour limit the savings are worth if the limit weighted tokens like the API prices them. Anthropic does not publish those weights, and the limit header has whole percents only. The line shows only when the window cost at least $0.50 at API prices and you used at least 2% of the limit.
- The provider stops you at 100% of the limit, so the share saved is at most 100% minus the share you used. At that cap the line says `≈ without context-guru you would have hit the 5-hour limit`.
- The window comes from the limit headers that the proxy saw on your last response. When the window is over, the window line does not show.
- `N requests not measured in tokens` counts requests with no token figure: requests from before the token count, and requests the proxy could not price. That is "not measured", not "saved nothing".
- The screen shows no dollars. A subscription does not pay them.
- With an API key, Bedrock or Vertex, the screen is the dollar view above, unchanged.

## Reading it

- The first line is `✓ Saved`: measured against the same requests without the proxy, at your gateway's prices, net of the proxy's own spend.
- The block headed `≈ Estimated` is spend you could still avoid. The rows overlap, so do not add them.
- Each item says what to do. `/compact` before a long break, for example, is the answer to cache misses caused by pauses.

## When it says something is wrong

The routing check tells apart the common cases: routing written but this session started before it (normal right after install; it applies to the next session), the proxy not answering, a base URL that is not this proxy's, and a proxy started with older settings than you have now. For anything else run [`/context-guru:doctor`](doctor.md).

`/context-guru:status --stats` prints the proxy's raw `/stats` JSON and nothing else.
