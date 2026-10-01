---
name: context-guru-status
description: Check the context-guru Codex profile, proxy health, routing, and measured statistics. Use when asked about status, health, savings, or troubleshooting.
---

# Check context-guru for Codex

Run `python3 ../../scripts/codex_plugin.py status` from this skill directory. Report the profile,
port, proxy health, and whether this process is routed through it. A healthy proxy alone does not
prove that this session uses it. Summarize `stats_json` without inventing savings.
