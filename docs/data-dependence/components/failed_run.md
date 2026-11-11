---
name: failed_run
kind: content
where: Store: the frozen decision and the original
off: ❌ **Does not work.** Shortens once, then **breaks the cache**. No way back.
metadata_only: ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata, and the cache stays stable. No way back.
everything: ✅ Works, with `expand`
decided_by: Osher Elhadad, first commit a91d6f4
order: 100
---

It replaces a failed run that the agent ran again with a fixed sentence. A "replaced" flag is enough to send the same sentence again.
