---
name: statusline
description: Enable or disable the context-guru status line — a terminal status line showing money saved (estimate) next to money spent, the prompt-cache state (warm/expiring/cold) from the proxy's own view, context fill against the served window, and an optional weekly tip, for whichever project is routed through the local proxy. Use when the user asks to show savings in the status line, add a status line for context-guru, see cache/keep-alive status in the terminal, or turn any of it off.
---

# context-guru status line

Wires `context-guru-plugin/scripts/statusline.py` into Claude Code's `statusLine` setting, through
`settings.py` — the same deterministic, conservative script that installs routing, extended to
manage this one additional top-level key (never a second settings editor, never a hand-edit).

**What it shows, once enabled and this project is routed** (default layout `balanced`):

```
saved $0.31 of $4.41 session ↻ $0.84 · saved $0.12 of $0.90 subagents
cache ● █████░ 13:40 ↻ │ next $0.03 warm · $0.30 cold │ ctx ███░░░░░ 41% │ ka 3 pings $0.04
ctx ███░░░░░ 41%
Tip: you haven't run /context-guru:save-more or /context-guru:remove-mcp-and-skills for over a week — worth a shot.
```

- **Money.** `saved $0.31 of $4.41 session`: `$4.41` is what the main agent's requests would
  have cost without context-guru, at the gateway's prices; `$0.31` is the saving against it, net
  of context-guru's own model calls and keep-alive pings, so it is always part of the total.
  `saved $0.12 of $0.90 subagents` is the same over every other request of the session
  (sub-agents, teammates, Claude Code's side requests), once there is one. Each listed agent
  shows its own `saved $z of $q` next to its clock. Only savings recorded per request count, so
  the line can read lower than the dashboard. Session only: no today or lifetime total.
  `--layout=detailed` adds `spent $4.10 · $2.73/h · up 1h30m` (Claude Code's own running cost, the
  average rate once five minutes have passed, and the session's age).
- **Cache clock.** A live countdown to expiry, green, amber in the last minute (a fifth of the
  lifetime on the 1h tier), red with `!` in the last 15 s. When the cache is cold the whole cache
  segment (and `next`) is left out: you already know, and it is noise. There is exactly ONE
  clock, the main agent's, from ONE source: the proxy's whenever it has a copy no older than 11 s,
  otherwise Claude Code's own (it used to hand over after 3 s and flip back at each refresh, which
  showed as two alternating timers). A cache kept warm by pings is drawn in **cyan** with `↻`. The proxy
  keeps the clock from the end of the last response, so a long answer is counted. It ticks every
  second (`refreshInterval: 1`, which `settings.py` writes; a value you tuned is kept). `13:40 ↻`
  means the proxy holds the cache warm with keep-alive pings, and the clock is the time until it
  goes cold (the last planned ping plus the lifetime); `↻` on a normal clock means the last touch
  was a ping. `saved $0.31 of $4.41 session ↻ $0.84`: the `↻ $0.84` is what this session's manual
  pings saved (net of their own cost, `manual_pings` in /api/stats), shown once there is one.
- **Next request.** `next $0.03 warm · $0.30 cold`: what the next request costs for the cached
  prefix if the cache is still warm (cache-read price) and if it has gone cold (cache-write price at
  the cache's lifetime tier), so the value of keeping it warm is on the line. Left out once cold. A proxy that predates the warm figure shows `next $0.30 if cold`; an unpriced model
  shows nothing, never $0.00. `--hide=next` removes it.
- **Sub-agents** are not on the line (one clock only; the Claude Code mod shows them). `--show=agents`
  brings back the old per-agent list (`agents reviewer active ● 4:58 · Explore ◐ 0:51`), and the
  `watch` layout is the dedicated per-agent view.
- **Keep-alive.** `ka 3 pings $0.04`: this session's pings and their cost, after the first one.
  The main clock and the context bar follow the main thread, not whichever agent spoke last.
- **Context.** Bar and percent against the model's context limit: the proxy's when it is a
  measured figure for the model the MAIN thread is on, else Claude Code's. (The proxy's top-level
  window describes the latest request on any thread; a sub-agent on a smaller-window model used to
  make a long main prompt read 100% until the main thread spoke again.)
- **Weekly tip** (balanced and detailed, its own last line, on by default): names each of
  `/context-guru:save-more` and `/context-guru:remove-mcp-and-skills` (the ones installed) not run
  in the last 7 days, read from `<state dir>/last-run.json`, never over the network; a new install
  waits a week. A narrow terminal gets `Tip: try /context-guru:save-more (not run for 7+ days)`
  first. `statusline.py --set-tips=off` turns it off, `--hide=tip` per command.
  The status line gives no other advice: that comes from those commands and the automatic sweep.
- A pending proxy update (`▲ update v0.3.2`) comes first so it cannot scroll off.

**Request budget.** A render makes at most one round trip. Answers are cached for all renders and
all sub-agents of a session in `${XDG_CACHE_HOME:-~/.cache}/context-guru/` (a private `0700`
directory, never a shared `/tmp` path): cache state for 10 s, session stats for 30 s. One render
per window refreshes, the others serve the cached copy; after a failure the next attempts back off
exponentially (1 s, 2 s, 4 s ... 5 min) and the last good copy is shown meanwhile. The Claude Code
mods read the same files through `cg.py cachestate --json --session <id>`, which prints the cached
cache state (plus `age_s` and `stale`) and refreshes it under the same rules; they never call the
proxy themselves.

**Layouts and toggles.** `statusline.py --set-layout=detailed` (or `minimal`, `balanced`, `auto`) remembers a
layout across renders without editing settings; bind it to an alias or a command. A remembered
layout outranks the installed command's flag until `auto`. Otherwise
`--layout=minimal|balanced|detailed` (or env `CG_STATUSLINE_LAYOUT`):

| layout | elements |
|---|---|
| minimal | saved, cache (`saved $0.31 of $4.41 │ cache ● ████░░ 3:57`) |
| balanced | update, saved, cache, next, ctx, ka, tip |
| detailed | balanced + `spent` + `upstream slow: 7.0s` (only when slow) + least-used tool/skill (`◇ github 1% remove`) |
| spark | `saved $4.20 today · last 14 days ▂▄▅▇█▂▄▅▇█▂▄▅▇` over the cache clock |
| strip | the last 20 requests as hit `■` / partial `▨` / miss `✖`, and the dollars misses cost today |
| heat | today's cache misses per hour (0h to 24h, darker = more) and the dollars they cost today |
| fill | context as a stacked bar (`▒` always loaded, `█` conversation), percent and time to full |
| watch | the main cache clock over a mini-bar and clock per sub-agent (`agent caches (2) ████ 4:22  █░░░ 0:52`, with the saving beside the clock) |

The last five are one-line versions of the terminal views (`/context-guru:*`), drawn with `tvlib.py`;
`CG_ASCII=1` swaps their glyphs for plain characters.

`--hide=a,b` / `--show=a,b` toggle single elements (`update save spend cache agents ctx ka latency tools tip`;
env `CG_STATUSLINE_HIDE` / `_SHOW`). `--cache` and `--keepalive` still work (`ka` = keep-alive net
saving). `NO_COLOR=1` (or `TERM=dumb`) removes colour. Width comes from the terminal (`--width=N`
or `CG_STATUSLINE_COLUMNS` override); a line wider than that drops the tip, tools, latency, the update
marker, the `saved` label and the context bar, in that order.

**Failure states.** Proxy refused: `cg ✕ proxy offline`. Slow: `cg: stats loading…`, then the last numbers with
`⏳`. Hung for ten minutes: `cg? not responding`. Cost: each render is a disk-cached read (2 s for
session/cache state, 60 s for day) with one 0.6 s deadline; typical render is ~40 ms.

**In every project that is NOT routed through context-guru, it renders nothing at all** — the
script self-gates exactly like the plugin's two hooks. That makes it *safe to render* regardless
of scope, but it is not a reason to default the *write* to user scope — see "Where to install it"
below.

**It never sends a keep-alive ping, and never will.** It only reads. Turning the keep-alive
mechanism on is a separate, explicit action — see `/context-guru:cache-strategy-picker`.

**It is on by default.** `/context-guru:install`'s own orchestrator (`install.sh --route`)
installs it in the same file it just wrote routing to, in the same run, right after routing
succeeds — no separate step, and it never fails the install if it can't (a write conflict just
leaves it skipped, reported as `statusline=skipped` in the install's own output). This skill is
for what that automatic install doesn't cover: turning an extra on, moving it to a different
scope, or putting it back after `off` — or installing it by hand in the rare case someone passed
`--no-statusline` and changed their mind.

## Where to install it

**Inherit the scope of this project's routing — never hardcode a path.** A status line installed
at user scope (`~/.claude/settings.json`) renders in *every* project on the machine, the same
blast radius `/context-guru:install` asks about before touching that file, so a status line asked
for "on this project" must not silently become machine-wide. Hardcoding a path here is exactly
what caused a real incident: an earlier version of this skill (and, separately, `install.sh`
itself) defaulted the status line to user scope unconditionally, so a project-scope install
silently wrote and backed up the user's machine-wide settings file every time.

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" resolve-scope
```

- `scope=`/`file=` — use this file for every command below. It is whichever file OUR routing
  already lives in (`source=recorded` when install itself told us, `source=inferred` when this
  project predates that record and the file was found by checking who owns routing there — either
  way, this is the scope the user already agreed to when they installed context-guru itself).
- `scope=(ask)` — no routing of ours anywhere for this project (context-guru is not installed
  here, or was removed). Ask project vs. machine-wide, the same two options `/context-guru:install`
  uses, and use `.claude/settings.local.json` for project or `~/.claude/settings.json` for
  machine-wide, matching whichever they pick.

Call the resolved path `$FILE` in the rest of this skill.

## 1. Look before you write

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" show --file "$FILE"
```

Read the `statusline=` line. `(unset)` means nothing is configured there yet — continue. Anything
else that is not ours (the next step will say so with `result=conflict`) means the user, or
another plugin, already owns that setting; stop and ask before replacing it, exactly like a
pre-existing `ANTHROPIC_BASE_URL` during install.

## 2. Install it

`--statusline` on its own (no `--url`) touches ONLY the `statusLine` key — it writes nothing to
`env`, so this never starts routing a scope that was not already routed:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\""
```

- `result=added` — report the `backup=` path as their undo, same as install.
- `result=unchanged` — already installed with this exact command; nothing to do.
- `result=conflict` — a statusLine already exists here and is not ours (`existing=` in the
  output). Ask before `--force`; the previous value is recorded and comes back on removal.
- `result=error reason=user_scope_needs_flag` — `$FILE` resolved to the machine-wide file and
  nobody has confirmed that scope for this project. `note=` and `writes=statusline` name the blast
  radius (renders in every project on the machine, not just this one). Ask, using that wording,
  then re-run with `--user-scope` appended — never add the flag on your own judgement.

A new session (or `claude` restarted) is needed to see the change take effect, the same as
`ANTHROPIC_BASE_URL` picking up live only mid-session.

## 3. Confirm

Open (or resume) a session in a project that IS routed through context-guru and watch the status
line render. Nothing to see yet is normal before the first response of a session has usage to
track — that is when this session's own total first becomes nonzero (see `_default_segment` in
`statusline.py`), not a broken feature.

## Change the layout or toggle an element

The toggle is the command string itself - re-run the same install call from step 2 with flags
appended. `settings.py` recognises it as ours (it wrote the previous command) and updates it in
place, no `--force` needed:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --layout=detailed --hide=tools"
```

Re-run step 2's bare command (no flags) to return to the `balanced` default. New session to see it
take effect.

## 4. Turn it off entirely

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" off --file "$FILE"
```

**Use `off`, not `remove`, unless the user means to uninstall routing too.** Since the status
line installs automatically alongside routing now, the two commonly live in the same settings
file — `remove` takes back *everything* `/context-guru:install` wrote there (routing included),
while `off` touches only the `statusLine` key and leaves routing exactly as it was. Restores
whatever `statusLine` (if anything) was there before, exactly like base-URL removal — never just
deletes and leaves the user with nothing.

`remove` still works too, and still does the right thing: if only the status line was ever
installed in a given file (no routing there), it takes back just that; if both were installed
together, it takes back both — that combined behavior is the uninstall path, not the "I just want
the status line gone" path `off` is for.
