---
name: statusline
description: Enable or disable the context-guru status line — a terminal status line showing money saved (estimate) next to money spent, the prompt-cache state (warm/expiring/cold) from the proxy's own view, context fill against the served window, and one actionable hint, for whichever project is routed through the local proxy. Use when the user asks to show savings in the status line, add a status line for context-guru, see cache/keep-alive status in the terminal, or turn any of it off.
---

# context-guru status line

Wires `context-guru-plugin/scripts/statusline.py` into Claude Code's `statusLine` setting, through
`settings.py` — the same deterministic, conservative script that installs routing, extended to
manage this one additional top-level key (never a second settings editor, never a hand-edit).

**What it shows, once enabled and this project is routed** (default layout `balanced`):

```
saved ≈$0.31 (spent $0.41) · day ≈$4.20 │ ● warm 4m │ ███····· 412.0k/1.0M 41%
saved ≈$0.31 (spent $0.41) · day ≈$4.20 │ ○ cold │ ███····· 412.0k/1.0M 41% │ ▸ cold on 412.0k: next send rewrites ≈$1.20 — /compact?
```

- **Money.** `saved ≈$0.31` is an ESTIMATE (`≈`): what the same requests would have cost
  uncompacted, at the gateway's prices, net of context-guru's own model spend and keep-alive
  pings. `(spent $0.41)` is observed, gateway-priced cost of what was actually sent. They are never
  added or divided. `day` is the same estimate since local midnight (everything your token can
  see on this proxy). Omitted for a brand-new session; a real nonzero spend with zero saved prints.
- **Cache.** `● warm 4m` / `◐ 42s left` (under a minute) / `○ cold` / `◌ no cache yet`. The proxy's
  server-side state (last cache-touching request, billed 5m or 1h tier, pings included) overrides
  Claude Code's own `prompt_cache` guess while fresh; otherwise Claude Code's is used.
- **Context.** Bar, tokens and percent against the window actually served: the proxy's when it is a
  measured figure for the model, else Claude Code's.
- **One hint**, only when it matters: cold with >=100k tokens (priced by the proxy), else context
  >=85% full, else a Claude Code-reported cache-miss cause under two minutes old.
- A pending proxy update (`▲ update v0.3.2`) comes first so it cannot scroll off.

**Layouts and toggles.** `--layout=minimal|balanced|detailed` (or env `CG_STATUSLINE_LAYOUT`):

| layout | elements |
|---|---|
| minimal | saved, cache, hint (`saved ≈$0.31 │ ● warm 4m`) |
| balanced | update, saved, cache, ctx, hint |
| detailed | balanced + `proxy: 3ms · upstream: 340ms` + least-used tool/skill (`◇ github 1% remove`) |

`--hide=a,b` / `--show=a,b` toggle single elements (`update save cache ctx hint latency tools ka`;
env `CG_STATUSLINE_HIDE` / `_SHOW`). `--cache` and `--keepalive` still work (`ka` = keep-alive net
saving). `NO_COLOR=1` (or `TERM=dumb`) removes colour. Width comes from the terminal (`--width=N`
or `CG_STATUSLINE_COLUMNS` override); a line wider than that drops tools, latency, the update marker,
the long hint form, the `saved` label and the context bar, in that order, and truncates the hint last.

**Failure states.** Proxy refused: `cg!`. Slow: `cg: stats loading…`, then the last numbers with
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
