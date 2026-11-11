# context-guru for IBM Bob

This plugin sends IBM Bob's model traffic through a local context-guru proxy on this Mac. The
proxy cuts token cost. The plugin installs the proxy, checks it and removes it.

macOS only for now. Bob IDE 2.3 and Bob Shell.

## What setup changes

- **A launchd agent** `com.rossoctl.context-guru.bob` runs `context-guru-proxy` on
  `127.0.0.1:PORT` (the first free port from 8891). It always runs and restarts after a crash.
- **One block in your shell startup file.** Bob started from the Dock runs your login shell once
  and copies its environment. The block sets `VITE_GATEWAY_BASE_URL` only in that run, so your
  terminals do not get it. It also sets `BOB_GATEWAY_URL`, so Bob Shell (`bob`) in a terminal uses
  the proxy.

  ```sh
  # >>> context-guru (bob) >>>
  # IBM Bob reads this when it starts. Remove it with the context-guru-uninstall skill.
  if [ "${VSCODE_RESOLVING_ENVIRONMENT:-}" = 1 ] && [ "${__CFBundleIdentifier:-}" = com.ibm.software.bob ]; then
    export VITE_GATEWAY_BASE_URL=http://127.0.0.1:PORT
  fi
  export BOB_GATEWAY_URL=http://127.0.0.1:PORT
  # <<< context-guru (bob) <<<
  ```

- **Two hooks** in `~/.bob/settings/settings.json` (a Bob plugin cannot carry hooks):
  - `SessionStart`: `ensure` starts the agent if the proxy is down. `signal` tells the proxy about
    the task. The proxy records it. It changes nothing else on Bob today: the keep-alive keeper,
    which uses it to retire a compacted session, does not track Bob's OpenAI route.
  - `UserPromptSubmit`: `check` adds one line to the model context when the proxy is down. Else it
    prints nothing.
- **State** in `~/.local/state/context-guru-bob/`: `install.json`, the proxy config
  `bob-PORT.yaml`, with `bob-ultrahigh` also Bob's prices `bob-PORT-prices.yaml`, the dashboard database `dashboard-PORT.db`, `proxy.log`, a copy of your rc file
  before the first change (`backup/`), and the reset script `context-guru-reset`.

By default the proxy runs only lossless components: `format`, `toon` and `cachesplit`. The opt-in
`bob-ultrahigh` preset also cuts content. The model gets cut content back through the expand repair
(test T4 passed on the real Bob IDE, 2026-11-11).

## Install

1. Copy this folder to `~/.bob/plugins/context-guru/`.
2. In Bob, ask: "set up context-guru". The `context-guru-setup` skill runs
   `bob_plugin.py setup --plan`, shows you what it will change, and asks you yes or no.
3. After setup, **quit Bob and open it again from the Dock.**
4. **Log in to Bob once.** Bob keeps its login per backend address. The proxy is a new address, so
   Bob asks for a new login. This is expected.

Without Bob, from a terminal:

```sh
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py setup --plan
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py setup --i-consent-to-traffic-interception
```

### Status bar

Setup also installs a status bar item in Bob: `CG ✓ ctx 21k/270k · cache 1h 54:10 · saved 0`. It
reads only counts and times from the local proxy.

**The status bar needs a build before setup.** Until the install script ships it (plan phase P7):

1. In the context-guru repo, run `make bob-statusbar`. It needs Node and npm. It writes
   `bob-plugin/context-guru/statusbar/context-guru-bob.vsix`.
2. Copy the plugin folder (step 1 above), so the `.vsix` is in `~/.bob/plugins/context-guru/statusbar/`.
3. Run setup.

If your default npm registry rejects your login, run
`make bob-statusbar NPM_REGISTRY=https://registry.npmjs.org/`.

Without the `.vsix`, setup prints `statusbar=skipped` and `statusbar_reason=no_vsix`. Everything
else still installs. Uninstall removes the extension when setup installed it.

Setup checks its own work. It starts your login shell the way Bob does and reads the variable
back. `rc_probe=ok` means Bob will see it. If any step fails, setup undoes the earlier steps.

### Which startup file

Setup picks the file from `$SHELL`:

| Shell | File |
|---|---|
| zsh | `~/.zshrc` |
| bash | `~/.bash_profile`, else `~/.bash_login`, else `~/.profile` (the first that exists) |
| other | none. Setup prints the block for you to add by hand. |

**bash users:** Bob starts bash as a login shell. A bash login shell does not read `~/.bashrc`.
So the block goes in `~/.bash_profile`, not in `~/.bashrc`. If your `~/.bash_profile` sources
`~/.bashrc`, that is fine too.

Setup never overwrites what is not its own. It leaves the file alone and tells you why when:

- you already set `VITE_GATEWAY_BASE_URL` or `BOB_GATEWAY_URL` yourself;
- the block between our markers was edited;
- the file is a chain of more than one symlink, or a dangling symlink. (One symlink, as in a
  dotfiles repo, is followed. The link stays a link.)
- `~/.bob/settings/settings.json` has comments or trailing commas, or is a link we do not follow.

**What "byte for byte" means.**

- **The rc file:** uninstall removes exactly our block, so the rest of the file stays byte for
  byte.
- **Bob's settings file:** to add our hooks, setup reads the JSON and writes it again. Your keys,
  their order, your hooks and the indent stay, but other spacing can change. Uninstall gives back
  your exact bytes only when the file is still what setup wrote. If you or Bob changed the file
  since, or you ran setup again after a change, uninstall removes only our hooks and writes the
  JSON again.
- **Numbers in the settings file** can change form when the file is written again: for example
  `1.0` becomes `1` (in the reset script) and `1e3` becomes `1000.0`. The value stays the same.
- **Duplicate keys:** setup and the reset script refuse a settings file with the same key twice,
  rather than drop one of the values.
- **A symlinked file:** one symlink (as in a dotfiles repo) is followed for both files. The link
  stays a link. A longer chain or a dangling link is refused.

## Status

Ask Bob "context-guru status", or run:

```sh
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py status
```

It reports `routed`, `proxy_up`, `hooks_installed`, and `last_bob_request_age_s`: how long ago
the newest Bob request reached the proxy. A block in the rc file alone does not prove Bob uses
it. Bob updates itself and restarts. A Bob started from a terminal or by its updater may not have
the variable. `result=no_bob_traffic` tells you this. The fix is to quit Bob and open it from the
Dock.

The dashboard is at `http://127.0.0.1:PORT/dashboard/`. Token figures are correct. Dollar figures
for Bob are not: Bob bills in Bobcoins, and the proxy does not price them yet. With `bob-ultrahigh`
the proxy prices them with estimated dollar rates (see below).

## Presets

```sh
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py configure --show
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py configure --preset off       # or lossless, bob-ultrahigh
```

Or ask Bob "which context-guru preset runs". Bob has three presets:

| Preset | Does |
|---|---|
| `lossless` (default) | Reformats requests (`format`, `toon`, `cachesplit`). Nothing is cut. |
| `bob-ultrahigh` (opt-in) | context-guru's `ultrahigh`, set up for Bob (see below). It cuts content, and the model can get it back. |
| `off` | Sends requests through as they are. |

### `bob-ultrahigh`

It is `ultrahigh` with 4 Bob changes:

1. **Summaries use Bob's own model.** No model is named. A summary call goes only to Bob's server,
   under your Bob login, and Bob bills it like any request. **These calls do not work on Bob yet:**
   they fail, and the request goes to Bob unchanged. A later release fixes this.
2. **`extract_llm` runs on Bob,** though Bob caches. Bob bills a cache rewrite at the input rate,
   so a rewrite costs little.
3. **The sweep may apply on a warm cache** (`warm: true`), behind its cost gate.
4. **Tool deferral keeps Bob's core tools** (see below).

It also gives the proxy Bob's prices (`prices.yaml`, as `MODEL_PRICES`): every input token at one
flat rate, a cache read and a cache write too. The sweep's cost gate needs that. Without it, the
gate prices a cache rewrite at Claude rates and cuts nothing on a warm cache. The dollar rates are
estimates; only the flat ratio is measured. So with this preset the dashboard's Bob dollar figures
change. Leaving the preset removes the prices.

The model sees 3 more tools: `context_guru_tools` (the tool search), `context_guru_expand` (gets cut
content back) and `context_guru_adjudicate` (the sweep's question).

#### Tool deferral

Bob's tool definitions are about 55% of every model call. `bob-ultrahigh` holds back the tools you
do not use:

- **What it holds back:** a tool that your last 3 Bob sessions declared and did not use, and that
  weighs at least 200 tokens. With fewer than 3 past sessions, it holds back nothing.
- **What it never holds back:** Bob's core tools: `read_file`, `write_file`, `apply_diff`,
  `search_and_replace`, `insert_content`, `execute_command`, `grep`, `glob`, `list_files`,
  `ask_followup_question`, `use_skill`, `switch_mode`, `update_todo_list`, `spawn_subagent`,
  `start_subtask`.
- **How a tool comes back:** the proxy adds a search tool, `context_guru_tools`, and a list of the
  held-back names. The model calls the search, and the proxy answers it and adds the tool back for
  the rest of the task. Bob never sees the search. One held-back tool costs one extra turn.
- **A direct call is not detected:** if the model calls a held-back tool directly instead of
  searching, Bob answers "Tool does not exist" and that call fails. On Bob the proxy does not
  detect this, so deferral stays on, and each repeated direct call wastes a turn.
- **The set stays fixed in a task,** so the prompt cache stays valid. A change reaches new tasks only.
- **The off switch:** if Bob's gateway refuses a request with held-back tools, the proxy sends the
  original request and stops deferral for that model. `doctor` warns (`tool_defer=warn`).
- **What it keeps:** tool names, use flags and counts only. No tool text, no prompts.

`status` shows the counts from the proxy's `/stats` when deferral ran: `tool_defer_requests`,
`tool_defer_deferred_tokens`, `tool_defer_add_backs`, `tool_defer_wrong_guesses`,
`tool_defer_turned_off`, `tool_defer_break`.

The other context-guru presets (`conservative` to `ultrahigh`) are not tested on Bob, so the plugin
refuses them. A change restarts the proxy. If the new proxy does not start, the plugin puts
back the old config and the old service, and says if that failed (`rolled_back=false`). A config
you edit by hand is kept: setup and `--cache-strategy` leave it, `configure --show` reports
`preset=custom`, and only `--preset NAME` replaces it. Setup, configure, update and uninstall take
a lock in the state folder; a second run at the same time stops with `result=busy`.

## Cache strategy

Only `none`. A keep-alive ping keeps a provider's prompt cache warm while you are idle. The proxy
pings only Anthropic and OpenAI Responses caches, and Bob's model call is OpenAI chat/completions,
so a ping strategy would do nothing for Bob. Also, Bob has no SessionEnd event, so a ping could not
stop when a Bob task ends.

## Update

```sh
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py update --check   # reads only
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py update
```

Or ask Bob "update context-guru". The update downloads the latest release, checks its SHA-256
against the release checksums, checks that it has the Bob gateway, then restarts the proxy. It keeps
the old binary as `bin/context-guru-proxy.previous`. If the new proxy does not answer, it goes back
to the old one and reports `rolled_back=true`. It updates only the plugin's own copy of the binary;
a binary from brew or your PATH you update yourself. `--check` makes one network call to GitHub.
Versions are compared by number. An installed version newer than the latest release is not
downgraded unless you add `--allow-downgrade`.

## Doctor

```sh
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py doctor
```

Or ask Bob "check context-guru". It reads context-guru's files and asks the proxy and launchd for
status. Each check prints `ok`, `warn` or `fail`, and a `fix=` line for each that is not ok:

- `proxy`, `service`: the proxy answers, our launchd agent is loaded on the recorded port.
- `rc_block`, `rc_probe`: the block is exactly ours. `rc_probe` runs only with `doctor --probe`: it
  starts your login shell the way Bob does and checks that it sees the address. That runs your
  shell startup files, so it is opt-in. Setup always runs it.
- `service`: also checks that the agent's `--config` is the managed config file.
- `preset`: `warn` when the config was edited by hand. It may hold a lossy component.
- `tool_defer`: only with `bob-ultrahigh`, when the proxy reports it. `warn` when the proxy turned
  tool deferral off at least once since it started (`/stats` `tool_defer.turned_off` or
  `client_search_disabled.break` above 0). On Bob, only a refused request turns it off.
- `hooks`, `binary`: our hooks are there; the binary has `--bob-upstream`.
- `bob_traffic`: a Bob request reached the proxy in the last 24 hours.
- `bob_bundle`: Bob's own code still has the names context-guru depends on (`VITE_GATEWAY_BASE_URL`,
  `region_domain`, the model path, `X-Task-Id`, the hook events, `Tool does not exist`). It also
  prints `bob_version`. A missing routing name is `fail`; a missing feature name is `warn`; Bob not
  installed is `warn`. Bob installed without its extension at the usual path is `fail` ("Bob's
  layout changed").

The exit code is 1 when a check fails, else 0.

## Uninstall

Ask Bob "uninstall context-guru", or run:

```sh
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py uninstall          # keeps the data
python3 ~/.bob/plugins/context-guru/scripts/bob_plugin.py uninstall --purge  # deletes the state folder
```

It removes the hooks, the rc block and the launchd agent. Then **quit and reopen Bob, and log in
once.** Bob goes back to its old address, and that needs one more login.

## When things are broken

If Bob cannot reach its backend, run this in a terminal. It needs no Python:

```sh
~/.local/state/context-guru-bob/context-guru-reset --yes
```

It removes our rc block, our hooks and our launchd agent, and keeps the data. It also works when
setup was stopped half way and wrote no record: it looks for our block in the startup files for
your `$SHELL`, and for our tagged hooks in Bob's settings file. It removes only what is exactly
ours. Then quit and reopen Bob, and log in once. Use `--dry-run` to see what it finds first.

Setup accepts only an `https` upstream on a `.bob.ibm.com` host (`--upstream`). It refuses a home
or state folder whose path has `"` or `\`, because the reset script reads paths with `sed`.

| Symptom | Cause | Fix |
|---|---|---|
| Bob says it cannot connect | The proxy is down | `launchctl kickstart gui/$(id -u)/com.rossoctl.context-guru.bob`, or the reset script |
| The model says "context-guru: the local proxy ... is not answering" | The `check` hook saw the proxy down | The same |
| `status` says `no_bob_traffic` | Bob started without the variable | Quit Bob, open it from the Dock |
| `rc_probe=miss` after setup | Your login shell does not read the file setup wrote | Add the block to the file your login shell reads |
| Bob asks you to log in after setup or uninstall | Bob keeps its login per backend address | Log in once. This is expected. |
| Bob does not use the proxy after a Bob update | Bob may have renamed its variable | Run `status`, and tell the context-guru team |

The proxy log is `~/.local/state/context-guru-bob/proxy.log`. In Bob's own extension log,
`non-SaaS deployment: http://127.0.0.1:PORT` means Bob took the address.

## Known gaps

- **Bob has no SessionEnd event.** Its `Stop` event fires after each task loop, not at the end of
  a task. So the proxy cannot learn that a Bob task ended. The cache keep-alive (off in this
  version) would keep a closed task warm until its own limits stop it.
- **The plugin sends only `SessionStart` to the proxy.** The proxy's `/signal` route accepts
  `SessionStart` and `SessionEnd`. Bob's other events carry model text (`Stop`, `PostCompact`) or
  your prompt (`UserPromptSubmit`). The plugin never sends, writes or prints them.

## Privacy

The hooks read only the session id, the event name and the `SessionStart` source. The
`UserPromptSubmit` hook never reads its input, so your prompt goes nowhere. Nothing logs a token.
The hooks always exit 0 within 5 seconds, so they never block Bob.

Bob asks you to approve each command a skill runs. Approve the read-only `status` and
`--plan`/`--dry-run` commands as you like. Keep setup and uninstall behind a per-run approval.
