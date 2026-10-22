# Cache

`/context-guru:insights cache`. `/context-guru:insights` shows it together with the other two.

**Question: is my cache about to cost me?**

The provider keeps your conversation cheap to reuse for a few minutes after each message. If you pause longer than that, the next message re-sends the whole conversation at a higher price. This screen shows how long you have, what a lapse would cost right now, and what past lapses cost.

![/context-guru:insights cache](img/cache-dark.svg#only-dark)
![/context-guru:insights cache](img/cache-light.svg#only-light)

```text
Cache is warm for 2:41 more. If it lapses, the next message costs $1.91 extra.

● ███████████░░░░░░░░░ 2:41 of 5:00   ping planned ↻

Cache misses in 7 days: 377 costing $145.67 extra   (measured)
  You paused longer than the cache lasts     214x  $107.79  ███████░░░
  Settings or tools changed mid-session       23x   $14.57  █░░░░░░░░░
  A new session started                       96x   $14.57  █░░░░░░░░░
  A subagent or side request started          41x    $7.28  ░░░░░░░░░░
  The conversation was rewound or compacted    3x    $1.46  ░░░░░░░░░░

The cache keeps your conversation cheap to reuse for a few minutes after each message.

More: /context-guru:insights cache details
```

When the cache has already lapsed the first line says so and gives the price of the next message:

```text
Cache is COLD. The next message re-sends the whole conversation: $1.91 extra.

○ ░░░░░░░░░░░░░░░░░░░░ 0:00 of 5:00   no ping planned

Cache misses in 7 days: 377 costing $145.67 extra   (measured)
  You paused longer than the cache lasts     214x  $107.79  ███████░░░
  Settings or tools changed mid-session       23x   $14.57  █░░░░░░░░░
  A new session started                       96x   $14.57  █░░░░░░░░░
  A subagent or side request started          41x    $7.28  ░░░░░░░░░░
  The conversation was rewound or compacted    3x    $1.46  ░░░░░░░░░░

The cache keeps your conversation cheap to reuse for a few minutes after each message.

More: /context-guru:insights cache details
```

## Reading it

- **Bar:** drains toward expiry. `ping planned ↻` means the proxy will touch the cache before it lapses.
- **Misses by cause:** each cause in plain words with its count and the extra it cost; the bar is the share. The dollars are measured from billed tokens; the cause is inferred from the pause length and message count.
- Ask for `details` for the costliest single misses.

The same clock runs in the [status line](statusline.md), live.
