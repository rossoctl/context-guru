# Plugin v2: three questions

The plugin answers three questions: how much am I saving, is my cache about to cost me, what
should I do. There are two parts, a status line and the `/context-guru:*` commands.

Recordings of every screen (asciinema casts, light and dark SVGs) are in `docs/plugin-v2/`.

## Status line

Two lines by default (`--layout=minimal` keeps one).

```
saved ≈$2.15 session · ≈$6.35 today · ★ ≈$212.40 lifetime
cache ◐ █░░░░░ 0:42 │ ctx ███░░░░░ 41% │ ▸ cache gone in 42s: a pause now costs ≈$1.20 to rewrite
```

| Piece | Meaning |
|---|---|
| `saved ≈$` | Modelled against the same requests uncompacted, at gateway prices, net of Context Guru's own spend. Session, today, lifetime are three windows of one measure. |
| `cache` bar | Drains toward expiry. Green warm, yellow in the last minute (or last fifth of the lifetime), red and empty when cold. `↻` means the proxy plans a keep-alive ping. |
| `ctx` bar | Percent filled, against the window the proxy actually served, not the one Claude Code assumes. `⚠ rewrite near` from 85%. |
| `▸` hint | At most one, most expensive first: cold prefix, cache about to lapse with no ping, context nearly full, last miss cause. |

The installer writes `"refreshInterval": 5` next to the command, so the countdown moves while you
read. Claude Code 2.1.296 accepts `refreshInterval` (minimum 1 second; the status line schema in
the CLI binary lists it). Older versions ignore the key and redraw on events only.

The all-time and today totals can take seconds on a large history. A render never waits for them:
it shows the cached value, and a detached refresher fills the cache for the next render.

Configure with `--layout=minimal|balanced|detailed`, `--hide=a,b`, `--show=a,b`, `--width=N`
(or `CG_STATUSLINE_LAYOUT`, `_HIDE`, `_SHOW`, `_COLUMNS`). `NO_COLOR=1` turns colour off. Colours
are the basic ANSI set plus dim, so they follow the terminal theme on light and dark backgrounds.

## Commands

One plugin, one prefix: everything is `/context-guru:*`.

| Command | The question | First line |
|---|---|---|
| `/context-guru:status` | How am I doing, what next? | Saved this week, and how much more is within reach |
| `/context-guru:savings` | How much did it save? | Saved in N days, as a share of what it would have cost |
| `/context-guru:cache` | Is my cache about to cost me? | Warm for M:SS, and what a lapse costs |
| `/context-guru:waste` | What do I carry and never use? | Count and dollars, then the command to remove the first |
| `/context-guru:session` | What happened in this session? | Turns, bill, misses, longest pause |
| `/context-guru:doctor` | Is it set up right? | Checks passing, and the first thing to fix |

Add `details` for the tables behind a screen. Output is plain Unicode with no colour codes, so it
reads on any theme. Saved dollars and estimated opportunity (observed spend you could
still avoid) are always in separate blocks, badged `✓ Saved` and `≈ Estimated`, and the estimated rows overlap, so they are not added.

## Codex

The Codex plugin has the same skill names (`context-guru-status` as the home screen, `-savings`,
`-cache`, `-waste`, `-session`), using the same scripts.

| Capability | Claude Code | Codex (checked October 2026) |
|---|---|---|
| Custom status line from a command | yes (`statusLine`, `refreshInterval`) | no. `/statusline` only picks from built-in items (for example context used and rate limits); a command-driven footer is an open request |
| Hooks | yes | yes, but the proxy needs none of them here |
| Slash namespace | `/context-guru:*` | skills, invoked by name |

So on Codex the cache countdown and savings badge are not available; ask `context-guru-cache` instead.

## Moved commands

One command per question. The `insights*` and `cost-*` commands moved and stay for one release as thin
redirects that say so and run the new command.

| Old | New |
|---|---|
| `/context-guru:insights` | `/context-guru:status` |
| `/context-guru:insights-capabilities` | `/context-guru:waste` |
| `/context-guru:insights-idle` | `/context-guru:cache` |
| `/context-guru:insights-components` | `/context-guru:savings` |
| `/context-guru:cost-today` | `/context-guru:status` |
| `/context-guru:cost-unused` | `/context-guru:waste` |
| `/context-guru:cost-cold`, `cost-compact` | `/context-guru:cache` |
| `/context-guru:cost-trends` | `/context-guru:savings` |

`/context-guru:statusline` still installs the status line; `/context-guru:doctor` checks that it is there.
