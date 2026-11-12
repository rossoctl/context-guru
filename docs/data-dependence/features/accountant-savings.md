---
name: Accountant savings (per-component figures from the accountant library)
kind: metadata
off: ❌ Off. It needs the dashboard, because the figure is attached to a dashboard event.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 180
---

To price a saving it must know which part of the conversation a rewrite changed and whether the provider had that part cached. It reads each request once and keeps only a hash and a size for each part, plus the cache breakpoints. The text is dropped as soon as it is hashed, and the hashes are held in memory, one set per conversation thread, plus each thread's last set for the sibling check (SharedWarm). So a restart does not lose them, it also saves them to one file beside the dashboard database (`<dashboard-db>.accountant-state.json`, readable by its owner only), every minute and when it stops, and loads them again when it starts. The file holds hashes, sizes, positions, and the ids and names the state needs (session and thread ids, tool and component names, model names, times). It holds no message text. It also holds one proxy version number, and it is ignored when that number differs or when it is more than one hour old. It is not written when there is no dashboard. It reads the tools and system sections the same way, as names, hashes and sizes, to report the envelope changes. The result it writes to the dashboard is dollar amounts for each component, with their split by tier. Since #998 it also writes the same figures as token counts by tier (fresh input, cache read, 5-minute and 1-hour cache write), saved and caused: per component row, and per request for the keep-alive rescue, the cache split, the 1-hour head and the ended-change break. They are numbers only. `/api/stats` sums them in its `tokens` object. No level changes what it does. Decided by: David Amid, #998.
