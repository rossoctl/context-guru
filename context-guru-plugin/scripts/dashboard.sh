#!/usr/bin/env bash
# Open this install's dashboard on the two tabs an advanced user decides removals from.
#
#   History  — what you installed and never used, across your whole history, including the months
#              before context-guru was installed. Counts only: there is no money in it, because a
#              transcript records what a request spent in total, never which tools it carried.
#   Cleanup  — what carrying each unused capability actually cost, priced from traffic this proxy
#              measured. Only the time since install, and the only place a dollar figure comes from.
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
# Nothing here writes or applies anything. Both pages show what could be removed; neither removes it.
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
HISTORY="${BASE}#behaviour/history"
CLEANUP="${BASE}#behaviour/insights"

# /api/history rather than /dashboard/: the page shell is static and would answer even if the API
# behind it were failing, which is the case the reader actually cares about.
if ! curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:${PORT}/api/history" 2>/dev/null; then
  echo "dashboard: the context-guru proxy is not answering on port ${PORT}, so the pages would"
  echo "not load. Start it with:"
  echo "  '$HERE/start-proxy.sh'"
  echo "or run /context-guru:status to see what is wrong."
  exit 1
fi

echo "History  — what you installed and never used, across all your history (no money):"
echo "  $HISTORY"
echo "Cleanup  — what carrying each unused capability has cost since install (real money):"
echo "  $CLEANUP"
echo
echo "Nothing on either page removes anything. Tick items on Cleanup to build the command, then"
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
# History first: it is the page that tells you what you never used. Cleanup prices it.
for url in "$HISTORY" "$CLEANUP"; do
  "$OPENER" "$url" >/dev/null 2>&1 || echo "dashboard: could not open $url; use the URL above" >&2
done
