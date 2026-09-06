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
# The trailing "/" is load-bearing: without it this is a PREFIX match, so PORT=8787 also matches
# a URL on 87871 -- and this hook would start our proxy on 8787 underneath a user routed to a
# different local proxy on that port, which is the exact case this gate exists to prevent. Every
# base URL we write ends in /anthropic, so the delimiter is always there to match.
# CONTEXT_GURU_FORCE=1 starts the proxy without routing being configured yet.
#
# The install needs the proxy up BEFORE it writes the routing key, so at that moment the gate below
# is false by definition. The skill used to satisfy it by prefixing the invocation with
# `ANTHROPIC_BASE_URL="http://127.0.0.1:<port>/anthropic"`, and that turned out to be a bad idea for
# two independent reasons:
#
#   * Claude Code's auto-mode classifier denied the command as [Traffic Redirection] — reasonably,
#     since the command text literally repoints ANTHROPIC_BASE_URL at a local interceptor. Observed
#     on a hosted agent, and it blocked the install at the last step.
#   * Bash permission rules match by command PREFIX, so an env-prefixed command cannot be covered by
#     a rule naming this script. Users could not grant it even if they wanted to.
#
# So the install asks for what it means — "start the proxy" — and nothing in the command line
# reassigns the variable that routes traffic.
# --force as an ARGUMENT, not only an environment variable.
#
# Bash permission rules match by command PREFIX, so `FOO=1 /path/to/start-proxy.sh` cannot be covered
# by any rule naming this script — the command does not begin with it. Every env-prefixed form is
# therefore ungrantable: a user who wants to approve "this plugin may start its proxy" has no way to
# say so. As an argument it is `<script> --force`, which `Bash(<scripts dir>/**)` covers.
#
# This does not make the classifier's objection go away, and it is not meant to. Starting a proxy that
# intercepts the session's model traffic is a decision for the user, and auto mode denying it on the
# agent's behalf is the correct outcome. What this changes is that the user now has a way to GRANT it
# — which is the difference between friction and a dead end.
# The flag is named for what it DOES, after `--force` was denied for what it sounded like:
#
#     Denied ∙ [Safety Bypass Flag] runs a third-party plugin's start-proxy.sh with --force (after
#     investigating that flag's role in bypassing checks) to redirect the agent's own API traffic
#
# Fair reading of the name, and the name was wrong. Nothing is being bypassed: the gate below asks
# "is this project routed to us?", and during an install the answer is legitimately "not yet, that is
# the next step". `--unrouted` says that. `--force` is still accepted so nothing that learned it
# breaks, but it is not what the skill or the docs tell anyone to use.
#
# --upstream <url> is an ARGUMENT for the same reason the flag is: CLAUDE_PLUGIN_OPTION_UPSTREAM is
# NOT present in the environment of a Bash tool call, even when the option is configured — verified on
# a hosted agent, where the option was set in user settings and the variable was absent. Plugin
# options reach HOOK environments; they do not reach a script the install skill runs. So the skill has
# to pass the upstream explicitly, and passing it as an env prefix is exactly what makes a command
# ungrantable.
START_UNROUTED="${CONTEXT_GURU_FORCE:-${CONTEXT_GURU_START_UNROUTED:-}}"
UPSTREAM_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --unrouted|--force) START_UNROUTED=1 ;;
    --upstream) UPSTREAM_ARG="${2:-}"; shift ;;
    --upstream=*) UPSTREAM_ARG="${1#--upstream=}" ;;
  esac
  shift
done
FORCE="$START_UNROUTED"

if [ "$FORCE" = 1 ]; then
  :
else
case "${ANTHROPIC_BASE_URL:-}" in
  *"127.0.0.1:${PORT}/"* | *"localhost:${PORT}/"* | *"[::1]:${PORT}/"*) ;;
  *)
    # Silent on STDOUT by design: this is the common case — every unrouted project — and a line
    # here would appear in sessions that have nothing to do with context-guru.
    #
    # But leave a breadcrumb in the log, because silence with no trace is indistinguishable from
    # "the script never ran", and that cost somebody a real debugging session. A human pasted a
    # long env-prefixed invocation, the paste split across two lines so the assignments became a
    # no-op statement, the script ran unrouted and exited here without a word — no output, no log,
    # no pidfile. Three rounds of guessing followed, and the fix was only found by reading this
    # branch in the source. One line costs nothing and answers it immediately.
    printf '%s declined: ANTHROPIC_BASE_URL=%s does not name port %s; pass --force to start anyway\n' \
      "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "${ANTHROPIC_BASE_URL:-unset}" "$PORT" >>"$LOG" 2>/dev/null
    exit 0 ;;
esac
fi

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
# Detach the proxy so it outlives this hook. --idle-exit is what eventually reaps it.
#
# Be precise about what each of these does, because they are NOT equivalent and the first version
# of this comment credited both with the same property: `setsid` puts the child in a new session
# and process group, so a signal sent to the session's process group does not reach it. The macOS
# fallback `nohup` does NOT -- it only makes the child ignore SIGHUP, and the child stays in this
# process group. `disown` below is bash job-table bookkeeping, not a process-group change either.
#
# In practice nohup+disown survives the cases that matter here (the hook returning, the terminal
# closing). Left as is rather than "fixed" with a subshell trap, because the mechanism works and
# the bug was the claim.
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

# --anthropic-upstream, when something else is already the gateway.
#
# On a hosted agent (a coding-agent pod, a managed workspace) $ANTHROPIC_BASE_URL is set by the
# PLATFORM to its own local gateway, and that gateway is not decoration: it holds the credential and
# it rewrites model names. One such pod maps `claude/haiku…` to the real model id, so a proxy that
# forwards straight to api.anthropic.com sends model names Anthropic has never heard of, and every
# request fails. Chaining behind it is the only configuration that works there.
#
# The binary already honours the ANTHROPIC_UPSTREAM environment variable (verified against the
# released v0.1.1 binary: with it set and no flag, requests arrive at the configured upstream). But
# relying on inheritance alone is fragile — this hook only sees what the session's env block passes
# it — so pass it EXPLICITLY when it is configured, and let the plugin option name it too.
# The argument wins: it is the only one of these three the install skill can actually rely on, since
# a Bash tool call sees neither the plugin option nor, necessarily, the session's own env.
UPSTREAM="${UPSTREAM_ARG:-${CLAUDE_PLUGIN_OPTION_UPSTREAM:-${ANTHROPIC_UPSTREAM:-}}}"
UPSTREAM_ARGS=()
if [ -n "$UPSTREAM" ]; then
  UPSTREAM_ARGS=(--anthropic-upstream "$UPSTREAM")
fi

PRESET="$PRESET" \
  "${STARTER[@]}" "$BIN" \
  --listen "127.0.0.1:${PORT}" \
  --idle-exit="$IDLE_EXIT" \
  --dashboard \
  --dashboard-db "${STATE}/dashboard-${PORT}.db" \
  "${UPSTREAM_ARGS[@]+"${UPSTREAM_ARGS[@]}"}" \
  >>"$LOG" 2>&1 &
started=$!
disown 2>/dev/null || true
# The pidfile is what uninstall uses. Written before the health wait so a proxy that comes up
# slowly is still stoppable, and it records the port it belongs to in its own name.
printf '%s\n' "$started" >"$PIDFILE" 2>/dev/null || true

# Budget on WALL CLOCK, not on an iteration count.
#
# This loop used to be `for _ in $(seq 1 60)` with `--max-time 2`, described as "up to ~15s". That
# arithmetic only holds when the port is REFUSED — then curl returns instantly and 60 x 0.25s is
# indeed ~15s. Against something that accepts and never answers (a hung proxy, a half-open socket,
# an unrelated service holding the port) every probe burns its full timeout instead: measured
# 2046ms each, so 60 iterations is ~122s, against this hook's 60s timeout.
#
# The consequence was not "a slow hook". The hook was KILLED at 60s, so the failure report below --
# log path, status pointer, last lines of the log -- never ran. A hung port is one of the likeliest
# ways to need that report, and it was the one case that never produced it.
#
# CONTEXT_GURU_HEALTH_BUDGET lets a caller with a tighter deadline ask for a shorter wait;
# check-proxy.sh runs from UserPromptSubmit and sets it low.
BUDGET="${CONTEXT_GURU_HEALTH_BUDGET:-15}"
case "$BUDGET" in
  ''|*[!0-9]*) BUDGET=15 ;;   # never let a junk value turn the arithmetic below into an error
esac
deadline=$(( $(date +%s) + BUDGET ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  # --max-time 1: this is a loopback /healthz on a proxy that has just been launched. The generous
  # timeout belongs on the idempotence probe above (where a false negative starts a SECOND proxy
  # and overwrites the pidfile with a pid that immediately exits), not on this one.
  if curl -fsS --max-time 1 "$HEALTH" >/dev/null 2>&1; then
      if [ -n "$UPSTREAM" ]; then
      note "proxy up on 127.0.0.1:${PORT} (preset ${PRESET}, idle-exit ${IDLE_EXIT}), chained behind ${UPSTREAM}."
      note "dashboard: http://127.0.0.1:${PORT}/dashboard/"
      exit 0
    fi
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
