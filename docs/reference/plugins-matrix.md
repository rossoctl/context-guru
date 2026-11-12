# Plugins, providers and settings

This page lists what each plugin offers for each provider. Use it to see, at a glance, which
cache strategy and which preset a user can pick. The cache-strategy tables are checked by a test
(`config/pluginsmatrix_test.go`). The other tables are checked by hand.

The state below holds once PR #1115 (strategy tags) and PR #1113 (Pi reads the table from the
proxy) are merged.

## The proxy's strategy table

The proxy owns the table (`config/strategy.go`, `Strategies`). Each row has a `Providers` tag.
The tag says which provider the strategy is meant for. The proxy does not enforce it. Each plugin
applies its own filter to the tag.

| Strategy | Providers | Spends |
|---|---|---|
| `none` | any | no |
| `auto` | anthropic, openai | yes |
| `1-ping` | anthropic, openai | yes |
| `2-ping` | anthropic, openai | yes |
| `1-hour-head` | anthropic | no |
| `30-min-ping` | openai | yes |

Tag values: `anthropic`, `openai` (the OpenAI Responses route), `openai-chat` (the OpenAI Chat
Completions route) and `any`. No row uses `openai-chat` yet. The proxy default is `auto`.

## Cache strategies each plugin offers

"Filter" is the rule the plugin applies to the `Providers` tag. The test copies each rule in Go.

| Plugin (provider) | Filter on `Providers` | Offers | Default |
|---|---|---|---|
| Claude Code (Anthropic) | has `anthropic` or `any` | `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head` | `auto` |
| Codex (OpenAI Responses) | not empty, and every tag is `openai` or `any` | `none`, `30-min-ping` | `30-min-ping` |
| OpenCode (Anthropic) | has `any` or `anthropic` | `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head` | `auto` |
| OpenCode (OpenAI) | has `any` or `openai` | `none`, `auto`, `1-ping`, `2-ping`, `30-min-ping` | `auto` |
| OpenCode (Anthropic and OpenAI) | has `any`, `anthropic` or `openai` | `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head`, `30-min-ping` | `auto` |
| Pi (Anthropic and OpenAI) | has `any`, or has both `anthropic` and `openai` | `none`, `auto`, `1-ping`, `2-ping` | `auto` |
| IBM Bob (OpenAI Chat Completions) | has `openai-chat` or `any` | `none` | `none` |

Notes:

- **OpenCode** routes the providers that the user configures. The offer follows the routed
  providers. A name that is not offered gives `none` and a reason.
- **Pi** uses one strategy for both providers. It offers only a row that serves both.
- **Bob** has no row for `openai-chat`, so only `none` fits. The plugin writes no strategy into its
  config, so it can apply only `none`.
- **Codex** keeps its own default. Its filter takes only a row whose tags are all `openai` or `any`. The proxy default (`auto`) also lists `anthropic`, so Codex does not offer it.
- **Claude Code** (`PLUGIN_PROVIDERS` in `context-guru-plugin/scripts/settings.py`) offers
  `1-hour-head` only for Anthropic.

## Presets

All plugins except Bob use the same picker ladder: `off`, `conservative`, `medium`, `high`,
`xhigh`, `ultrahigh`. See [Choose a preset](presets.md) for what each one runs. The ladder is a
plugin choice. The proxy registers more presets.

| Plugin | Presets offered | Default preset |
|---|---|---|
| Claude Code | `off`, `conservative`, `medium`, `high`, `xhigh`, `ultrahigh` | `off` |
| Codex | same ladder | `off` |
| OpenCode | same ladder | `off` |
| Pi | same ladder | `off` |
| IBM Bob | `off`, `lossless`, `bob-ultrahigh` | `bob-ultrahigh` |

`bob-ultrahigh` is ultrahigh with four Bob changes. `lossless` is not a registered proxy preset.
Bob writes its own pipeline body.

## Proxy, route and minimum version

| Plugin | Proxy route | Minimum proxy version | Plugin version file |
|---|---|---|---|
| Claude Code | `POST /anthropic/v1/messages` | v0.7.1 (`STRATEGY_MIN_PROXY`, `settings.py`) | `context-guru-plugin/.claude-plugin/plugin.json`, `.claude-plugin/marketplace.json` |
| Codex | `POST /openai/v1/responses` | v0.7.1 for strategies (`STRATEGIES_RELEASE`); v0.4.0 for the plugin (`MINIMUM_CODEX_RELEASE`) | `codex-marketplace/plugins/context-guru/.codex-plugin/plugin.json`, `.agents/plugins/marketplace.json` |
| OpenCode | `/anthropic/v1` and `/openai/v1`, per routed provider | v0.7.1 for strategies. The plugin downloads `PINNED_VERSION` | `PINNED_VERSION` in `opencode-plugin/index.js` (no `package.json`) |
| Pi | `/anthropic` and `/openai/v1` | v0.7.1 (`STRATEGY_MIN_PROXY`, `pi_plugin.py`); v0.4.0 for the package (`MINIMUM_RELEASE`) | `pi-package/package.json` |
| IBM Bob | `POST /inference/v1/chat/completions` | v0.7.1 for the plugin (`MINIMUM_BOB_RELEASE`); v0.7.2 for `bob-ultrahigh` | `bob-plugin/context-guru/plugin.json` |

## How the user changes each setting

| Plugin | Preset | Cache strategy |
|---|---|---|
| Claude Code | skill `preset-picker` | skill `cache-strategy-picker` |
| Codex | skill `preset-picker` | skill `cache-strategy-picker` |
| OpenCode | `/context-guru-preset` (chat) or the `ctrl+p` palette | `/context-guru-strategy` (chat) or the `ctrl+p` palette |
| Pi | `/context-guru preset [name]` | `/context-guru cache-strategy [name]` |
| IBM Bob | `bob_plugin.py configure --preset <name>` or skill `preset-picker` | skill `cache-strategy-picker` (lists `none` only) |

OpenCode also reads the `preset` and `cacheStrategy` plugin options. A saved choice wins over the
options.

## Keep-alive on each provider

A keep-alive ping keeps the prompt cache warm while the user is away. Which pings run depends on
the strategy and the provider.

| Provider | Ping timing | Source |
|---|---|---|
| Anthropic | 5-minute cache: first ping at 240 s. 1-hour cache: first ping at 59 minutes. Number of pings from the strategy | `auto`, `1-ping`, `2-ping` in `config/strategy.go` |
| OpenAI Responses | **28 minutes** (the 30-minute minimum cache lifetime minus 2 minutes) | `OpenAIDefaultKeepAliveIdle` in `internal/modelinfo/cachefrac.go` |
| OpenAI Chat Completions (Bob) | **No pings.** The proxy pings only Anthropic and OpenAI Responses caches | `NO_KEEPALIVE_REASON` in `bob_plugin.py` |

For Anthropic, the prefix floor depends on the wait cause: 20k tokens for a tool wait, background
work, a teammate or a subagent, and 80k for a main thread that waits for a person or for an unknown
cause. The OpenAI ping uses the same policy object. This page does not claim that the floors apply
the same way on OpenAI. Check `proxy/keepalive.go` before you rely on it.

## How to keep it current

1. Change a `Providers` tag or a row in `config/strategy.go`: update the first two tables.
2. Change a plugin filter: update the "Cache strategies" table and the Go copy of the filter in
   `config/pluginsmatrix_test.go`.
3. Add a plugin or a provider route: add a row to each table and a filter to the test.
4. Run `go test ./config/ -run TestPluginsMatrix`. The test fails and names this page when the
   strategy tables and the code disagree.

What the test does **not** check:

- It cannot read the Python and JavaScript filters. It checks the Go copy of each rule against
  the table. When a plugin changes its filter, change the Go copy as well.
- Presets, routes, versions, commands and keep-alive timing are checked by hand.
