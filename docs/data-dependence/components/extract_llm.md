---
name: extract_llm
kind: content
where: Store: the model's rewrite, a copy for other sessions, and the original. Memory: counts per session, and the time of the last stash refusal.
off: ❌ **Does not work.** Pays one model call per new large output, and uses it for one turn. Then it **breaks the cache**. Without prompt caching it pays again every turn, up to 8 calls.
metadata_only: ❌ **Does not work**: the same as Off, and it approves **more** calls. It remembers an output, and counts on a reuse that cannot happen. **Cannot work.**
everything: ✅ Works: one call, then free reuse
decided_by: Osher Elhadad, #51; storage rules in #40 and #188
order: 200
---

A model rewrites each large tool output, and the words differ each time. To send the same bytes next turn, it must keep the rewrite, which is content.
