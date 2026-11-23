# Design: one-command installer for context-guru on OpenCode

Date: 2026-11-23. Status: draft. Decisions from the owner: install OpenCode after a yes; no key handling;
build on the Pi installer branch (#1294) and use its pattern.

## Summary

One command installs OpenCode when it is missing, installs the context-guru OpenCode plugin, and turns it on
for every project:

```sh
curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-opencode.sh | bash
curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-opencode.sh | bash -s uninstall
```

The user answers at most 3 questions: install OpenCode, one consent yes, and an optional test prompt.

## Why

Today a user clones the git repo (the plugin is published nowhere else), and edits `opencode.json` and
`tui.json` by hand with an absolute path. An update needs a `git pull`. A second clone adds a second entry, and
two copies of the plugin load.

## Scope

In scope:
- macOS and Linux. A bash bootstrap and a Python installer, as for Pi.
- OpenCode: install it when it is missing, or update it when it is older than the plugin supports. Each change
  needs a yes.
- The plugin from the release, checked by SHA-256, at one stable path.
- The global `opencode.json` (`index.js`) and `tui.json` (`tui.js`) entries.
- An optional test prompt that checks that the proxy started.
- `uninstall`: everything the installer added, and only that.

Out of scope:
- Keys and gateways. The plugin uses OpenCode's own login (`opencode auth login`) and adds no key. The
  installer never reads, asks for, or stores a key.
- Per-project install. Windows. The plugin's preset and cache strategy questions (the defaults stay: preset
  `off`, the proxy's default strategy; the installer says how to change them).

## Behavior change and data dependence

- **Behavior change:** none for existing users. The release gets 3 new files. Nothing in the plugin or the proxy
  changes.
- **Data dependence:** none. The installer reads version numbers and the keys of OpenCode's 2 config files.

## OpenCode versions

- The plugin needs OpenCode **1.18.29** or later (the v1 `server()` export). It is tested on **1.19.5** only.
- npm's latest is 1.19.6 today. The installer installs the **tested** version, 1.19.5, not the latest.
  `CG_OC_OPENCODE_VERSION` overrides it.
- Missing: ask "Install OpenCode 1.19.5?". Older than 1.18.29: ask "Update OpenCode <old> to 1.19.5?"; no stops
  (the plugin cannot load). 1.18.29 or later: keep it. When it is not 1.19.5, say that it is not tested.
- After npm, run `opencode --version` again. When `opencode` is not on `PATH`, or an older one comes first
  (Homebrew, `~/.opencode/bin`), stop before the consent step and name it.
- Install with `npm install -g opencode-ai@<version> --registry <registry>`. `<registry>` is
  `https://registry.npmjs.org/` by default; `CG_OC_NPM_REGISTRY` overrides it. Never `sudo`.
- No npm: stop, and name OpenCode's own install page.

## Files

| File | What |
|---|---|
| `opencode-plugin/release/install-opencode.sh` | Bootstrap: the same as `install-pi.sh` with other names. |
| `opencode-plugin/release/package.sh` | Builds `context-guru-opencode-<tag>.tar.gz` (top folder `opencode-plugin/`, no tests, no release files) and `context-guru-opencode-SHA256SUMS`. |
| `opencode-plugin/scripts/install_opencode.py` | The installer: every decision and every change. |
| `opencode-plugin/scripts/test_install_opencode.py` | Unit tests. npm, opencode and the proxy's health URL are faked at their boundary. |
| `opencode-plugin/release/test_release.py` | The bootstrap piped into bash, with a local HTTP server as the release. |
| `.github/workflows/release.yaml` | Build the tarball before GoReleaser; upload the tarball, the sums and `install-opencode.sh`. |
| `.github/workflows/opencode-plugin.yaml` | Also runs the 2 Python test files. |
| `opencode-plugin/README.md`, `docs/how-to/install-opencode-plugin.md` | The one command first; the manual steps stay below it. |
| `docs/data-dependence/features/opencode-installer.md` | Its data-dependence row. |

## Paths

- Config folder: `$XDG_CONFIG_HOME/opencode`, else `~/.config/opencode`. Files: `opencode.json` and `tui.json`.
- Plugin copy: `${XDG_DATA_HOME:-~/.local/share}/context-guru-opencode/plugin/`.
- Installer record: `<state root>/installer.json`. The state root is the plugin's own:
  `$CONTEXT_GURU_STATE`, else `${XDG_STATE_HOME:-~/.local/state}/context-guru-opencode`.

## Flow of `install`

1. **Checks.** A terminal exists. OpenCode's config files, when they exist, are JSON objects. A file with
   comments (JSONC) or an `opencode.jsonc` stops the install: the installer prints the 2 entries to add by
   hand. Nothing changed.
2. **OpenCode.** See [OpenCode versions](#opencode-versions).
3. **Consent.** Say what turns on: every project's Anthropic and OpenAI requests go through a local proxy; the
   proxy sends keep-alive requests with the user's own credential while the user is away (the default cache
   strategy); preset `off` sends bodies unchanged. Ask "Turn context-guru on for OpenCode? [y/N]". No: stop.
   Nothing changed (OpenCode stays, if step 2 installed it).
4. **Plugin.** Copy the plugin to the stable path (replace an older copy there).
5. **Config.** A backup of both files goes to `<state root>/installer-backup/` first. Writes are atomic.
   - In `opencode.json` `plugin` and `tui.json` `plugin`: remove every other entry that is a context-guru
     plugin, and add the stable path. An entry is ours when it is a local path to a file whose text has the
     marker `opencode-context-guru`. Two copies must not load.
   - An old entry in tuple form (`[path, {options}]`) keeps its options on the new path.
   - The record lists each file, whether the installer created it, and whether `plugin` was absent. It does
     not list the entries it replaced: their only copy is the backup of the first install.
   - Each file gets `$schema` only when the installer creates it.
6. **Finish.** Say "Restart OpenCode". Ask "Send one test prompt? It costs one short request [y/N]". Yes: run
   `opencode run "Reply with the single word: ok"` in a temp folder, show the answer. Then find the
   temp folder's project state folder, and run that project's `context-guru-status` script (it checks `/healthz`). `proxy=up` and an answer: "✓". `opencode run` can print a provider error and exit 0, so an `Error:` in its output counts as no answer. No
   provider login: name `opencode auth login`. Then run that project's `context-guru-reset` and delete its state
   folder: the check's proxy would idle for 24 h for a folder that is gone.

## Flow of `uninstall`

1. **Stop the proxies.** Run each `<state root>/<project>/context-guru-reset` that exists. It stops the
   recorded proxy and releases its shared binary. This comes first, so a config file it cannot change does not
   leave a proxy running.
2. **Config.** Remove every entry of this plugin from both files, also one the user added by hand, and the
   recorded path when the copy is gone. Put back nothing else. A file with comments, or an `opencode.jsonc`,
   is not changed: the installer names the entry to remove by hand, does the other steps, and exits 1. A `plugin` list the installer created
   goes when it is empty. A file the installer created goes when only `$schema` is left.
3. **Plugin copy.** Delete the stable path.
4. **Local data.** Ask "Delete context-guru's local data too? [Y/n]". Yes deletes the state root (logs,
   dashboard databases, binaries). No deletes only the record and the backups.
5. **OpenCode.** Ask "Uninstall OpenCode too? [y/N]" only when the record says the installer installed it.

## Failure modes

| Failure | What the user sees | State after |
|---|---|---|
| No terminal | "Run it in a terminal window." | Nothing changed |
| Download or SHA-256 mismatch | "Nothing was installed." | Nothing changed |
| Config is JSONC or not an object | The 2 entries to add by hand | Nothing changed |
| No npm, OpenCode missing | OpenCode's install page | Nothing changed |
| npm fails (permissions, registry) | npm's message, `CG_OC_NPM_REGISTRY` | Nothing changed |
| No consent | "context-guru stays off." | OpenCode only, if installed in step 2 |
| Test prompt fails | Its output, and the log path | Installed; the plugin fails open |

## Tests

- `test_install_opencode.py`: each flow step, both answers to each question, each failure row, tuple options
  carried over, a second install is idempotent, uninstall removes only what install added, and user entries
  stay.
- `test_release.py`: the bootstrap piped into bash against a local release folder; SHA mismatch; unsafe names.
- A real run on this Mac with a temp `HOME`: install, the test prompt, uninstall.

## Open points

- The tested version (1.19.5) is a pin in the installer. A test of a new OpenCode version must update it.
