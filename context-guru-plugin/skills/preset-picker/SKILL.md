---
name: preset-picker
description: Show which preset is running and switch between them - `off`, `conservative` (the recommended default for "carry less"), `medium`, `high`, `xhigh`. Use when the user asks which preset is set, to change what context-guru trims, to carry less context, or says /plugin configure is awkward for this.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/settings.py)
---

# Pick a preset

`/plugin configure` can set this too, but it is a blank options form: five fields, no
explanation, no sense of what `conservative` actually does before you pick it. This skill is that same
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
| `conservative` | trims obvious waste — the safe first step into "carry less" |
| `medium` | `conservative` plus a cheap model that keeps only what looks relevant in recent tool output, and a summary prepared in the background that is used only if you come back after the cache expired |
| `high` | `medium`, but the summary of older turns is used as soon as the context window is nearly full |
| `xhigh` | the same as `high` in this release. The deep-adjudication sweep is off until it is ready |

Recommend `conservative` to someone who wants to carry less context but hasn't said they want an
LLM pass: it is deterministic, so nothing about the request path depends on a model call
succeeding. `conservative`, `medium` and `high` are an escalating ladder — each is a strict superset of
the one below it. `xhigh` is the same as `high` in this release, not a superset. So "somewhere in
between" usually means one tier up from wherever they are now, not a custom mix.

## 3. Switch

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" preset set --name <name>
```

Writes into whichever settings file already holds this plugin's configured options, so it lands
where the running proxy will actually read it. If nothing is configured yet, it inherits whatever
scope THIS project's *routing* already uses (recorded when `/context-guru:install` ran, read back
via the same `resolve_install_scope()` the statusline skill uses) — a project routed at project
scope gets its preset there too, one routed at user scope gets it there, with no extra flag either
way. Only a project with no routing at all falls back to project-local, since there is nothing yet
to inherit. Pass `--user-scope` to deliberately diverge from that — a preset the user wants on
every project even though routing itself is project-scoped.

## 4. A restart is required

Like a cache-strategy change, this takes effect at the **next proxy start**, not this session.
Say so plainly rather than implying it applies immediately — `/context-guru:status` after a
restart is the way to confirm it stuck.

## Do not

- Do not invent a preset name. `settings.py preset list` is the authoritative set; anything else
  exits 2 with `reason=unknown_preset`.
- Do not push someone toward `high` or `xhigh` by default. `medium` is already the first tier that
  makes its own model calls and spends on its own; `high`/`xhigh` use the summary of older turns
  while you work, not only after a cold return. Offer them, don't default to them.
