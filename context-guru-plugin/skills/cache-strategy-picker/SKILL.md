---
name: cache-strategy-picker
description: Show which named cache strategy is in effect and switch between them - `none`, `5-min-ping` (the default, which spends the caller's own credential on idle pings) and `1-hour-head`. Use when the user asks which cache strategy is running, to change it, to turn idle keep-alive pings on or off, to stop spending money between turns, or to keep the cache warm.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/settings.py)
---

# Pick a cache strategy

Three mechanisms, three names. The point of the names is that switching back is one word:
"put it back on `5-min-ping`" has to be a sentence, not an archaeology exercise over four
tuning numbers.

**You do not write the config file, and you do not compose its contents.** `settings.py strategy`
is the only thing that knows what a name means. It writes atomically, refuses to touch a file it
did not write, and states the preset explicitly — because `--config` REPLACES `--preset` rather
than layering over it, so a config that forgot that line would silently turn compaction off at the
moment a strategy was armed.

## 1. Resolve the port and preset first — do not default them

**You cannot read `$CLAUDE_PLUGIN_OPTION_PORT` here.** Claude Code puts those variables into HOOK
environments only, never into a Bash tool call, so `${CLAUDE_PLUGIN_OPTION_PORT:-8787}` in a
command always expands to 8787 whatever the user configured. For this skill that is not a
cosmetic error: **the config file is named after the port**, so on a proxy configured for 4041 a
defaulted 8787 writes `keepalive-8787.yaml` — a file nothing ever reads — and then reports success.

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" config
```

Read the fallback **per option**, not from `source=`: only keys the user actually set are printed,
so somebody who set the port and never touched the preset gets a real `source=` and no
`option_preset=` line. Any option not listed is unconfigured — use the `plugin.json` default for
that one alone (preset `cache`, cache strategy `5-min-ping`).

**Not for the port.** It is allocated per project rather than defaulted, so 8787 is a guess at
another project's proxy and the whole failure above — a `keepalive-<port>.yaml` nothing reads,
reported as success — happens just the same. Ask the one command that resolves it, the same one the
hooks and the status line use:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" port install
```

`result=ok port=<n>` is the port this directory's install is on, and a strategy is armed on that
install whether or not this session happens to be routed through it (`routed=false` with
`not_routed_why=` is worth one line, not a refusal). `result=unrouted` means nothing here is
installed — stop and say so rather than falling back to 8787, because there is no proxy for a
strategy to be armed on.

## 2. Report what is in effect

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" strategy show --port <port>
```

- `strategy=none` with `file=(none)` — no config for this port. That is not a fault or an
  "unknown": it is exactly what `none` means, and it is what a `--cache-strategy none` install
  leaves behind.
- `strategy=(unnamed)` — armed before strategies had names (a config written by the older
  `/context-guru:keepalive`). Re-setting it with a name is safe and is what gives them the word
  back.
- `strategy=(foreign)` — something we did not write is at our path. Say so and leave it alone;
  do not offer to overwrite it.

If the user asks whether pings are actually happening, that is a different question from which
strategy is armed — read the live counters straight off the proxy, no dashboard query needed:

```bash
curl -fsS --max-time 3 "http://127.0.0.1:<port>/stats" | \
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("keepalive"), d.get("savings"))'
```

`keepalive` is the always-on in-memory ping ledger (`pings: 0` on a freshly-started proxy is
expected — nothing has gone idle yet). `savings` is present only with `--dashboard`, and each of
its scopes (`current`/`live`/`all`) carries the reconciled `keepalive_net_usd` — whether the pings
actually paid for themselves, not just that they fired.

## 3. Switch

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" strategy set --name <name> --port <port> --preset <preset>
```

| Name | ELI5 | Cost |
|---|---|---|
| `none` | no keep-alive pings, nothing extra happens | free |
| `5-min-ping` | pings the cache now and then so it doesn't go cold while you're away | spends a little of your own credential between turns |
| `1-hour-head` | asks the provider for a longer-lived cache instead of pinging, only on prefixes >=50k tokens | free, but on today's models it usually buys nothing — see below |

**Say the cost before switching TO `5-min-ping`, once, in one line.** It spends the caller's money
(or usage-limit budget) while nobody is at the keyboard, and it applies to every session routed
through this proxy, not just this one. It is the default because holding the cache warm across an
idle gap is the thing most people install this for — not because it is free.

**Be honest about `1-hour-head` rather than selling it.** In practice the 1-hour tier gets granted
on Haiku but silently downgraded on Sonnet, so on the models most people run, expect $0 benefit
unless `Usage.CacheWrite1h` says otherwise. Offer it as a measurement, not an upgrade.

**And say the gate out loud when you offer it.** This strategy only does anything on a large prefix
(tens of thousands of tokens) — below that threshold it's a no-op, so a user watching
`Usage.CacheWrite1h` stay at zero on a small request shouldn't conclude the tier was refused; the
request may just be too small to qualify.

**Never turn a strategy on from a display path.** The status line renders on every keystroke; if it
could arm this, a display hook would double as an unbounded traffic generator. Arming is always
something a user asks for, here.

## 4. A restart is required, and saying otherwise is the failure mode

`start-proxy.sh` picks the config up **only when it starts a proxy**, and it starts one only if
nothing already answers `/healthz`. So a proxy that is already running keeps its current strategy
until it is stopped and started again. `settings.py` says this in its own `note=`; pass it on rather
than implying the change is live.

Stop it exactly the way `/context-guru:uninstall` step 2 does — pidfile first, ownership CONFIRMED
before any signal, **never** a `pkill` pattern — then:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh" --unrouted
```

Confirm it came back and that it picked up what you wrote: the startup note now names the strategy
(`proxy up on 127.0.0.1:<port> (preset cache, cache strategy 5-min-ping, idle-exit 24h)`), which is
the cheapest available proof. `curl -fsS http://127.0.0.1:<port>/healthz` if you want the second one.

If the user would rather not restart now, that is fine and worth stating plainly: the strategy is
recorded and takes effect at the next session's start.

## Reading the numbers honestly

- `keepalive_ping_usd` is what the pings spent; `keepalive_saved_usd` is the prefix re-creations
  they avoided; `keepalive_net_usd` is the difference. **A negative net means the strategy is
  costing more than it saves on this traffic** — say that plainly and offer `none`, rather than
  reporting a ping count as if it were a win.
- The status line (`/context-guru:statusline --keepalive`) shows `ka ≤Nmiss $X.XX` — the misses
  prevented and the NET money that saved — once that net is actually positive, never before, and
  it never triggers a ping itself.

## Do not

- Do not hand-write or hand-edit `keepalive-<port>.yaml`. The picker refuses a file whose first line
  is not its marker, and an edit that removes the marker also removes the user's ability to switch
  back by name.
- Do not invent a strategy name. `settings.py strategy list` is the authoritative set; anything else
  exits 2 with `reason=unknown_strategy`.
- Do not claim a change is in effect because the write succeeded. The restart is the claim.
