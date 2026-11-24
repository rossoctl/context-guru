# Design: one-command installer for context-guru on Pi

Date: 2026-11-23. Status: approved in principle by the owner (release asset in the repo; key in the OS secret store).

## Summary

One command installs or updates Pi, installs the context-guru Pi package, connects Pi to the Anthropic API or a
company gateway, and turns context-guru on:

```sh
curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-pi.sh | bash
curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-pi.sh | bash -s uninstall
```

The user answers a few questions: install or update Pi, the gateway URL (or none), the key, and one consent yes.

## Why

Today a user needs 5 manual steps: install Pi with npm, clone the git repo (the package is published nowhere
else), `pi install <path>`, edit `~/.pi/agent/models.json` by hand for a gateway (URL, auth, model id, key),
and `/context-guru setup` in Pi. The clone and the hand-edited JSON with a secret in it are the hard parts.

## Scope

In scope:
- macOS and Linux. Bash bootstrap plus a Python installer (Python 3 is already a requirement of the package).
- Pi: install when missing, update when older than the latest, each after a yes.
- The Pi package from the release, checked by SHA-256, at one stable path.
- The Anthropic connection: the public API, or a gateway with its URL, auth type and model id.
- The key in the OS secret store; `models.json` holds only a command that reads it.
- context-guru setup (chain behind a gateway), a status check, an optional test prompt.
- `uninstall`: everything the installer added, and only that.

Out of scope:
- OpenAI keys (setup routes OpenAI too; the user's existing OpenAI login keeps working through the chain).
- Windows. Pi's own login flows (`/login`). Installing Node.js itself (the installer says what to install).

## Behavior change and data dependence

- **Behavior change:** none for existing users. The release gets 3 new files. Nothing in the proxy, the package
  or `pi_plugin.py` changes behavior.
- **Data dependence:** none. The installer reads no customer data. It handles one secret, the key (see Security).

## Files

| File | What |
|---|---|
| `pi-package/release/install-pi.sh` | Bootstrap, about 70 lines, the same pattern as `install-bob.sh`: download the tarball and its SHA-256 list, check, unpack to a temp folder, run the Python installer with the answers from `/dev/tty`. |
| `pi-package/release/package.sh` | Builds `context-guru-pi-<version>.tar.gz` from `pi-package/` (no tests, no dev files) and `context-guru-pi-SHA256SUMS`. |
| `pi-package/scripts/install_pi.py` | The installer: every decision and every change. |
| `pi-package/scripts/test_install_pi.py` | Unit tests: npm, pi, the secret store, the network and `pi_plugin.py` are faked at their boundary. |
| `pi-package/release/test_release.py` | The bootstrap piped into bash, with a local HTTP server as the release (as Bob's test does). |
| `.github/workflows/release.yaml` | Build the tarball before GoReleaser publishes; upload the tarball, the sums and `install-pi.sh`. |
| `pi-package/README.md`, `docs/how-to/install-pi-package.md` | The one command first; the manual steps stay below it. |
| `docs/data-dependence/features/pi-installer.md` | Its data-dependence row (no customer data). |

## Flow of `install`

Each step either finishes or stops with a message and the reason. Pi changes only after a yes (step 2). The
package is installed in step 3. Pi's connection (`models.json`, `settings.json`, the stored key) changes only in
step 6, after the key check.

1. **Checks.** `python3` and `node` exist; Node is 22.19 or later (Pi 1.5.2 and 1.6.0 need it). A terminal
   exists for the questions. Otherwise stop and say what to install.
2. **Pi.** `command -v pi`.
   - Missing: ask "Install Pi <latest>?". Yes: `npm install -g @earendil-works/pi-coding-agent@<latest>`.
   - Older than `<latest>`: ask "Update Pi <old> to <latest>?". No keeps the old version.
   - `<latest>` comes from `npm view ... version --registry <registry>`. `<registry>` is
     `https://registry.npmjs.org/` by default: company npm settings can point to a registry without Pi (seen:
     E401). `CG_PI_NPM_REGISTRY` overrides it.
   - When `npm view` fails and Pi is installed, keep that Pi, show npm's message, and go on. Nothing in that
     case needs npm.
   - When npm fails on permissions (a global prefix the user cannot write), stop and show npm's message. Never
     run `sudo`.
3. **Package.** The bootstrap already checked the SHA-256. Copy the package to
   `${XDG_DATA_HOME:-~/.local/share}/context-guru-pi/package` (replace an older copy there). One stable path:
   an update never adds a second entry.
   - Remove every other entry in Pi's `packages` that is a context-guru Pi package (its `package.json` name is
     `@rossoctl/context-guru-pi`), with `pi remove`. Two copies must not load (#1207).
   - `pi install <stable path>`.
4. **Connection.** Read `~/.pi/agent/models.json` (or `$PI_CODING_AGENT_DIR/models.json`).
   - It already has an Anthropic route (a `baseUrl`, or an `apiKey`): show its host (the gateway behind the
     proxy when context-guru routes Pi; no key) and ask "Keep this connection?" (default yes). Yes skips to
     step 7. No: the new connection replaces `apiKey`, `baseUrl`, `authHeader`, `headers` and `models`, so the
     new key never goes to the old endpoint.
   - Else ask: "Gateway URL (empty for the Anthropic API)". For a gateway also: "Does it want the key as
     Authorization: Bearer? [Y/n]" and "Model id at the gateway".
   - "Key:" read without echo (`getpass` on the terminal).
5. **Key check.** One request to `<base>/v1/messages` with `max_tokens: 1`, with the chosen auth header, the key
   from memory. 200 goes on. 401/403: "the key is not accepted", ask again (3 tries). Other codes and network
   errors: show the code and the host, ask again or stop. The key never appears in a message.
6. **Store and write.** Only now does anything change.
   - The key goes into the secret store (see Security). `models.json` gets
     `"apiKey": "!<command that prints the key>"`. Pi runs it at request time.
   - `models.json` gets `baseUrl` and `authHeader` for a gateway, and a `models` entry for the gateway's model
     id. `settings.json` gets `defaultProvider`/`defaultModel` only when it has none.
   - A backup of both files goes to `<state>/installer-backup/` first. Writes are atomic, mode 0600.
   - A record, `<state>/installer.json`, lists each key the installer changed with its old and its new value
     (the store item and the package path too), so `uninstall` puts back exactly what was there.
7. **Setup.** `pi_plugin.py setup --plan`, then show the consent text (the chain one for a gateway) and ask
   "Turn context-guru on? [y/N]". Yes: `setup --i-consent-to-traffic-interception [--on-conflict chain]
   --cache-strategy <the plan's strategy>`, and `configure --preset <the plan's preset>` when setup started
   another one. A plan that is neither `planned` nor `needs_decision` stops with its reason.
   - `health_check_failed` (#1288, a cold first start): run `pi_plugin.py uninstall` (it stops the half
     service), wait 10 s, run setup once more. Still failing: stop, say Pi itself works, name the log.
   - No: stop. Pi is installed and connected; context-guru is off. Say how to turn it on later.
8. **Finish.** Show `pi_plugin.py status` (route, port, preset, strategy). Ask "Send one test prompt?" (it costs
   one short request). Yes: `pi -p "Reply with the single word: ok"` and show the answer.

## Flow of `uninstall`

1. When context-guru routes Pi and Pi windows are open (`pgrep -u <uid>`), name them and ask "Go on
   anyway? [y/N]": a window keeps its settings in memory (#1341). No stops. Then `pi_plugin.py uninstall`
   (restores the route in `models.json`, stops and removes the service, deletes the downloaded proxy). The
   extension in a window that stays open switches to the real endpoint on its next prompt.
2. `pi remove <stable path>`, and delete the package folder.
3. With the record: delete the secret-store item; in `models.json` and `settings.json`, put back the old value
   of each key the installer changed (or remove a key that was absent), only while it still has the value the
   installer wrote (a value the user changed stays). A `models.json` the installer created goes when empty.
   If `pi_plugin.py uninstall` could not put the route back, stop here and name the reset command.
4. Ask "Uninstall Pi too?" (default no) only when the record says the installer installed Pi.
5. Ask "Delete context-guru's local data too?" (default yes): yes deletes `<state>` (log, dashboard
   database); no deletes only the installer's record and backups.

## Security

- **The key stays in memory and in the secret store.** It is never on a command line (other users can read
  argv), never in a log, a message, an exception text or the record.
- **macOS:** the Keychain, through `security -i` with the command on stdin, so the key is not in argv.
  Item: service `context-guru-pi-anthropic`, account `$USER`. Read command:
  `security find-generic-password -s context-guru-pi-anthropic -a <user> -w`.
- **Linux:** `secret-tool store` (reads the secret from stdin) when it exists; else a file
  `${XDG_CONFIG_HOME:-~/.config}/context-guru-pi/anthropic-key`, mode 0600, in a 0700 folder, read with `cat`.
  The installer says which one it used.
- **A test drives a canary key** through install and uninstall and asserts it is absent from stdout, stderr, the
  record, the backups and every argv the fakes saw. Only the secret store fake and the request header hold it.
- **Integrity:** the bootstrap checks the tarball against the release's SHA-256 list (`shasum -a 256` on macOS,
  `sha256sum` on Linux) and takes the file name only from that list. A mismatch stops before anything runs.
- **Fail closed:** no terminal, no consent, a failed key check, or a failed download changes nothing.
- **Consent:** the yes comes from the person at the terminal. The installer never passes
  `--i-consent-to-traffic-interception` without it, and has no `--yes` for setup.

## Failure modes

| Failure | What the user sees | State after |
|---|---|---|
| No Node, or too old | "Install Node.js 22.19 or later from nodejs.org, then run this again." | Nothing changed |
| npm cannot reach the registry, no Pi | npm's message, and `CG_PI_NPM_REGISTRY` | Nothing changed |
| npm cannot reach the registry, Pi installed | npm's message; "Pi <version> stays" | The install goes on |
| npm needs root | npm's message, and "fix the npm prefix or install Pi yourself" | Nothing changed |
| Key rejected 3 times | "the key is not accepted at <host>" | Pi installed (if asked), package installed, no connection change |
| Secret store refuses | its error, without the key | No connection change |
| Setup fails twice | "context-guru is off; Pi works directly", the log path | Connection written; context-guru off |
| Download or SHA-256 mismatch | "Nothing was installed" | Nothing changed |

## Tests

- `test_install_pi.py` (unit, fakes at the boundary: a fake `npm`, `pi`, `security`/`secret-tool`, an HTTP
  server for the key check, a fake `pi_plugin.py` recorder): every flow step, both answers of each question,
  each failure row, the canary key, uninstall removes only what install wrote, a second install is idempotent.
- `test_release.py`: the bootstrap piped into bash against a local release folder; SHA mismatch; unsafe names.
- Real runs: a clean macOS user with no Pi; with Pi 1.5.2 (update to the latest, now 1.6.0); with an existing
  gateway connection (keep it); uninstall restores `models.json` exactly. Linux: in a container, with and
  without `secret-tool`.

## Plan

1. This design. Then the Python installer, test first, step by step as in the flow.
2. The bootstrap, the packager, and their test.
3. The release workflow change.
4. Docs: the install page and the README, with the one command first.
5. Real runs on this Mac (macOS) and in a Linux container; Pi 1.6.0 is not tested with the package yet, so the
   run on 1.6.0 also checks the package.
6. One PR, `offload-review`, the owner's approval.

## Open points

- Pi 1.6.0 is untested with the package. If the run finds a problem, the installer offers the tested version
  (1.5.2) as the default until it is fixed.
- #1288 (setup's 5 s wait) affects the first install. The installer works around it (step 7). The real fix is a
  separate PR, which changes a time limit and needs the owner's yes.
