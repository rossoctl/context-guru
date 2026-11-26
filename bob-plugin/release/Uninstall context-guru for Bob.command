#!/bin/bash
# Remove context-guru from IBM Bob. macOS only. Double-click to run.
#
# It removes the shell file block, the two hooks, the proxy service, the status bar item, the
# plugin folder and the install files, then checks that nothing is left. It keeps your dashboard
# history unless you say to delete it. It works when the plugin folder is gone: it then uses the
# plugin in this folder (only when it matches SHA256SUMS), or the reset script setup copied out.
# It deletes nothing of ours until the undo has fully worked, so you can always run it again.
# --yes skips the questions and always keeps the history (the installer uses it).
# The log holds steps and results only.
set -u

YES=0
[ "${1:-}" = --yes ] && YES=1

HERE=$(cd "$(dirname "$0")" && pwd)
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru-bob"
DEST="$HOME/.bob/plugins/context-guru"
LOG="$HOME/Library/Logs/context-guru-bob-install.log"
LABEL=com.rossoctl.context-guru.bob
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
SETTINGS="$HOME/.bob/settings/settings.json"
EXTENSION=rossoctl.context-guru-bob
BOB_PROCESS='IBM Bob.app/Contents/MacOS/IBM Bob'
QUIT_WAIT_S=${CG_BOB_QUIT_WAIT_S:-30}

log() {
  mkdir -p "$(dirname "$LOG")" 2>/dev/null
  (umask 077; printf '%s uninstall: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >> "$LOG") 2>/dev/null
  return 0
}
say() { printf '%s\n' "$*"; }
stop() { say "$1"; log "stopped: $2"; exit "${3:-1}"; }

# Bob's command line, as bob_plugin.find_bobide finds it.
bobide() {
  for path in ${CONTEXT_GURU_BOBIDE:+"$CONTEXT_GURU_BOBIDE"} \
      "$HOME/Applications/IBM Bob.app/Contents/Resources/app/bin/bobide" \
      "/Applications/IBM Bob.app/Contents/Resources/app/bin/bobide"; do
    if [ -x "$path" ]; then "$path" "$@"; return; fi
  done
  return 127
}

# Return 0 when folder $1 is the package as shipped: no links, every file listed in SHA256SUMS,
# every sum right. Else print why and return 1. Finder's .DS_Store files are allowed.
verify_package() {
  [ -f "$1/SHA256SUMS" ] || { say "SHA256SUMS is missing"; return 1; }
  link=$(find "$1" -type l -print | head -n 1)
  [ -z "$link" ] || { say "The package holds a link: ${link#"$1"/}"; return 1; }
  extra=$(cd "$1" && comm -23 \
    <(find . -type f ! -name SHA256SUMS ! -name .DS_Store | sed 's|^\./||' | LC_ALL=C sort) \
    <(sed 's/^[0-9a-f]*  //' SHA256SUMS | LC_ALL=C sort) | head -n 1)
  [ -z "$extra" ] || { say "A file is not listed in SHA256SUMS: $extra"; return 1; }
  (cd "$1" && shasum -a 256 -c -s SHA256SUMS) >/dev/null 2>&1 ||
    { say "A file does not match SHA256SUMS"; return 1; }
}

# The plugin folder is ours when its plugin.json has our id and home page.
plugin_is_ours() {
  [ -d "$1" ] && [ ! -L "$1" ] && grep -q '"id": "context-guru"' "$1/plugin.json" 2>/dev/null &&
    grep -q '"homepage": "https://github.com/rossoctl/context-guru"' "$1/plugin.json" 2>/dev/null
}

# The files in the state folder that the install owns and that exist: the binary, the hooks
# backup, the rc backup, the proxy config, the record, the reset script, the proxy log, the lock.
# Not the dashboard history (dashboard-PORT.db and its -wal, -shm files): that is the user's.
owned() {
  for path in "$STATE/bin" "$STATE/hooks" "$STATE/backup" "$STATE/install.json" \
      "$STATE/install.tmp" "$STATE/context-guru-reset" "$STATE/proxy.log" "$STATE/lock" \
      "$STATE"/bob-*.yaml "$STATE"/bob-*.tmp; do
    [ -e "$path" ] || [ -L "$path" ] && printf '%s\n' "$path"
  done
  return 0
}

[ "$YES" = 1 ] || say "context-guru for IBM Bob: uninstaller"
log "start yes=$YES"
[ "$(uname -s)" = Darwin ] || stop "This uninstaller is for macOS only." not_macos
if [ -e "$DEST" ] && ! plugin_is_ours "$DEST"; then
  stop "$DEST is not ours (it is not the context-guru plugin). Nothing changed." plugin_not_ours
fi

# ---------------------------------------------------------------- fully quit Bob
# A Bob that keeps running keeps the proxy address after the proxy is gone.
if pgrep -f "$BOB_PROCESS" >/dev/null 2>&1; then
  say "Quitting IBM Bob…"
  osascript -e 'quit app "IBM Bob"' >/dev/null 2>&1
  waited=0
  while pgrep -f "$BOB_PROCESS" >/dev/null 2>&1 && [ "$waited" -lt "$QUIT_WAIT_S" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  if pgrep -f "$BOB_PROCESS" >/dev/null 2>&1; then
    stop "IBM Bob is still running. Quit it (Bob menu, Quit IBM Bob), then run this file again.
Nothing changed." bob_still_running
  fi
fi
log "bob_quit=ok"

# ---------------------------------------------------------------- what removes the changes
# The installed plugin, else the one in this folder. Both need Python. Else the reset script,
# which needs no Python but leaves the status bar item (removed below with bobide).
TOOL=""
PACKAGE_OK=0
[ -d "$HERE/plugin" ] && verify_package "$HERE" >/dev/null && PACKAGE_OK=1
if python3 -I -c 'import sys' >/dev/null 2>&1; then
  if [ -f "$DEST/scripts/bob_plugin.py" ]; then
    TOOL=python; PLUGIN="$DEST/scripts/bob_plugin.py"
  elif [ "$PACKAGE_OK" = 1 ]; then
    TOOL=python; PLUGIN="$HERE/plugin/scripts/bob_plugin.py"
  fi
fi
if [ -z "$TOOL" ]; then
  if [ -f "$STATE/context-guru-reset" ]; then
    TOOL=reset; RESET="$STATE/context-guru-reset"
  elif [ "$PACKAGE_OK" = 1 ]; then
    TOOL=reset; RESET="$HERE/plugin/scripts/reset.sh"
  fi
fi
log "tool=${TOOL:-none}"
if [ -z "$TOOL" ]; then
  stop "Nothing here can remove context-guru: the plugin and the reset script are gone, or this
folder does not match SHA256SUMS. Download the release again, then run this file again from it.
Nothing changed." no_tool
fi
# Read before the undo removes the record: the listener check below needs it.
PORT=$(sed -n 's/^  "port": \([0-9][0-9]*\),\{0,1\}$/\1/p' "$STATE/install.json" 2>/dev/null | head -n 1)

run_tool() {
  if [ "$TOOL" = python ]; then
    (cd "$HOME" && python3 -I -B "$PLUGIN" uninstall "$@")
  else
    (cd "$HOME" && /bin/sh "$RESET" "$@")
  fi
}

if [ "$YES" = 0 ]; then
  say ""
  say "What it removes:"
  run_tool --dry-run 2>&1 | sed 's/^/  /'
  say "  plugin=$DEST"
  say "  install files in $STATE (your dashboard history is kept unless you say so)"
  say ""
  printf 'Remove context-guru from IBM Bob? [y/N] '
  read -r answer || answer=""
  case "$answer" in
    y|Y|yes|YES|Yes) log "consent=yes" ;;
    *) say "Nothing changed."; log "consent=no"; exit 0 ;;
  esac
fi

# ---------------------------------------------------------------- undo the changes
OUT=$(mktemp "${TMPDIR:-/tmp}/cg-bob-uninstall.XXXXXX")
trap 'rm -f "$OUT"' EXIT
if [ "$TOOL" = python ]; then run_tool > "$OUT" 2>&1; else run_tool --yes > "$OUT" 2>&1; fi
code=$?
result=$(sed -n 's/^result=//p' "$OUT" | tail -n 1)
log "remove result=${result:-none} exit=$code tool=$TOOL"
[ "$YES" = 1 ] || sed 's/^/  /' "$OUT"
if [ "$code" != 0 ] || [ "$result" != removed ]; then
  [ "$YES" = 1 ] && sed 's/^/  /' "$OUT"
  say ""
  say "Some parts are left (see the lines above). The plugin and its install files are kept, so"
  say "you can fix that part and run this file again. Log: $LOG"
  exit 1
fi

# ---------------------------------------------------------------- remove what the install owns
# The status bar item: the reset script does not remove it, and a lost record forgets it.
if bobide --list-extensions 2>/dev/null | grep -qx "$EXTENSION"; then
  bobide --uninstall-extension "$EXTENSION" >/dev/null 2>&1
  log "statusbar_scan=removed"
fi
rm -rf "$DEST"
rmdir "$HOME/.bob/plugins" 2>/dev/null
if [ -d "$STATE" ]; then
  owned | while IFS= read -r path; do rm -rf "$path"; done
  if [ -n "$(ls -A "$STATE")" ] && [ "$YES" = 0 ]; then
    printf 'Also delete your context-guru dashboard history? [y/N] '
    read -r answer || answer=""
    case "$answer" in
      y|Y|yes|YES|Yes) rm -rf "$STATE"; log "history=deleted" ;;
      *) say "Your dashboard history is kept in $STATE"; log "history=kept" ;;
    esac
  fi
  rmdir "$STATE" 2>/dev/null
fi

# ---------------------------------------------------------------- check nothing is left
ROWS=""
FAILED=""
check() {
  if [ "$1" = 0 ]; then ROWS="${ROWS}✓ $2
"; else ROWS="${ROWS}✗ $2
"; FAILED=1; fi
  log "verify $2 = $([ "$1" = 0 ] && echo ok || echo fail)"
}
block=0
for name in .bash_profile .bash_login .profile .zshrc; do
  grep -q '^# >>> context-guru (bob) >>>$' "$HOME/$name" 2>/dev/null && block=1
done
check "$block" "no context-guru block in your shell files"
hooks=0
grep -q '# context-guru-bob-hook' "$SETTINGS" 2>/dev/null && hooks=1
check "$hooks" "no context-guru hooks in Bob's settings"
agent=0
[ -e "$PLIST" ] && agent=1
check "$agent" "no proxy service"
extension=0
bobide --list-extensions 2>/dev/null | grep -qx "$EXTENSION" && extension=1
check "$extension" "no status bar item in Bob"
leftover=0
[ -e "$DEST" ] && leftover=1
[ -n "$(owned)" ] && leftover=1
check "$leftover" "no plugin folder or install files"
# Only our port: another program may use a port in 8891-8990. launchd stops the proxy soon after
# bootout, not at once.
if [ -n "$PORT" ]; then
  listener=1
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if ! lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then listener=0; break; fi
    sleep 0.5
  done
  check "$listener" "the proxy no longer listens on port $PORT"
fi
say ""
printf '%s' "$ROWS"

if [ -n "$FAILED" ]; then
  [ "$YES" = 1 ] && sed 's/^/  /' "$OUT"
  say "Some parts are left (✗ above). Fix them, then run this file again. Log: $LOG"
  log "result=partly_removed"
  exit 1
fi
log "result=removed"
if [ "$YES" = 0 ]; then
  say ""
  say "Done. context-guru is removed from IBM Bob."
  say "Open Bob from the Dock and log in once. Bob keeps its login per backend address."
fi
exit 0
