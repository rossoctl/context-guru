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
case "${ANTHROPIC_BASE_URL:-}" in
  *"127.0.0.1:${PORT}"* | *"localhost:${PORT}"* | *"[::1]:${PORT}"*) ;;
  *) exit 0 ;;
esac

if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  exit 0
fi

# Try to start it first — the common case is an idle-exit between prompts, and recovering silently
# is better than reporting a problem the user then has to act on.
if [ -x "${CLAUDE_PLUGIN_ROOT:-}/scripts/start-proxy.sh" ]; then
  "${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh" >/dev/null 2>&1 || true
  if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
    exit 0
  fi
fi

LOG="${TMPDIR:-/tmp}/context-guru-proxy-${PORT}.log"
cat <<EOF
context-guru: this project is routed through http://127.0.0.1:${PORT}/anthropic, and nothing is
answering there. **Your request will hang with no error message** — that is what a dead proxy looks
like from inside Claude Code, and it is why this note exists rather than a skill.

To fix it now, in a terminal:
  context-guru-proxy --listen 127.0.0.1:${PORT} --preset ${CLAUDE_PLUGIN_OPTION_PRESET:-cache}
Log from the last attempt: ${LOG}

To stop routing entirely and get working immediately, remove env.ANTHROPIC_BASE_URL from
.claude/settings.local.json (or run /context-guru:uninstall from a session that still works).
EOF
exit 0
