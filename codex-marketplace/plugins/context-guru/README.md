# context-guru for Codex

This is the Codex plugin bundle for context-guru. It routes ordinary Codex sessions through a
loopback proxy, exposes health and statistics skills, and can remove only the state it owns.

Register the GitHub marketplace and install the plugin:

```sh
codex plugin marketplace add rossoctl/context-guru --sparse .agents --sparse codex-marketplace
codex plugin add context-guru@context-guru
```

The sparse paths keep Codex from cloning the monorepo's unrelated source and history. Without them,
the marketplace clone can exceed Codex's 30-second timeout. Let the first command finish before
running the second command.

Run marketplace registration from an ordinary shell. You can install the plugin afterward either
with the second command above or from `/plugins` inside Codex. Asking an existing Codex session to
run the shell commands is possible, but provides no advantage: Codex only loads newly installed
plugin skills in a new session, so you must restart Codex either way.

Start Codex and invoke the setup skill:

```text
$context-guru-setup
```

Setup asks before routing model traffic, preserves the currently selected provider as the proxy's
upstream, and updates `~/.codex/config.toml`. The change takes effect in the next session; after
that, start Codex normally with `codex`.

The proxy runs as a user service—`systemd --user` on Linux or a LaunchAgent on macOS—rather than as
a child of a Codex command. This lets it survive command-sandbox teardown and restart independently
after a failure or login. Proxy lifecycle does not depend on a `SessionStart` hook because Codex
hooks cannot request the permissions needed to escape their command sandbox.

The plugin's only hooks are `SessionEnd` and `SessionStart` (`compact`, `resume`, `clear`): each posts a
content-free event (session id, event name, reason) to the local proxy so the cache keep-alive stops
pinging a session you have closed or compacted. They need loopback access only, which Codex's command
sandbox allows, and they run once you trust them in `/hooks`; until then the keep-alive behaves as it
does without them.

Codex uses OpenAI's Responses API. Content reduction defaults to `off`, so requests are forwarded
without trimming. Cache keep-alive is enabled by default: eligible OpenAI Responses sessions are
refreshed shortly before their 30-minute cache lifetime ends. You can opt into content reduction
separately after verifying the routed setup.

Use `$context-guru-preset-picker` to inspect or change the reduction level, and
`$context-guru-cache-strategy-picker` to inspect or change OpenAI cache keep-alive. Both preserve
the other setting and restart the owned proxy. `$context-guru-insights` produces a deterministic,
ranked report from measured proxy data; its cost arithmetic and exact fix commands come from code,
not model estimation.

For a narrower report, use `$context-guru-insights-capabilities`,
`$context-guru-insights-components`, or `$context-guru-insights-idle`.

During setup, the plugin downloads the latest `context-guru-proxy` release for the current platform,
verifies it against the release's SHA-256 checksums, and installs it into its private state directory.
An existing `context-guru-proxy` on `PATH` is reused instead. Codex requires proxy release v0.4.0
or newer.

Setup copies a standalone recovery command to
`~/.local/state/context-guru-codex/context-guru-reset`. Use it from an ordinary, unrouted shell if
the proxy is down and Codex cannot start. It restores the previous default provider, removes only
the marked provider block, removes only context-guru's owned user service, and deletes the proxy
binary downloaded into the plugin's private state directory. A proxy binary reused from `PATH` is
never removed. Older installations that predate user-service supervision retain the guarded
recorded-process cleanup as a fallback.

## Operations and troubleshooting

| Task | Command |
|---|---|
| Check routing and proxy health | `$context-guru-status` |
| Print raw proxy statistics JSON | `$context-guru-status --stats` |
| Check for a proxy update | `$context-guru-update` |
| Install a proxy update | `~/.local/state/context-guru-codex/context-guru-update` from an ordinary shell |
| Plan removal and show instructions | `$context-guru-uninstall` |
| Remove routing and stop the proxy | `~/.local/state/context-guru-codex/context-guru-reset --yes` from an ordinary shell |

Proxy updates are not automatic on Codex. Run `$context-guru-update` periodically to check. When
an update is available, exit Codex and run the standalone updater shown above; then start a new
session. Updating the Codex plugin bundle is separate and uses the marketplace command below.

Codex may ask permission when these skills need to access the local proxy, write configuration or
state under your home directory, download an update, or restart the proxy. A sandboxed command can
be blocked from `127.0.0.1` even when the proxy is healthy; status and insights therefore retry
their read-only health checks with permission instead of reporting a false outage.

Do not perform proxy update or removal from a routed Codex session. Codex selects its transport
when the session starts, so lifecycle operations on that proxy belong in an ordinary shell.
`$context-guru-update` and `$context-guru-uninstall` therefore check or plan only and direct you to
the corresponding standalone command. Exit Codex, run it, then start a new session.

The uninstall skill and recovery script restore the provider that was selected before setup. They
do not remove the Codex plugin registration. To remove that too, from an ordinary shell:

```sh
codex plugin remove context-guru@context-guru
codex plugin marketplace remove context-guru
```

To refresh the plugin code without removing it:

```sh
codex plugin marketplace upgrade context-guru
```

Registrations created with the sparse install command retain those paths during upgrades. If an
older, non-sparse registration times out while upgrading, use the two removal commands above, then
repeat the sparse marketplace and plugin install commands at the top of this page. This changes the
plugin registration only; it does not remove the existing proxy routing or state.

Restart Codex after installing or refreshing plugin code. Routing or proxy changes made by setup
take effect for model traffic in the next Codex process.
