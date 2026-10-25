---
name: statusline
description: Enable or disable the context-guru status line — a terminal status line showing money saved (estimate), the prompt-cache clock (warm/expiring/cold) from the proxy's own view, and context fill against the served window, for whichever project is routed through the local proxy. Use when the user asks to show savings in the status line, add a status line for context-guru, see cache/keep-alive status in the terminal, or turn any of it off.
---

# context-guru status line

Wires `context-guru-plugin/scripts/statusline.py` into Claude Code's `statusLine` setting, through
`settings.py` — the same deterministic, conservative script that installs routing, extended to
manage this one additional top-level key (never a second settings editor, never a hand-edit).

**What it shows, once enabled and this project is routed** (default layout `balanced`):

```
saved $18.60 of $61.40 session · saved $4.20 of $20.50 subagents
cache ● █████░ 4:00 ↻ │ ctx ███░░░░░ 41%
```

The status line owns three facts: the money, the main cache clock and the context bar. The Context
Guru mod owns the price of the next message, the sub-agent caches, the weekly tip and Ping.

- **Money.** `saved $18.60 of $61.40 session`: `$61.40` is what the main agent's requests would
  have cost without context-guru, at the gateway's prices; `$18.60` is the saving against it, net
  of context-guru's own model calls and keep-alive pings, so it is always part of the total.
  `saved $4.20 of $20.50 subagents` is the same over every other request of the session
  (sub-agents, teammates, Claude Code's side requests), once there is one. The line shows no
  per-agent status. Only savings recorded per request count, so the line can read lower than the
  dashboard. Session only: no today or lifetime total. Money is `$12.34`, `$0.06`, `<$0.01` under a
  cent, and no decimals from $100.
  `--layout=detailed` adds `spent $4.10 · $2.73/h · up 1h30m` (Claude Code's own running cost, the
  average rate once five minutes have passed, and the session's age).
- **Subscription.** When Claude Code sends `rate_limits` (a Claude subscription), line 1 becomes
  `5h 34% · saved 1.2M rd / 85.0k wr` instead of money: the real share of the 5-hour limit used, and
  the tokens compaction kept out of this window, as would-be cache reads (`replay_tokens`) and
  writes (`saved_unique`). Plain, unweighted counts, compaction only (`--layout=detailed` says so). `spent` and `keep-alive … $` are hidden. An ended window shows nothing. API keys, Bedrock and Vertex keep the money view.
- **Cache clock.** A live countdown to expiry (`m:ss`, `h:mm:ss` from one hour), green, amber in the
  last minute (a fifth of the lifetime on the 1h tier), red with `!` in the last 15 s. When the
  cache is cold the segment stays, dim, never red or amber: `cache ○ cold 3:12` (how long ago it
  expired; `cache ○ cold` alone when the time is unknown, and in `minimal`). The next message pays
  the rewrite, and the line must not shrink at that moment. There is exactly ONE clock, the main
  agent's, from ONE source: the proxy's whenever it has a copy no older than 45 s, otherwise Claude
  Code's own. A cache kept warm by pings is drawn in **cyan** with `↻`. The proxy keeps the clock
  from the end of the last response, so a long answer is counted. It ticks every second
  (`refreshInterval: 1`, which `settings.py` writes; a value you tuned is kept). `13:40 ↻` means
  the proxy holds the cache warm with keep-alive pings, and the clock is the time until it goes
  cold (the last planned ping plus the lifetime); `↻` on a normal clock means the last touch was a
  ping. `saved $0.31 of $4.41 session ↻ $0.84`: the `↻ $0.84` is what this session's manual
  pings saved (net of their own cost, `manual_pings` in /api/stats), shown once there is one.
- **Old data.** Money and the context bar never vanish because a document is old. Past the age the
  proxy copy is trusted, the last figures stay, marked `⏳`, and the clock is computed from the
  document's own timestamps at any age. `cg: stats loading…` shows only when there never was a
  document.
- **Context.** Bar and percent against the model's context limit: the proxy's when it is a
  measured figure for the model the MAIN thread is on, else Claude Code's. Adds `⚠ rewrite near`
  from 85%.
- **Not on the line, by default.** The next-message price (`next $0.06 warm · $0.78 cold`), the
  sub-agent caches and the weekly tip are in the mod's band and pane. The keep-alive count is gone
  from `balanced`; `detailed` shows it in plain words, `keep-alive 3 pings · $0.04`. Opt in with
  `--show=next`, `--show=agents`, `--show=ka` or `--show=tip`. The tip names each of
  `/context-guru:save-more` and `/context-guru:remove-mcp-and-skills` (the ones installed) not run
  in the last 7 days, read from `<state dir>/last-run.json`, never over the network; a new install
  waits a week. `statusline.py --set-tips=off` turns it off.
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
| minimal | saved, cache (`saved $18.60 of $61.40 │ cache ● █████░ 4:00`; cold is `cache ○ cold`) |
| balanced | update, saved, cache, ctx |
| detailed | balanced + `spent` + `keep-alive 3 pings · $0.04` + `upstream slow: 7.0s` (only when slow) + least-used tool/skill (`◇ github 1% remove`) |

The layouts `spark`, `strip`, `heat`, `fill` and `watch` are retired from the status line: they are
sections of the mod's pane (`/context-guru:context-guru`: SAVED LATELY, CACHE HITS, MISSES BY HOUR,
CONTEXT, AGENTS). A flagged retired layout falls back to `balanced`, silently. A remembered one is ignored: the installed layout applies.

`--hide=a,b` / `--show=a,b` toggle single elements (`update save spend cache next agents ctx ka latency tools tip`;
env `CG_STATUSLINE_HIDE` / `_SHOW`). `--cache` and `--keepalive` still work. `NO_COLOR=1` (or `TERM=dumb`)
removes colour. Width comes from the terminal (`--width=N` or `CG_STATUSLINE_COLUMNS` override); a
line wider than that sheds, in order: spend and latency (detailed), keep-alive, the update marker, the
`subagents` phrase, the `session` label and the context bar. The clock shortens last.

**Failure states.** Proxy refused: `cg ✕ proxy offline`. Slow: `cg: stats loading…`, then the last numbers with
`⏳`. Hung for ten minutes: `cg? not responding` (unless a copy of the cache state exists: then the figures stay, with `⏳`). Cost: each render is a disk-cached read (2 s for
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
