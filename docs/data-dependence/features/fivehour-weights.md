---
name: **Learned 5-hour weights** (#1047, `GET /api/me/fivehour-weights`)
kind: metadata
off: ❌ Off (it needs the dashboard). The Overview estimate and the cache strategy forecast stay at API prices.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 255
---

On a Claude subscription, the dashboard learns how much of the 5-hour limit one token of each kind uses. It reads only numbers already stored on each request row: the 5-hour share and reset time from the limit headers (#984), the billed token counts by kind, the time, and the model name. It stores nothing new: it holds the fit in memory for 15 minutes. With enough data, the Overview's 5-hour estimate and the Cache strategy forecast use these weights in place of API prices. Decided by: David Amid, #1047 (2026-11-11).
