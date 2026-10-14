#!/usr/bin/env bash
# SessionEnd / SessionStart / Notification hook: tell the proxy the user left, that the conversation
# was compacted (its old cached prefix is dead), that the user is back, or that Claude is blocked on a human.
#
# CONTENT-FREE by construction. The hook payload holds the prompt, the transcript path and tool
# input; this reads it only to take three closed-set fields (session id, event name, reason or
# source) and sends exactly those. The credential goes to curl on stdin, never on its command line.
#
# WHERE IT GOES. The event goes to the origin of ANTHROPIC_BASE_URL. By default (`auto`) only when
# that is a context-guru proxy this session already talks to: a loopback URL (the plugin's own proxy)
# or a hosted service, recognised by the x-context-guru-token line in ANTHROPIC_CUSTOM_HEADERS. `on`
# sends to any http(s) base URL; `off` (CONTEXT_GURU_CLIENT_SIGNALS or the `client_signals` plugin
# option) sends nothing.
#
# CREDENTIAL. The hosted token header when there is one, else the token Claude Code sends there
# (ANTHROPIC_AUTH_TOKEN, else ANTHROPIC_API_KEY), else none: the plugin's local proxy needs none, and
# a subscription login exports no token. Under `on` a host that is not a context-guru proxy receives
# that token on POST /signal, as it already does on every request, plus a path it does not serve.
#
# Never blocks and never fails: every exit is 0, the request has a 2 s ceiling, and a lost event
# costs nothing — the proxy's keep-alive then behaves as it does without signals.
set -uo pipefail
MODE="${CONTEXT_GURU_CLIENT_SIGNALS:-${CLAUDE_PLUGIN_OPTION_CLIENT_SIGNALS:-auto}}"
CGHDR="$(printf '%s\n' "${ANTHROPIC_CUSTOM_HEADERS:-}" | grep -i -m1 '^[[:space:]]*x-context-guru-token:' | tr -d '\r' | sed 's/^[[:space:]]*//')"
case "$MODE" in
  off | 0 | false) exit 0 ;;
  on | 1 | true) ;;
  *) case "${ANTHROPIC_BASE_URL:-}" in
       http://127.0.0.1:* | http://localhost:* | http://\[::1\]:*) ;;
       *) [ -n "$CGHDR" ] || exit 0 ;;
     esac ;;
esac
case "${ANTHROPIC_BASE_URL:-}" in http*) ;; *) exit 0 ;; esac
BODY="$(python3 -c '
import json, sys, re
d = json.load(sys.stdin)
ev = d.get("hook_event_name", "")
kind = {"SessionEnd": d.get("reason"), "SessionStart": d.get("source")}.get(ev, d.get("notification_type", ""))
sid = d.get("session_id", "")
if ev not in ("SessionEnd", "SessionStart", "Notification") or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", sid):
    sys.exit(1)
print(json.dumps({"session": sid, "event": ev, "kind": kind if isinstance(kind, str) else ""}))
' 2>/dev/null)" || exit 0
URL="$(printf '%s' "$ANTHROPIC_BASE_URL" | sed -E 's#^(https?://[^/]+).*#\1#')/signal"
if [ -n "$CGHDR" ]; then HDR="$CGHDR"
elif [ -n "${ANTHROPIC_AUTH_TOKEN:-}" ]; then HDR="Authorization: Bearer $ANTHROPIC_AUTH_TOKEN"
elif [ -n "${ANTHROPIC_API_KEY:-}" ]; then HDR="x-api-key: $ANTHROPIC_API_KEY"
else HDR="x-context-guru-client: hook"; fi # no credential: a harmless header keeps the one send path
send() { printf 'header = "%s"\n' "$HDR" | curl -sS -o /dev/null --max-time 2 -K - -H 'Content-Type: application/json' --data "$BODY" "$URL" >/dev/null 2>&1; }
case "$BODY" in
  *SessionEnd*) send ;;                    # the process is about to exit: finish the one request here
  # Hook groups of one event run in parallel, so a SessionStart event can race the hook that starts
  # the proxy: retry a refused connection twice, a second apart, then give up silently (detached).
  *) ( for _ in 1 2 3; do send && break; sleep 1; done & ) >/dev/null 2>&1 ;;
esac
exit 0
