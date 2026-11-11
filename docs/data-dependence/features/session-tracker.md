---
name: Session tracker
kind: metadata
off: 🟡 Works, metadata
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 160
---

It needs a stable label for each conversation. When the client sends none, it hashes the start of the conversation. The Bob IDE sends its task id in a request header (`X-Task-Id`), and only the Bob model route reads it. That is metadata only, and it works at every storage level.
