# Terminal views

Five commands that draw instead of list. Each opens with one sentence and a dollar figure, then a picture.
All read the local proxy (GET only), take a few seconds at worst, and never wait on a slow proxy: if it does not
answer, they show the last picture and say how old it is.

| Command | Question it answers |
|---|---|
| `/context-guru:watch` | Which of my caches is about to cost me, and what did I save today? |
| `/context-guru:timeline` | What happened in this session, and which moments were expensive? |
| `/context-guru:chart [14\|30]` | How much did I save each day, from which lever, and what is still avoidable? |
| `/context-guru:heatmap [days]` | When do my pauses cost me? |
| `/context-guru:fill` | How full is the context, what is in it, and when is it full? |

`watch` is full-screen and live. A skill's output is a static block, so `/context-guru:watch` prints one frame and
the command that runs the live view in a second terminal: `python3 <plugin>/scripts/tv.py watch`. `q` quits, `r`
refreshes. It fetches in the background every 5 seconds and redraws every second, so the countdown moves even
when the proxy is slow.

## Drawing rules

- Observed and estimated are never drawn alike: observed is solid (`█`), an estimate is hatched (`▒`) and labelled `≈`.
- Everything reads without colour. Colour repeats what a glyph already says.
- `NO_COLOR` turns colour off. Colour is also off when the output is not a terminal, so a skill's text stays plain.
- `--ascii` (or `CG_ASCII=1`, or a non-UTF-8 locale) swaps every glyph for ASCII.
- `--theme light|dark` (or `CG_THEME`) picks the heat ramp. Other colours are the terminal's own 16, so they follow its theme.
- Width follows the terminal (40 to 140 columns). Lines wrap or lose their least important columns; they never run over.

## What each view reads

| View | Endpoints | Notes |
|---|---|---|
| watch | `/api/sessions`, `/api/cachestate`, `/api/requests`, `/api/stats`, `/api/series` | One row per live thread: the main conversation and each subagent. The proxy does not report when the next keep-alive ping fires, so a row shows `↻` when the last touch was a ping. |
| timeline | `/api/requests` | Misses are read from the token columns. Extra dollars = cost above a normal turn of the same model. |
| chart | `/api/stats`, `/api/series`, `/api/spend/explain` | Days are UTC days, as in the dashboard. |
| heatmap | `/api/requests` | The newest 6,000 requests in the window; the footer says how many it covered. The first request of a thread is not counted: it always writes. |
| fill | `/api/requests`, `/api/cachestate` | The split is the first request of the main thread (instructions and tools) against everything added since. The proxy does not record how much of the rest is tool output. |

Recordings: `tv-*.cast` (asciinema) and `tv-*-dark.svg` / `tv-*-light.svg`, from a synthetic history.
