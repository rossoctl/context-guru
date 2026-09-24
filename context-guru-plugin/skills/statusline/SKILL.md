---
name: statusline
description: Enable or disable the context-guru status line — a terminal status line showing the context-window bar, this session's running savings against its own running cost/tokens, the proxy/upstream latency split, and a least-used-tool hint, for whichever project is routed through the local proxy. Use when the user asks to show savings in the status line, add a status line for context-guru, see cache/keep-alive status in the terminal, or turn any of it off.
---

# context-guru status line

Wires `context-guru-plugin/scripts/statusline.py` into Claude Code's `statusLine` setting, through
`settings.py` — the same deterministic, conservative script that installs routing, extended to
manage this one additional top-level key (never a second settings editor, never a hand-edit).

**What it shows by default, once enabled and this project is routed:**

```
████····  100/200.0k 50% | $0.03/12k saved of $0.41/187k | proxy: 3ms · upstream: 340ms | ◇ github 1% remove
```

Four segments, each independently optional — a segment whose numbers are not available just does
not print:

- **The context bar** — tokens used against the model's real context window, coloured green
  under 50%, yellow under 70%, red above. Read straight off Claude Code's own statusLine payload
  (`context_window.total_input_tokens` / `.context_window_size` / `.used_percentage`); no extra
  network call.
- **What THIS session saved, against what it has spent so far** — `$0.03/12k saved of
  $0.41/187k`. The first pair is this session's own savings (`total_saved_usd` / `saved_unique`,
  scoped to this one session_id — see the script's own `_fetch_stats`); the second is this
  session's own running cost and tokens, read straight off Claude Code's own statusLine payload
  (`cost.total_cost_usd`, `context_window`). Omitted, not shown as zeroes, before this session has
  spent anything at all — a real $0 saved once there IS a total to compare it to still prints.
- **The proxy/upstream latency split** — `proxy: 3ms · upstream: 340ms`, ContextGuru's own added
  latency next to what the upstream provider took, each labelled. Both are `/api/stats`' own
  `cg_latency_ms_avg` / `upstream_ms_avg`; nothing here is derived.
- **The least-used MCP server or skill this session** — `◇ github 1% remove` (at 1% or under of
  this session's tool/skill uses) or `◇ some-skill 8% move` (at 20% or under, i.e. move it to
  project scope rather than global). Checked against every MCP server named in
  `~/.claude/settings.json` and every skill this plugin ships — not a system-prompt enumeration
  (nothing exposes that), so this is what the session's own transcript tail shows was actually
  called, not a claim about what any one session's prompt loaded.

**In every project that is NOT routed through context-guru, it renders nothing at all** — the
script self-gates exactly like the plugin's two hooks. That makes it *safe to render* regardless
of scope, but it is not a reason to default the *write* to user scope — see "Where to install it"
below.

**Two more extras are off by default.** The prompt-cache TTL countdown (`cache 4:12` / `cache
cold`) and the keep-alive savings counter (`ka ≤2miss $0.07`) are extras, each behind its own flag
on the installed command — see "Turn an extra on" below. Neither is shown until you ask for it.

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

## Turn an extra on

The toggle is the command string itself — re-run the same install call from step 2 with a flag
appended. `settings.py` already recognises this as ours (it wrote the previous command) and
updates it in place, no `--force` needed:

```bash
# cache TTL countdown
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --cache"

# keep-alive net saving + misses prevented
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --keepalive"

# both
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
  --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --cache --keepalive"
```

## Turn an extra back off

Re-run step 2's bare command (no flags) to return to savings-only. New session to see it take
effect, same as turning one on.

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
