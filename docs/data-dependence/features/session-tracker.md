---
name: Session tracker
kind: metadata
off: 🟡 Works, metadata
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 160
---

It needs a stable label for each conversation. When the client sends none, it hashes the start of the conversation. The Bob IDE sends its task id in a request header (`X-Task-Id`), and only the Bob model route reads it. Codex sends its own session id in a request header (`session-id`), and only a caller that says it is Codex reads it. That keeps one Codex session whole after a compaction. That is metadata only, and it works at every storage level.
