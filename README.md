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
<img src="docs/img/context-guru-savings.png" alt="context-guru saves 5–15% of your API cost in four ways" width="720" />
</p>

## Install (Claude Code plugin)

```
/plugin marketplace add rossoctl/context-guru
/plugin install context-guru@context-guru
/reload-plugins     # REQUIRED — next lines else answer "Unknown command"
/permissions        # add a new rule → paste the line below → Project settings (local)
Bash(/Users/{you}/.claude/plugins/cache/context-guru/**)
/context-guru:install
```

<!-- If you want to install it for your entire org instead of one machine, follow the proxy
     installation guide: docs/setup.md -->

Check what it's saving:

```
/context-guru:status
```

It reports the preset and cache strategy running, and the dollars saved so far — keep-alive
savings, any content-trimming savings, and the net. Want the raw numbers instead of the reading of
them? `/context-guru:status --stats` prints the `/stats` endpoint verbatim.

If you want to also carry less, not just pay less, opt into content trimming:

```
/context-guru:preset-picker   # → house, the safe first step into trimming
```

### Status line

A terminal status line is installed **on by default** alongside the plugin — no separate step. It
shows the context-window bar, this session's running savings against its own running cost, the
proxy/upstream latency split, and a least-used-tool hint, and it renders nothing at all in a
project that isn't routed through context-guru, so it's safe even before you run
`/context-guru:install` anywhere. Turn it off any time with:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" off --file ~/.claude/settings.json
```

More: [docs/how-to/install-plugin.md](docs/how-to/install-plugin.md), or ask
`/context-guru:statusline` to enable an extra segment or turn it back on.

Everything else — architecture, the full benchmark, every component, the proxy/gateway path,
config reference — is in **[docs/design.md](docs/design.md)** and
**[docs/get-started/how-it-saves.md](docs/get-started/how-it-saves.md)**.

**If something breaks and Claude can't fix it**, run the recovery script — no session, no proxy,
no network needed:

```
~/.local/state/context-guru/context-guru-reset
```

Prefer it over `/context-guru:uninstall` or `/plugin uninstall context-guru@context-guru` when a
session is actually stuck: a dead proxy fails every request, including the ones an uninstall
command would need. More: [docs/how-to/install-plugin.md#troubleshooting](docs/how-to/install-plugin.md#troubleshooting).

## Updating

Claude Code checks the marketplace for updates in the background and prompts you to
`/reload-plugins` when one lands — or update on demand:

```
/plugin marketplace update rossoctl/context-guru
/reload-plugins
```

The plugin's version tracks each proxy release, so an update to the proxy is always an update
you get prompted for. More: [docs/how-to/install-plugin.md](docs/how-to/install-plugin.md#upgrading).

## License

Apache-2.0. See [LICENSE](LICENSE). A [Rossoctl](https://github.com/rossoctl) platform component.
