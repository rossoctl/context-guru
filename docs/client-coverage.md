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

## Rules that name one client's text, tools or fields

Each rule below lives in `agent/claudecode` (or `agent/codex`) and is read through `agent.Client`
(`agent/rules.go`). **fallback** for every rule here: a client with no rule of its own gets Claude
Code's rule, as the code did before the rules moved. The exception is the tools that wait on a
person (`AsksHuman`, `IsQuestionTool`): the fallback is the union of Claude Code's and Codex's lists. A zero `Client` (a stored row, an offline
transcript, a host that passes none) gets the same. No source shows that another client sends
different text, so only Claude Code has rules for these, and Codex has its own question tool.

| Rule | Claude Code | Codex | OpenCode | Pi | Bob |
|---|---|---|---|---|---|
| Client label on a ping row and a trace span (`Client.Label`; no client gives `unknown`) | covered | covered | covered: first User-Agent token | covered: `pi` | covered |
| Injected text: prefixes and wrapper tags (`Client.Injected`; wake cause and transcript reconstruction) | covered | fallback: Claude Code's | fallback: Claude Code's | fallback: Claude Code's | fallback: Claude Code's |
| Subagent tool names and the subagent type field (`IsSubagentTool`) | covered: `Agent`, `Task` | fallback | fallback | fallback | fallback |
| Built-in subagent types (`IsBuiltinSubagentType`) | covered | fallback | fallback | fallback | fallback |
| Built-in tool list, with `ToolSearch` (`IsBuiltinTool`) | covered | fallback | fallback | fallback | fallback |
| Tools that wait on a person (`AsksHuman`, `IsQuestionTool`) | covered: `AskUserQuestion`, `ExitPlanMode` | covered: `request_user_input` | fallback: both lists | fallback: both lists | fallback: both lists |
| Per-person id in the body (`Client.UserID`: `metadata.user_id`) | covered | fallback: the field is absent, so `""` | fallback | fallback | fallback |

- **Sources:** the injected prefixes and tags were read off real Claude Code transcripts (the
  corpus the comments in `agent/claudecode/inject.go` cite). The source file and version were not
  recorded when they were written. The Codex question tool is `openai/codex` f64f003,
  `codex-rs/core/src/tools/handlers/request_user_input_spec.rs`.
- **The two readers of injected text match on purpose in different ways.** The wake cause labels a
  whole block, so one tag anywhere or a prefix at the start makes it `system`. The transcript
  reconstruction keeps the real text next to a tag, so it strips tags and drops a message that
  starts with a prefix.
- **Compaction settings** (`compactionpoint`) stay where they are. The settings types of each
  client (`ClaudeCode`, `Bob`, `Codex`), their `Resolve` methods and `Settings.Pick` form one unit.
  `Resolve` returns `compactionpoint.Point`, and `Settings` holds the types. Moving the types into
  the agent packages makes those packages import `compactionpoint`, and `compactionpoint` import
  them: a cycle. Moving only the file readers would split the env names and file keys from the
  types they fill.
- **Callers with no request** (`dash` declaration rows, `record` reconstruction) pass the zero
  `Client`. They get the fallback. `respfacts.Spawns` also passes it, but its caller does have the
  request's `Client`: the stream readers belong to another change, which threads it through.

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
