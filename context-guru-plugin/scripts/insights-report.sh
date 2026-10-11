#!/usr/bin/env bash
# Generate the unused-capability report and print how to open it.
#
# WHY THIS EXISTS. insights.py --report prints a path, and the page it writes prints a command to
# regenerate itself at a different session window. Both are correct and neither is runnable without
# knowing where this script lives: the plugin is installed into a versioned cache directory nobody
# ever cd's into, so `python3 insights.py` resolves from nowhere. A reader who wants a report at a
# different window had to reconstruct an absolute path by hand from a page that could not tell them
# what it was.
#
# So this wrapper is the one thing a user needs to remember. It finds its own directory, runs the
# report, and tells them the single command to open it. Everything it does is read-only except
# writing the report file itself.
#
# It deliberately does NOT open a browser. On a headless box or over SSH that is an error rather
# than a convenience, and a command somebody ran to read numbers should not take over their screen.
# The path is printed; opening it is their decision.
set -uo pipefail

HERE="$(unset CDPATH; \cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)" || {
  echo "insights-report: cannot resolve my own directory" >&2
  exit 1
}

SCRIPT="$HERE/insights.py"
[ -r "$SCRIPT" ] || { echo "insights-report: $SCRIPT not found" >&2; exit 1; }

AREA="capabilities"
SESSIONS=""
OPEN=0

usage() {
  cat <<'USAGE'
usage: insights-report.sh [--sessions N] [--area AREA] [--open]

Writes the unused-capability report and prints its path.

  --sessions N   analyse only the N most recently active sessions. Use this when a plugin was
                 installed recently: over all history its skills are averaged across sessions
                 that never declared them, which understates what they cost now.
  --area AREA    capabilities (default), components, idle, env or all.
  --open         also open the report in the default browser. Off by default: on a headless
                 box or over SSH, launching a browser is an error, not a convenience.

Nothing here changes your configuration. The report prints commands for you to run yourself.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --sessions)
      shift
      [ $# -gt 0 ] || { echo "insights-report: --sessions needs a number" >&2; exit 2; }
      case "$1" in
        ''|*[!0-9]*) echo "insights-report: --sessions wants a positive integer, got '$1'" >&2; exit 2;;
      esac
      [ "$1" -gt 0 ] || { echo "insights-report: --sessions must be at least 1" >&2; exit 2; }
      SESSIONS="$1"
      ;;
    --sessions=*) set -- "--sessions" "${1#*=}" "${@:2}"; continue;;
    --area)
      shift
      [ $# -gt 0 ] || { echo "insights-report: --area needs a value" >&2; exit 2; }
      case "$1" in
        capabilities|components|idle|env|all) AREA="$1";;
        *) echo "insights-report: unknown area '$1' (capabilities|components|idle|env|all)" >&2; exit 2;;
      esac
      ;;
    --area=*) set -- "--area" "${1#*=}" "${@:2}"; continue;;
    --open) OPEN=1;;
    -h|--help) usage; exit 0;;
    *) echo "insights-report: unknown argument '$1'" >&2; usage >&2; exit 2;;
  esac
  shift
done

command -v python3 >/dev/null 2>&1 || {
  echo "insights-report: python3 is not on PATH" >&2
  exit 1
}

ARGS=("$AREA" "--report")
[ -n "$SESSIONS" ] && ARGS+=("--sessions" "$SESSIONS")

# The script emits flat key=value lines; the path is one of them. Captured rather than streamed so
# a failure can be reported with its own output instead of a bare exit code.
OUT="$(python3 "$SCRIPT" "${ARGS[@]}" 2>&1)" || {
  echo "insights-report: the report failed to generate:" >&2
  printf '%s\n' "$OUT" >&2
  exit 1
}

PATH_LINE="$(printf '%s\n' "$OUT" | sed -n 's/^report_path=//p' | tail -1)"
REFUSED="$(printf '%s\n' "$OUT" | sed -n 's/^report_refused=//p' | tail -1)"

if [ -n "$REFUSED" ]; then
  # Not an error: there was nothing to measure, and a page of zeroes would read as a clean bill of
  # health. The script's own findings carry the fix.
  echo "No report written: $REFUSED"
  printf '%s\n' "$OUT" | sed -n 's/^finding\.[0-9]*\.\(title\|fix\)=/  /p'
  # The most likely cause by far is the working directory, not a missing install: the port is
  # resolved from the settings files in scope, so running this from anywhere else declines. Say so,
  # because "nothing routes this directory" is easy to read as "context-guru is not installed".
  echo
  echo "Run this from the project that is routed through the proxy. The port comes from that"
  echo "directory's settings, so from anywhere else it declines rather than pricing another"
  echo "project's traffic as yours."
  exit 0
fi

[ -n "$PATH_LINE" ] || {
  echo "insights-report: the report ran but printed no path. Its output was:" >&2
  printf '%s\n' "$OUT" >&2
  exit 1
}

ANALYZED="$(printf '%s\n' "$OUT" | sed -n 's/^window_sessions_analyzed=//p' | tail -1)"
REMOVABLE="$(printf '%s\n' "$OUT" | sed -n 's/^inventory_removable_usd=//p' | tail -1)"
ITEMS="$(printf '%s\n' "$OUT" | sed -n 's/^inventory_items=//p' | tail -1)"

echo "Report written: $PATH_LINE"
[ -n "$ITEMS" ] && [ -n "$REMOVABLE" ] &&
  echo "  ${ITEMS} unused item(s), \$${REMOVABLE} over the measured window"
[ -n "$ANALYZED" ] && echo "  analysed ${ANALYZED} session(s)${SESSIONS:+ (asked for $SESSIONS)}"
echo
echo "Open it:"
case "$(uname -s)" in
  Darwin) echo "  open '$PATH_LINE'";;
  *)      echo "  xdg-open '$PATH_LINE'";;
esac
echo
echo "Different window:"
echo "  '$HERE/insights-report.sh' --sessions 10"

if [ "$OPEN" = "1" ]; then
  case "$(uname -s)" in
    Darwin) open "$PATH_LINE" >/dev/null 2>&1 || echo "insights-report: could not open it" >&2;;
    *) command -v xdg-open >/dev/null 2>&1 &&
         { xdg-open "$PATH_LINE" >/dev/null 2>&1 || echo "insights-report: could not open it" >&2; } ||
         echo "insights-report: no xdg-open on PATH; open the path above yourself" >&2;;
  esac
fi
