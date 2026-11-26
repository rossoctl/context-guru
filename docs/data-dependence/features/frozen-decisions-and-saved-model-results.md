---
name: Frozen decisions and saved model results
kind: content
off: ❌ Gone
metadata_only: ❌ Gone. The rule-based components can use a flag instead.
everything: ✅ Works. Kept for about 2.8 hours in memory. **With the dashboard on (`--session-state`, on by default): kept 24 hours in memory and also written to the dashboard database, then loaded back when the session returns.**
order: 20
---

After Context Guru shortens a cached message, it must send the same text every later turn. Otherwise the user pays to write the cache again.
