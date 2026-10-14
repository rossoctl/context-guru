# Save more

**Question: which config and which keep-alive strategy would have cost me least, on my own history?**

`/context-guru:save-more` replays your past sessions on the proxy under every config Context Guru ships (each preset) and every keep-alive strategy, and shows what each would have saved. It recommends the best of each, asks you once, and changes nothing without a yes.

| Command | What it does |
|---|---|
| `/context-guru:save-more` | both reports, a combined summary, one question at the end |
| `/context-guru:save-more config` | every shipped preset; `--custom` also searches single components, their combinations and their order |
| `/context-guru:save-more cache` | every keep-alive strategy, with the savings by day of week and hour |
| `/context-guru:save-more apply <config\|cache> <option>@<basis>` | make a change the screen offered (after a yes) |
| `/context-guru:save-more revert <config\|cache>` | undo the last change made here |

Add `details` for every option and the held-out comparison, and `--days N` for the window (default 30 days). The replay can take a few minutes; on a terminal a seconds counter runs meanwhile.

Codex: `$context-guru-save-more`, with the same words.

## The screen

Each report opens with a card that answers two questions in plain words: what your **current** setting saved so far, and what the **recommended** change would add, per week, with its likely range. Then a ranked table of every option, and the caveats.

```text
╭─ Save more: config ─────────────────────────────────────────────────────────────────────────╮
│ So far: your current config saved $9.40 in 30 days (observed).                              │
│ Change: preset conservative -> medium would add about $1.49 a week (likely $0.40 to $2.70). │
╰─────────────────────────────────────────────────────────────────────────────────────────────╯
medium would have saved $6.40 more than your current conservative preset on the requests that could
be replayed.

     option                                       saved  spent      net
  ★  medium        + cheap-model relevance pass  $17.90  $2.10  ≈$15.80  ████████████
     high          + summary near a full window  $18.40  $3.50  ≈$14.90  ███████████░
  ▸  current       Your config (conservative)     $9.60  $0.20    $9.40  ███████░░░░░
  ◦  conservative  Trim obvious waste             $9.60  $0.20    $9.40  ███████░░░░░
     off           No compaction                     $0     $0       $0  ░░░░░░░░░░░░
  ★ recommended   ▸ yours now   ◦ the default   ≈ partly estimated
  1 more not evaluated: too few cold turns in the window to replay
  saved: what it avoided paying · spent: what it paid itself · net = saved - spent, on your past
  sessions

  counterfactual · every shipped preset replayed on your stored requests · 2026-09-08..2026-10-08 ·
  40 sessions, 3,100 requests
  ! The replay covers 14% of requests; on the rest the presets were not compared.
  ! medium and high include model components whose output is estimated, not re-run.

Recommended: preset conservative -> medium (takes effect now).
Why: highest net on your own sessions, after its own model spend
Apply it? Say yes, or run: /context-guru:save-more apply config medium@a1b2c3d4

╭─ Save more: cache ──────────────────────────────────────────────────────────────────────────────╮
│ So far: your current keep-alive strategy saved $31.10 in 30 days (observed).                    │
│ Change: cache strategy 5-min-ping -> adaptive would add about $2.89 a week (likely $0.72 to     │
│ $4.90).                                                                                         │
│ Before -> after: $300.00 billed on these sessions, $287.00 with the change.                     │
╰─────────────────────────────────────────────────────────────────────────────────────────────────╯
switch to adaptive keep-alive to save about $12.40 more over the last 30 days.
  best vs default +$12.40 (likely $3.10 to $21.00)  ·  best vs current +$12.40 (likely $3.10 to
  $21.00)

      option                            saved  spent     net
  ★   adaptive    Adaptive keep-alive  $51.20  $7.70  $43.50  ████████████
  ▸◦  5-min-ping  Ping every 5 min     $40.10  $9.00  $31.10  █████████░░░
      none        No pings                 $0     $0      $0  ░░░░░░░░░░░░
  ★ recommended   ▸ yours now   ◦ the default
  1 more not evaluated: too little history: 13 held-out sessions (need 20)
  saved: what it avoided paying · spent: what it paid itself · net = saved - spent, on your past
  sessions

  What adaptive keep-alive adds over the default, by hour (UTC)
        0     6     12    18   23
  Mon   ░░░ ░░ ░░▒░░▒█░░▓█▓░░ ░░
  Tue   ░░░░░░░░▓░░░▓▒▒▓▒▒█░░░░░
  Wed   ░░░░░░░░░▓█▓█▒▓▓▓▒█░░░ ░
  Thu   ░░░░░░ ░░░░█░▒▒█░▒▓░░░░░
  Fri   ░░░░░░░░▓▒ ▒▒▓█▓▓▓▓ ░░░░
  Sat   ░░░░  ░░░  ░░░ ░░░░░░░░░
  Sun   ░░░░░░░░ ░░░░ ░░░░░░░░░░
        ░ little   █ most (up to $0.38 in one hour slot)

  counterfactual · replay of past sessions · 2026-09-08..2026-10-08 · 412 sessions, 18,301 requests
  ! 31 sessions ended inside the window; their last gap is censored.

Recommended: cache strategy 5-min-ping -> adaptive (takes effect at the next proxy restart).
Why: it had the highest net on your own sessions
Apply it? Say yes, or run: /context-guru:save-more apply cache adaptive@3f9a1c

╭─ Save more: both ────────────────────────────────────────────────────────╮
│ config  saved so far    $9.40   recommended change adds about $1.49/week │
│ cache   saved so far   $31.10   recommended change adds about $2.89/week │
╰──────────────────────────────────────────────────────────────────────────╯
Question: apply both, only one of them, or neither?
```

- `★` is the recommended option, `▸` the one you have now, `◦` the default. When the option with the highest net is not recommended (it failed the proxy's own check, or the gain is too small to trust) it is marked `☆`.
- **saved** is what the option avoided paying, **spent** what it paid for itself (pings, Context Guru's own model calls), **net** is saved minus spent. `≈` marks a figure that is partly estimated rather than replayed (a model component whose output could not be re-run).
- For cache, the grid shows where in the week the recommended strategy adds its savings: darker cells saved more. Hours are UTC.
- The footer says how the numbers were obtained (`observed` or `counterfactual`), the window, and how many sessions and requests it covers. Lines starting with `!` are caveats about the data.

It reads the same at 80 columns and without Unicode or colour (`CG_ASCII=1`, `NO_COLOR=1`):

```text
+- Save more: config ---------------------------------------------------------+
| So far: your current config saved $9.40 in 30 days (observed).              |
| Change: preset conservative -> medium would add about $1.49 a week (likely  |
| $0.40 to $2.70).                                                            |
+-----------------------------------------------------------------------------+
medium would have saved $6.40 more than your current conservative preset on the
requests that could be replayed.

     option                                 saved  spent      net
  *  medium        + cheap-model relevan~  $17.90  $2.10  ~$15.80  ############
     high          + summary near a full~  $18.40  $3.50  ~$14.90  ###########-
  >  current       Your config (conserva~   $9.60  $0.20    $9.40  #######-----
  d  conservative  Trim obvious waste       $9.60  $0.20    $9.40  #######-----
     off           No compaction               $0     $0       $0  ------------
  * recommended   > yours now   d the default   ~ partly estimated
  1 more not evaluated: too few cold turns in the window to replay
  saved: what it avoided paying | spent: what it paid itself | net = saved -
  spent, on your past sessions

  counterfactual | every shipped preset replayed on your stored requests |
  2026-09-08..2026-10-08 | 40 sessions, 3,100 requests
  ! The replay covers 14% of requests; on the rest the presets were not
  compared.
  ! medium and high include model components whose output is estimated, not re-
  run.

Recommended: preset conservative -> medium (takes effect now).
Why: highest net on your own sessions, after its own model spend
Apply it? Say yes, or run: /context-guru:save-more apply config medium@a1b2c3d4

+- Save more: cache ----------------------------------------------------------+
| So far: your current keep-alive strategy saved $31.10 in 30 days            |
| (observed).                                                                 |
| Change: cache strategy 5-min-ping -> adaptive would add about $2.89 a week  |
| (likely $0.72 to $4.90).                                                    |
| Before -> after: $300.00 billed on these sessions, $287.00 with the change. |
+-----------------------------------------------------------------------------+
switch to adaptive keep-alive to save about $12.40 more over the last 30 days.
  best vs default +$12.40 (likely $3.10 to $21.00)  |  best vs current +$12.40
  (likely $3.10 to $21.00)

      option                            saved  spent     net
  *   adaptive    Adaptive keep-alive  $51.20  $7.70  $43.50  ############
  >d  5-min-ping  Ping every 5 min     $40.10  $9.00  $31.10  #########---
      none        No pings                 $0     $0      $0  ------------
  * recommended   > yours now   d the default
  1 more not evaluated: too little history: 13 held-out sessions (need 20)
  saved: what it avoided paying | spent: what it paid itself | net = saved -
  spent, on your past sessions

  What adaptive keep-alive adds over the default, by hour (UTC)
        0     6     12    18   23
  Mon   ... .. ..:..:#..*#*.. ..
  Tue   ........*...*::*::#.....
  Wed   .........*#*#:***:#... .
  Thu   ...... ....#.::#.:*.....
  Fri   ........*: ::*#**** ....
  Sat   ....  ...  ... .........
  Sun   ........ .... ..........
        . little   # most (up to $0.38 in one hour slot)

  counterfactual | replay of past sessions | 2026-09-08..2026-10-08 | 412
  sessions, 18,301 requests
  ! 31 sessions ended inside the window; their last gap is censored.

Recommended: cache strategy 5-min-ping -> adaptive (takes effect at the next
proxy restart).
Why: it had the highest net on your own sessions
Apply it? Say yes, or run: /context-guru:save-more apply cache adaptive@3f9a1c

+- Save more: both --------------------------------------------------------+
| config  saved so far    $9.40   recommended change adds about $1.49/week |
| cache   saved so far   $31.10   recommended change adds about $2.89/week |
+--------------------------------------------------------------------------+
Question: apply both, only one of them, or neither?
```

## Applying, and undoing

Looking never changes anything. When there is a recommendation, the report ends with an `Apply it?` line, and the command asks one question: "Apply both recommended changes?" with the choices both, only config, only cache, or neither. In Claude Code, the change itself is a separate command (`cg.py apply` or `cg.py revert`), so Claude Code also asks permission to run it; reads are pre-approved, changes never are. Run in a terminal, the command asks `[y/N]` for each change; the default is No.

```text
Applied: preset set to medium (takes effect now).
Undo: /context-guru:save-more revert config
```

`revert` puts back the setting from before your last change made here:

```text
Applied: Restored the pipeline from before your last change (takes effect now).
```

If the numbers changed since you saw them, nothing is written and the current report is shown again. When the proxy's own check fails (a keep-alive plan that did not beat the default on your most recent sessions, for example) nothing goes live; switching it on regardless is a second, separate question, sent only with `--force`:

```text
Not applied: The go-live check did not pass, so it runs in shadow and changes nothing.
Apply anyway, although that check failed: /context-guru:save-more apply cache by-segment@3f9a1c --force
```

The words may come in either order (`apply cache X` or `apply X cache`). A change typed under a read command (`save-more revert config`, `config revert`) is not run; the command prints the exact one to run instead.

## When it cannot run

| What you see | What to do |
|---|---|
| `No Context Guru proxy is routing this session` | `/context-guru:install`, or point at a proxy with `CG_URL=http://127.0.0.1:PORT` |
| `The proxy at ... is not answering` | open a new session (it starts the proxy) or run `/context-guru:update` |
| `This proxy is older than this command` | `/context-guru:update` |
| `The analysis took longer than 5 minutes` | a shorter window: `--days 7` |
| `This proxy needs a sign-in for this` | on the hosted service, set `CG_TOKEN` to your context-guru token (taken from the agent's own `cg_live_` key when there is one), or change it in the dashboard |
| any other sentence | the proxy's own reason (for example, too many reports at once); act on it |

## Moved from `/context-guru:adapt`

| Before | Now |
|---|---|
| `/context-guru:adapt compaction` | `/context-guru:save-more config` |
| `/context-guru:adapt cache` | `/context-guru:save-more cache` |
| `/context-guru:adapt tools` | `/context-guru:remove-mcp-and-skills` |
| `/context-guru:adapt` | `/context-guru:save-more` |

`/context-guru:adapt` still works for one release: it says where the command moved and shows the new screen.

## The report format

On the hosted service the command authenticates with the account's own proxy token in `x-context-guru-token`; these routes accept it for the caller's own account only. A local proxy answers loopback callers without one.

Each report is one endpoint on the proxy's dashboard API, and each change is a POST to a second one. Integrations can use them directly. `config` is served at `compaction`, `cache` at `cache`, and `/context-guru:remove-mcp-and-skills` at `tools`.

```
GET  /api/adapt/{compaction|cache|tools}?since=<ms>&until=<ms>[&custom=1]
POST /api/adapt/{kind}/apply?since=<ms>&until=<ms>     {"option": "<id>", "basis": "<basis_id>"}
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
  "to_date": {"saved_usd": 0, "since": 0, "until": 0, "class": "observed", "items": 0},
  "current": "<option id>", "default": "<option id>", "best": "<option id>",
  "options": [{"id": "", "label": "", "detail": "",
               "class": "counterfactual | estimate",
               "billed_usd": 0, "saved_usd": 0, "spend_usd": 0, "net_usd": 0,
               "sessions_helped": 0, "sessions_hurt": 0}],
  "recommendation": {"option": "<id>", "change": "one line", "net_delta_usd": 0, "per_week_usd": 0,
                     "ci95": [0, 0], "items_n": 0, "effective": "now | restart | next_session", "reason": ""},
  "heatmap": {"label": "", "tz": "UTC", "values": [[0, "... 24 hours"], "... 7 days, Monday first"]},
  "removed": [{"kind": "", "name": "", "since_ms": 0, "saved_usd": 0}],
  "items": [{"kind": "", "name": "", "usd": 0, "last_used_ms": null, "remove": null}],
  "warnings": []
}
```

- Money is in US dollars at the prices the proxy is configured with. Every option is evaluated on the same window and the same sessions.
- `saved_usd` is gross (versus no lever at all), `spend_usd` is what the option itself spends, `net_usd = saved_usd - spend_usd`.
- `recommendation` is `null` when the current setting is already the best, or when there is too little data to say; `summary` then says which.
- Optional extras the screen shows when present: `relative` (paired differences such as `best_vs_default`, each `{usd, ci95}`, printed right under the summary), `disagreement` (one line, shown under the summary), per-option `held_out` (`vs_default_usd`, `vs_default_ci95`, `builder_gate.pass`; shown under `details`), `unavailable` (an option that could not be evaluated: `net_usd` is `null`, never 0), and `calibration`.
- `to_date` is what the current setting saved so far (for tools: what earlier removals saved), the first line of the card; missing means not measured, never 0. `per_week_usd` and `ci95` give the card's second line. `heatmap` (cache) and `removed` (tools) are drawn when present. `custom=1` (config) adds searched combinations to the shipped presets.
- `items` is used by `tools` only, and never lists a built-in tool; the screen drops any item whose kind is not `mcp_server`, `mcp_tool` or `skill`. `sessions_helped`, `sessions_hurt`, `detail`, `per_month_usd` and `reason` are optional.

The apply answer is `{"ok": true, "applied": "<id>", "previous": "<id>", "effective": "...", "note": "one line", "undo": {"option": "<id>", "basis": "<basis_id>"}}`. A body of `{"option": "revert"}` (no basis) undoes the last change made through this route; its answer has the same shape. `"force": true` is sent only after the user said yes a second time, when an earlier answer had `"ok": false` with `"force_available": true`. Errors are a non-2xx status with `{"error": "a sentence"}`: 400 for an unknown option, 409 when `basis` no longer matches the current report, 401 or 403 when the caller is not allowed to change the setting, 429 when too many reports or changes run at once. `basis` is required (400 without it). The command always sends one: for an option typed without one it reads the current report first and sends that report's `basis_id`. `undo` may carry its own `basis` (the report after the write), which the `Undo:` line includes. 501 means the proxy has no configuration it can write; its `error` says what to do instead.
