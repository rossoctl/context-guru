---
name: cacheinject
kind: metadata
where: Store: one short hash per message of the last turn
off: ✅ Works, with less. It puts cache bookmarks only at fixed positions, so an edited earlier message costs the full price.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
decided_by: Osher Elhadad, first commit a91d6f4; budget in #32
order: 50
---

It puts a cache bookmark just before the first message that changed. To find that message, it remembers a short hash of each message from the last turn, never the text.
