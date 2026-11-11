---
name: **Body stash**
kind: content
off: ❌ **Should be off. Today it is still on**, with 2 full bodies per thread.
metadata_only: ❌ Off. The sweep's question then cannot be asked, and `cache_aware_summarizer` stops.
everything: ✅ Works
order: 80
---

To ask the model a cheap question, Context Guru adds the question to the exact text it sent last turn. The provider already cached that text. The sweep keeps **2 more full copies of the conversation per thread**, outside the stash, for a thread the keep-alive can ping (see `extract_llm_sweep` above).
