---
name: context-guru-setup
description: Set up context-guru for Codex CLI. Use when asked to install, enable, configure, or start context-guru for Codex.
---

# Set up context-guru for Codex

First run `python3 ../../scripts/codex_plugin.py setup --plan` from this skill directory. It writes
nothing. Read and relay its `consent_question=` as one explicit yes/no question. A missing or
ambiguous answer is no.

Explain that setup changes the default provider in `$CODEX_HOME/config.toml`, so every new Codex
session routes through the local proxy. Explain that the current session cannot change transport,
and name the standalone reset path printed by setup. Only after explicit consent, run
`python3 ../../scripts/codex_plugin.py setup --i-consent-to-traffic-interception` with
`sandbox_permissions="require_escalated"`, explaining that setup must write user configuration
and state, download a release when needed, start the local proxy, and verify loopback health. Do
not first run the mutating command in the sandbox: a denied write could leave a partial setup.

If the binary is missing, setup downloads the matching published release and verifies its SHA-256
checksum before installing it into plugin-owned state. On success, report the emitted `launch=`
command. Routing starts in the next ordinary `codex` process; it cannot change the transport of the
current session.
