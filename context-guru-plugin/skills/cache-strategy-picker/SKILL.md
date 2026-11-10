---
name: cache-strategy-picker
description: Show the cache strategy and switch it. Use when asked which one runs, to turn keep-alive pings on or off, or to keep the cache warm.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/settings.py)
---

# Pick a cache strategy

Each cache mechanism has a name. The point of the names is that switching back is one word:
"put it back on `auto`" has to be a sentence, not an archaeology exercise over four tuning numbers.
**The proxy owns the names and what they mean.** `settings.py strategy list` asks the installed
`context-guru-proxy` for them. This skill keeps no list.

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
that one alone (preset `off`, cache strategy `auto`).

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

First read the names the proxy offers for Claude Code:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" strategy list
```

- `names=` is the full set. Offer only these. Claude Code is routed to Anthropic only, so the list
  holds no OpenAI-only strategy.
- `strategy_<name>=` is the proxy's own description. Show it to the user as it is, word for word. It
  carries the numbers and any gate or measurement caveat. Do not shorten it or add your own.
- `spends_<name>=true` means the strategy spends the caller's own credential. `false` means it does not.
- `default=` is the name used when the user chose none. `aliases=` are old names (`old:new`) that still work.
- `result=error reason=proxy_too_old` means the installed proxy is older than v0.7.1. Say so and tell
  the user to run `/context-guru:update`. Do not guess a list.
- `reason=no_proxy_binary` means no proxy is installed: tell the user to run `/context-guru:install`.
  `reason=proxy_unrunnable` means the binary named by `--bin` or `CONTEXT_GURU_BIN` cannot run: show
  the `note=`. Do not guess a list in either case.

Then switch:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" strategy set --name <name> --port <port> --preset <preset>
```

The file it writes names the strategy (`cache:` / `strategy: <name>`). The proxy fills in the keys.
`none` writes no file at all.

**Say the cost before switching to any strategy with `spends=true`, once, in one line.** It spends the
caller's money (or usage-limit budget) while nobody is at the keyboard, and it applies to every session
routed through this proxy, not just this one. The default is on because holding the cache warm across an
idle gap is the thing most people install this for, not because it is free.

**Do not sell a strategy with `spends=false` and a measurement caveat.** Read its description aloud
with the caveat, and say any size gate out loud: below the gate the strategy does nothing, so a user
watching `Usage.CacheWrite1h` stay at zero on a small request should not conclude the tier was refused.
Offer it as a measurement, not an upgrade.

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
(`proxy up on 127.0.0.1:<port> (preset cache, cache strategy auto, idle-exit 24h)`), which is
the cheapest available proof. `curl -fsS http://127.0.0.1:<port>/healthz` if you want the second one.

If the user would rather not restart now, that is fine and worth stating plainly: the strategy is
recorded and takes effect at the next session's start.

## Reading the numbers honestly

- `keepalive_ping_usd` is what the pings spent; `keepalive_saved_usd` is the prefix re-creations
  they avoided; `keepalive_net_usd` is the difference. **A negative net means the strategy is
  costing more than it saves on this traffic** — say that plainly and offer `none`, rather than
  reporting a ping count as if it were a win.
- The `detailed` status line layout (or `/context-guru:statusline --keepalive`) shows
  `keep-alive 3 pings · $0.04` — the pings sent and what they cost — after the first ping. It
  never triggers a ping itself.

## Do not

- Do not hand-write or hand-edit `keepalive-<port>.yaml`. The picker refuses a file whose first line
  is not its marker, and an edit that removes the marker also removes the user's ability to switch
  back by name.
- Do not invent a strategy name. `settings.py strategy list` is the authoritative set; anything else
  exits 2 with `reason=unknown_strategy`.
- Do not copy the names or descriptions into your answer from memory. Read them from `strategy list`.
- Do not claim a change is in effect because the write succeeded. The restart is the claim.
