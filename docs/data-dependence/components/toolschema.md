---
name: toolschema
kind: content
where: Memory: trimmed tool declarations, for the whole process, with no size limit
off: ✅ Works. **Today the cache of trimmed declarations stays on.** It should be off.
metadata_only: ✅ Works without the cache. Only speed changes.
everything: ✅ Works
decided_by: Osher Elhadad, #84
order: 30
---

The agent sends the same tool list every turn, so it keeps the trimmed list to save work. The trim always gives the same result, so the cache only adds speed.
