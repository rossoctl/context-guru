---
name: context-guru-cache-strategy-picker
description: Inspect or change the Codex prompt-cache keep-alive strategy.
---

# Choose a cache strategy

Run `python3 ../../scripts/codex_plugin.py configure --show`. Explain both choices and ask before
changing them: `30-min-ping` refreshes eligible OpenAI Responses caches shortly before their
30-minute lifetime and spends a small amount of the user's own quota; `none` sends no idle pings.
Then run `python3 ../../scripts/codex_plugin.py configure --cache-strategy NAME` and report whether
the proxy restarted successfully. Run the change with
`sandbox_permissions="require_escalated"`, explaining that it writes the user's context-guru
configuration and restarts the local proxy. Do not offer Anthropic's `auto` (formerly `5-min-ping`), `1-ping`, `2-ping` or
`1-hour-head`; those are different provider semantics.
