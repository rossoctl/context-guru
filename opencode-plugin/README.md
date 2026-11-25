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
- One request waits about 5 s for a restart (up to 6.5 s when the health check also hangs). Requests
  that come while the restart runs go direct at once. A binary that exits fails the restart at once. After a
  failed restart, requests go direct for 30 s and start no new restart. Routing comes back at the
  first request after the proxy is healthy again. In the 30 s after a failed restart, it comes back
  when the 30 s end, even if `/context-guru-install` started the proxy earlier.
- After `/context-guru-uninstall`, open sessions go direct at once. No OpenCode restart is needed.
- The OpenCode log gets one line when the proxy goes down (with the reason, for example "exited with code 1"),
  and one when it is back. A refused connection logs the provider host and the error code only.
  No line holds headers or bodies, and there is no line for each direct request.
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
first startup downloads release v0.7.8 when `context-guru-proxy` is not already on `PATH`,
verifies its SHA-256 checksum, and stores it in `~/.local/state/context-guru-opencode/bin/<version>/`.
All projects share this one copy per version. Each project links to the version it uses.
`/context-guru-uninstall --purge` removes a shared copy only when no other project still uses it.

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
**The proxy must be v0.7.1 or later.** An older binary cannot list strategies, so the plugin does not start it and logs an error. If the plugin downloaded that binary, `/context-guru-update` installs the latest release; then restart OpenCode.
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
| `/context-guru-uninstall` | Stops the proxy and disables routing after confirmation; `--purge` also removes the binary the plugin downloaded (a shared copy only when no other project uses it) and the state files, except the `disabled` marker and the proxy's `dashboard.db.accountant-state.json` |
| `/context-guru-preset` | Chat command. Lists the presets, switches after your yes in the chat. The native dialog is in the `ctrl+p` palette |
| `/context-guru-strategy` | Chat command. Lists the cache strategies, switches after your yes in the chat. The native dialog is in the `ctrl+p` palette |

After `/context-guru-uninstall --yes` and an OpenCode restart, the plugin registers no command and
no `ctrl+p` entry for the project. Until the restart, `/context-guru-status` prints `enabled=no`.
To turn context-guru on again, run `sh <state directory>/context-guru-install` from any shell, then
restart OpenCode. The start log names the script path.

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

## Lifecycle signals

The plugin tells the proxy when a routed session ends or is compacted, so the keep-alive stops pings for a dead cache entry.

| OpenCode event | Version | Signal to `/signal` |
|---|---|---|
| `session.compacted` | v1 | `SessionStart`, `compact` |
| `session.compaction.ended` | v2 | `SessionStart`, `compact` |
| `session.deleted` | v1, v2 | `SessionEnd`, `other` |
| `permission.asked` | v1, v2 | `Notification`, `permission_prompt` |
| `permission.v2.asked` | v1 only (SDK type) | `Notification`, `permission_prompt` |
| `question.asked` | v1 | `Notification`, `elicitation_dialog` |

- The body holds only the session id (the one in `x-context-guru-session`), an event name and a kind.
- The plugin signals only for a session that already sent a request through the proxy.
- A signal has a 1 s timeout. A lost signal changes nothing.
- Set `CONTEXT_GURU_CLIENT_SIGNALS=off` to turn signals off.

## Not ported yet

These Claude Code plugin features do not exist here:

- `doctor`, `insights`, `save-more`, `remove-mcp-and-skills`, `cache-campaign`
- `statusline` and the Claude Code session hooks (OpenCode sends its own lifecycle signals, see above)
- the `x-context-guru-ping` header for manual pings
- the keep-alive plan from `cache-campaign`

See the [matrix of plugins, providers and settings](../docs/reference/plugins-matrix.md) for what each plugin offers per provider.
