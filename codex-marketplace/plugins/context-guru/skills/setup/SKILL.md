---
name: context-guru-setup
description: Set up context-guru for Codex CLI. Use when asked to install, enable, configure, or start context-guru for Codex.
---

# Set up context-guru for Codex

Explain that this routes model traffic (prompts, source and tool output) through a loopback proxy,
and obtain explicit consent before changing configuration.

Run `python3 ../../scripts/codex_plugin.py setup` from this skill directory. It creates a dedicated
`$CODEX_HOME/context-guru.config.toml` profile and does not edit the main Codex configuration.

If the binary is missing, ask the user to install `context-guru-proxy` on PATH or build this checkout
with `make build`. On success, report the emitted `launch=` command. Routing starts in a new Codex
process; it cannot change the transport of the current session.
