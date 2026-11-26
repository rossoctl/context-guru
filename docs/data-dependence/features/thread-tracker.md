---
name: Thread tracker
kind: metadata
off: 🟡 Works, metadata
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 150
---

It tells the main agent from its subagents. It uses hashes to check if a new request continues an earlier one. Since #947 it also marks the first request after the main agent's own compaction. It finds the client's own compaction request in two ways (#1314). First, a mark that needs no text: the `x-context-guru-request-kind: compaction` header that the OpenCode and Pi plugins set, and Codex's own `x-codex-turn-metadata` (its `request_kind` field). This is metadata only. Second, a match on the opening of the client's summary prompt in the last turn only (Claude Code, Bob Shell, Codex, OpenCode and Pi), read from `messages` or from the Responses `input`. This reads the text of the last turn. It is a deliberate exception, the same as before, and it is the fallback for a client that sends no mark or a host that passes no headers. With storage off or at the metadata level, the mark still works and the phrase match still runs on the request in flight. Nothing is stored. The proxy sends the mark to the accountant library and stores it on the request row (`client_compaction`: `unknown` or empty), where the replay cap reads it. A request the tracker could not judge (no session id, no message list) stores no mark. It is one short label, with no text. Since #1310 it also reads four more request headers to name a subagent: Codex's `x-codex-parent-thread-id` and `thread-id`, and OpenCode's `x-opencode-parent-session-id` and `x-opencode-session-id`. They are ids. They carry no text. The level does not change.
