# Information mapping

This document says which information appears where, which task it serves, and where
the value comes from. The proposed layout is in
[the proposal diagram](assets/04-band-proposed.svg). The current layout is in
[the as-is diagram](assets/01-band-as-is.svg).

"Source" uses these marks:

- **Data**: a field that exists in the `cachestate` document today.
- **Derived**: computed from fields that exist. The rule is an inference and was not
  checked in the plugin source.
- **Missing**: no field was found. The band cannot show it without new data or a rule.

## 1. Proposed band, element by element

| Row | Element | Task | Source | Rule or field |
|---|---|---|---|---|
| CACHE | Dark bar left of the `now` tick | T2 | Data | Elapsed since the last use: `now_ms − max(last_ts, end_ts)` |
| CACHE | White `now` tick | T2 | Fixed | Marks the present |
| CACHE | Green bar | T2 | Data | Built-in TTL left: `max(last_ts, end_ts) + ttl_s − now_ms` |
| CACHE | Dashed cyan bar, one per planned extension | T4, T11, T12 | Derived | Count from `pings_left`. Start and length from `next_ping_ms` and `held_until_ms` (see section 2). In 0.7.2 `nextPingIn()` already gives the time to the next ping |
| CACHE | Solid cyan bar | T4, T11 | Derived | The extension that is sent and is burning. `keepalive` is `True` while the last use was a ping |
| CACHE | Outlined solid bar | T4, T11 | Derived | The extension that was just activated, seconds before the previous block ends |
| CACHE | Number at the bar end | T2 | Data | `held_until_ms − now_ms` when a plan exists, else the entry expiry |
| CACHE | Dark bar and `7:06 ⏳` (cold) | T1, T2 | Data | Time since the cache went cold; `⏳` when `stale` is true |
| CACHE | Link `more 5 mins ($0.01)` | T3 | Data and Missing | Runs `cg.py ping`. Cost shown is `next_warm_usd`, which is not a measured ping cost. On a subscription the unit must be tokens (`nextPrice()`) |
| NEXT | `$0.01 now vs $0.28 if cold` | T1 | Data | `next_warm_usd`, `next_cold_usd` (else `rewrite_usd`) |
| NEXT (cold) | `$0.29 next` | T1 | Data | `next_cold_usd` |
| SESSION | `saved $x of $y (p%)`; on a subscription `5h 34% · this session saved 85.0k wr` | T5 | Data | `money.main + money.others` of `saved_usd` and `baseline_usd`; percentage is derived |
| SESSION | `ctx ██░░░░░░ 11%` | T7 | Data | tokens ÷ `window`; `window_exact` says if the window was measured |
| SESSION | Context verdict (one word) | T8, T9, T10 | Missing | No field and no rule exist |
| SESSION | `dash ↗` | T6 | Data | `url` of the snapshot |
| AGENTS | `● name 7m ↻` | T9 | Data | `threads` entries with id `a:…` and a name; clock as for the main thread |
| Header | `hide` (top right) | — | Fixed | Sets the hidden flag of the band |
| Hover | Card on a bar | T2, T4, T12 | Derived | Per-block list; see section 3 |
| Hover | Card on the verdict | T8–T10 | Missing | Reason and numbers; depends on the verdict rules |

## 2. How the extension bars are derived

The `cachestate` document does not list each planned ping. It has:

| Field | Value in the live read | Use |
|---|---|---|
| `pings_left` | 2 | Number of dashed bars (X minus the ones used) |
| `next_ping_ms` | 40 s ahead | Start of the first planned block |
| `held_until_ms` | 610 s ahead | End of the last planned block |
| `ttl_s` | 300 | Length of one window |

From the live read: `610 − 40 = 570 s` between the first ping and the plan end. With
two pings and a 300 s window, that is `270 + 300`. So the second ping was due about
270 s after the first, and the last block added `+4:30`. The first block added
`+1:05`, because ping 1 lands 40 s into a window that still had 275 s left.

**This split is an inference from one read.** If the plugin schedules the pings by a
different rule, the segment lengths in the mockup change. The number at the bar end
(`held_until_ms`) does not depend on the split.

## 3. Hover card content

| Row in the card | Example | Source |
|---|---|---|
| Name and kind of block | `extension 1 (auto)` | Derived |
| Time to start | `in 0:40` | `next_ping_ms − now_ms` |
| Time added | `+1:05` | Derived (section 2) |
| `built-in TTL` | `ends 4:35` | Data |
| `cold at` | `10:10` | `held_until_ms − now_ms` |

The card opens over the NEXT and SESSION rows. An absolute-position box does not move
other rows. The mod API offers hover-reveal on boxes, but no tooltip element.

## 4. What changed from the current band

| Current element | Proposed | Reason |
|---|---|---|
| `◆ next $0.04 · $0.92 cold` | `NEXT $0.01 now vs $0.28 if cold` on its own row | Row 1 becomes the cache row (T2, T3, T4) |
| `[Ping]` button | Link `more 5 mins ($0.01)` | It is manual and costs money; the price is part of the label (T3) |
| `[hide]` on row 1 | Top-right corner of the panel | Frees row 1 |
| `dash ↗` on row 1 | End of the SESSION row | Groups it with session data (T5, T6) |
| `cache ● 11:51 ↻` on row 2 | Bar and one number on the CACHE row | A shape replaces the words, the glyph and the `↻` (N7) |
| `saved $3.65 of $15.52` | Same, with a percentage | T5 |
| `ctx 37%` | Same, with a verdict | T7–T10 |
| Sub-agent chips | Kept as AGENTS row | T9 |
| Rotating tip row | Removed | Decision D9 |
| `↻ next ping 4:12` text (0.7.2) | Replaced by the bars; Q10 | The bars show the next ping and the plan |
| `▲ update` (0.7.2) | Not placed in the mockups | Needs a place on the SESSION row |
| Words `warm`, `held`, `cold in` | Removed | The bar shows the state (D1, D2) |

## 5. Color and shape rules in the proposal

| Visual | Meaning |
|---|---|
| Dark bar | Time already passed |
| Green bar | Built-in TTL left |
| Cyan dashed bar | Planned extension, not paid |
| Cyan solid bar | Extension sent, paid |
| Cyan solid bar with white outline | Extension just activated |
| White tick | Now |
| Amber number (`≈15:10`) | The total is an estimate |
| Dim number and `⏳` | Cold, or data not fresh |
| Blue underlined text | Link (`more 5 mins`, `dash ↗`) |

## 6. Gaps

- The context verdict has no source (T8 to T10).
- The ping cost has no source (T3).
- The segment lengths of planned extensions are inferred (section 2).
- The effect of a manual click on the total is not known (Q1 in the task document).
