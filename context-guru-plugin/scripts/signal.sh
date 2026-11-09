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
STDIN="$(cat)"
# Tool deferral (#485). The user's ENABLE_TOOL_SEARCH opt-out is kept per session id, so every
# SessionStart that can start a new id (startup, resume, fork, clear) tells the proxy again, on its
# own route (/tool-search, which works with keep-alive off). Only a value that turns search off is
# sent: false, 0, no, off, in any case, the values the proxy reads as off (tooldefer.SearchOffValue).
# Without one there is nothing to say: a plain startup or fork then sends nothing at all.
TSOFF=0
case "$(printf %s "${ENABLE_TOOL_SEARCH:-}" | tr '[:upper:]' '[:lower:]')" in false|0|no|off) TSOFF=1 ;; esac
export TSOFF
BODIES="$(python3 -c '
import json, os, sys, re
d = json.load(sys.stdin)
ev = d.get("hook_event_name", "")
kind = {"SessionEnd": d.get("reason"), "SessionStart": d.get("source")}.get(ev, d.get("notification_type", ""))
kind = kind if isinstance(kind, str) else ""
sid = d.get("session_id", "")
if ev not in ("SessionEnd", "SessionStart", "Notification") or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", sid):
    sys.exit(1)
# One body per line, each prefixed with its route.
if not (ev == "SessionStart" and kind in ("startup", "fork")):
    # The keep-alive event. startup and fork carry nothing for it (the matcher only runs this hook
    # for them so the opt-out below can be sent).
    print("/signal " + json.dumps({"session": sid, "event": ev, "kind": kind}))
if ev == "SessionStart" and kind in ("startup", "resume", "fork", "clear") and os.environ.get("TSOFF") == "1":
    # The RAW value, so the proxy can tell a deliberate "false" from "auto" (the wire cannot). A short
    # closed-shape value, never content. The 16-character cut only shortens what is sent: every off
    # value is at most 5 characters, and the shell above sends nothing unless the WHOLE value is one,
    # so a long value can never be cut into an off value.
    print("/tool-search " + json.dumps({"session": sid, "enable_tool_search": os.environ.get("ENABLE_TOOL_SEARCH", "")[:16]}))
' 2>/dev/null <<<"$STDIN")" || exit 0
[ -n "$BODIES" ] || exit 0
ORIGIN="$(printf '%s' "$ANTHROPIC_BASE_URL" | sed -E 's#^(https?://[^/]+).*#\1#')"
if [ -n "$CGHDR" ]; then HDR="$CGHDR"
elif [ -n "${ANTHROPIC_AUTH_TOKEN:-}" ]; then HDR="Authorization: Bearer $ANTHROPIC_AUTH_TOKEN"
elif [ -n "${ANTHROPIC_API_KEY:-}" ]; then HDR="x-api-key: $ANTHROPIC_API_KEY"
else HDR="x-context-guru-client: hook"; fi # no credential: a harmless header keeps the one send path
# send ROUTE BODY
send() { printf 'header = "%s"\n' "$HDR" | curl -sS -o /dev/null --max-time 2 -K - -H 'Content-Type: application/json' --data "$2" "$ORIGIN$1" >/dev/null 2>&1; }
while IFS= read -r line; do
  route="${line%% *}" body="${line#* }"
  case "$body" in
    *SessionEnd*) send "$route" "$body" ;; # the process is about to exit: finish the one request here
    # Hook groups of one event run in parallel, so a SessionStart event can race the hook that starts
    # the proxy: retry a refused connection twice, a second apart, then give up silently (detached).
    *) ( for _ in 1 2 3; do send "$route" "$body" && break; sleep 1; done & ) >/dev/null 2>&1 ;;
  esac
done <<<"$BODIES"
exit 0
