---
name: context-guru-status
description: Check Codex default routing, context-guru proxy health, and measured statistics. Use when asked about status, health, savings, or troubleshooting.
---

# Check context-guru for Codex

If invoked with `--stats`, run `python3 ../../scripts/codex_plugin.py status --stats` and reply with
the file path it prints—no narration, interpretation, health summary, or additional formatting. The
raw `/stats` JSON is in that file, exactly as the proxy sent it; do not print the JSON itself. If
loopback access is blocked, retry that same read-only command with
`sandbox_permissions="require_escalated"`; never replace unavailable raw output with invented or
previously cached statistics.

Run `python3 ../../scripts/codex_plugin.py status` from this skill directory. Report the config,
port, proxy health, and whether default routing is configured. A healthy proxy alone does not
prove that this session uses it. Summarize `stats_json` without inventing savings.

Codex command sandboxes may block requests to the local `127.0.0.1` health endpoint. If the
command reports `proxy_up=false`, retry the same read-only command with
`sandbox_permissions="require_escalated"`, explaining that loopback access is needed to verify
proxy health. Use the retried result for the report. If that check cannot run, say proxy health
is unverified; the sandboxed result alone does not establish that the proxy is down.

After the health report, run `python3 ../../scripts/cg.py home` from this skill directory and print
its output exactly as it is in one code block: dollars saved, today against yesterday, and the top
three things to do. If loopback access is blocked, retry it with `sandbox_permissions="require_escalated"`.
