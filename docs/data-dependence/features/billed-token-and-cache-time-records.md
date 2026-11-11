---
name: Billed-token and cache-time records
kind: metadata
off: ❌ Gone. `summarize` cannot tell how full the window is. A warm cache can look cold when one conversation comes under two ids.
metadata_only: 🟡 Works, metadata. This record also makes `summarize` start, and so start to waste calls.
everything: 🟡 Works, metadata
order: 30
---

To know how full the window is, Context Guru needs the size the provider billed last turn. It also needs the time the cache was last used. The billed size is kept per session and per thread (one integer each). The per-thread one lets `coref` measure how fast one thread grows. Bob's model route also reports its cache writes, and the 1-hour part of them, in its OpenAI-style usage block. Only that route reads these counts. They are metadata, so this works the same at every storage level.
