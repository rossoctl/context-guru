---
name: **Session state table** (`--session-state`, #769; on with `--dashboard`)
kind: content and metadata
off: ❌ Off. Dashboard off, `--session-state=false`, or store off: nothing is written.
metadata_only: 🟡 Works, metadata: only the keys of counts, hashes, ids and verdicts are written (`cg:bin:`, `cg:bint:`, `cg:len:`, `cg:ttl:`, `cg:seen:`, `cg:swd:`, `cg:swk:`, `cg:swx:`, `cg:rst:`, `cg:use:`, `cg:tdf:`). Set `--session-state-metadata-only`. The table is in the dashboard database.
everything: ✅ Works. **Also writes the content keys to the dashboard database**: the summary text (`cg:sum:`), `extract_llm` results (`cg:res:`, `cg:xres:`), freeze decisions (`cg:frz:`) and the sweep's held answer (`cg:swh:`). The rewind payloads (the stash) are never written.
order: 40
---

A restart, or a pause of more than 2.8 hours, deletes the summary checkpoint and the last billed input from memory. The next turn then sends the full transcript, which can be over the window, and the summary cannot start. A table lets the proxy give a returning session the same state it had. The table is **not encrypted**. It has its own retention, not the dashboard's. A row lives 30 days after its last write. An eviction or an archive can end it earlier, when no other session uses its key. Decided by: David Amid, 2026-10-29 (the dashboard database, on with the dashboard). David Amid confirmed the data-dependence change for `extract_llm_sweep`. Rows to confirm: `cache_aware_summarizer`, `summarize` (Itay Nakash).
