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

The default is `preset: off` with the `5-min-ping` cache strategy. Configure the tuple form when
needed:

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

Restart OpenCode after changing plugin configuration. The proxy dashboard is available at the
port printed in the OpenCode log under `/dashboard/`. Each project gets its own port and state.

## Commands

The plugin registers four commands:

| Command | What it does |
|---|---|
| `/context-guru-status` | Proxy up or down, port, version, routing, and savings from `/stats` |
| `/context-guru-install` | Idempotent repair: clears a disable marker, restarts a stopped proxy |
| `/context-guru-update` | Checks for a newer proxy binary; installs it after confirmation |
| `/context-guru-uninstall` | Stops the proxy and disables routing after confirmation; `--purge` also removes the binary and state files |

`update` and `uninstall` only check and plan when invoked; they change nothing until you confirm,
and the confirmation surfaces as your agent's shell-permission prompt. Uninstall takes effect at
the next OpenCode restart, and `/context-guru-install` undoes it.

Each project's scripts live in its state directory (`~/.local/state/context-guru-opencode/`)
alongside `context-guru-reset`, so they also work without OpenCode.

Not ported from the Claude Code plugin yet: `preset-picker`, `cache-strategy-picker`, `doctor`,
`insights`, `save-more`, `remove-mcp-and-skills`, `cache-campaign`, `statusline`, and the session
signal hooks.
