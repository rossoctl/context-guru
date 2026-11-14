# Analysis: the band as it is today

This document describes the band that the `context-guru` Claude Code plugin draws above
the prompt. See [the as-is diagram](assets/01-band-as-is.svg).

Two sources were read:

- The **internal build** (version 0.6.1, `hooks/register.tsx` and `hooks/model.ts`), and
  one live session of it. The screenshots, the numbers and the diagram come from here.
- This repo's plugin (version 0.7.2), in
  [`context-guru-plugin/hooks/`](../../../context-guru-plugin/hooks/). It was read, not
  run. Section 8 lists where it differs.

The plugin is a Claude Code "mod".

## 1. Structure of the band

The band has up to four rows. Row 1 always shows. The other rows show only when
the band has height for them.

| Row | Content | Example |
|---|---|---|
| 1 | Next-message price (warm and cold), Ping button, hide button, dashboard link | `◆ next $0.04 · $0.92 cold [Ping] [hide] dash ↗` |
| 2 | Status row: session saving, cache clock, context fill | `saved $3.65 of $15.52 │ cache ● 11:51 ↻ │ ctx 37%` |
| 3 | One chip per named sub-agent cache | `└ agents ● general-purpose 7m ↻` |
| 4 | Rotating command tip (one command per 12 s), or a weekly tip | `try /cg:insights — see where your money goes` (in 0.7.2: `/context-guru:insights`) |

The text "From the cg plugin" above the band is added by Claude Code. The mod does not
draw it.

In narrow mode (under 80 columns) the labels are short (`next`, `cold`, `dash`)
and the bars on `cache` and `ctx` are not drawn.

## 2. Data flow

1. The mod never uses the network. It calls `scripts/cg.py`.
2. `cg.py cachestate --json` reads the status line's on-disk cache. Rows 1 to 3 use it.
3. `view()` in `model.ts` turns that document into the values the band draws.
4. The band polls every 15 s. A one-second tick redraws only the clock row.
5. `cg.py ping` is the one write. It runs only when the user presses **Ping**.

## 3. Colors in the cache element

| Condition | Glyph | Color |
|---|---|---|
| A ping was sent, or a keep-alive plan exists (`↻`) | ● | cyan (takes priority) |
| Last 15 s, no ping or plan | ● | red, with `!` after the time |
| Expiring: `left ≤ max(60 s, TTL ÷ 5)` | ◐ | amber |
| Warm | ● | green |
| Cold | ○ | dim; the time counts up |

`ctx` is green up to 50%, amber up to 70%, and red above that. From 85% the wide
layout adds `⚠ rewrite near`.

## 4. What the live data showed

A `cachestate` document read during the session contained these values:

| Field | Value | Meaning |
|---|---|---|
| `ttl_s` | 300 | The cache entry lives 5 minutes after its last use. |
| entry expiry | about 73 s ahead | `max(last_ts, end_ts) + ttl_s − now_ms` |
| `keepalive` | `False` | No ping had been sent. |
| `pings_left` | 2 | Two pings were planned. |
| `next_ping_ms` | 40 s ahead | The first ping was due in 40 s. |
| `held_until_ms` | 610 s ahead (10:10) | The plan expected the cache to stay warm until then. |
| `manual_pings.count` | 0 | The Ping button was not pressed. |
| `window` | 1,000,000 | The model's measured context window. |

A later read in the same session showed `held_until_ms` 813 s ahead while the entry
had 275 s left.

## 5. Why the clock can show more than 5 minutes

This is how the user's screenshot showed `11:51` with a 5-minute TTL.

- `clockOf()` in `model.ts` returns the plan's horizon (`held_until_ms`) whenever the
  entry is still warm and `held_until_ms` is later than the entry's own expiry.
- In the first read above: 40 s to ping 1, then the rest of the plan. The data gives
  only the total after ping 1 (610 − 40 = 570 s). A split of 270 s to ping 2 plus
  300 s of hold fits that total, but it is an inference, not a field.

So the clock is a **forecast** that assumes every planned ping succeeds. It is not
the time the cache is guaranteed to stay warm.

## 6. Problems in the current band

| # | Problem | Evidence |
|---|---|---|
| P1 | The clock is a forecast, but the label and the number look like a deadline. | Section 5. If a ping fails, the cache goes cold at the entry expiry (73 s in the read above). |
| P2 | `↻` and the cyan color appear when a plan **exists**, before any ping is sent. | `pinged = remS > 0 && (isKeepalive \|\| isHeld)` in `cacheClock()`; `keepalive` was `False` in the read above. |
| P3 | The user cannot tell a planned extension (free, potential) from a sent one (paid). | Only a count (`pings_left`) exists in the data; the band shows none of it. |
| P4 | The user cannot see how many extensions ContextGuru offers (X), or how many are used. | Same as P3. |
| P5 | In the internal build (0.6.1) the automatic keep-alive and the manual **Ping** button are shown together without a clear difference. Version 0.7.2 reduces this (section 8) but shows the two as text, not as a shape. | 0.6.1: row 1 button next to a clock that already counts pings. 0.7.2: the button is hidden while an automatic ping is planned. |
| P6 | The band has no help for context decisions: compact, use a sub-agent, start a new session. | Only `ctx 37%` exists. |
| P7 | Text carries meaning that a shape could carry. | `next message`, `if cold`, state words, tips. |
| P8 | In the cold state the Ping button disappears with no explanation, and both prices become one. | `register.tsx` draws the cold price with the warning color, but a screenshot of the running band shows it white. The two disagree and the cause was not found. |

## 7. Facts that were checked and facts that were not

**Checked in the source or in live data:** the structure in section 1, the data flow,
the color rules, the fields in section 4, and the clock rule in section 5. The clock rule
(`clockOf`) and the `↻` rule (`cacheClock`) are the same in 0.6.1 and 0.7.2.

**Not checked:**
- Whether the 0.7.2 band behaves as read. It was not run.
- Whether the VS Code surface passes the pointer to the band, so that a hover card works.
  The mod API has hover-reveal for boxes. It has no tooltip element.
- What a manual ping adds to the plan, and whether it counts against X.
- Where X is configured, and whether the cache document carries the plan's total.
- Whether the data has what a compact, sub-agent or new-session verdict needs.
- The cost of one ping. The proposal shows `$0.01`, which is the warm price.

## 8. What version 0.7.2 does differently

These are the differences found by comparing `context-guru-plugin/hooks/` with the
internal 0.6.1 files. Some of them solve part of the problems in section 6.

| Area | Version 0.7.2 (this repo) | Version 0.6.1 (internal) |
|---|---|---|
| Next automatic ping | `nextPingIn()` in `model.ts` computes the seconds to the next ping, from `pings_left` and `next_ping_ms`, only when it fires before the entry expires. Row 1 shows `↻ next ping 4:12` (`↻ ping 4:12` on a narrow band, `↻ pinging…` at 0:00). | The fields exist in the data. The band does not use them. |
| Ping button | `showPing()` hides the button while an automatic ping fires before the cache expires. It shows the button when the proxy sends no `next_ping_ms` (fail open). | The button is always shown for a warm, pingable cache. |
| Price unit | `nextPrice()` shows dollars with an API key. On a Claude subscription it shows tokens: `85.0k rd` (cached read) and `85.0k wr` (rewrite). | Dollars only. |
| Row 2 on a subscription | `5h 34% · this session saved 85.0k wr`, in place of money. | Money only. |
| Update marker | `▲ update` on row 2. | None. |
| Cache color | Cyan while pinged, except in the last 15 s, which stay red with `!`. | Cyan whenever pinged, with no `!`. |
| Dashboard link | The loopback proxy of the session (`http://localhost:PORT/dashboard/`); no link if the session is not routed through it. | One fixed hosted URL. |
| Finished agents | A chip state `done` with the reason "this agent has finished, so there is nothing to keep warm". | None. |
| Commands, pane | `/context-guru:*` commands; the pane is `/context-guru`. | `/cg:*`; the pane is `/pane`. |

Two consequences for this proposal:

- The `↻ next ping` text in 0.7.2 already answers part of T4 (trust the automatic
  keep-alive) and part of P5. The proposal replaces that text with bars.
- The link `more 5 mins ($0.01)` must show tokens, not dollars, on a subscription (Q9 in
  the task document).
