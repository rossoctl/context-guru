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
# Same state directory the starter uses, so the command printed below writes its dashboard DB
# where the hook-started proxy would have, and not into the user's repository.
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru"
cat <<EOF
context-guru: this project is routed through http://127.0.0.1:${PORT}/anthropic, and nothing is
answering there. **Your request will hang with no error message** — that is what a dead proxy looks
like from inside Claude Code, and it is why this note exists rather than a skill.

To fix it now, in a terminal (the dashboard flags matter: without them /dashboard/ is a 404, and
this proxy would then hold the port for the whole --idle-exit window with no way to notice why):
  context-guru-proxy --listen 127.0.0.1:${PORT} --preset ${CLAUDE_PLUGIN_OPTION_PRESET:-cache} \\
    --dashboard --dashboard-db "${STATE_DIR}/dashboard-${PORT}.db"
Log from the last attempt: ${LOG}

To stop routing entirely and get working immediately, remove env.ANTHROPIC_BASE_URL from
.claude/settings.local.json (or run /context-guru:uninstall from a session that still works).
EOF
exit 0
