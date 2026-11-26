---
name: cache_aware_summarizer
kind: content
where: Store: the summary and the replaced turns. Memory: copies of turns, for summaries during idle time, after a cold turn, and for one request's lifetime while a summary waits for that request's body to be stashed (#722).
off: ❌ **Does not work.** **Does not start** by default, because it cannot tell how full the window is.
metadata_only: ❌ **Does not work.** It keeps copies of turns in memory, which is content. On Anthropic it does nothing and costs nothing. On OpenAI it **pays and throws the summary away** every turn. **Cannot work.**
everything: ✅ Works
decided_by: Itay Nakash, #256
order: 230
---

Like `summarize`, it must keep the summary to send the same bytes later. It also keeps the conversation in memory, to summarize while the user is idle.
