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

A subscription pays in usage limits, not dollars. So on a subscription the screen starts with the tokens saved:

```text
This session:       saved 610.0k rd / 42.0k wr
This 5-hour window: saved 1.2M rd / 85.0k wr
                    ≈ 3% of the 5-hour limit (estimate at API-price weights)

At API prices (a subscription does not pay these):
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

- **This session** and **This 5-hour window** count the tokens that compaction kept out of the prompts. `rd` is tokens that would have been re-sent as cache reads. `wr` is tokens that would have been written to the cache once.
- The line that starts with `≈` is an estimate. It says what share of the 5-hour limit the savings are worth if the limit weighted tokens like the API prices them. Anthropic does not publish those weights, and the limit header has whole percents only. The line shows only when the window cost at least $0.50 at API prices and you used at least 2% of the limit.
- The window comes from the limit headers that the proxy saw on your last response. When the window is over, only the session line shows.
- The dollar screen follows, at API prices. A subscription does not pay these dollars. They show where the savings came from.
- With an API key, Bedrock or Vertex, the screen is the dollar view above, unchanged.

## Reading it

- The first line is `✓ Saved`: measured against the same requests without the proxy, at your gateway's prices, net of the proxy's own spend.
- The block headed `≈ Estimated` is spend you could still avoid. The rows overlap, so do not add them.
- Each item says what to do. `/compact` before a long break, for example, is the answer to cache misses caused by pauses.

## When it says something is wrong

The routing check tells apart the common cases: routing written but this session started before it (normal right after install; it applies to the next session), the proxy not answering, a base URL that is not this proxy's, and a proxy started with older settings than you have now. For anything else run [`/context-guru:doctor`](doctor.md).

`/context-guru:status --stats` prints the proxy's raw `/stats` JSON and nothing else.
