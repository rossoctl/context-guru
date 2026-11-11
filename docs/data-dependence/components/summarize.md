---
name: summarize
kind: content
where: Store: the model-written summary and the replaced turns. Memory: a copy of the turns while the model call runs.
off: ❌ **Does not work.** **Does not start** by default, because it cannot tell how full the window is. Without a session (`/compact`) it works in one request. No way back.
metadata_only: ❌ **Does not work.** **Pays for a summary every turn above 90% full, and throws it away.** The next turn can wait up to 120 seconds for it. **Cannot work.**
everything: ✅ Works
decided_by: Osher Elhadad, commit 68e8b51; checkpoint in #430
order: 220
---

It replaces old turns with a model-written summary, and the words differ each time. It must keep the summary to send the same bytes next turn.
