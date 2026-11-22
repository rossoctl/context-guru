---
name: Wait-cause and lifecycle signals
kind: metadata
off: ❌ Off (it needs the dashboard)
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 190
---

To tell why the agent paused, it reads the request once. It keeps only counts and ids. Since #857 it also reads the **tool name** of the last tool call, to tell that an agent has finished (`done`). The keep-alive then sends no pings for that thread. A tool name is metadata, and the rule works at every storage level. Since #858 the keep-alive also uses the **wait cause** (from the stop reason, the tool names and the open-task counts), whether the thread id starts with `a:`, the token count of the prefix and the time of day, to pick the ping count, to ignore a strategy's time windows for a wait that is not for a person, and to skip the sweep question during a tool wait. Since #1225 the turn's wait cause is also read from the request body while the request goes upstream, by a function that writes and stores nothing, so the keep-alive plans the turn at response time. The dashboard writer reuses that reading, so the body is normally read once (twice when the scan was not ready in time). It sends no body and no text to the keep-alive: only open-task counts. The body is already in memory during the request, and the level does not change. It also keeps one flag per thread: "has a tool call been seen". All of it is metadata, and it works at every storage level.
