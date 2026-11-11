---
name: context-guru-cache-strategy-picker
description: Inspect or change the Codex prompt-cache keep-alive strategy. Use when asked which strategy runs or to turn keep-alive pings on or off.
---

# Choose a cache strategy

Run `python3 ../../scripts/codex_plugin.py configure --show`. The proxy owns the list of strategies.
The output has a `strategies=` field with the name, spend flag and text of each choice for Codex,
and `cache_strategy=` with the one that runs. Read the choices and their texts from that output.
Do not use a list from memory. Explain each choice and ask before changing them. As of today they
are `30-min-ping`, which refreshes eligible OpenAI Responses caches shortly before their 30-minute
lifetime and spends a small amount of the user's own quota, and `none`, which sends no idle pings.
Then run `python3 ../../scripts/codex_plugin.py configure --cache-strategy NAME` and report whether
the proxy restarted successfully. Run the change with
`sandbox_permissions="require_escalated"`, explaining that it writes the user's context-guru
configuration and restarts the local proxy. Do not offer Anthropic's `auto` (formerly `5-min-ping`), `1-ping`, `2-ping` or
`1-hour-head`; those are different provider semantics. If the output says the proxy cannot list
cache strategies, tell the user to update the proxy to v0.7.1 or later (`context-guru-update`).
