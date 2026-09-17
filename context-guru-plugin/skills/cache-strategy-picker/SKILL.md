---
name: cache-strategy-picker
description: Show which named cache strategy is in effect and switch between them - `split`, `5-min-ping` (the default, which spends the caller's own credential on idle pings) and `1-hour-head`. Use when the user asks which cache strategy is running, to change it, to turn idle keep-alive pings on or off, to stop spending money between turns, or to keep the cache warm.
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
that one alone (port 8787, preset `cache`, cache strategy `5-min-ping`).

## 2. Report what is in effect

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" strategy show --port <port>
```

- `strategy=split` with `file=(none)` — no config for this port. That is not a fault or an
  "unknown": it is exactly what `split` means, and it is what a `--cache-strategy split` install
  leaves behind.
- `strategy=(unnamed)` — armed before strategies had names (a config written by the older
  `/context-guru:keepalive`). Re-setting it with a name is safe and is what gives them the word
  back.
- `strategy=(foreign)` — something we did not write is at our path. Say so and leave it alone;
  do not offer to overwrite it.

If the user asks whether pings are actually happening, that is a different question from which
strategy is armed — read the live counters:

```bash
curl -fsS --max-time 3 "http://127.0.0.1:<port>/api/stats" | \
  python3 -c 'import json,sys; d=json.load(sys.stdin); print({k: d[k] for k in d if k.startswith("keepalive_")})'
```

`keepalive_pings: 0` on a freshly-started proxy is expected — nothing has gone idle yet.

## 3. Switch

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" strategy set --name <name> --port <port> --preset <preset>
```

| Name | What it does | Cost |
|---|---|---|
| `split` | the preset's `cachesplit` alone. Written as the ABSENCE of a config | free — no model calls |
| `5-min-ping` | split **+** an idle ping at 280 s (just under the provider's 5-minute TTL), ≤2 per idle span, ≥20k-token prefix, ≤$0.25/ping | **spends the caller's own credential between turns** |
| `1-hour-head` | split **+** the 1-hour tier on the `tools`/`system` breakpoints, **only on >=50k-token prefixes** | free, and often $0 of benefit — see below |

**Say the cost before switching TO `5-min-ping`, once, in one line.** It spends the caller's money
(or usage-limit budget) while nobody is at the keyboard, and it applies to every session routed
through this proxy, not just this one. It is the default because holding the cache warm across an
idle gap is the thing most people install this for — not because it is free.

**Be honest about `1-hour-head` rather than selling it.** `config/config.go` records it measured
live: GRANTED on `claude-haiku-4-5` (36,251 of 36,574 tokens written at the 1h tier) and **silently
downgraded on `claude-sonnet-5`** (0 of 48,212, with an otherwise normal 200). Zero 1h writes appear
in 19,805 production requests. So on the Opus/Sonnet models most users run, the honest projection is
**$0**, and `Usage.CacheWrite1h` is the only thing that says otherwise. Offer it as a measurement,
not an upgrade.

**And say the gate out loud when you offer it.** This strategy ships `head_ttl_min_tokens: 50000`, so
it does nothing whatever on a prefix under 50k tokens. That threshold is what makes it pay at all
(+$48.81, against −$18.34 applied blanket), so it is not a flaw — but **both measurements above are
below it**: 36,574 tokens on Haiku, 48,212 on Sonnet. A user who arms this on Haiku 4.5, the one model
where the tier was granted, and then checks a request the size of the one that was measured, sees no 1h
label — and would reasonably conclude the tier was refused when in fact the request was too small to
ask. If they are watching `Usage.CacheWrite1h` for zero, tell them which of the two reasons they are
looking at.

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
  costing more than it saves on this traffic** — say that plainly and offer `split`, rather than
  reporting a ping count as if it were a win.
- The status line (`/context-guru:statusline`) shows `ka Np` once pings have actually happened —
  never before, and it never triggers one itself.

## Do not

- Do not hand-write or hand-edit `keepalive-<port>.yaml`. The picker refuses a file whose first line
  is not its marker, and an edit that removes the marker also removes the user's ability to switch
  back by name.
- Do not invent a strategy name. `settings.py strategy list` is the authoritative set; anything else
  exits 2 with `reason=unknown_strategy`.
- Do not claim a change is in effect because the write succeeded. The restart is the claim.
