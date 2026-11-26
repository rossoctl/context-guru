---
name: **Subscription headline** (dashboard Overview, #985)
kind: metadata
off: ❌ Off (it needs the dashboard)
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 140
---

On a Claude subscription the Overview shows every saving in tokens, not dollars, and an estimate of the 5-hour limit share. It reads the auth kind and the latest rate-limit numbers the proxy kept for the account (#984), and the token counts by tier and the cost totals the dashboard already has (#998). Since #1023 the Sessions tab shows the same token counts per session, summed from the same stored numbers. Since #1042 and #1048 the Cache strategy tab and its tables show them too. It keeps nothing new. Decided by: David Amid, #985 (2026-11-09) and #998.
