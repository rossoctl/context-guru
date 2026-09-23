---
name: preset-picker
description: Show which preset is running and switch between them - `off`, `cache`, `house` (the recommended default for "carry less"), `codesmart`, `housellm`. Use when the user asks which preset is set, to change what context-guru trims, to carry less context, or says /plugin configure is awkward for this.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/settings.py)
---

# Pick a preset

`/plugin configure` can set this too, but it is a blank options form: five fields, no
explanation, no sense of what `house` actually does before you pick it. This skill is that same
write — `settings.py preset` — made nameable and explainable, same reason
[`/context-guru:cache-strategy-picker`](../cache-strategy-picker/SKILL.md) exists for
`cache_strategy` rather than leaving it to the generic form too.

**You do not write the settings file, and you do not compose its contents.** `settings.py preset`
is the only thing that knows where the configured options actually live and how to update them
without disturbing anything else in that file.

## 1. Report what is in effect

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" preset show
```

`file=(none)` means nothing has been configured yet and the plugin default (`off`) applies — not
a fault, just unset.

## 2. Offer the choices, one line each

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" preset list
```

| Name | ELI5 |
|---|---|
| `off` | nothing runs, requests go through untouched |
| `cache` | keeps the cache warm, drops nothing |
| `house` | trims obvious waste — the safe first step into "carry less" |
| `codesmart` | `house` plus a cheap model that keeps only what looks relevant |
| `housellm` | the deepest cut — a compaction model rewrites the conversation, and spends on its own |

Recommend `house` to someone who wants to carry less context but hasn't said they want an LLM
pass: it is deterministic, so nothing about the request path depends on a model call succeeding.

## 3. Switch

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" preset set --name <name>
```

Writes into whichever settings file already holds this plugin's configured options — project or
user scope, wherever `/plugin install` put it — so it lands where the running proxy will actually
read it. If nothing is configured yet, it defaults to project-local scope
(`.claude/settings.local.json`), matching what `/context-guru:install` recommends; pass
`--user-scope` only if the user wants every project on the machine to pick this up.

## 4. A restart is required

Like a cache-strategy change, this takes effect at the **next proxy start**, not this session.
Say so plainly rather than implying it applies immediately — `/context-guru:status` after a
restart is the way to confirm it stuck.

## Do not

- Do not invent a preset name. `settings.py preset list` is the authoritative set; anything else
  exits 2 with `reason=unknown_preset`.
- Do not push someone toward `housellm` by default. It is the only preset here that makes its own
  model calls and spends on its own; offer it, don't default to it.
