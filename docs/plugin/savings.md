# Savings

**Question: how much did it save?**

![/context-guru:savings](img/savings-dark.svg#only-dark)
![/context-guru:savings](img/savings-light.svg#only-light)

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

More: /context-guru:savings details
```

## Reading it

- **First line:** dollars saved in the window (default 7 days), net of what the proxy itself spent, and the share of the cost the same work would have had without it.
- **✓ Saved** lists where the dollars came from, by lever, with a trend for the window. A negative row, such as the proxy's own model spend, is subtracted, not hidden.
- **≈ Estimated** is spend you could still avoid. The rows overlap: do not add them.

Ask for `details` to also see the bill and what the provider's own prompt cache saved against no caching at all. That figure is a provider feature, so it is not counted above.

Change the window with `--days N`.
