---
name: statusline
description: Enable or disable the context-guru status line — a terminal status line showing the prompt-cache TTL countdown and running savings for whichever project is routed through the local proxy. Use when the user asks to show cache status in the status line, add a status line for context-guru, see savings in the terminal, or turn the status line off.
---

# context-guru status line

Wires `context-guru-plugin/scripts/statusline.py` into Claude Code's `statusLine` setting, through
`settings.py` — the same deterministic, conservative script that installs routing, extended to
manage this one additional top-level key (never a second settings editor, never a hand-edit).

**What it shows, once enabled and this project is routed:** a countdown to the prompt-cache TTL
going cold (`cache 4:12` / `cache cold`), followed by running savings in dollars and tokens once
they are non-zero (`$0.42/3.1k saved`), and how many keep-alive pings have fired (`ka 2p`) once
any have. **In every project that is NOT routed through context-guru, it renders nothing at
all** — the script self-gates exactly like the plugin's two hooks, so installing it is safe even
at user scope.

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
line render. Nothing to see yet is normal on the first turn of a session — the cache stopper needs
at least one response with usage to track, and the savings segment stays hidden until it has
something non-zero to report (see `_savings_segment` in `statusline.py`).

## 4. Remove it

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" remove --file ~/.claude/settings.json
```

Restores whatever `statusLine` (if anything) was there before, exactly like base-URL removal —
never just deletes and leaves the user with nothing. If routing was ALSO configured in the same
file, this same call removes both; if only the status line was ever installed there, it is the
only thing this touches.
