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
| Codex | fallback (#1318) | fallback (#1318) | fallback: content hash (#1322) | covered | fallback: not detected (#1314) | covered |
| OpenCode | fallback (#1318) | fallback (#1318) | covered: plugin header | fallback: none sent (#1317) | fallback: not detected (#1314) | fallback: first User-Agent token (#1312) |
| Pi | fallback (#1318) | fallback (#1318) | covered: plugin header | covered | fallback: not detected (#1314) | fallback: first User-Agent token (#1312) |
| Bob | fallback | fallback | covered | fallback: start and compact only | covered | covered |

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
| bifrost adapter | fallback: no headers | fallback: no headers | covered: host sets the session | fallback: no `/signal` route | fallback: no headers | n/a |
| Cortex | fallback | fallback | fallback | fallback | fallback | n/a |

The Cortex row is not checked against Cortex code. The owner of Cortex must confirm it.
