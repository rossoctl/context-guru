#!/usr/bin/env bash
# UserPromptSubmit hook: if this project is routed and the proxy is NOT answering, say so before
# the request goes out.
#
# This exists because of the worst failure mode in the whole plugin: with routing configured and no
# proxy listening, a prompt produces **nothing at all**. No error on stdout, no error on stderr,
# no timeout the user can interpret — the session simply hangs. That is the state after any crash,
# any reboot, and after every `--idle-exit`.
#
# `/context-guru:status` diagnoses it correctly and cannot be reached: invoking a skill needs Claude
# to respond, which needs an API call, which is the broken thing. A hook is the only thing left
# that runs without a model turn, and UserPromptSubmit is the last moment before the request.
#
# Every exit is 0 and this never blocks a prompt. It is a note, not a gate: the user may be about to
# ask something that does not need the API, and a hook that refuses prompts would be a worse
# failure than the one it reports.
set -uo pipefail

PORT="${CLAUDE_PLUGIN_OPTION_PORT:-8787}"

# Same gate as the starter, for the same reason: this plugin is installed at user scope, so this
# hook runs in every project the user has. Unrouted projects must never hear from it.
# The trailing "/" makes this an exact port match rather than a prefix one -- see the same gate
# in start-proxy.sh. PORT=8787 must not match a URL on 87871.
case "${ANTHROPIC_BASE_URL:-}" in
  *"127.0.0.1:${PORT}/"* | *"localhost:${PORT}/"* | *"[::1]:${PORT}/"*) ;;
  *) exit 0 ;;
esac

if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  exit 0
fi

# Try to start it first — the common case is an idle-exit between prompts, and recovering silently
# is better than reporting a problem the user then has to act on.
#
# THE WHOLE PATH MUST FIT IN THIS HOOK'S TIMEOUT, which is the thing that was wrong here: with
# start-proxy.sh's default 15s health wait, this script measured 19s against a 10s timeout. So the
# hook was killed before reaching the note below and the user saw NOTHING — the identical symptom
# this hook exists to replace with an explanation.
#
# Two changes, because either alone leaves a hole:
#   * ask start-proxy.sh for a 5s wait (it honours CONTEXT_GURU_HEALTH_BUDGET). A proxy that has
#     not answered in 5s is not going to answer inside a prompt's patience anyway, and the note
#     below is a better outcome than a longer silence.
#   * hooks.json allows 30s, so even the slow shapes — a port that accepts and stalls costs a full
#     --max-time on each of the three probes here — finish with room to spare.
#
# The note is deliberately still LAST rather than printed up front. Printing first would guarantee
# the user sees it, but this path's common case is a silent successful recovery, and a scary note
# on every idle-exit recovery is noise on a path that is working. The budget is what makes the
# ordering safe; measured end to end at ~6s for the dead-proxy path, and there is a test that
# reads the timeout out of hooks.json so the two cannot drift apart again.
if [ -x "${CLAUDE_PLUGIN_ROOT:-}/scripts/start-proxy.sh" ]; then
  CONTEXT_GURU_HEALTH_BUDGET="${CONTEXT_GURU_HEALTH_BUDGET:-5}" \
    "${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh" >/dev/null 2>&1 || true
  if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
    exit 0
  fi
fi

LOG="${TMPDIR:-/tmp}/context-guru-proxy-${PORT}.log"
STARTER="${CLAUDE_PLUGIN_ROOT:-}/scripts/start-proxy.sh"

# The recovery command DELEGATES to start-proxy.sh instead of restating its command line.
#
# It used to print a hand-rolled `context-guru-proxy --listen … --preset …`, and that duplicate had
# drifted from the real launch path in four ways at once. It named CLAUDE_PLUGIN_OPTION_PRESET, which
# --config replaces anyway; and it omitted --config, --anthropic-upstream and --idle-exit. Pasting it
# therefore turned keep-alive OFF for somebody who had explicitly enabled it and was paying for the
# pings, bypassed a configured gateway (which on a platform whose gateway rewrites model names makes
# every request fail), and left a proxy that never idle-exits holding the port. Every one of those was
# silent, at the moment the user is already troubleshooting.
#
# A repaired flag list would drift again the next time start-proxy.sh gains one — which is exactly how
# those four were each missed in turn. Delegation cannot.
#
# Everything is passed as FLAGS, never as an env prefix. start-proxy.sh's own gate comment records what
# an env prefix costs a human: a pasted invocation split across two lines, the assignments became a
# no-op statement, the script ran unrouted and exited without a word. That is why --preset and
# --idle-exit gained argument forms alongside --port and --upstream.
#
# --unrouted is required, not optional: settings `env` values reach hook processes, not interactive
# shells, so ANTHROPIC_BASE_URL is absent in the user's terminal and start-proxy.sh's gate would
# otherwise decline and do nothing. The port and preset must be passed for the same reason — the
# CLAUDE_PLUGIN_OPTION_* values this hook can see are invisible there, so a bare invocation would
# start on 8787 with the default preset.
PRESET="${CLAUDE_PLUGIN_OPTION_PRESET:-cache}"
IDLE_EXIT="${CLAUDE_PLUGIN_OPTION_IDLE_EXIT:-24h}"
UPSTREAM="${CLAUDE_PLUGIN_OPTION_UPSTREAM:-${ANTHROPIC_UPSTREAM:-}}"
if [ -x "$STARTER" ]; then
  # Every interpolated value is SINGLE-quoted in the printed text, because this string is not run here
  # — it is pasted into a human's shell, where it is re-parsed. An upstream is the realistic case:
  # `http://gw.example:4000/v1?tenant=acme&mode=chain` unquoted makes the `&` background the command at
  # that point, so the paste exits 0, starts a proxy, and chains it to a TRUNCATED upstream
  # (…?tenant=acme) with nothing said. A space truncates the same way, and `;` would run the remainder.
  #
  # Single quotes rather than double: double quotes still expand `$` and backticks in the user's shell,
  # so a value containing either would be silently rewritten at paste time. Single quotes suppress all
  # of it, and none of these values can legitimately contain an apostrophe.
  RECOVER="\"${STARTER}\" --unrouted --port '${PORT}' --preset '${PRESET}' --idle-exit '${IDLE_EXIT}'"
  if [ -n "$UPSTREAM" ]; then
    RECOVER="${RECOVER} --upstream '${UPSTREAM}'"
  fi
  HOW="To fix it now, in a terminal. This is the same script the hook runs, so it picks up your
keep-alive config, the dashboard flags and the pidfile uninstall looks for, without you restating any
of them:
  ${RECOVER}
Log from the last attempt: ${LOG}"
else
  # No starter to point at means the install is broken in a way the user has to fix anyway. Say that,
  # rather than falling back to a hand-written command line — reintroducing the duplicate here would
  # reintroduce the drift, in the one situation where nothing can be verified.
  HOW="This plugin's start-proxy.sh is not where this hook expects it:
  ${STARTER}
so there is no command to hand you that would be safe to run — writing one out would mean restating
the proxy's flags, and a stale copy of that list is what made this note wrong before. Reinstall with
/context-guru:install from a session that still works.
Log from the last attempt: ${LOG}"
fi

cat <<EOF
context-guru: this project is routed through http://127.0.0.1:${PORT}/anthropic, and nothing is
answering there. **Your request will hang with no error message** — that is what a dead proxy looks
like from inside Claude Code, and it is why this note exists rather than a skill.

${HOW}

To stop routing entirely and get working immediately, remove env.ANTHROPIC_BASE_URL from
.claude/settings.local.json (or run /context-guru:uninstall from a session that still works).
EOF
exit 0
