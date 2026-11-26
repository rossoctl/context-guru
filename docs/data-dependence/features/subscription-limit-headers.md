---
name: **Subscription limit headers** (#984, #995, #1016, `GET /api/me/limits`)
kind: metadata
off: ❌ Off (it needs the dashboard). The proxy reads the headers and keeps nothing.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 250
---

To show how much of the 5-hour limit a session used, and later to learn how the limit counts each token kind, the proxy stores the used share, the reset time and the status word from the provider's response headers on each request row. Keep-alive pings are real requests too, so the proxy stores the same values on each ping row (#995). A summary or sweep call that takes the place of a ping counts as that ping (#1016). Only an answer sent with the user's own credential counts. Then the share stays current while the user is idle. It also stores one yes or no: did the client send a subscription (OAuth) token. It tests only the token's fixed prefix and never stores the token. These are numbers, short status words and a flag, not content.
