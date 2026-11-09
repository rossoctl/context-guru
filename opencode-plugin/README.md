# Context Guru for OpenCode

This plugin starts a local `context-guru-proxy` and routes OpenCode's Anthropic and OpenAI provider
requests through it. It preserves configured provider gateway hosts and path prefixes as the proxy's
upstreams, adds the OpenCode session ID to each request, and fails open if the proxy cannot start.
If the proxy later exits — idle timeout or crash — the plugin restarts it on the same port before
the next request.

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
first startup downloads the release binary when `context-guru-proxy` is not already on `PATH`,
verifies its SHA-256 checksum, and stores it under `~/.local/state/context-guru-opencode/`.

The default is `preset: off` with the `auto` cache strategy. These match the Claude Code plugin.
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
Cache strategies: `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head`. The old name `5-min-ping` still works. It means `auto`.
Pings spend your own credential while you are away. `none` sends no pings.

A choice saved by `/context-guru-preset` or `/context-guru-strategy` wins over these options.

Restart OpenCode after changing plugin configuration. The proxy dashboard is available at the
port printed in the OpenCode log under `/dashboard/`. Each project gets its own port and state.

## Commands

The plugin registers six commands:

| Command | What it does |
|---|---|
| `/context-guru-status` | Proxy up or down, port, version, routing, and savings from `/stats` |
| `/context-guru-install` | Idempotent repair: clears a disable marker, restarts a stopped proxy |
| `/context-guru-update` | Checks for a newer proxy binary; installs it after confirmation |
| `/context-guru-uninstall` | Stops the proxy and disables routing after confirmation; `--purge` also removes the binary and state files |
| `/context-guru-preset` | Shows the saved and running preset, lists the choices, switches after your yes |
| `/context-guru-strategy` | Shows the cache strategy, lists the choices, switches after your yes |

The two pickers save your choice first. A restart applies it. A restart costs one cache rewrite
in every open session of this project, so the command asks first. If you say no, the choice
applies at the next OpenCode start.

`update` and `uninstall` only check and plan when invoked; they change nothing until you confirm,
and the confirmation surfaces as your agent's shell-permission prompt. Uninstall takes effect at
the next OpenCode restart, and `/context-guru-install` undoes it.

Each project's scripts live in its state directory (`~/.local/state/context-guru-opencode/`)
alongside `context-guru-reset`, so they also work without OpenCode.

## Safety

- Only a provider the plugin routed gets the `x-context-guru-session` header.
- Two OpenCode instances share one lock. They cannot pick the same port or write the same pid file.
- Your own gateway (for example `localhost:4000`) stays the upstream. The plugin does not replace it.

## Not ported yet

These Claude Code plugin features do not exist here:

- `doctor`, `insights`, `save-more`, `remove-mcp-and-skills`, `cache-campaign`
- `statusline` and the session signal hooks
- the `x-context-guru-ping` header for manual pings
- the keep-alive plan from `cache-campaign`
