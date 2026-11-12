---
name: mask
kind: content
where: Store: the frozen decision and the original
off: ❌ **Does not work.** Masks a message for one turn, then **breaks the cache**. No way back.
metadata_only: ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata, and the cache stays stable. No way back.
everything: ✅ Works, with `expand`
decided_by: Osher Elhadad, first commit a91d6f4; storage rules in #187
order: 90
---

It hides old tool outputs, and must send the same masked text every later turn. It builds that text from the message alone, so a "masked" flag is enough to build it again.
