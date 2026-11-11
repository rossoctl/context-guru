#!/usr/bin/env bash
# Open this install's dashboard on the Cleanup tab, where an advanced user decides removals: what
# carrying each unused capability actually cost, priced from traffic this proxy measured. Only the
# time since install.
#
# THE PORT IS RESOLVED, NEVER GUESSED. Ports are allocated per project, so "the default" is
# whichever project installed first, and a guessed port shows another install's spend under this
# user's name. settings.py does the resolution, in the same order the hooks and the status line use.
#
# THE PROXY IS CHECKED BEFORE ANYTHING OPENS. A browser landing on "connection refused" reads as a
# broken product. This one opens a browser, because opening it is the
# whole point of the command — but the URLs are always printed too, since over SSH or on a headless
# machine the open does nothing and the URLs are then the only way in. `--no-open` prints only.
#
# Nothing here writes or applies anything. The page shows what could be removed; it removes nothing.
set -uo pipefail

HERE="$(unset CDPATH; \cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)" || {
  echo "dashboard: cannot resolve my own directory" >&2
  exit 1
}

OPEN=1
for arg in "$@"; do
  case "$arg" in
    --no-open) OPEN=0 ;;
    -h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "dashboard: unknown argument: $arg (try --no-open)" >&2; exit 2 ;;
  esac
done

# CONTEXT_GURU_DASHBOARD_PORT exists for the tests, which serve a fake dashboard on a port of their
# own. Every real run goes through settings.py.
PORT="${CONTEXT_GURU_DASHBOARD_PORT:-}"
if [ -z "$PORT" ]; then
  RES="$("$HERE/settings.py" port install 2>/dev/null)"
  if [ "$(printf '%s\n' "$RES" | sed -n 's/^result=//p' | head -1)" = "ok" ]; then
    PORT="$(printf '%s\n' "$RES" | sed -n 's/^port=//p' | head -1)"
  fi
fi
case "$PORT" in
  ''|*[!0-9]*)
    echo "dashboard: context-guru is not installed for this directory, so there is no dashboard"
    echo "to open. Install it with /context-guru:install, then run this again."
    exit 1 ;;
esac

BASE="http://127.0.0.1:${PORT}/dashboard/"
CLEANUP="${BASE}#savings/insights"

# A dashboard API route rather than /dashboard/: the page shell is static and would answer even if
# the API behind it were failing, which is the case the reader actually cares about. /api/capture
# reads only in-memory counters, so the probe stays cheap.
if ! curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:${PORT}/api/capture" 2>/dev/null; then
  echo "dashboard: the context-guru proxy is not answering on port ${PORT}, so the pages would"
  echo "not load. Start it with:"
  echo "  '$HERE/start-proxy.sh'"
  echo "or run /context-guru:status to see what is wrong."
  exit 1
fi

echo "Cleanup  — what carrying each unused capability has cost since install (real money):"
echo "  $CLEANUP"
echo
echo "Nothing on the page removes anything. Tick items on Cleanup to build the command, then"
echo "copy it and run it yourself, or tell Claude which ones to remove."

[ "$OPEN" = "1" ] || exit 0

# CONTEXT_GURU_DASHBOARD_OPENER exists for the tests, so no test ever starts a real browser.
OPENER="${CONTEXT_GURU_DASHBOARD_OPENER:-}"
if [ -z "$OPENER" ]; then
  case "$(uname -s)" in
    Darwin) OPENER=open ;;
    *) command -v xdg-open >/dev/null 2>&1 && OPENER=xdg-open ;;
  esac
fi
if [ -z "$OPENER" ]; then
  echo
  echo "dashboard: no way to open a browser here (no xdg-open on PATH); use the URLs above." >&2
  exit 0
fi
"$OPENER" "$CLEANUP" >/dev/null 2>&1 || echo "dashboard: could not open $CLEANUP; use the URL above" >&2
