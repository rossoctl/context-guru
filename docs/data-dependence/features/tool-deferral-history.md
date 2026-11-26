---
name: **Tool deferral history** (`ultrahigh`, #485)
kind: metadata
off: ❌ Off with no durable place: no dashboard database and tool deferral's own file cannot be opened. Then it lives in memory only, a restart starts it from zero, and nothing is deferred until 3 new sessions. With the Store off nothing is deferred at all.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 50
---

To defer only the tools a user does not use, it keeps, per user, session and tool: whether the session declared the tool, whether it used or loaded it, the project folder and the time. The rule groups MCP tools by server (`mcp__<server>__*`) when it reads this; the table still holds tool names only. It keeps the newest 64 sessions per user, plus the newest 10 sessions of each project, up to 256 sessions per user in all. The extra sessions are there because the rule checks whether this project used a tool in its last 10 sessions; without them, a user in many projects would push a project's older sessions out, and the check would not see them. It deletes sessions idle longer than the session-state retention (30 days by default). The table is `tool_defer_history`, in the dashboard database when there is one, else in tool deferral's own file. A user's first deferral request copies that user's own sessions, and no other user's, from the dashboard's inventory tables. It also keeps the provider check result per endpoint, model and route (`tool_defer_probe`); that row describes an endpoint, not a user. An account purge deletes the user's history rows, thread records and opt-out flags. Tool names, ids, paths, flags and times only. Nothing is shared between users: the history, the records and the opt-outs are keyed by user, and the pending opt-outs are capped per user. The check spends the user's own credential, under $0.01 per check, once a day per model (a failed check is retried after 15 minutes). Decided by: David Amid, #485, 2026-11-02.
