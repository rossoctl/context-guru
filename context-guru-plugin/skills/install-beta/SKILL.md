---
name: install-beta
description: Install a beta (pre-release) of the context-guru proxy, and restart the proxy onto it if you want. For beta testers.
disable-model-invocation: true
---

# Install a context-guru beta

<!-- Deliberately NO `allowed-tools:` and NO `!` block, as in install/SKILL.md and update/SKILL.md.
     This skill replaces the binary that carries every model request. It must not run while the
     skill renders, and it must not grant itself the permission the classifier asks about. -->

This skill installs a **beta** of the proxy binary. A beta is a pre-release: it is for testing,
and it can have bugs. `/context-guru:update` and `/context-guru:install` never pick a beta.

`install-beta.sh` does the work. It reads no session text and no customer data. Run it as
written. Do not rebuild its steps by hand.

## 1. Say what will happen, in four lines

- It downloads the beta binary (about 30 MB) and checks it against the release's checksums.
  A bad checksum stops the install.
- It puts the binary in `~/.local/bin`. It does **not** restart a running proxy. The old binary
  keeps running until a restart.
- It sets the **update answer to `never`** if it was `always`. Reason: bug #1254. Auto-update
  would install the older stable release over the beta. The script says when it does this.
- It does not edit the Claude Code plugin. If the plugin is older than the beta, it says so.

Ask for a tag only if the user gave none. With no tag, the script takes the newest pre-release.

## 2. Install

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/install-beta.sh" [<tag>]
```

Read `result=` first.

- `result=installed`: report `tag=`, `installed_version=` and `install_checksum=verified`.
- `result=error`: report `reason=` as it is and stop. Do not retry with other flags.
  - `github_rate_limited` says nothing about whether a beta exists. Tell the user to wait,
    or to pass the tag.
  - `install_failed` with `checksum_*` means the download is **not** installed.
  - `version_mismatch` means the file on disk is not the beta. Say so; do not call it done.

Report these lines when they appear:

- `update_answer_was=` / `update_answer_now=never`: the script changed a setting. Say the old
  value, the new value, and the way back:
  `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer always`.
  Name #1254. Say the user may turn it on again once #1254 is fixed and released.
- `plugin_older=true`: the plugin is older than the plugin in the beta. Give
  `plugin_update_command=` (run it in Claude Code), then restart Claude Code.
- `path_shadow=`: another `context-guru-proxy` comes first on PATH. Name it.

## 3. Ask before a restart

A running proxy still uses its old binary. **A restart cuts every session routed through that
port**, in every project and window that uses it. Say this. Then ask once: restart now, or
later?

- **Later**: say the beta takes effect at the next proxy start. Stop here.
- **Now**: find the port, then run the restart with the **beta tag's** scripts. The script does
  this itself when you pass `--restart`:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/install-beta.sh" <tag> --restart [--port <n>]
```

Pass `--port` only when the user names one. Without it the script uses this project's port. If
it says `reason=port_unknown`, ask which port. Never guess one.

The restart:

- stops the proxy by its pidfile, and only after it checks the pid is a `context-guru-proxy`
  on that port, owned by the user. It never uses `pkill` or `killall`;
- starts the beta with `--preset <the current preset>`. A restart without a preset would reset
  it to `off` (#1244);
- waits for `/healthz`.

Report `restart=done` and `healthz=ok`, and show the `start_proxy:` lines (they hold the startup
note). On `restart=failed`, `pid_not_ours`, `pid_not_yours` or `stop_timeout`, report the
`reason=`. Nothing was forced.

## 4. Later steps use the beta's scripts

The installed plugin can be older than the beta. Its `settings.py strategy list` reads a fixed
table and does not know new strategies such as `1-hour-on-wait`. For `strategy`, `preset` and
restart steps right after this install, use `beta_scripts=` from the output:

```bash
"<beta_scripts>/settings.py" strategy list
```

That folder is a temporary copy. It goes away when the machine cleans its temp folder. After a
plugin update (`plugin_update_command=`), `${CLAUDE_PLUGIN_ROOT}/scripts` has the same code.

## Go back to the stable release

```bash
CONTEXT_GURU_VERSION=<previous tag> "${CLAUDE_PLUGIN_ROOT}/scripts/install.sh"
```

Then restart the proxy (`/context-guru:preset-picker` restarts it). If the user wants
auto-update again, run `settings.py update-check answer --answer always`.
