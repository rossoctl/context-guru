---
name: toolfilter
kind: metadata
where: Nothing kept for `remove`. With `defer: auto` (the `ultrahigh` preset, or the Bob plugin's `bob-ultrahigh`): one record per thread with the names of the deferred tools (`cg:tdf:`; on GPT, GLM and Kimi, where our own search tool runs, a second record under the same prefix also lists the tools a search added back and, for each search the client received, its call id and the tool names it asked for, #825; on a Claude Code thread with its own `ToolSearch`, the thread record also keeps each search call id and the names the proxy added to its result, #898), and a session's tool search opt-out flag (`cg:tso:`), both pinned. They live in the session-state table when it is on (`--dashboard`), else in tool deferral's own file, `context-guru-tooldefer.db`, next to the dashboard database path. Plus the tool history (see *Tool deferral history* below). No tool text kept. In Claude Code mode (#898) the feature also reads the text of a `ToolSearch` result, in two places: the "No matching deferred tools found" text, and the error text "not loaded" or "does not exist". It reads them in the request and keeps nothing of them.
off: ✅ Works for `remove`. `defer: auto` defers nothing when there is no durable place for the record: no session-state table and the file cannot be opened (a read-only folder, `--dashboard-db=:memory:`). A record that dies with the process would move tools inside live cached prefixes at the next start. With the Store off nothing is deferred.
metadata_only: 🟡 Works, metadata. The record, the flag and the history are tool names, ids, flags and counts.
everything: 🟡 Works, metadata. The same as metadata only: deferral keeps no content.
decided_by: Osher Elhadad, #84; deferral: David Amid, #485, 2026-11-02
order: 80
---

It removes the declarations the operator listed, from that fixed list and the current request. Deferral must send the same set on every turn of a thread, also across a restart, so it keeps the tool names it deferred on disk, and it decides from which tools the user declared and used before.
