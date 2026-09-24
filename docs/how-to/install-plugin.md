# Install as a Claude Code plugin

No toolchain, reversible, and the routing decision is per repo.

```
/plugin marketplace add rossoctl/context-guru
/plugin install context-guru@context-guru
/reload-plugins
/context-guru:install
```

The first two are once per machine. The last is once per repo, and it is the one that decides which
sessions get routed. `/reload-plugins` is not optional — until you run it, this session has no
`/context-guru:*` skills, so `/context-guru:install` answers `Unknown command`. Starting a fresh
session works too; reloading is just quicker.

### Recommended first: grant the plugin's scripts once

**Do this first if sessions run under auto mode or a restrictive permission policy.**
`/context-guru:install` opens by running `install.sh --route --plan` in a `` !`` `` block. If that
command is denied, the skill's instructions never reach the model at all, and it may improvise a
wrong suggestion instead of asking for approval.

1. Run `/permissions` → **add a new rule**.
2. Paste the rule below **alone** — not as a JSON object — with your own absolute path (the one
   `/context-guru:install` prints if you're unsure):

   ```
   Bash(/Users/{you}/.claude/plugins/cache/context-guru/**)
   ```

3. Save it to **Project settings (local)** (`.claude/settings.local.json`, gitignored).

Use an absolute path — `~` is not expanded in permission rules. One rule covers every command the
plugin runs, because starting the proxy and writing the routing key are the two steps that put your
model traffic through a locally installed binary, and Claude Code is right to ask about that
explicitly. See [If the install is blocked](#if-the-install-is-blocked) for the alternatives.

### Pick a plugin scope

`/plugin install` asks where **the plugin itself** goes:

| Option | Written to | Who gets it |
|---|---|---|
| Install for you (user scope) | `~/.claude/settings.json` | you, every project on this machine |
| Install for all collaborators (project scope) | `<repo>/.claude/settings.json` | everyone who clones the repo (committed) |
| Install for you, in this repo only (local scope) | `<repo>/.claude/settings.local.json` | you, this repo only (gitignored) |

**Local scope is the recommended pick, especially the first time** — nothing left behind if you
decide not to keep it. Move to user scope later by re-running `/plugin install` with a different
answer; there's no migration step.

This is a separate question from *routing scope*, which `/context-guru:install` asks next and which
decides which sessions' traffic actually goes through the proxy (table below).

### `/plugin configure` — five options, all with working defaults

Open it, press **Save configuration**, and nothing changes. Only one of these usually needs
setting, and only in one situation. The Preset and Cache strategy rows have their own pickers —
`/context-guru:preset-picker` and `/context-guru:cache-strategy-picker` — since the raw configure
form has no explanation of what each choice does; use those for the two rows that need judgment.

| Option | Default | Change it when |
|---|---|---|
| **Proxy port** | `8787` | something already holds 8787 (not 4000 — that collides with litellm) |
| **Preset** | `off` | you want context editing at all. `off` is passthrough: nothing dropped, no model called. `/context-guru:preset-picker` explains each one |
| **Cache strategy** | `5-min-ping` | you don't want keep-alive pings that spend a little of your own quota to keep the cache warm. `none` turns it off. `/context-guru:cache-strategy-picker` explains whether it's paying for itself on `keepalive_net_usd` |
| **Idle exit** | `24h` | rarely — floor is `max(2 × store.ttl_seconds, 1h)` |
| **Upstream base URL** | *(empty)* | something else is already the gateway (a corporate proxy, a hosted-agent pod) — set it to whatever `ANTHROPIC_BASE_URL` already contains so context-guru chains behind it instead of replacing it |

## You do not need an API key

Setting `ANTHROPIC_BASE_URL` **without** a credential variable leaves your claude.ai login alone: a
Pro or Max subscription keeps working, with your usage limits and billing unchanged. You can
evaluate context-guru on your own sessions with no API key at all.

One caveat: on subscription billing the saving lands in **usage limits**, not dollars — the cost
figures on `/context-guru:status` and the dashboard are list-price estimates. Do not let the
installer add `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`; that's what would move you onto metered
billing, and it won't do it on its own.

## What gets installed where

| Thing | Scope | How often |
|---|---|---|
| Plugin and its skills | your user settings | once per machine |
| Proxy binary | `~/.local/bin` | once per machine |
| **Routing — the `env` block** | **this project by default**, `--global` opt-in | **once per repo** |
| The proxy process | started on demand, exits when idle | automatic |

## Which file the routing goes in

| Write target | Reaches | If the proxy is down |
|---|---|---|
| `.claude/settings.local.json` | you, this repo, gitignored | one repo — **the default** |
| `.claude/settings.json` | everyone who clones the repo | one repo, whole team |
| `~/.claude/settings.json` (`--global`) | every project on the machine | every Claude Code session you have |

The default is project-local: a global base URL pointing at `localhost` means a dead proxy breaks
Claude Code everywhere. `env` blocks merge per key across scopes, so this is safe to combine with a
base URL you already set elsewhere.

## What it does to your requests

The default preset is `off` — nothing in the request body is touched. Under the default preset the
only thing the plugin does is keep-alive: the `5-min-ping` cache strategy pings just under the
provider's 5-minute cache TTL so an idle session's prompt cache doesn't expire between turns. That
spends your own quota; `/context-guru:cache-strategy-picker` names it or turns it off (`none`).

A fresh session has no idle gap to keep warm yet, so its first request reads zero savings — that's
expected, not broken.

Other presets (`cache`, `house`, `codesmart`, `housellm`) add components that edit the request body
too — see [docs/reference/presets.md](../reference/presets.md).

## Then

- `/context-guru:status` — is it routed, is it up, and what has it saved.
- Dashboard: `http://127.0.0.1:8787/dashboard/`.
- `/context-guru:uninstall` — removes the one settings key (with a backup) and stops the proxy.
- `/context-guru:statusline` — puts this session's running savings in your terminal status line.
  Two extra segments are off by default; turn either on with (`$FILE` is whatever
  `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" resolve-scope` reports as `file=` — the file your
  status line already lives in, since it follows whatever scope routing itself used, not a fixed
  `~/.claude/settings.json`):
  ```bash
  "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
    --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --cache"
  ```
  for a countdown to the prompt-cache going cold, or
  ```bash
  "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file "$FILE" \
    --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --keepalive"
  ```
  for the net savings from idle keep-alive pings. Both flags can be passed together; drop the flag
  and re-run to turn an extra back off.

## If the install is blocked

Starting the proxy routes this session's API traffic through a locally-installed binary, so Claude
Code asks about it. Installing a plugin by name isn't the same as consenting to intercept your API
traffic, and under auto mode a classifier may deny it outright.

Three ways through:

1. **Approve it when asked.**
2. **Run the two commands yourself** with the `!` prefix — the skill prints them if it is blocked.
3. **Add the permission rule** from [above](#recommended-first-grant-the-plugins-scripts-once) if
   you'd rather not be asked each time.

Expect two separate gates without a rule in place: starting the proxy, and writing the routing key
(the sharper one — it's your credential passing through this binary going forward). On a chained
install this is required, since that's how the platform gateway keeps authenticating.

## Troubleshooting

**Every request fails or hangs, and `/context-guru:uninstall` cannot run.** Run the recovery script,
which needs no working Claude session:

```bash
~/.local/state/context-guru/context-guru-reset
```

It restores every settings file the plugin edited and reports anything it can't fix on its own (a
credential exported in your shell, for instance). Add `--dry-run` to see the plan first. It
restores from a `context-guru-settings-json/` folder beside each settings file — see
[plugin-recovery-files.md](plugin-recovery-files.md) for what's in it.

**"Nothing happened after `/context-guru:install`."** The setting applies to a **new** session; the
one you ran it in already has its environment. Start a new session.

**A prompt hangs with no output at all.** A dead proxy on a routed project. Start it by hand
(`context-guru-proxy --listen 127.0.0.1:8787 --preset cache`) or remove `env.ANTHROPIC_BASE_URL`
from `.claude/settings.local.json` to get working immediately.

**Requests fail with a connection error.** The proxy isn't running and this project is routed.
`/context-guru:status` will say so; the log is `${TMPDIR:-/tmp}/context-guru-proxy-<port>.log`.

**"The proxy binary is not on PATH."** The installer puts it in `~/.local/bin`. Add that to your
`PATH`.

**Port 8787 is taken.** Change it in `/plugin configure`. Avoid 4000 — litellm defaults to it.

**The status line is blank in a routed project.** Blank is normal before the first response of a
session. Check `/context-guru:status` for the same numbers to confirm it's not simply early.

**`--idle-exit` is refused at startup.** A threshold below roughly 5h34m (2× the store's default
entry lifetime) is rejected, because exiting clears in-memory cache state. Raise the threshold, or
raise `store.ttl_seconds` if the short lifetime is deliberate.

## Upgrading

**The plugin** (skills, hooks, scripts) updates the way any Claude Code marketplace plugin does —
Claude Code prompts you to `/reload-plugins`, or check on demand:

```
/plugin marketplace update rossoctl/context-guru
/reload-plugins
```

**The proxy binary is released separately, and updating the plugin does not update it.** A routed
session checks for a newer release at most once every 5 minutes and, the first time it finds one,
tells you once — it does not keep asking on every later session. You get three choices:

| Answer | What happens |
|---|---|
| Update now | run `/context-guru:update`, or the command the notice prints |
| Always update automatically | the SessionStart hook downloads and installs new releases itself, from then on. It stages the binary in the background and never restarts the proxy mid-session — the new binary takes effect at your *next* session, not the one that downloaded it |
| Do nothing | that release is muted for good, but a *later* release still asks — declining v0.3.0 never hides a v0.4.0 that ships a real fix |

Check or act on it any time with `/context-guru:update`, without waiting for the next notice. Under
the hood this is still `install.sh` with `CONTEXT_GURU_UPGRADE=1` (or `CONTEXT_GURU_VERSION=vX.Y.Z`
to pin a version) — the notice and the skill just save you from remembering that flag exists.
