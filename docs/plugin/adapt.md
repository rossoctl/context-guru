# Adapt

**Question: which setting would have cost me least, on my own history?**

`/context-guru:adapt` replays your past sessions on the proxy under each available setting and shows, side by side, what the current setting, the default and the best one would have cost. It recommends at most one change, asks you before making it, and makes it only on a yes.

| Subcommand | What it tunes |
|---|---|
| `/context-guru:adapt cache` | the cache keep-alive strategy (when, and whether, idle sessions are pinged) |
| `/context-guru:adapt compaction` | the tail-compaction configuration |
| `/context-guru:adapt tools` | MCP servers, skills and tools you carry but never use |
| `/context-guru:adapt` | one line for each of the three |

Add `details` for each option's settings and per-option notes, and `--days N` to change the window (default 30 days). The replay can take a few minutes; on a terminal a seconds counter runs while it works.

## Example

```text
Cache: switch to adaptive keep-alive to save about $12.40 more over the last 30 days.

                           saved  spent     net
  ▸  Ping every 5 min     $40.10  $9.00  $31.10  █████████░░░  current, default
  ★  Adaptive keep-alive  $51.20  $7.70  $43.50  ████████████  best
     No pings                 $0     $0      $0  ░░░░░░░░░░░░

  saved = what the option avoided paying; spent = what it paid for itself; net = saved - spent.
  counterfactual · replay of past sessions · 2026-09-08..2026-10-08 · 412 sessions, 18,301 requests
  ! 31 sessions ended inside the window; their last gap is censored.

Change: cache strategy 5-min-ping -> adaptive (takes effect at the next proxy restart).
Why: most idle gaps here end between 5 and 20 minutes
Apply it? Say yes, or run: /context-guru:adapt cache apply adaptive@3f9a1c
```

- The first line is the answer.
- `▸` marks the setting you have now, `★` the best one; the last column also names the default.
- **saved** is what the option avoided paying, **spent** is what the option paid for itself (keep-alive pings, Context Guru's own model calls), and **net** is saved minus spent. The bars compare net.
- The footer line says how the numbers were obtained: `observed` (what was actually billed) or `counterfactual` (a replay of the same sessions under the other setting), the window, and how many sessions and requests it covers. Lines starting with `!` are caveats about the data.

The overview runs all three and prints one line each. Each row is a separate change measured on its own, so the rows are not added together:

```text
The best single change would have saved $31.40 over the last 30 days.

  (each row is a separate change measured on its own; do not add them)
  cache       $12.40  cache strategy 5-min-ping -> adaptive
  compaction          the compaction replay failed: no sessions in the window
  tools       $31.40  filter mcp__playwright, mcp__figma, skill__brand-voice and 3 more

See one, and apply it: /context-guru:adapt cache | compaction | tools
```

For `tools`, the unused items are listed below the table:

```text
Tools: 9 declared items were never used; filtering the 6 with enough evidence would have saved $31.40.

                                                                  saved  spent     net
  ▸  Keep everything                                                 $0     $0      $0  ░░░░░░░░░░░░  current, default
  ★  Filter the 6 unused items seen in 5+ sessions over 7+ days  $31.40     $0  $31.40  ███████████░  best
     Filter all 9 unused items                                   $33.90     $0  $33.90  ████████████

  saved = what the option avoided paying; spent = what it paid for itself; net = saved - spent.
  counterfactual · unused declarations priced on the requests that carried them · 2026-09-08..2026-10-08 · 64 sessions, 5,210 requests
  ! Unused means unused in this window, not that it will never be needed.
  ! Filtering changes the cached prefix, so the next request re-writes it once; that cost is not included.

Unused in this window (5 of 9):
  mcp server  playwright             $18.40  never used
  mcp server  figma                   $6.20  never used
  skill       brand-voice             $3.10  never used
  mcp tool    sentry__list_releases   $2.20  never used
  skill       slides                  $1.50  never used

Change: filter mcp__playwright, mcp__figma, skill__brand-voice and 3 more (takes effect now).
Why: declared in 5+ sessions over 7+ days and never called
Apply it? Say yes, or run: /context-guru:adapt tools apply remove-sufficient@8c21e0d4
```

## Applying a change

Nothing is changed by looking. When there is a recommendation, the screen ends with an `Apply it?` line:

- In Claude Code, the command asks you one yes/no question. It applies the change only if you answer **Yes, apply it**; anything else leaves your settings as they were.
- Run directly in a terminal, `cg.py adapt cache` asks `Apply this change? [y/N]`; the default is No.
- To apply by hand, run the command on the `Apply it?` line.

```text
Applied: cache strategy set to adaptive (takes effect at the next proxy restart).
Undo: /context-guru:adapt cache apply 5-min-ping
```

Some changes take a while on the proxy (building a keep-alive plan, for example). The command waits for up to two minutes, with a seconds counter on a terminal, then prints the result the same way:

```text
Applied: 6 unused items filtered (playwright, figma, brand-voice and 3 more) (takes effect now).
Undo: /context-guru:adapt tools apply keep
```

The token after `apply` is the option and the report it came from. If the numbers have changed since you saw them (new sessions arrived, for example), the proxy refuses, nothing is written, and the current report is shown again so you can decide on what is true now:

```text
Not applied: the numbers changed since you saw them. Here they are again; nothing was written.

Cache: switch to adaptive keep-alive to save about $12.40 more over the last 30 days.

                           saved  spent     net
  ▸  Ping every 5 min     $40.10  $9.00  $31.10  █████████░░░  current, default
  ★  Adaptive keep-alive  $51.20  $7.70  $43.50  ████████████  best
     No pings                 $0     $0      $0  ░░░░░░░░░░░░

  saved = what the option avoided paying; spent = what it paid for itself; net = saved - spent.
  counterfactual · replay of past sessions · 2026-09-08..2026-10-08 · 412 sessions, 18,301 requests
  ! 31 sessions ended inside the window; their last gap is censored.

Change: cache strategy 5-min-ping -> adaptive (takes effect at the next proxy restart).
Why: most idle gaps here end between 5 and 20 minutes
Apply it? Say yes, or run: /context-guru:adapt cache apply adaptive@3f9a1c
```

The `Undo:` line puts the previous setting back. Some changes take effect only after the proxy restarts or from the next session; the result line says which.

## Codex

The same command is the skill `$context-guru-adapt`, with the same words: `$context-guru-adapt cache`, `$context-guru-adapt tools details`. It runs the same script. Codex asks the yes/no question as a message and applies only after you answer yes.

## When it cannot run

| What you see | What to do |
|---|---|
| `No Context Guru proxy is routing this session` | `/context-guru:install`, or point at a proxy with `CG_URL=http://127.0.0.1:PORT` |
| `The proxy at ... is not answering` | open a new session (it starts the proxy) or run `/context-guru:update` |
| `This proxy is older than /context-guru:adapt` | `/context-guru:update` |
| `The analysis took longer than 5 minutes` | a shorter window: `--days 7` |
| `This proxy needs a dashboard sign-in` | on the hosted service, sign in to the dashboard and change the setting there |

## The report format

Each subcommand reads one endpoint on the proxy's dashboard API and, after a yes, posts to a second one. Integrations can use them directly.

```
GET  /api/adapt/{cache|compaction|tools}?since=<ms>&until=<ms>
POST /api/adapt/{kind}/apply     {"option": "<id>", "basis": "<basis_id>"}
```

The report:

```json
{
  "kind": "cache", "version": 1,
  "summary": "one sentence: the answer",
  "window": {"since": 0, "until": 0, "sessions": 0, "requests": 0},
  "class": "observed | counterfactual",
  "method": "how the options were evaluated, in a few words",
  "basis_id": "short id of this computation",
  "current": "<option id>", "default": "<option id>", "best": "<option id>",
  "options": [{"id": "", "label": "", "detail": "",
               "billed_usd": 0, "saved_usd": 0, "spend_usd": 0, "net_usd": 0,
               "sessions_helped": 0, "sessions_hurt": 0}],
  "recommendation": {"option": "<id>", "change": "one line", "net_delta_usd": 0,
                     "per_month_usd": 0, "effective": "now | restart | next_session", "reason": ""},
  "items": [{"kind": "", "name": "", "usd": 0, "last_used_ms": null, "remove": null}],
  "warnings": []
}
```

- Money is in US dollars at the prices the proxy is configured with. Every option is evaluated on the same window and the same sessions.
- `saved_usd` is gross (versus no lever at all), `spend_usd` is what the option itself spends, `net_usd = saved_usd - spend_usd`.
- `recommendation` is `null` when the current setting is already the best, or when there is too little data to say; `summary` then says which.
- `items` is used by `tools` only. `sessions_helped`, `sessions_hurt`, `detail`, `per_month_usd` and `reason` are optional.

The apply answer is `{"ok": true, "applied": "<id>", "previous": "<id>", "effective": "...", "note": "one line", "undo": {"option": "<id>"}}`. Errors are a non-2xx status with `{"error": "a sentence"}`: 400 for an unknown option, 409 when `basis` no longer matches the current report, 401 or 403 when the caller is not allowed to change the setting. `basis` may be omitted (an undo, for example); then no staleness check is made.
