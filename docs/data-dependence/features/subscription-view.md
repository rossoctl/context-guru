---
name: Subscription view of `/context-guru:status` and `/context-guru:insights savings` (#986)
kind: metadata
off: ❌ Off (the saved tokens need the dashboard). The screen says there is no data yet, as today.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 210
---

On a Claude subscription the screens show everything in tokens, not dollars. They read the token counts by tier the dashboard already keeps (`tokens` in `/api/stats`, #998), the dollar totals for the 5-hour estimate, and the 5-hour usage share from the limit headers of the last response (`GET /api/me/limits`, #984). These are numbers only. They read no request text and keep nothing new.
