---
name: agentdiet
kind: content
where: Store: the frozen decision and the original step
off: ❌ **Does not work.** **Pays one model call per turn.** The step it shortened last turn comes back in full, which **breaks the cache**.
metadata_only: ❌ **Does not work**: the same as Off. **Cannot work.**
everything: ✅ Works
decided_by: Itay Nakash, #66
order: 240
---

A model rewrites one older agent step each turn, and the words differ each time. The history stays short only if Context Guru keeps every rewrite.
