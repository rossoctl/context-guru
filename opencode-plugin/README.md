# Context Guru for OpenCode

This plugin starts a local `context-guru-proxy` and routes OpenCode's Anthropic and OpenAI provider
requests through it. It preserves configured provider gateway hosts and path prefixes as the proxy's
upstreams, adds the OpenCode session ID to each request, and fails open if the proxy cannot start.
If the proxy later exits — idle timeout or crash — the plugin restarts it on the same port before
the next request.

When the proxy is down, OpenCode v1 requests go straight to the provider (the fallback):

- The plugin sends a request direct only when the proxy cannot have received it: the connection was
  refused, or the proxy did not pass its health check, so the request was never sent to it.
- It never sends a request twice. A 4xx or 5xx from the proxy, a reset or a time-out after the
  connection opened, a stream that broke, and a cancel reach OpenCode unchanged.
- A direct request has your own headers and body. The plugin removes only its own
  `x-context-guru-session` header. Direct requests get no context-guru work, and `/stats` does not count them.
- A request waits about 5 s for a restart (up to 6.5 s when the health check also hangs). After a
  failed restart, requests go direct for 30 s and start no new restart. Routing comes back at the
  first request after the proxy is healthy again. In the 30 s after a failed restart, it comes back
  when the 30 s end, even if `/context-guru-install` started the proxy earlier.
- After `/context-guru-uninstall`, open sessions go direct at once. No OpenCode restart is needed.
- The log line names the provider host and the error code only.
- Set the plugin option `"fallback": false` to turn all of this off. Then requests fail while the proxy is down, as before.

The plugin supports both OpenCode generations from one file: OpenCode 1.18.29 and later call the
v1 `server()` export (`config` and `chat.headers` hooks), and OpenCode 2 calls the v2 `setup()`
export (provider, command, and session-hook domains). Older OpenCode 1.x releases that only
accept a function default export are not supported.

Install the local plugin in `~/.config/opencode/opencode.json` for all projects:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["/absolute/path/to/context-guru/opencode-plugin/index.js"]
}
```

On OpenCode 2 the config key is `plugins` (plural); the same local path works:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["/absolute/path/to/context-guru/opencode-plugin/index.js"]
}
```

For a project-only install, put the same local path in the project's `opencode.json` instead. The
first startup downloads release v0.7.4 when `context-guru-proxy` is not already on `PATH`,
verifies its SHA-256 checksum, and stores it under `~/.local/state/context-guru-opencode/`.

The default is `preset: off` with the `auto` cache strategy. These match the Claude Code plugin.
`auto` fits Anthropic and OpenAI. If a chosen strategy does not fit the routed providers (for example `1-hour-head` on an OpenAI-only setup), the plugin runs `none` and logs why.
Set other values with the tuple form:

```json
{
  "plugin": [["/absolute/path/to/context-guru/opencode-plugin/index.js", {"preset": "conservative", "cacheStrategy": "none"}]]
}
```

On OpenCode 2, options use the entry-object form:

```json
{
  "plugins": [{ "package": "/absolute/path/to/context-guru/opencode-plugin/index.js", "options": { "preset": "conservative", "cacheStrategy": "none" } }]
}
```

Presets: `off`, `conservative`, `medium`, `high`, `xhigh`, `ultrahigh`.
Cache strategies: the proxy owns the list. The plugin reads it with `context-guru-proxy strategies` and keeps no copy.
The plugin offers only the strategies that fit a provider it routes. The default is the proxy default, `auto`.
If `auto` does not fit the routed providers, the plugin runs `none` and logs why.
The old name `5-min-ping` still works. The proxy reads it as `auto`.
The plugin writes only `cache.strategy: <name>` to `proxy.yaml`. The proxy fills in the keys.
**The proxy must be v0.7.1 or later.** An older binary cannot list strategies, so the plugin does not start it and logs an error.
Pings spend your own credential while you are away. `none` sends no pings.

A choice saved by `/context-guru-preset` or `/context-guru-strategy` wins over these options.
The option `"fallback": false` turns off the direct send when the proxy is down (see above). The default is `true`.

Restart OpenCode after changing plugin configuration. The proxy dashboard is available at the
port printed in the OpenCode log under `/dashboard/`. Each project gets its own port and state.

## Commands

The plugin registers six commands:

| Command | What it does |
|---|---|
| `/context-guru-status` | Proxy up or down, port, version, routing, and savings from `/stats` |
| `/context-guru-install` | Idempotent repair: clears a disable marker, restarts a stopped proxy |
| `/context-guru-update` | Checks for a newer proxy binary; installs it after confirmation |
| `/context-guru-uninstall` | Stops the proxy and disables routing after confirmation; `--purge` also removes the binary the plugin downloaded and the state files, except the `disabled` marker and the proxy's `dashboard.db.accountant-state.json` |
| `/context-guru-preset` | Chat command. Lists the presets, switches after your yes in the chat. The native dialog is in the `ctrl+p` palette |
| `/context-guru-strategy` | Chat command. Lists the cache strategies, switches after your yes in the chat. The native dialog is in the `ctrl+p` palette |

The two pickers save your choice first. A restart applies it. A restart costs one cache rewrite
in every open session of this project, so the command asks first. If you say no, the choice
applies at the next OpenCode start.

### Native dialogs in the TUI

`tui.js` is a second plugin entry. OpenCode loads TUI plugins from `tui.json`, and a module cannot
export both `server` and `tui`. Add it to `~/.config/opencode/tui.json`:

```json
{
  "$schema": "https://opencode.ai/tui.json",
  "plugin": ["/absolute/path/to/context-guru/opencode-plugin/tui.js"]
}
```

- `tui.js` registers two commands with `api.keymap.registerLayer`. In OpenCode 1.19.5 they show in the `ctrl+p` palette under "Context Guru". They have no slash name, so `/context-guru-preset` and `/context-guru-strategy` always open the chat commands.
- A select list marks the current choice. Two confirm dialogs follow: save, then restart.
- Without `tui.json`, or in `opencode run`, web and desktop, use the chat commands.
- Both ways run the same `set NAME [--restart]` script.

## Safety

- The plugin never stores message text; the dashboard shows metadata only. Every proxy it starts gets `--dashboard-content=false` and `DASHBOARD_CONTENT=0`, even when `DASHBOARD_CONTENT` is set in your environment.
- Only a provider the plugin routed gets the `x-context-guru-session` header.
- Two OpenCode instances share one lock for choosing a port, starting the proxy, and writing the records. The holder refreshes the lock every 5 s, and a lock nobody refreshed for 30 s is broken. The download of the release does not hold the lock. The plugin counts a proxy as started only while its own child process is alive.
- A stop or reset signals a process only when it runs the binary path the plugin recorded.
- Your own gateway (for example `localhost:4000`) stays the upstream. The plugin does not replace it.

## Not ported yet

These Claude Code plugin features do not exist here:

- `doctor`, `insights`, `save-more`, `remove-mcp-and-skills`, `cache-campaign`
- `statusline` and the session signal hooks
- the `x-context-guru-ping` header for manual pings
- the keep-alive plan from `cache-campaign`

See the [matrix of plugins, providers and settings](../docs/reference/plugins-matrix.md) for what each plugin offers per provider.
