# Pi package and Claude Code plugin compared

Context Guru has a client plugin for Claude Code and a package for [Pi](https://pi.dev). Both
install the same local proxy and route the client's model traffic through it. They differ in how
they route, which providers they cover and what they show you.

- Claude Code plugin: [`context-guru-plugin/`](https://github.com/rossoctl/context-guru/tree/main/context-guru-plugin),
  documented in [Claude Code plugin](../plugin/index.md).
- Pi package: [`pi-package/`](https://github.com/rossoctl/context-guru/tree/main/pi-package),
  documented in its [README](https://github.com/rossoctl/context-guru/blob/main/pi-package/README.md).

State on 2026-11-10: Claude Code plugin 0.6.7, Pi package as merged in #977 and #982. Rows marked
**(open PR)** describe a change that is not merged yet.

## What is the same

- **One proxy binary**, `context-guru-proxy`, with the same presets: `off`, `conservative`, `medium`,
  `high`, `xhigh`, `ultrahigh`.
- **The same cache-strategy names do the same thing.** A Pi test fails when a shared strategy sets
  different cache keys from the Claude Code plugin.
- **One consent question before any change.** Setup asks once and changes nothing without a yes.
- **A reset script outside the plugin**, written in plain `sh`, for when routing breaks and no
  command can run.

## Comparison

| Area | Claude Code plugin | Pi package |
|---|---|---|
| **How traffic is routed** | `ANTHROPIC_BASE_URL` in a Claude Code settings file | `baseUrl` per provider in `~/.pi/agent/models.json` |
| **Providers** | Anthropic | Anthropic and OpenAI. `openai-codex` (ChatGPT backend) is not routed |
| **Scope** | One project by default, or the whole machine with `--global`. One port per project | Always the whole machine: one proxy for all Pi sessions, because `models.json` is per user |
| **How the proxy starts** | The `SessionStart` hook starts it. The `UserPromptSubmit` hook restarts it when it is down | A user service (launchd on macOS, systemd on Linux) starts it at login and restarts it after a crash |
| **Check from inside a session** | Every session start and every prompt | **(open PR)** At session start: a health check with a 5-second limit, in the background. If the proxy does not answer, one service restart, then one notice |
| **After a laptop restart** | The next Claude Code session starts the proxy | The service starts the proxy at login, before Pi opens |
| **Existing base URL** | Asks: chain behind it, or replace it | Today: refuses and changes nothing. **(open PR)** Asks: chain, replace, or no change |
| **Session id** | Claude Code sends it | The extension adds the `x-context-guru-session` header |
| **Lifecycle signals** | Session start, session end, notification | Session end and compaction |
| **Client's own cache warming** | Not applicable | Stopped for routed models, so only the proxy pings |
| **Cache strategies** | `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head` | `none`, `auto`, `1-ping`, `2-ping` |
| **Commands** | 19 skills: install, status, preset and strategy pickers, insights, save-more, remove-mcp-and-skills, cache-campaign, doctor, session, update, uninstall, terminal views | One command, `/context-guru`, with `setup`, `status`, `preset`, `cache-strategy`, `update`, `uninstall` |
| **What you see in a session** | The band above the prompt and a pane: next-message price, Ping, savings, sub-agent caches, tips. An optional status line | Nothing in the session. `status` prints a text summary and the dashboard link |
| **Manual ping, beep, update check at session start** | Yes | No |
| **Dashboard** | `http://127.0.0.1:<port>/dashboard/` | The same, on the Pi proxy's port |
| **Tests** | About 25 Go test files, plus Python and TSX tests | Python and Node tests, run through one Go wrapper |

## Gaps in the Pi package

In order of value:

1. **A status widget in Pi**, like the band: savings and the next-message price.
2. **Insights and save-more.** The Pi proxy serves the same `/stats`, so the Claude Code plugin's
   reports could read it.
3. **Per-project scope.** Pi has no per-project `models.json`, so this needs another way to route.

Chain mode and the session-start check were gaps too. Each one has an open PR.
