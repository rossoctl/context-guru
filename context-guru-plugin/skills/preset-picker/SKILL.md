---
name: preset-picker
description: Show the running preset and switch it (`off`, `conservative`, `medium`, `high`, `xhigh`, `ultrahigh`). Use when the user asks which preset is set, to change what context-guru trims, or to carry less context.
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
| `xhigh` | `high` plus a deep-adjudication sweep over turns whose cache has already gone cold — the deepest cut |
| `ultrahigh` | **Experimental.** `xhigh` plus tool deferral: tools your own last sessions never used are sent as names only, and the model loads one through search when it needs it. Opt-in. Turn it off for one session with `ENABLE_TOOL_SEARCH=false` |

Recommend `conservative` to someone who wants to carry less context but hasn't said they want an
LLM pass: it is deterministic, so nothing about the request path depends on a model call
succeeding. `medium`, `high`, `xhigh` and `ultrahigh` are an escalating ladder — each is a strict superset of
the one below it — so "somewhere in between" usually means one tier up from wherever they are now,
not a custom mix.

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

`preset show` and `preset set` also print `running=` (the preset the proxy runs now, from its
`/stats`; `no_proxy` when nothing answers). `gap=` appears when the saved and the running preset
differ.

## 4. A restart applies it, after one yes

The running proxy keeps its old preset until it restarts. When `preset set` prints
`restart_needed=true`, tell the user in plain words:

- The new preset is saved.
- A restart applies it now.
- **A restart costs one cache rewrite in every open session on this port.**
- If they say no, the old preset runs until the next session start.

Then ask for a yes. Do not restart without it. On yes:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" preset restart --name <name>
```

It restarts the proxy through `start-proxy.sh`, the same path a session start uses (`strategy sync`,
then a fingerprint compare, then a stop by pidfile and a start). It never kills by pattern, and it
leaves a proxy that another project owns alone. It then reads `/stats` to confirm. Report
`result=restarted` (the new preset runs) or `result=not_applied` with the `starter=` lines that say why.

For `ultrahigh`, `preset restart` then asks the RESTARTED proxy to start the provider check that tool
deferral needs, and prints `probe=asked` (or why not: `not_routed`, `unavailable`). The check runs at
your next request, in the background. The first session on each model after the restart is not
deferred. `preset set` sends no probe while the old proxy runs: that proxy would take it and do
nothing with it.

On no, say the old preset runs until the next session start. `/context-guru:status` and the status line
show `preset: <running> (<saved> after a restart)` until then.

## Do not

- Do not invent a preset name. `settings.py preset list` is the authoritative set; anything else
  exits 2 with `reason=unknown_preset`.
- Do not push someone toward `high` or `xhigh` by default. `medium` is already the first tier that
  makes its own model calls and spends on its own; `high`/`xhigh` use the summary of older turns
  while you work, not only after a cold return. Offer them, don't default to them.
- Do not suggest `ultrahigh` unless the user asks for it or asks about tool cost. It is opt-in: it
  changes which tool definitions the model sees at the start, and the model must search to load one.
