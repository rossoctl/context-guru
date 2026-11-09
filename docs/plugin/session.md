# Session

**Question: what happened in this session?**

![/context-guru:session](img/session-dark.svg#only-dark)
![/context-guru:session](img/session-light.svg#only-light)

```text
In this session, cache misses cost you about $4.20 of the $13.50 billed (2 misses in 31 messages).

▪▪▮▪↻·▪▪················✖▪▮▪▪▪▪▮·······✖▪▪▼▪▪▪▮▮
12:31                                      13:40
▪ message  ▮ busy  ✖ cache miss (cost extra)  ▼ conversation compacted  ↻ keep-alive ping  · idle

Longest pause: 25:00. Next time: run /compact before a break longer than 5 minutes.

More: /context-guru:session details
```

## Reading it

- **First line:** a dollar verdict: what cache misses cost you in this window, out of what was billed, and how many there were. That extra is an estimate (each miss compared with a typical hit in the same window).
- **Strip:** one cell per slice of time, over the last four hours at most; a long session says which window it shows. `▪` message, `▮` busy, `✖` cache miss, `▼` the conversation was compacted, `↻` keep-alive ping, `·` idle.
- **Last line:** the longest pause, and the one action that would have helped.

Ask for `details` to list the longest pauses and whether the cache survived each. Pass `--session ID` to look at another session.
