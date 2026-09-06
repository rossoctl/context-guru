---
name: statusline
description: Enable or disable the context-guru status line — a terminal status line showing this session's running savings against its own running cost/tokens, for whichever project is routed through the local proxy. Use when the user asks to show savings in the status line, add a status line for context-guru, see cache/keep-alive status in the terminal, or turn any of it off.
---

# context-guru status line

Wires `context-guru-plugin/scripts/statusline.py` into Claude Code's `statusLine` setting, through
`settings.py` — the same deterministic, conservative script that installs routing, extended to
manage this one additional top-level key (never a second settings editor, never a hand-edit).

**What it shows by default, once enabled and this project is routed:** what THIS session saved,
against what it has spent so far — `$0.03/12k saved of $0.41/187k`. The first pair is this
session's own savings (`total_saved_usd` / `saved_unique`, scoped to this one session_id — see
the script's own `_fetch_stats`); the second is this session's own running cost and tokens, read
straight off Claude Code's own statusLine payload (`cost.total_cost_usd`, `context_window`).
Omitted, not shown as zeroes, before this session has spent anything at all — a real $0 saved
once there IS a total to compare it to still prints. **In every project that is NOT routed
through context-guru, it renders nothing at all** — the script self-gates exactly like the
plugin's two hooks, so installing it is safe even at user scope.

**Everything else is off by default.** The prompt-cache TTL countdown (`cache 4:12` / `cache
cold`) and the keep-alive ping counter (`ka 2p`) are extras, each behind its own flag on the
installed command — see "Turn an extra on" below. Neither is shown until you ask for it.

**It never sends a keep-alive ping, and never will.** It only reads. Turning the keep-alive
mechanism on is a separate, explicit action — see `/context-guru:keepalive`.

## Where to install it

Default to **user scope** (`~/.claude/settings.json`), unlike `/context-guru:install`'s
project-first default: a status line is a property of the terminal, not of one repository, and
because the script renders nothing in unrouted projects, installing it once at user scope is safe
regardless of which projects you later route. Ask before a different scope only if the user
names one.

## 1. Look before you write

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" show --file ~/.claude/settings.json
```

Read the `statusline=` line. `(unset)` means nothing is configured there yet — continue. Anything
else that is not ours (the next step will say so with `result=conflict`) means the user, or
another plugin, already owns that setting; stop and ask before replacing it, exactly like a
pre-existing `ANTHROPIC_BASE_URL` during install.

## 2. Install it

`--statusline` on its own (no `--url`) touches ONLY the `statusLine` key — it writes nothing to
`env`, so this never starts routing a scope that was not already routed:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\""
```

- `result=added` — report the `backup=` path as their undo, same as install.
- `result=unchanged` — already installed with this exact command; nothing to do.
- `result=conflict` — a statusLine already exists here and is not ours (`existing=` in the
  output). Ask before `--force`; the previous value is recorded and comes back on removal.

A new session (or `claude` restarted) is needed to see the change take effect, the same as
`ANTHROPIC_BASE_URL` picking up live only mid-session.

## 3. Confirm

Open (or resume) a session in a project that IS routed through context-guru and watch the status
line render. Nothing to see yet is normal before the first response of a session has usage to
track — that is when this session's own total first becomes nonzero (see `_default_segment` in
`statusline.py`), not a broken feature.

## Turn an extra on

The toggle is the command string itself — re-run the same install call from step 2 with a flag
appended. `settings.py` already recognises this as ours (it wrote the previous command) and
updates it in place, no `--force` needed:

```bash
# cache TTL countdown
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --cache"

# keep-alive ping counter
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --keepalive"

# both
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --cache --keepalive"
```

## Turn an extra back off

Re-run step 2's bare command (no flags) to return to savings-only. New session to see it take
effect, same as turning one on.

## 4. Remove it entirely

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" remove --file ~/.claude/settings.json
```

Restores whatever `statusLine` (if anything) was there before, exactly like base-URL removal —
never just deletes and leaves the user with nothing. If routing was ALSO configured in the same
file, this same call removes both; if only the status line was ever installed there, it is the
only thing this touches.
