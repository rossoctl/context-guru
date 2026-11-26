# Client coverage

This page says, for each client and each client-specific feature, what the code does today.
`CLAUDE.md`, *Client-specific code*, says when to update it.

- **covered:** the code uses the client's own signal.
- **fallback:** the code uses the generic result. A note says what it is.
- **n/a:** the feature does not apply to this client.

Checked against main on 2026-11-25. The release check keeps this table current.

## By client

| Client | Subagent thread | Wait cause | Session key | Lifecycle signals | Compaction detection | Label | Our tools: inject and in-band answer |
|---|---|---|---|---|---|---|---|
| Claude Code | covered | covered | covered | covered | covered | covered | not checked |
| Codex | covered: `thread-id` with `x-codex-parent-thread-id` (`agent.Client.SubagentID`) | covered: `request_user_input` and the terminal markers (Codex client only). An end turn with nothing open reads `unknown` | covered: `session-id` header | covered (no notification) | covered: `request_kind` in the client metadata | covered | fallback: continuations go over WebSocket; the proxy answers 426 and the client falls back to full-history HTTP. A fail-open in a stream hands back the raw call; the client returns an error to the model and continues (see notes) |
| OpenCode | covered: `x-opencode-parent-session-id`; the thread id is `x-opencode-session-id` | fallback: an end turn with nothing open reads `unknown` (auto) | covered: `x-opencode-session-id` | covered | covered: plugin header `x-context-guru-request-kind` | covered: first User-Agent token (`opencode/<version>`) | fallback: same as Codex (WebSocket 426, then full-history HTTP; raw call on a fail-open, error to the model) |
| Pi | fallback: prefix match | fallback: an end turn with nothing open reads `unknown` (auto) | covered: plugin header | covered | covered: plugin header `x-context-guru-request-kind` | covered: `x-context-guru-client` | fallback: raw call on a fail-open; the client returns an error to the model and continues (from the report, not verified here). WebSocket: not checked |
| Bob | fallback | fallback: reads `human` | covered | fallback: start and compact only | covered | covered | not checked |

Notes:

- **Subagent thread** and **Wait cause** for Claude Code read `x-claude-code-agent-id` and the
  Claude Code markers. Codex and OpenCode name a subagent with their own headers (above).
  Codex adds its question tool and terminal markers. For a client that is not Claude Code or Bob,
  an end turn with nothing open reads `unknown`, not `human`. Pi and OpenCode have no wait-cause
  signal of their own yet.
- **Pi and OpenCode** send the session key in `x-context-guru-session`. The plugin sets it.
- **Bob** is on hold. Its fallbacks stay as they are.
- **One `agent.Identify` per request.** The host calls it once. Library code takes the `agent.Client`
  value and reads no header again.
- **Our tools** means the injected `context_guru_expand` tool and the in-band answer to its call.
  - Codex and OpenCode send continuations over WebSocket. The proxy answers the upgrade with 426
    (`proxy/proxy.go`, the WebSocket upgrade handler). Both clients then send the full history over
    HTTP on every turn, so the in-band answer works.
  - On a fail-open in the middle of a stream, the client gets the raw `context_guru_expand` call.
    Each client returns an "unknown tool" error to the model and continues the turn. The cost is
    one wasted turn per leaked call. Sources:
    Codex `codex-rs/core/src/tools/registry.rs:838-858` (f64f003);
    OpenCode `packages/ai/src/tool-runtime.ts:23-31` (2.4.1);
    Pi `dist/agent-loop.js:488-493` (from the report, not verified here).

## By host

A host without a client signal gives the generic result.

| Host | Subagent thread | Wait cause | Session key | Lifecycle signals | Compaction detection | Label | Our tools: inject and in-band answer |
|---|---|---|---|---|---|---|---|
| proxy | per client, as above | per client, as above | per client, as above | per client, as above | per client, as above | per client, as above | per client, as above |
| bifrost adapter | fallback: no headers | fallback: no headers | covered: host sets the session from `x-context-guru-session` | fallback: no `/signal` route | fallback: no headers | n/a | not checked |
| Cortex | **none: primary only**, worse than the proxy's prefix fallback (no `Opts.ThreadOf`). The effect is narrower: only per-thread component state and accounting, which falls back to its own "main" rule | n/a: no keep-alive in Cortex | covered: host id (`pctx.SessionID` or the A2A contextId); a request with no session is skipped | n/a: no `/signal` route in the plugin | fallback | n/a | not checked |

- **Cortex** source: `core/plugins/contextguru/plugin.go` in `rossoctl/cortex`. It wires no thread
  tracker, so every request is the primary thread. It has `pctx.Headers`, so the plugin
  can call `agent.Identify`. `waitcause` is public, so Cortex can call
  `waitcause.ClassifyWaitingForClient` with that `Client`. Cortex has no keep-alive, so it has no use
  for the wait cause today.
- **bifrost adapter:** `resolveSession` sees only a session id from the host context. It never sees
  request headers, so `agent.Identify` needs the transport to pass them.
- **Cortex gap:** wire `apply.Opts.ThreadOf` with `thread.ResolveSession` and
  `agent.Identify(pctx.Headers)`. Owner: Cortex repo. A follow-up issue is to be decided with the Cortex integration owner.

## Prediction model agent value

The live value is `Client.ModelAgent(provider)`. It equals what training reads from the stored row.

| Client | Value the model gets |
|---|---|
| Claude Code (CLI) | `claude-cli` |
| Codex | `codex` |
| OpenCode | `opencode` |
| Pi | the provider token: `anthropic` or `openai` |
| Bob | `bob` |

Only `claude-cli`, `openai` and `asyncopenai` have a one-hot in the model. Any other value reads as unseen.
