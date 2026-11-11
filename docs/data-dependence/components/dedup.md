---
name: dedup
kind: content
where: Store: the original
off: ✅ Works, no `expand`. Same bytes every turn. The note still says "expand", with nothing to expand.
metadata_only: ✅ Works, no `expand`
everything: ✅ Works
decided_by: Osher Elhadad, first commit a91d6f4
order: 160
---

It replaces a repeated tool output with a pointer to the earlier copy in the same request. It keeps the repeat only so the agent can get it back.
