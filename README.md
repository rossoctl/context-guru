<div align="center">

<img src="docs/img/context-guru.png" alt="context-guru" width="320" />

# context-guru

**Provider-agnostic context engineering for LLM agents.**

[![Docs](https://img.shields.io/badge/docs-online-009688.svg)](https://rossoctl.github.io/context-guru/)
[![Go Reference](https://img.shields.io/badge/pkg.go.dev-reference-007d9c.svg)](https://pkg.go.dev/github.com/rossoctl/context-guru)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)

</div>

---

context-guru cuts the token cost of your agent's traffic in two ways: **carry less context**
(drop redundant tool output, collapse superseded runs, summarize before you hit the limit), and
**pay less for what you still carry** (keep your prompt cache warm, split the volatile tail off
the system prompt so the rest stays cacheable). Paying less is the **default** — it's on out of
the box, before you opt into anything that trims content.

Full docs: **[rossoctl.github.io/context-guru](https://rossoctl.github.io/context-guru/)**.

<p align="center">
<img src="docs/img/context_guru_stats_sqaure.png" alt="context-guru saves 5–15% of your API cost in four ways" width="720" />
</p>

## Install

Choose where context-guru runs, then choose your agent. Only the instructions you expand are shown.

<details open>
<summary><strong>Personal use</strong> — run context-guru locally for your own sessions</summary>

<details>
<summary><strong>Claude Code</strong></summary>

In Claude Code:

```text
/plugin marketplace add rossoctl/context-guru
/plugin install context-guru@context-guru
/reload-plugins
/permissions
```

Add this local project permission, replacing `{you}` with your username:

```text
Bash(/Users/{you}/.claude/plugins/cache/context-guru/**)
```

Then install and verify:

```text
/context-guru:install
/context-guru:status
```

The status line is enabled by default. Toggle it with `/context-guru:statusline off` or
`/context-guru:statusline on`.

[Detailed Claude Code installation and troubleshooting](docs/how-to/install-plugin.md)

</details>

<details>
<summary><strong>Codex</strong></summary>

In a terminal:

```sh
git clone --depth 1 https://github.com/rossoctl/context-guru.git
make -C context-guru build
codex plugin marketplace add ./context-guru/codex-marketplace
codex plugin add context-guru@context-guru
```

Start Codex, ask it to **set up context-guru**, and approve routing through the local proxy. Then
start routed sessions with:

```sh
codex -p context-guru
```

The plugin uses a separate profile and leaves your main Codex configuration unchanged. Its
standalone escape hatch is `~/.local/state/context-guru-codex/context-guru-reset`.

Codex Responses traffic is currently observable passthrough; native Responses context trimming is
not yet enabled.

[Codex plugin details](codex-marketplace/plugins/context-guru/README.md)

</details>

</details>

<details>
<summary><strong>Enterprise use</strong> — connect to an organization-hosted context-guru</summary>

Ask your operator for the service URL, your `cg_live_...` token, and the organization CA if the
service uses private TLS. Your provider API key stays in its existing variable; the context-guru
token is a separate header.

<details>
<summary><strong>Claude Code</strong></summary>

```sh
export ANTHROPIC_BASE_URL=https://cg.example.com/anthropic
export ANTHROPIC_CUSTOM_HEADERS="x-context-guru-token: cg_live_xxxxxxxx"
claude
```

Confirm that your dashboard request count increases. A value in Claude Code's `settings.json` can
silently override the exported base URL.

[Enterprise Claude Code connection and verification](docs/hosted.md#user-setup)

</details>

<details>
<summary><strong>Codex</strong></summary>

Keep the context-guru token out of `config.toml` by exporting it:

```sh
export CONTEXT_GURU_TOKEN=cg_live_xxxxxxxx
```

Add a dedicated provider to `~/.codex/config.toml`:

```toml
[model_providers.context-guru-enterprise]
name = "context-guru enterprise"
base_url = "https://cg.example.com/openai/v1"
wire_api = "responses"
requires_openai_auth = true
env_http_headers = { "x-context-guru-token" = "CONTEXT_GURU_TOKEN" }
```

Then run `codex -c model_provider=context-guru-enterprise`. Confirm that your dashboard request
count increases.

[Enterprise deployment and connection guide](docs/hosted.md#user-setup)

</details>

Operators: [deploy and administer the hosted service](docs/hosted.md).

</details>

For Claude Code, `/context-guru:status` reports the active preset, cache strategy, and measured
savings. `/context-guru:status --stats` prints the raw `/stats` response. To opt into content
trimming, run `/context-guru:preset-picker` and start with `conservative`.

## Presets

Change what gets trimmed with:

```
/context-guru:preset-picker
```

It's an effort ladder — each tier is everything in the one before it, plus more:

| Preset | What it adds | Spends on its own |
|---|---|---|
| `off` | nothing — requests forwarded untouched (the default; only keep-alive spends, if that's on) | no |
| `conservative` | deterministic trimming of tool output (repeats, dead runs) — no model calls | no |
| `medium` | `conservative` plus a cheap model that keeps only what looks relevant in recent tool output | yes |
| `high` | `medium` plus a summarizer that compacts older turns once the context window is nearly full | yes |
| `xhigh` | `high` plus a periodic deep-adjudication sweep over turns whose prompt cache has gone cold — the deepest cut | yes |

Run the picker above any time to switch tiers — it explains each one before asking which to set.
A restart is required — it takes effect at the next proxy start, not the current session; run
`/context-guru:status` afterward to confirm it stuck. Full pipelines:
[docs/reference/presets.md](docs/reference/presets.md).

Everything else — architecture, the full benchmark, every component, the proxy/gateway path,
config reference — is in **[docs/design.md](docs/design.md)** and
**[docs/get-started/how-it-saves.md](docs/get-started/how-it-saves.md)**.

## Updating

The proxy **binary** is what matters and what changes often. A routed session checks for a newer
release every 5 minutes and tells you once, with three answers: update now, always update
automatically, or do nothing for that release. Update it any time with:

```
/context-guru:update
```

**The plugin itself** (skills, hooks, scripts) rarely needs updating — most releases only touch the
proxy binary, which `/context-guru:update` already covers. If you do want the latest plugin code:

```
/plugin marketplace update rossoctl/context-guru
/reload-plugins
```

More: [docs/how-to/install-plugin.md](docs/how-to/install-plugin.md#upgrading).

## Troubleshooting

**If something breaks and Claude can't fix it**, run the recovery script — no session, no proxy,
no network needed:

```
~/.local/state/context-guru/context-guru-reset
```

Prefer it over `/context-guru:uninstall` or `/plugin uninstall context-guru@context-guru` when a
session is actually stuck: a dead proxy fails every request, including the ones an uninstall
command would need. More: [docs/how-to/install-plugin.md#troubleshooting](docs/how-to/install-plugin.md#troubleshooting).

**The two are not the same, though.** `/context-guru:uninstall` undoes **one** install — the one
routing the project you run it in — and leaves any other alone; the recovery script un-routes
**every** settings file the plugin edited, machine-wide one included.

```
/context-guru:uninstall
```

More:
[docs/how-to/install-plugin.md](docs/how-to/install-plugin.md#removing-it-uninstall-or-reset).

## Upgrading from project level to user level

Installed in one project and now want it everywhere? Install again with `--global` and keep both —
each gets its own port, and a project you later reset falls back to the machine-wide one.

```
/context-guru:install --global
```

More:
[docs/how-to/install-plugin.md](docs/how-to/install-plugin.md#upgrading-from-project-level-to-user-level).

## License

Apache-2.0. See [LICENSE](LICENSE). A [Rossoctl](https://github.com/rossoctl) platform component.
