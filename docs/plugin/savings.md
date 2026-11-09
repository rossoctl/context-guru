# Savings

`/context-guru:insights savings`. `/context-guru:insights` shows it together with the other two.

**Question: how much did it save?**

![/context-guru:insights savings](img/savings-dark.svg#only-dark)
![/context-guru:insights savings](img/savings-light.svg#only-light)

```text
Saved $41.20 in 7 days, net of Context Guru's own spend (4.6% of the $898.05 the same work would have cost).

✓ Saved   (vs the same requests without Context Guru, at gateway prices)  ▇▁▁█▇▇█
  Compaction                      $33.90  ████████████
  Keep-alive pings                 $6.30  ██░░░░░░░░░░
  Tool filtering                   $7.85  ███░░░░░░░░░
  Context Guru's own model spend  -$4.10  ░░░░░░░░░░░░

≈ Estimated   (spend you could still avoid; rows overlap, do not add them)
  You paused longer than the cache lasts, 214 times  $107.79  measured
  Settings or tools changed mid-session               $23.31  measured

More: /context-guru:insights savings details
```

## Reading it

- **First line:** dollars saved in the window (default 7 days), net of what the proxy itself spent, and the share of the cost the same work would have had without it.
- **✓ Saved** lists where the dollars came from, by lever, with a trend for the window. A negative row, such as the proxy's own model spend, is subtracted, not hidden.
- **≈ Estimated** is spend you could still avoid. The rows overlap: do not add them.

## On a Claude subscription

A subscription pays in usage limits, not dollars. So on a subscription the screen starts with the tokens saved:

```text
This session:       saved 610.0k rd / 42.0k wr
This 5-hour window: saved 1.2M rd / 85.0k wr
                    ≈ 3% of the 5-hour limit (estimate at API-price weights)

At API prices (a subscription does not pay these):
Saved $41.20 in 7 days, net of Context Guru's own spend (4.6% of the $898.05 the same work would have cost).

✓ Saved   (vs the same requests without Context Guru, at gateway prices)  ▇▁▁█▇▇█
  Compaction                      $33.90  ████████████
  Keep-alive pings                 $6.30  ██░░░░░░░░░░
  Tool filtering                   $7.85  ███░░░░░░░░░
  Context Guru's own model spend  -$4.10  ░░░░░░░░░░░░

≈ Estimated   (spend you could still avoid; rows overlap, do not add them)
  You paused longer than the cache lasts, 214 times  $107.79  measured
  Settings or tools changed mid-session               $23.31  measured

More: /context-guru:insights savings details
```

- **This session** and **This 5-hour window** count the tokens that compaction kept out of the prompts. `rd` is tokens that would have been re-sent as cache reads. `wr` is tokens that would have been written to the cache once.
- The line that starts with `≈` is an estimate. It says what share of the 5-hour limit the savings are worth if the limit weighted tokens like the API prices them. Anthropic does not publish those weights, and the limit header has whole percents only. The line shows only when the window cost at least $0.50 at API prices and you used at least 2% of the limit.
- The window comes from the limit headers that the proxy saw on your last response. When the window is over, only the session line shows.
- The dollar screen follows, at API prices. A subscription does not pay these dollars. They show where the savings came from.
- With an API key, Bedrock or Vertex, the screen is the dollar view above, unchanged.

## Details

Ask for `details` to also see the bill and what the provider's own prompt cache saved against no caching at all. That figure is a provider feature, so it is not counted above.

Change the window with `--days N`.
