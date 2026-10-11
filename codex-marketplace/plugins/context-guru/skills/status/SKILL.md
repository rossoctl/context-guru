---
name: context-guru-status
description: Check Codex profile routing, context-guru proxy health, and measured statistics. Use when asked about status, health, savings, or troubleshooting.
---

# Check context-guru for Codex

If invoked with `--stats`, run `python3 ../../scripts/codex_plugin.py status --stats` and print its
stdout verbatim—no narration, interpretation, health summary, or additional formatting. If
loopback access is blocked, retry that same read-only command with
`sandbox_permissions="require_escalated"`; never replace unavailable raw output with invented or
previously cached statistics.

Run `python3 ../../scripts/codex_plugin.py status` from this skill directory. Report the config,
port, proxy health, and whether the profile is configured. A healthy proxy alone does not
prove that this session uses it. Summarize `stats_json` without inventing savings.

Codex command sandboxes may block requests to the local `127.0.0.1` health endpoint. If the
command reports `proxy_up=false`, retry the same read-only command with
`sandbox_permissions="require_escalated"`, explaining that loopback access is needed to verify
proxy health. Use the retried result for the report. If that check cannot run, say proxy health
is unverified; the sandboxed result alone does not establish that the proxy is down.
