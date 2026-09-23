---
name: keepalive
description: Superseded by /context-guru:cache-strategy-picker. Kept so the name keeps working for anyone who learned it - idle keep-alive is now the named cache strategy `5-min-ping`, and the picker is where it is switched on or off.
---

# Keep-alive is now a named strategy

This skill used to write `keepalive-<port>.yaml` itself, from a heredoc embedded in this file.
It no longer does, and that is the point: the file's contents are now decided in exactly one
place, `settings.py strategy`, so two skills cannot drift about what "keep-alive on" means.

**Use [`/context-guru:cache-strategy-picker`](../cache-strategy-picker/SKILL.md).** Invoke it and
follow it; do not reimplement any part of it here, and do not hand-write the config file.

The mapping, so nothing is lost in the rename:

| what this skill used to do | the strategy name now |
|---|---|
| keep-alive ON, with the default tuning | `5-min-ping` — **and it is the install default** |
| keep-alive OFF | `none` (the preset runs alone, no pings, no spend) |

Two things worth carrying over verbatim, because they are the reason this mechanism is
deliberate rather than automatic:

- **`5-min-ping` spends the caller's own credential** (or usage-limit budget) on idle turns, while
  nobody is at the keyboard, for every session routed through the proxy. It is the default because
  holding the cache warm across an idle gap is what most people install this for — not because it
  is free. `keepalive_net_usd` is what says whether it is paying for itself on real traffic; a
  negative net means say so and offer `none`. Read it straight off `/stats`' `savings` block
  (`current`/`live`/`all` scopes, present when `--dashboard` is on) — no need to open the dashboard
  for this one number.
- **Nothing on a render path may ever arm it.** The status line draws on every keystroke; if a
  display hook could turn this on, it would double as an unbounded traffic generator.
