---
name: context-guru-onboarding
description: First-run guide for context-guru on Codex. Use when the plugin was just installed or the user asks how to start.
---

# Get started with context-guru on Codex

1. Run `python3 ../../scripts/codex_plugin.py doctor` (read-only; retry with
   `sandbox_permissions="require_escalated"` if loopback is blocked) and report the result in one
   line.
2. If `installed=FAIL`, follow the `context-guru-setup` skill. Tell the user what it does: it adds
   a `context-guru` profile and starts a local proxy; plain `codex` is never changed, so a stopped
   proxy cannot prevent Codex from starting.
3. Tell the user to exit and start routed sessions with the `launch=` path setup printed
   (`~/.local/state/context-guru-codex/context-guru-codex`, optionally aliased). If the proxy is
   down at launch it is restarted; if it cannot be, Codex starts unrouted with a warning.
4. State what to expect: the default reduction level is `off`, so requests are forwarded
   unchanged and savings appear only from cache keep-alive. Savings need a few routed sessions
   before `$context-guru-insights` has enough data; it refuses projections on short windows.
5. Mention that Codex has no status line for plugins; `$context-guru-status` and
   `$context-guru-insights` are the in-session views.
