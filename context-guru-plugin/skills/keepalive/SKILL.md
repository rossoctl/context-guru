---
name: keepalive
description: Report whether the idle prompt-cache keep-alive is running, and turn it on or off. It spends the caller's own credential to hold the cache warm between turns, so it is off by default and this is the explicit, opt-in way to change that — never done automatically by the status line or any hook. Use when the user asks to enable/disable keep-alive, keep the cache warm, send a keep-alive ping, or stop idle pings from spending money.
---

# context-guru keep-alive

The proxy already has a real idle keep-alive scheduler (`proxy/keepalive.go`): once turned on,
it pings a session's cached prefix shortly before the provider's TTL would drop it, so a session
that comes back inside the gap still hits cache. **This skill does not build a second one** — it
is the one deterministic, explicit way to switch the existing mechanism on or off for the local
proxy this plugin starts.

**Why this is not automatic, and never will be from the status line.** A ping spends the caller's
own money (or usage-limit budget) between turns, while nobody is at the keyboard. The status line
renders on every keystroke; if IT could arm this, a display hook would double as an unbounded
traffic generator. Turning it on is therefore always something a user asks for, here, once.

## First: get the configured port and preset

**You cannot read `$CLAUDE_PLUGIN_OPTION_PORT` or `$CLAUDE_PLUGIN_OPTION_PRESET` here.** Claude Code
puts those variables into HOOK environments only, never into a Bash tool call, so
`${CLAUDE_PLUGIN_OPTION_PORT:-8787}` in a command always expands to 8787 whatever the user configured.

For this skill that is worse than a misreport, because both values decide what gets written:

- the config file is **named after the port**, so on a proxy configured for 4041 the enable step would
  write `keepalive-8787.yaml` — a file nothing ever reads. The toggle would report success and change
  nothing, and step 1 would then report on 8787 and confirm it.
- `--config` **replaces** `--preset` entirely, so a defaulted `cache` would silently turn a configured
  `house` preset off at the moment keep-alive turned on.

The values are on disk, so read them:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" config
```

Use its `option_port=` and `option_preset=`, or 8787 and `cache` if it reports `source=(none)`. Then
substitute both into the `<port>` / `<preset>` placeholders in every block below. If the port is
anything other than 8787, say so in your summary.

## 1. Report current activity

```bash
PORT="<port>"
curl -fsS --max-time 3 "http://127.0.0.1:${PORT}/api/stats" | \
  python3 -c 'import json,sys; d=json.load(sys.stdin); print({k: d[k] for k in d if k.startswith("keepalive_")})'
```

`keepalive_pings: 0` and no `--config` in effect means it has never been turned on for this
proxy — that is the expected state for every install by default, not a fault. If pings are
non-zero but the user expected it to be OFF, jump to step 3.

## 2. Turn it on

Ask once, plainly: this will spend the caller's credential on idle turns to keep the cache warm,
and it applies to every session routed through this proxy, not just this one.

Write the config this mechanism reads (never hand-edited — same "look before you write"
conservatism as `settings.py`, just for a file that is ours alone rather than the user's
`settings.json`):

```bash
PORT="<port>"
PRESET="<preset>"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru"
mkdir -p "$STATE"
CFG="${STATE}/keepalive-${PORT}.yaml"
MARKER="# context-guru: written by /context-guru:keepalive"
if [ -f "$CFG" ] && ! head -1 "$CFG" | grep -qF "$MARKER"; then
  echo "REFUSING: ${CFG} already exists and was not written by this skill; edit or remove it by hand first"
else
  cat > "$CFG" <<EOF
$MARKER
# Keeps the preset explicit on purpose: passing --config to context-guru-proxy REPLACES
# --preset entirely rather than layering over it, so a config missing this line would silently
# turn compaction off the moment keep-alive turned on.
preset: ${PRESET}
cache:
  keepalive: true
EOF
  echo "wrote ${CFG}"
fi
```

`start-proxy.sh` picks this file up automatically the next time it starts the proxy on this
port — no settings.json change, no plugin option. But `start-proxy.sh` only STARTS a proxy that
is not already answering `/healthz` (see its own idempotence comment), so a proxy already running
has to be stopped first for the new config to take effect:

Stop it exactly the way `/context-guru:uninstall`'s step 2 does — pidfile first, ownership
CONFIRMED before any signal is sent, never a `pkill` pattern. Run `/context-guru:uninstall`'s stop
block, or the plugin's own equivalent, then:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh" --unrouted
```

Confirm it actually picked the config up — `curl -fsS http://127.0.0.1:${PORT}/healthz` first,
then check step 1's `keepalive_pings` again after a session has gone idle for a while.

## 3. Turn it off

```bash
PORT="<port>"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru"
CFG="${STATE}/keepalive-${PORT}.yaml"
MARKER="# context-guru: written by /context-guru:keepalive"
if [ -f "$CFG" ] && head -1 "$CFG" | grep -qF "$MARKER"; then
  rm -f "$CFG"
  echo "removed ${CFG}; keep-alive turns off the next time the proxy (re)starts"
else
  echo "(no keepalive config of ours at ${CFG} — nothing to remove)"
fi
```

Same restart requirement as step 2: a proxy already running keeps its CURRENT config until it is
stopped and started again. Removing the file only changes what the NEXT start does.

## Reading the numbers honestly

- `keepalive_ping_usd` is what the pings spent; `keepalive_saved_usd` is the prefix re-creations
  they avoided; `keepalive_net_usd` is the difference. A negative net means the mechanism is
  costing more than it saves on this traffic — say so plainly rather than reporting pings as a
  win by themselves.
- The status line (`/context-guru:statusline` to enable it) shows `ka Np` once pings have
  actually happened — never before, and it never triggers one itself.
