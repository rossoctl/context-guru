---
name: Thread tracker
kind: metadata
off: 🟡 Works, metadata
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 150
---

It tells the main agent from its subagents. It uses hashes to check if a new request continues an earlier one. Since #947 it also marks the first request after the main agent's own compaction. The proxy sends the mark to the accountant library and stores it on the request row (`client_compaction`: `unknown` or empty), where the replay cap reads it. A request the tracker could not judge (no session id, no message list) stores no mark. It is one short label, with no text.
