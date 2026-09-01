#!/usr/bin/env bash
# SessionStart hook: make sure the proxy this project routes to is actually listening.
#
# Five properties, each of which is a way this can be wrong:
#
# 1. IT SELF-GATES ON $ANTHROPIC_BASE_URL. The plugin installs at USER scope, so this hook
#    runs in every project the user has — including every project they never routed, where
#    starting a proxy is pure waste. Settings `env` values are written into the process
#    environment and hook processes inherit it, so the variable naming our port IS the
#    per-project enablement signal. No second config to keep in sync, and it degrades
#    correctly: delete the env key by hand and this hook stops doing anything, by itself.
#
# 2. IT IS IDEMPOTENT. SessionStart also fires on `clear`, `compact`, `resume` and `fork`, not
#    just `startup` — a long session re-fires it repeatedly. So: probe /healthz, and start
#    something only if nothing answers.
#
# 3. IT IS SYNCHRONOUS. The hook is deliberately NOT marked async: it returns only once
#    /healthz answers, which is what closes the race with the session's first API request.
#    A request that beats the proxy up gets a connection error, and Claude Code's retry does
#    not make that invisible.
#
# 4. THE PORT IS FIXED, not negotiated. The URL in settings was written before this ran and
#    cannot be renegotiated, so both sides read the same configured value.
#
# 5. IT NEVER FAILS THE SESSION. Every exit is 0. A proxy that will not start must leave the
#    user with a working Claude Code and a note about it — the alternative is a plugin that
#    can brick every session on the machine, which is the biggest risk in this whole feature.
set -uo pipefail

PORT="${CLAUDE_PLUGIN_OPTION_PORT:-8787}"
PRESET="${CLAUDE_PLUGIN_OPTION_PRESET:-cache}"
IDLE_EXIT="${CLAUDE_PLUGIN_OPTION_IDLE_EXIT:-24h}"
BIN="${CONTEXT_GURU_BIN:-context-guru-proxy}"
LOG="${TMPDIR:-/tmp}/context-guru-proxy-${PORT}.log"
HEALTH="http://127.0.0.1:${PORT}/healthz"

note() { printf 'context-guru: %s\n' "$*"; }

# --- (1) the gate ------------------------------------------------------------------------
# Match the port, not merely the word "localhost": a user routing to a DIFFERENT local proxy
# (litellm, their own gateway) must not have ours started underneath them.
case "${ANTHROPIC_BASE_URL:-}" in
  *"127.0.0.1:${PORT}"* | *"localhost:${PORT}"* | *"[::1]:${PORT}"*) ;;
  *)
    # Silent by design. This is the common case — every unrouted project — and a line of
    # output here would appear in sessions that have nothing to do with context-guru.
    exit 0 ;;
esac

# --- (2) already up? ---------------------------------------------------------------------
if curl -fsS --max-time 2 "$HEALTH" >/dev/null 2>&1; then
  exit 0
fi

if ! command -v "$BIN" >/dev/null 2>&1 && [ ! -x "$BIN" ]; then
  note "routing is configured for port ${PORT} but the proxy binary ('${BIN}') is not on PATH."
  note "run /context-guru:install to install it, or /context-guru:uninstall to stop routing."
  exit 0
fi

# --- (3) start it, and wait for it to answer ---------------------------------------------
# setsid detaches the proxy from this hook's process group so it survives the hook returning
# and is not killed with the session's process tree. --idle-exit is what eventually reaps it.
STARTER=(setsid)
command -v setsid >/dev/null 2>&1 || STARTER=(nohup)   # macOS has no setsid

# --listen on the COMMAND LINE, not LISTEN_ADDR in the environment.
#
# The port has to be visible in `argv`. When it was passed through the environment, nothing that
# needed to find this specific proxy could: `pkill -f "context-guru-proxy.*$PORT"` matched no
# proxy at all, and did match the shell running it — so uninstall killed the user's own Claude
# Code session and left the proxy holding the port.
#
# The pidfile below is the primary handle; argv visibility is what makes `ps` and `pgrep` honest.
#
# --dashboard, with its database in the state directory: the skills and the line printed below
# advertise the dashboard as the place the cache effect is visible, and without this flag that URL
# was a 404. Its default DB path is `./context-guru-dashboard.db` — the current directory, i.e.
# the user's repository — so the path must be set explicitly or the plugin litters the project it
# was invited into.
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru"
mkdir -p "$STATE" 2>/dev/null || STATE="${TMPDIR:-/tmp}"
PIDFILE="${STATE}/proxy-${PORT}.pid"

PRESET="$PRESET" \
  "${STARTER[@]}" "$BIN" \
  --listen "127.0.0.1:${PORT}" \
  --idle-exit="$IDLE_EXIT" \
  --dashboard \
  --dashboard-db "${STATE}/dashboard-${PORT}.db" \
  >>"$LOG" 2>&1 &
started=$!
disown 2>/dev/null || true
# The pidfile is what uninstall uses. Written before the health wait so a proxy that comes up
# slowly is still stoppable, and it records the port it belongs to in its own name.
printf '%s\n' "$started" >"$PIDFILE" 2>/dev/null || true

# Up to ~15s. A cold start is well under a second; the budget is for a loaded laptop, and the
# hook's own timeout (60s in hooks.json) is the real backstop.
for _ in $(seq 1 60); do
  if curl -fsS --max-time 2 "$HEALTH" >/dev/null 2>&1; then
    note "proxy up on 127.0.0.1:${PORT} (preset ${PRESET}, idle-exit ${IDLE_EXIT})."
    note "dashboard: http://127.0.0.1:${PORT}/dashboard/"
    exit 0
  fi
  sleep 0.25
done

# --- (5) failed, and the session still has to work ---------------------------------------
note "the proxy did not come up on port ${PORT}; this session's requests will fail until it does."
note "log: ${LOG}"
note "fix it with /context-guru:status, or stop routing with /context-guru:uninstall."
if [ -s "$LOG" ]; then
  note "last lines:"
  tail -n 5 "$LOG" | sed 's/^/context-guru:   /'
fi
exit 0
