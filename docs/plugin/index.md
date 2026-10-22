# Claude Code plugin

The plugin answers three questions without leaving the terminal: **how much am I saving, is my cache about to cost me, what should I do?** It is a status line that is always visible, and a short set of commands that each answer one question.

![The home screen](img/status-dark.svg#only-dark)
![The home screen](img/status-light.svg#only-light)

## Install

```
/plugin marketplace add rossoctl/context-guru
/plugin install context-guru@context-guru
/reload-plugins
/context-guru:install
```

The first two run once per machine; the last runs once per repo and decides which sessions are routed through the proxy. Full steps, scopes and the permission rule: [Install as a Claude Code plugin](../how-to/install-plugin.md).

The install also turns the [status line](statusline.md) on in the same settings file as the routing.

## Commands

One command per question. Each opens with one line that answers it, then a small picture; add `details` for the tables behind it.

| Command | The question it answers | Page |
|---|---|---|
| `/context-guru:status` | Is it working, how am I doing, what next? | [Home and status](status.md) |
| `/context-guru:insights` | Savings, cache and tools, one after another | [Insights](insights.md) |
| `/context-guru:insights savings` | How much did it save? | [Savings](savings.md) |
| `/context-guru:insights cache` | Is my cache about to cost me? | [Cache](cache.md) |
| `/context-guru:insights tools` | What do I carry and never use? | [Tools](waste.md) |
| `/context-guru:session` | What happened in this session? | [Session](session.md) |
| `/context-guru:doctor` | Is it set up right? | [Doctor](doctor.md) |
| `/context-guru:save-more` | Which config and keep-alive strategy would have cost least on my history? Change them, after a yes. | [Save more](save-more.md) |
| `/context-guru:remove-mcp-and-skills` | Which MCP servers and skills do I never use, and what does removing them save? | [Remove MCP servers and skills](remove-mcp-and-skills.md) |
| `/context-guru:cache-campaign` | Would a keep-alive campaign built from a tenant's history beat what it does today? (managers) | [Cache campaign](cache-campaign.md) |

Five more views draw in the terminal: `/context-guru:watch`, `timeline`, `chart`, `heatmap` and `fill`. They are on [Terminal views](terminal-views.md).

`/context-guru:statusline` still installs, changes and removes the status line; `/context-guru:doctor` checks that it is there.

## How the screens are written

- **Dollars first.** Every screen opens with a dollar figure, never a token count (ask for `details` if you want sizes).
- **Saved and Estimated are different things.** `✓ Saved` is what the proxy saved you, compared with the same requests without it, at your gateway's prices, net of its own spend. `≈ Estimated` is spend you could still avoid; those rows overlap, so they are not added together. Plain facts, straight from the provider's usage, are labelled `measured`.
- **Plain text for the six commands; colour for the views.** `status`, `insights`, `session` and `doctor` print plain Unicode with no colour codes, so they read the same on a light and a dark terminal. The five [terminal views](terminal-views.md) use colour (with `--theme light|dark`), and fall back to plain when output is not a terminal or `NO_COLOR` is set. The status line uses the basic terminal colours.

## Update

| What | How |
|---|---|
| The proxy | `/context-guru:update` (Claude Code checks in routed sessions and offers it) |
| The plugin | `/plugin marketplace update rossoctl/context-guru`, then `/reload-plugins` |

The proxy is released separately; updating the plugin does not update it. Details: [Updating](../how-to/install-plugin.md#upgrading).

## Uninstall

`/context-guru:uninstall` undoes one install: it removes the routing key it added (with a backup), restores any status line you had before, and stops the proxy. If a session cannot run commands because every request fails, use the recovery script in [The plugin's recovery files](../how-to/plugin-recovery-files.md).

## Codex (experimental)

Codex has the same skills under the same names, invoked by name: `$context-guru-status`, `$context-guru-insights`, `$context-guru-session`, `$context-guru-save-more`, `$context-guru-remove-mcp-and-skills`. They run the same scripts. Install, update and removal are in the [Codex plugin guide](https://github.com/rossoctl/context-guru/blob/main/codex-marketplace/plugins/context-guru/README.md).

| Capability | Claude Code | Codex |
|---|---|---|
| Commands | `/context-guru:*` | skills, by name |
| Status line from a command | yes (`statusLine` with `refreshInterval`) | no: `/statusline` only picks from built-in items such as context used and rate limits |
| Live cache clock and savings badge | yes | not available; ask `$context-guru-insights cache` instead |

## Next

[Status line](statusline.md) · [Configuration and troubleshooting](configuration.md)
