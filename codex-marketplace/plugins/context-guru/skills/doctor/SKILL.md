---
name: context-guru-doctor
description: Diagnose why Codex is not routed through context-guru or why the proxy is unhealthy. Use for "is it working", "why no savings", or after any proxy error.
---

# Diagnose context-guru for Codex

Run `python3 ../../scripts/codex_plugin.py doctor` from this skill directory. It is read-only and
prints one `name=ok` or `name=FAIL fix=<command>` line per check. Relay every FAIL with its exact
fix and nothing else. `session_routed=FAIL` only means this session was not started with the
`context-guru-codex` launcher; it is not an error for a plain `codex` session.

Codex command sandboxes may block loopback. If `proxy_healthy=FAIL`, retry the same read-only
command with `sandbox_permissions="require_escalated"`, explaining that loopback access is needed.
If that retry cannot run, report proxy health as unverified rather than down.
