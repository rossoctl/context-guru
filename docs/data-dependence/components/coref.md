---
name: coref
kind: content
where: Store: the frozen decision, the original, and a count of cache rebuilds it spent (metadata)
off: ❌ **Does not work.** **No spending limit**, because the count always reads zero. Each cut lasts one turn, then **breaks the cache**.
metadata_only: ❌ **Does not work today, and is harmful.** The limit works, but each cut undoes itself next turn. **Turn it off.** **With a change:** 🟡 it works on metadata.
everything: ✅ Works, with `expand`
decided_by: David Amid, commit b224704; docs/proposals/coref-compaction.md
order: 190
---

It removes tool outputs that nothing later refers to, and so changes the cached history on purpose. It must remember which outputs it cut, and how many cache rebuilds it spent.
