# Client coverage

This page says, for each client and each client-specific feature, what the code does today.
`CLAUDE.md`, *Client-specific code*, says when to update it.

- **covered:** the code uses the client's own signal.
- **fallback:** the code uses the generic result. A note says what it is.
- **n/a:** the feature does not apply to this client.

State is from `origin/main` at the time of writing. An open PR is a note, not a status.

## By client

| Client | Subagent thread | Wait cause | Session key | Lifecycle signals | Compaction detection | Label |
|---|---|---|---|---|---|---|
| Claude Code | covered | covered | covered | covered | covered | covered |
| Codex | covered by #1318 (open): `x-codex-parent-thread-id`, `thread-id` | covered by #1318 (open): `request_user_input`, background-terminal text | fallback: content hash (covered by #1322, open: `session-id` header) | covered (no notification) | fallback: not detected (#1314) | covered |
| OpenCode | covered by #1318 (open): `x-opencode-parent-session-id` | fallback (#1318 reads `unknown`, not `human`) | covered: plugin header | covered by #1317 (open, approved) | fallback: not detected (#1314) | fallback: first User-Agent token (#1312) |
| Pi | fallback (#1318 adds no Pi signal) | fallback (#1318 reads `unknown`, not `human`) | covered: plugin header | covered | fallback: not detected (#1314) | fallback: first User-Agent token (#1312) |
| Bob | fallback | fallback: reads `human` | covered | fallback: start and compact only | covered | covered |

Notes:

- **Subagent thread** and **Wait cause** for Claude Code read `x-claude-code-agent-id` and the
  Claude Code markers. Other clients read as the main agent waiting for a person until #1318 lands.
- **Pi and OpenCode** send the session key in `x-context-guru-session`. The plugin sets it.
- **Bob** is on hold. Its fallbacks stay as they are.
- **#1312** moves the Claude Code rules into `agent/claudecode` and adds the OpenCode and Pi labels.
  Update this table when it merges.

## By host

A host without a client signal gives the generic result.

| Host | Subagent thread | Wait cause | Session key | Lifecycle signals | Compaction detection | Label |
|---|---|---|---|---|---|---|
| proxy | per client, as above | per client, as above | per client, as above | per client, as above | per client, as above | per client, as above |
| bifrost adapter | fallback: no headers | fallback: no headers | covered: host sets the session from `x-context-guru-session` | fallback: no `/signal` route | fallback: no headers | n/a |
| Cortex | **none: primary only**, worse than the proxy's prefix fallback (no `Opts.ThreadOf`). The effect is narrower: only per-thread component state and accounting, which falls back to its own "main" rule | n/a: no keep-alive in Cortex | covered: host id (`pctx.SessionID` or the A2A contextId); a request with no session is skipped | n/a: no `/signal` route in the plugin | fallback | n/a |

- **Cortex** source: `core/plugins/contextguru/plugin.go` in `rossoctl/cortex`. It wires no thread
  tracker, so every request is the primary thread. It has `pctx.Headers`, so `agent.Identify`
  works there once #1312 lands.
- **bifrost adapter:** `resolveSession` sees only a session id from the host context. It never sees
  request headers, so `agent.Identify` needs the transport to pass them.
- **Cortex gap:** wire `apply.Opts.ThreadOf` with `thread.ResolveSession` and
  `agent.Identify(pctx.Headers)`. Owner: Cortex repo. A follow-up issue is to be decided with the Cortex integration owner.
