---
name: Costly settings check (`/context-guru:save-more settings`, `GET /api/adapt/settings`)
kind: metadata
off: ❌ Off (the cost needs the dashboard). The plugin still finds the settings and says "cost not measured yet".
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 200
---

It counts and prices dashboard rows by request shape: model name, tool count, `max_tokens` and the stream flag. One check also uses the upstream status (400) of a Haiku row in the same session. It reads no request text and keeps nothing new. The plugin reads only the named keys of the user's own settings files (the betas setting, the recap setting, `ANTHROPIC_SMALL_FAST_MODEL` and `ANTHROPIC_DEFAULT_HAIKU_MODEL`) on the user's machine, and sends none of them to the proxy (#615).
