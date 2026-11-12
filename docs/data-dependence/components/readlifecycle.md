---
name: readlifecycle
kind: content
where: Store: the frozen decision and the original file read
off: ❌ **Does not work.** Removes an old read once, then **breaks the cache**. No way back.
metadata_only: ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata if it also keeps its label ("stale" or "superseded"). No way back.
everything: ✅ Works, with `expand`
decided_by: Osher Elhadad, #89
order: 120
---

It removes file reads that a later edit or read made out of date. The label can change later in the conversation, so it must keep the label it first used.
