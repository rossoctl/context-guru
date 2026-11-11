# Terminal views

Views that draw in the terminal. They read the same data as the commands and follow the same rules: dollars first, `✓ Saved` and `≈ Estimated` kept apart, no raw token counts. Every example below is the shipped output on synthetic data.

## Display settings

The six commands print plain text. The views use colour on a terminal; `--theme` picks light or dark and `NO_COLOR` turns it off.

- Width follows the terminal, 40 to 140 columns; lines wrap or drop their least important column, never overflow.
- `NO_COLOR` turns colour off. Colour is also off when output is not a terminal, so a command run in chat stays plain. Every view reads the same without colour.
- `--ascii` (or `CG_ASCII=1`, or a non-UTF-8 locale) swaps every glyph for ASCII.
- `--theme light|dark` (or `CG_THEME`) picks the heat ramp; other colours are the terminal's own 16.
- A proxy that does not answer within 4 seconds is not waited for: the view shows its last picture and how old it is.

## /context-guru:watch

**Question: Which cache is about to cost me, and what did I save today?**

![`/context-guru:watch`](terminal-views/watch-dark.svg#only-dark)
![`/context-guru:watch`](terminal-views/watch-light.svg#only-light)

```text
$43.95 saved today. 2 of 3 caches are cold: their next message costs about
$0.65 extra.

Caches  time left before the next message costs extra
●  session-A/main   opus-5    █████░░░░░░░  2:14     ctx 34%
○  session-A/sub-1  sonnet-5  ░░░░░░░░░░░░  COLD     ctx 9%
○  session-B/main   opus-5    ░░░░░░░░░░░░  COLD     ctx 58%

Saved today $43.95  █▅█▁▆▁▄▂▁▆█▅▄▁▁▅

Last 20 requests  oldest to newest
■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■↻✖
■ hit  ▨ partial  ✖ miss  ↻ ping

Do: the next message costs extra; /compact first to cut it.

updated 22s ago   q quit  r refresh
```

One row per live thread, the main conversation and each subagent. The bar drains to expiry; `COLD` means the next message pays to re-send the conversation. `↻` marks a keep-alive ping. The last-20 strip shows `■` hit, `▨` partial, `✖` miss. The proxy does not report when the next ping fires, so none is shown.

Live view: run `python3 <plugin>/scripts/tv.py watch` in a second terminal. Keys: `q` quit, `r` refresh. It redraws every second and fetches in the background, so a slow proxy never freezes it. In a chat a skill prints one frame and that command.

## /context-guru:timeline

**Question: What happened in this session, and which moments were expensive?**

![`/context-guru:timeline`](terminal-views/timeline-dark.svg#only-dark)
![`/context-guru:timeline`](terminal-views/timeline-light.svg#only-light)

```text
Cache misses cost about $2.22 of $12.58 billed in this session (3 of 126
messages).

                        $0.76                    $1.20
▮▮▮▮▮▮▮▪▮▮▮▮▪▮▮▮▪▮▮▮▮───✖▮▪▮▪▮▮▮▮▮▮▪▮▮▮▮▮▪▮──↻───✖▮▮▮▮▪▮▮▮▮▪▼▮▮▮▪▮▪▮▪─────↻──✖
12:31             13:15             13:59             14:43              15:27

▪ message   ▮ busy   ✖ cache miss, extra cost above it   ▼ compacted
↻ keep-alive ping   ─ pause over 5 min

Most expensive moments
14:22  $1.20  after a pause of 8m
13:27  $0.76  after a pause of 9m
15:27  $0.25  after a pause of 8m

Do: run /compact before a break longer than 5 minutes, so the extra cost is
smaller.
Extra = cost above a normal turn of the same model (estimate).
```

Misses are read from the token columns. The dollar figure above each `✖` is its extra cost above a normal turn of the same model (an estimate). Pauses over 5 minutes are drawn as a line.

## /context-guru:chart [14|30]

**Question: How much did I save each day, from which lever, and what is still avoidable?**

![`/context-guru:chart [14|30]`](terminal-views/chart-dark.svg#only-dark)
![`/context-guru:chart [14|30]`](terminal-views/chart-light.svg#only-light)

```text
Saved $43.95 in 14 days (4.9% of the bill). Best day Tue $4.83.

✓ Saved per day  measured, net of Context Guru's own spend
  $4.83             ██ ▃▃          ▇▇    ▄▄
           ▃▃ ▂▂    ██ ██          ██    ██
           ██ ██    ██ ██          ██    ██ ▅▅
  $2.42 ██ ██ ██    ██ ██ ▇▇       ██    ██ ██
        ██ ██ ██    ██ ██ ██       ██ ▁▁ ██ ██
        ██ ██ ██    ██ ██ ██ ▆▆    ██ ██ ██ ██
        F  S  S  M  T  W  T  F  S  S  M  T  W  T
        Sep 25                            Oct 08

By lever  measured, same window; adds up to the total
Compaction                      $33.90  ██████████████
Keep-alive pings                 $6.30  ███░░░░░░░░░░░
Tool filtering                   $7.85  ███░░░░░░░░░░░
Context Guru's own model spend  -$4.10
Total                           $43.95

≈ Estimated  spend you could still avoid; rows overlap, do not add
You paused longer than the cache lasts, 214 times    $108  ▒▒▒▒▒▒▒▒▒▒▒▒▒▒
Settings or tools changed mid-session              $23.31  ▒▒▒░░░░░░░░░░░

Do: run /compact before a long break.
```

Days are UTC days, as in the dashboard. Rows under **By lever** add up to the total. **≈ Estimated** is drawn hatched and is not part of the saved total.

## /context-guru:heatmap [days]

**Question: When do my pauses cost me?**

![`/context-guru:heatmap [days]`](terminal-views/heatmap-dark.svg#only-dark)
![`/context-guru:heatmap [days]`](terminal-views/heatmap-light.svg#only-light)

```text
Cache misses cost you about $21.89 in the last 28 days. Costliest hour: Thu
13:00 ($2.00).

    0     3     6     9     12    15    18    21
Mon                   ░░  ░░  ▓▓    ░░                  $2.73
Tue                   ░░  ░░  ▓▓░░  ░░                  $2.73
Wed                   ░░  ░░  ▓▓░░  ░░                  $2.73
Thu                   ░░  ░░░░████░░░░                  $5.54
Fri                   ░░  ░░  ▓▓    ░░                  $2.73
Sat                   ░░  ░░  ▓▓    ░░                  $2.73
Sun                   ░░  ░░  ▓▓    ░░                  $2.73

Scale    ░░▒▒▓▓██  $0 to $2.00 per hour; blank = no miss

Do: before Thu 13:00, run /compact if you will step away.
Hours are local time (UTC+00:00). 88 misses; extra cost is an estimate.
```

Weekday by hour in local time. Covers up to the newest 6,000 requests and says so when it is partial. The first request of a thread is not counted: it always writes.

## /context-guru:fill

**Question: How full is the context, what is in it, and when is it full?**

![`/context-guru:fill`](terminal-views/fill-dark.svg#only-dark)
![`/context-guru:fill`](terminal-views/fill-light.svg#only-light)

```text
Context is 34% full; at this pace it is full in 2h12m. A cache miss now costs
$0.57.

▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒███░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░ 34%

▒  Always loaded (instructions and tools)  30%
█  Conversation so far                      4%
░  Free                                    66%

Do: nothing, there is room.
```

The split is the first request of the main thread (instructions and tools) against everything added since. The proxy does not record how much of the rest is tool output.
