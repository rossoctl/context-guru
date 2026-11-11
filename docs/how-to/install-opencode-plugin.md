# Install the OpenCode plugin

> **Experimental.** The plugin is new. It ran in a real OpenCode only once (1.19.5). It needs proxy v0.7.1 or later.

The OpenCode plugin routes Anthropic and OpenAI model requests through a local context-guru proxy.
It is project-safe, reversible, and does not add an API key.

## Install

Add the local plugin file to the global OpenCode config for every project:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["/absolute/path/to/context-guru/opencode-plugin/index.js"]
}
```

OpenCode stores this file at `~/.config/opencode/opencode.json`. For one project only, put the same
entry in that project's `opencode.json`.

Restart OpenCode after changing the config. On the first startup, the plugin:

1. Reuses `context-guru-proxy` from `PATH`, or downloads release **v0.7.2** and verifies its SHA-256 checksum. Set `CONTEXT_GURU_VERSION` to pick another release.
2. Starts one loopback proxy for the project, using a free port beginning at `8787`.
3. Routes Anthropic requests to `/anthropic/v1` and OpenAI requests to `/openai/v1`.
4. Preserves any configured provider gateway host and path prefix as the proxy's upstream, avoiding a duplicate trailing `/v1`.

The proxy state, log, dashboard database, and recovery command live under:

```text
~/.local/state/context-guru-opencode/<project-id>/
```

The exact project directory is printed to the OpenCode log. The dashboard is at
`http://127.0.0.1:<port>/dashboard/`.

## Configure

The default preset is `off`, so request bodies are forwarded unchanged. The default cache strategy
is `auto`, the same as the Claude Code plugin. It sends keep-alive requests with your own provider
credential while you are away. Set other values with the tuple form:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["/absolute/path/to/context-guru/opencode-plugin/index.js", {"preset": "conservative", "cacheStrategy": "none"}]
  ]
}
```

- Presets: `off`, `conservative`, `medium`, `high`, `xhigh`, `ultrahigh`.
- Cache strategies: the proxy owns the list. The plugin reads it with `context-guru-proxy strategies`. It keeps no copy.
- The plugin offers only the strategies that fit a provider it routes. An OpenAI-only setup does not see `auto`.
- The default is the proxy default, `auto`. If `auto` does not fit the routed providers, the plugin runs `none` and logs why.
- The old name `5-min-ping` still works. The proxy reads it as `auto`.
- The plugin writes only `cache.strategy: <name>` to `proxy.yaml`. The proxy fills in the keys.
- **The proxy must be v0.7.1 or later.** An older binary has no `strategies` command. The plugin then does not start it. It logs an error that says to update the proxy.
- `none` sends no keep-alive requests.

## What the plugin stores

The plugin never stores message text; the dashboard shows metadata only.

- Every proxy the plugin starts runs with `--dashboard-content=false` and `DASHBOARD_CONTENT=0`. This holds even when `DASHBOARD_CONTENT` is set in your environment.
- The dashboard still keeps counts, sizes, ids, and the names and text of tool declarations.
- There is no option to turn content capture on. The plugin has no way to attach to a proxy you start yourself.

## Change the preset or strategy in a session

- `/context-guru-preset` shows the saved preset. It lists the choices.
- `/context-guru-strategy` shows the cache strategy. It lists the choices.
- Both ask before they switch. Both save the choice first.
- A restart applies the choice now. It costs one cache rewrite in every open session of the project.
- If you say no, the choice applies at the next OpenCode start.
- A saved choice wins over the plugin options.

### Native dialogs (TUI)

In the OpenCode TUI, the two commands open a select list. The current choice is marked.
Two confirm dialogs follow: one to save the choice, one to restart the proxy.
This needs a second plugin entry, because OpenCode loads TUI plugins from `tui.json`.
Add this to `~/.config/opencode/tui.json` (or the project `tui.json`):

```json
{
  "$schema": "https://opencode.ai/tui.json",
  "plugin": ["/absolute/path/to/context-guru/opencode-plugin/tui.js"]
}
```

Restart OpenCode after the change.

**Tested in OpenCode 1.19.5.** Press `ctrl+p` and search for "Context Guru". Two entries appear: "Context Guru: preset" and
"Context Guru: cache strategy". Each one opens the select list, then the two confirm dialogs. The choice is saved only after the first yes.

The native dialogs have no slash name. `/context-guru-preset` and `/context-guru-strategy` are the chat commands.
They always run in chat, also in the TUI.

Where the dialogs do not exist (`opencode run`, web, desktop, or no `tui.json` entry), the same commands
run in chat. The agent shows the choices and asks for your yes in the conversation.
Both ways run the same script, so the result is the same.

## Other commands

- `/context-guru-status` shows proxy state and savings.
- `/context-guru-install` repairs the install.
- `/context-guru-update` checks for a new proxy binary. It installs after you confirm.
- `/context-guru-uninstall` stops the proxy and disables routing after you confirm.

If OpenCode already uses a gateway, configure its provider `options.baseURL` normally. The plugin
passes that URL to context-guru instead of replacing it. It does not overwrite API keys or provider
authentication.

## Remove or recover

Remove the local plugin path from the OpenCode config and restart OpenCode. The proxy will stop after
its idle timeout. To stop it immediately, run the `context-guru-reset` command from the project state
directory shown in the log, then restart OpenCode.

The reset command only stops a proxy recorded by this plugin. It checks that the process runs the
**binary path the plugin recorded**, so a binary with any file name is stopped, and a reused process
id is not. It does not remove a proxy found on `PATH`, provider credentials, or unrelated OpenCode
configuration.

`/context-guru-uninstall --yes --purge` also removes the saved choices (`settings.env`), the strategy
files, the logs, and the command scripts, except `context-guru-install`, which undoes the uninstall.
Two files stay: the `disabled` marker, and `dashboard.db.accountant-state.json`, which the proxy writes.
The next OpenCode start writes the command scripts again. The `disabled` marker keeps routing off.

## Troubleshooting

**OpenCode starts but requests do not use context-guru.** Check that the selected model belongs to
the `anthropic` or `openai` provider. Other providers are left untouched by design.

**Requests fail with a connection error.** Read `proxy.log` in the project state directory. The
plugin does not rewrite provider configuration when the proxy health check fails, so restarting
OpenCode restores the original provider endpoint.

**You changed your API key, and requests still fail with 401.** The proxy keeps the environment it started with. It does not read a new key. Run `/context-guru-uninstall --yes`, then `/context-guru-install`, and restart OpenCode with the new key set. Or stop the recorded proxy with the `context-guru-reset` script.

**A gateway stops working after the plugin routes it.** The plugin adds `/v1` to the proxy path, and
the proxy appends the rest to your gateway `baseURL`. A gateway `baseURL` must therefore end in `/v1`,
or in no path at all. A gateway that serves at `baseURL/messages` without `/v1` returns 404.

**The proxy is not on PATH.** The plugin downloads a release automatically. Set `CONTEXT_GURU_BIN`
to an executable path to use a locally installed binary, or set `CONTEXT_GURU_VERSION` to pin a
release tag.

## Not ported yet

These Claude Code plugin features do not exist in the OpenCode plugin:

- `doctor`, `insights`, `save-more`, `remove-mcp-and-skills`, `cache-campaign`
- the status line and the session signal hooks
- the manual ping header (`x-context-guru-ping`)
