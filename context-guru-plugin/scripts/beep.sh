#!/usr/bin/env bash
# Stop / Notification hook: play a short sound when Claude finishes a turn or is blocked on you.
#
# LOCAL ONLY. Nothing is sent to the proxy or anywhere else, and the hook payload on stdin is
# never read.
#
# SWITCH. On by default. `off` (CONTEXT_GURU_BEEP, else the `beep` plugin option) plays nothing.
# The environment variable wins, so one shell can differ from the saved option.
#
# PLAYER. The first of afplay (macOS), paplay, aplay (Linux) that is installed and has a sound
# file to play; else a terminal bell on the controlling terminal. The player runs detached, so
# the hook returns at once.
#
# Never blocks and never fails: every exit is 0. A machine with no way to play a sound stays silent.
set -uo pipefail
MODE="${CONTEXT_GURU_BEEP:-${CLAUDE_PLUGIN_OPTION_BEEP:-on}}"
case "$MODE" in
  off | 0 | false | no) exit 0 ;;
esac
cat >/dev/null 2>&1 # drain the payload so Claude Code never sees a broken pipe

play() { ( "$@" >/dev/null 2>&1 & ) >/dev/null 2>&1; }
if command -v afplay >/dev/null 2>&1 && [ -f /System/Library/Sounds/Ping.aiff ]; then
  play afplay /System/Library/Sounds/Ping.aiff
elif command -v paplay >/dev/null 2>&1 && [ -f /usr/share/sounds/freedesktop/stereo/complete.oga ]; then
  play paplay /usr/share/sounds/freedesktop/stereo/complete.oga
elif command -v aplay >/dev/null 2>&1 && [ -f /usr/share/sounds/alsa/Front_Center.wav ]; then
  play aplay -q /usr/share/sounds/alsa/Front_Center.wav
else
  printf '\a' >/dev/tty 2>/dev/null
fi
exit 0
