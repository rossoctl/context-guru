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

A subscription pays in usage limits, not dollars. So on a subscription the screen shows everything in tokens:

```text
This session:       saved 42.0k wr · 610.0k rd
This 5-hour window: saved 85.0k wr · 1.2M rd · cost 9.0k rd
                    ≈ 3% of the 5-hour limit (estimate at API-price weights)

7 days:             saved 214.0k wr · 1.6M rd · 12.0k in · cost 3.0k wr · 96.0k rd
                    3 requests not measured in tokens

✓ By feature   (tokens by tier: wr = cache write, wr1h = 1-hour cache write, rd = cache read, in = input)
  mask                                 saved 160.0k wr · 1.3M rd · cost 3.0k wr
  toolfilter                           saved 30.0k wr · 300.0k rd · 12.0k in
  Keep-alive rescue                    saved 24.0k wr · cost 96.0k rd
  Keep-alive pings (not in the total)  cost 900 wr · 410.0k rd · 30 in

≈ Estimated   (waste you could still avoid; rows overlap)
  You paused longer than the cache lasts, 214 times  measured
  Settings or tools changed mid-session              measured

More: /context-guru:insights savings details
```

- Every figure is in tokens, by tier. `wr` is a 5-minute cache write, `wr1h` a 1-hour cache write, `rd` a cache read and `in` fresh input. The screen never adds the tiers into one number: Anthropic does not publish how the 5-hour limit weights them.
- `saved` is tokens the requests did not send or write. `cost` is tokens a change made the requests pay. A feature can save in one tier and pay in another. For example, the keep-alive rescue saves a cache write and pays cache reads.
- **This session**, **This 5-hour window** and **7 days** are the totals of every feature.
- **✓ By feature** has one line for each compaction component (by its name), then `Cache split`, `Keep-alive rescue`, `1-hour cache` and `Ended change`. A feature with no tokens has no line.
- **Keep-alive pings** is what the cache strategy's pings paid. A ping is not agent traffic, so it is not in the total. The keep-alive's net is the rescue line less the pings line.
- The line that starts with `≈` and ends with `(estimate at API-price weights)` is an estimate. It says what share of the 5-hour limit the savings are worth if the limit weighted tokens like the API prices them. Anthropic does not publish those weights, and the limit header has whole percents only. The line shows only when the window cost at least $0.50 at API prices and you used at least 2% of the limit.
- The provider stops you at 100% of the limit, so the share saved is at most 100% minus the share you used. At that cap the line says `≈ without context-guru you would have hit the 5-hour limit`.
- The window comes from the limit headers that the proxy saw on your last response. When the window is over, the window line does not show.
- `N requests not measured in tokens` counts requests with no token figure: requests from before the token count, and requests the proxy could not price. That is "not measured", not "saved nothing".
- The screen shows no dollars. A subscription does not pay them. So `details` adds nothing here: the bill and the manual pings are in dollars.
- With an API key, Bedrock or Vertex, the screen is the dollar view above, unchanged.

## Details

Ask for `details` to also see the bill and what the provider's own prompt cache saved against no caching at all. That figure is a provider feature, so it is not counted above.

Change the window with `--days N`.
