# Install the OpenCode plugin

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

1. Reuses `context-guru-proxy` from `PATH`, or downloads the current release and verifies its SHA-256 checksum.
2. Starts one loopback proxy for the project, using a free port beginning at `8787`.
3. Routes Anthropic requests to `/anthropic` and OpenAI requests to `/openai/v1`.
4. Preserves any configured provider gateway host and path prefix as the proxy's upstream, avoiding a duplicate trailing `/v1`.

The proxy state, log, dashboard database, and recovery command live under:

```text
~/.local/state/context-guru-opencode/<project-id>/
```

The exact project directory is printed to the OpenCode log. The dashboard is at
`http://127.0.0.1:<port>/dashboard/`.

## Configure

The default preset is `off`, so request bodies are forwarded unchanged. The default cache strategy
is `5-min-ping`, which sends keep-alive requests using the configured provider credential. Set a
different preset or disable keep-alive with the tuple form:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["/absolute/path/to/context-guru/opencode-plugin/index.js", {"preset": "conservative", "cacheStrategy": "none"}]
  ]
}
```

Available presets are `off`, `conservative`, `medium`, `high`, and `xhigh`. Available cache
strategies are `5-min-ping` and `none`. Keep-alive can spend provider quota; `none` does not send
those requests.

If OpenCode already uses a gateway, configure its provider `options.baseURL` normally. The plugin
passes that URL to context-guru instead of replacing it. It does not overwrite API keys or provider
authentication.

## Remove or recover

Remove the local plugin path from the OpenCode config and restart OpenCode. The proxy will stop after
its idle timeout. To stop it immediately, run the `context-guru-reset` command from the project state
directory shown in the log, then restart OpenCode.

The reset command only stops a proxy recorded by this plugin. It does not remove a proxy found on
`PATH`, provider credentials, or unrelated OpenCode configuration.

## Troubleshooting

**OpenCode starts but requests do not use context-guru.** Check that the selected model belongs to
the `anthropic` or `openai` provider. Other providers are left untouched by design.

**Requests fail with a connection error.** Read `proxy.log` in the project state directory. The
plugin does not rewrite provider configuration when the proxy health check fails, so restarting
OpenCode restores the original provider endpoint.

**The proxy is not on PATH.** The plugin downloads a release automatically. Set `CONTEXT_GURU_BIN`
to an executable path to use a locally installed binary, or set `CONTEXT_GURU_VERSION` to pin a
release tag.
