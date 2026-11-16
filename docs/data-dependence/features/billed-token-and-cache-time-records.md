---
name: Billed-token and cache-time records
kind: metadata
off: ❌ Gone. `summarize` cannot tell how full the window is. A warm cache can look cold when one conversation comes under two ids.
metadata_only: 🟡 Works, metadata. This record also makes `summarize` start, and so start to waste calls.
everything: 🟡 Works, metadata
order: 30
---

To know how full the window is, Context Guru needs the size the provider billed last turn. It also needs the time the cache was last used. The billed size is kept per session and per thread (one integer each). The per-thread one lets `coref` measure how fast one thread grows. Bob's model route also reports its cache writes, and the 1-hour part of them, in its OpenAI-style usage block. Only that route reads these counts. They are metadata, so this works the same at every storage level. Bob's model route has at least 2 backends that count tokens differently, so on that route only, the proxy also writes a backend label on each request row (`backend`: `A`, `B`, `other` or empty) and counts the labels in `/stats` (`bob_backend`). It makes the label from whether the reply id starts with `chatcmpl-`, from which field names the usage block has, and from the reply's model name (Bob's helper calls, `security` and `router`, get no label). It keeps only the label, never the id. When the label is `other`, it logs one warning per new kind of shape with the first 4 characters of the id, its length and 5 yes/no flags. The label is metadata, so it works the same at every storage level. With the dashboard off, only the `/stats` counts exist.
