# opencode-context-guru

This plugin starts a local `context-guru-proxy` and routes OpenCode's Anthropic and OpenAI provider
requests through it. It preserves configured provider base URLs as the proxy's upstreams, adds the
OpenCode session ID to each request, and fails open if the proxy cannot start.

Install it in `~/.config/opencode/opencode.json` for all projects:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["opencode-context-guru"]
}
```

For a project-only install, put the same entry in the project's `opencode.json` instead. The first
startup downloads the release binary when `context-guru-proxy` is not already on `PATH`, verifies
its SHA-256 checksum, and stores it under `~/.local/state/context-guru-opencode/`.

The default is `preset: off` with the `5-min-ping` cache strategy. Configure the tuple form when
needed:

```json
{
  "plugin": [["opencode-context-guru", {"preset": "conservative", "cacheStrategy": "none"}]]
}
```

Restart OpenCode after changing plugin configuration. The proxy dashboard is available at the
port printed in the OpenCode log under `/dashboard/`. Each project gets its own port and state.
