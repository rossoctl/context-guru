#!/bin/bash
# Install context-guru for IBM Bob from this release folder. macOS only. Double-click to run.
#
# It works offline: the proxy, the plugin and the status bar are in this folder. It changes
# nothing until you say yes. On any failure it runs the uninstaller next to it, which puts your
# files back byte for byte. The log holds steps and results only: never your answers, prompts,
# tokens or file contents. Ctrl-C after the first change undoes the install too.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
UNINSTALLER="$HERE/Uninstall context-guru for Bob.command"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru-bob"
DEST="$HOME/.bob/plugins/context-guru"
LOG="$HOME/Library/Logs/context-guru-bob-install.log"
BOB_PROCESS='IBM Bob.app/Contents/MacOS/IBM Bob'
QUIT_WAIT_S=${CG_BOB_QUIT_WAIT_S:-30}

log() {
  mkdir -p "$(dirname "$LOG")" 2>/dev/null
  (umask 077; printf '%s install: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >> "$LOG") 2>/dev/null
  return 0
}
say() { printf '%s\n' "$*"; }
stop() { say "$1"; log "stopped: $2"; exit "${3:-1}"; }
# The value of one key=value line in a file of facts, or nothing.
fact() { sed -n "s/^$1=//p" "$2" | head -n 1; }

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

say "context-guru for IBM Bob: installer"
log "start package=$HERE"

# ---------------------------------------------------------------- 1. preflight (reads only)
[ "$(uname -s)" = Darwin ] || stop "This installer is for macOS only." not_macos
case "$(uname -m)" in
  arm64) ARCH=darwin-arm64 ;;
  x86_64) ARCH=darwin-amd64 ;;
  *) stop "This Mac's processor ($(uname -m)) is not supported." "arch=$(uname -m)" ;;
esac
log "arch=$ARCH"

if [ -n "${CONTEXT_GURU_BOB_APP:-}" ]; then
  BOB_APP=$CONTEXT_GURU_BOB_APP
elif [ -d "$HOME/Applications/IBM Bob.app" ]; then
  BOB_APP="$HOME/Applications/IBM Bob.app"
else
  BOB_APP="/Applications/IBM Bob.app"
fi
[ -d "$BOB_APP" ] || stop "Install IBM Bob first, then run this file again." bob_missing
say "✓ IBM Bob is installed."

# Without the Command Line Tools, /usr/bin/python3 is a stub that opens an install dialog. Do not
# start it then: check xcode-select first.
clt_message="Python 3 is missing. Open Terminal, run: xcode-select --install
When that finishes, run this file again."
if [ "$(command -v python3)" = /usr/bin/python3 ] && ! xcode-select -p >/dev/null 2>&1; then
  stop "$clt_message" no_command_line_tools
fi
if ! python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)' >/dev/null 2>&1; then
  if ! xcode-select -p >/dev/null 2>&1; then stop "$clt_message" no_command_line_tools; fi
  stop "python3 does not work, or it is older than 3.9. Install Python 3.9 or newer." python_broken
fi
say "✓ Python 3 works."

BIN_SRC="$HERE/bin/$ARCH/context-guru-proxy"
VSIX="$HERE/plugin/statusbar/context-guru-bob.vsix"
for need in "$BIN_SRC" "$HERE/plugin/scripts/bob_plugin.py" "$HERE/plugin/scripts/reset.sh" \
            "$UNINSTALLER" "$HERE/SHA256SUMS"; do
  [ -f "$need" ] || stop "The package is not complete: ${need#"$HERE"/} is missing. Download it again." incomplete
done
if ! why=$(verify_package "$HERE"); then
  stop "The package is damaged: $why. Download the package again." package_check_failed
fi
HAVE_VSIX=1
[ -f "$VSIX" ] || HAVE_VSIX=0
say "✓ The package is complete."
log "preflight=ok vsix=$HAVE_VSIX"

# ---------------------------------------------------------------- 2. already installed?
if [ -e "$DEST" ] && ! plugin_is_ours "$DEST"; then
  stop "$DEST is not ours (it is not the context-guru plugin). Move it away, then run this file
again. Nothing changed." plugin_not_ours
fi
PLIST="$HOME/Library/LaunchAgents/com.rossoctl.context-guru.bob.plist"
installed=0
{ [ -f "$STATE/install.json" ] || [ -e "$DEST" ]; } && installed=1
[ -f "$PLIST" ] && /usr/bin/plutil -extract ContextGuruManaged raw -o - "$PLIST" 2>/dev/null |
  grep -qx true && installed=1
for name in .bash_profile .bash_login .profile .zshrc; do
  grep -q '^# >>> context-guru (bob) >>>$' "$HOME/$name" 2>/dev/null && installed=1
done
REINSTALL=0
if [ "$installed" = 1 ]; then
  say ""
  say "context-guru for Bob is already installed."
  say "Reinstall removes the old install first. It keeps your dashboard history."
  printf 'Type r to reinstall, or q to quit. [r/Q] '
  read -r answer || answer=""
  case "$answer" in
    r|R) REINSTALL=1; log "already_installed choice=reinstall" ;;
    *) say "Nothing changed."; log "already_installed choice=quit"; exit 0 ;;
  esac
fi

# ---------------------------------------------------------------- 3. the plan and one question
PLAN=$(mktemp "${TMPDIR:-/tmp}/cg-bob-plan.XXXXXX")
trap 'rm -f "$PLAN"' EXIT
(cd "$HOME" && python3 -I -B "$HERE/plugin/scripts/bob_plugin.py" setup --plan) > "$PLAN" 2>&1 ||
  stop "Setup could not make its plan. Nothing changed. Log: $LOG" plan_failed
say ""
say "The plan:"
grep -v '^consent_question=' "$PLAN" | sed 's/^/  /'
say ""
say "This adds a block to your shell file, two hooks to Bob's settings, a local proxy service,"
say "the plugin folder and a status bar item in Bob. Bob's model traffic then goes through the"
say "proxy on this Mac. 'Uninstall context-guru for Bob.command' undoes all of it."
printf 'Install context-guru for Bob? [y/N] '
read -r answer || answer=""
case "$answer" in
  y|Y|yes|YES|Yes) log "consent=yes" ;;
  *) say "Nothing changed."; log "consent=no"; exit 0 ;;
esac

# ---------------------------------------------------------------- 4. fully quit Bob
# A Bob that keeps running keeps its old backend address.
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
say "✓ IBM Bob is not running."
log "bob_quit=ok"

# ---------------------------------------------------------------- 5. install
if [ "$REINSTALL" = 1 ]; then
  say "Removing the old install…"
  if ! /bin/bash "$UNINSTALLER" --yes > "$PLAN" 2>&1; then
    sed 's/^/  /' "$PLAN"
    stop "The old install could not be removed. Log: $LOG" reinstall_uninstall_failed
  fi
  log "reinstall_uninstall=ok"
fi

ROWS=""
FAILED=""
row() { ROWS="$ROWS$1 $2
"; log "verify $2 = $1"; }
fail() { row "✗" "$1"; FAILED="${FAILED:+$FAILED, }$1"; }

rollback() {
  trap '' HUP INT TERM
  say ""
  if [ -n "${1:-}" ]; then
    FAILED="${FAILED:+$FAILED, }interrupted"
    say "The install was interrupted. Undoing it…"
  else
    say "Something failed. Undoing the install…"
  fi
  if /bin/bash "$UNINSTALLER" --yes > "$PLAN" 2>&1; then
    if [ "$REINSTALL" = 1 ]; then
      # The old install was removed before this one started, so there is nothing older to restore.
      say "Undone. The old install was removed first, so context-guru is not installed now."
      say "Bob talks to IBM directly. Run this file again to install it."
    else
      say "Undone. Your files are as they were before."
    fi
    log "rollback=ok"
  else
    sed 's/^/  /' "$PLAN"
    say "The undo did not finish. Run 'Uninstall context-guru for Bob.command' again."
    log "rollback=failed"
  fi
  say "What failed: $FAILED"
  say "Log: $LOG"
  exit 1
}

say "Installing…"
# From the first change on, Ctrl-C or a closed window undoes the install.
trap 'rollback signal' HUP INT TERM
BIN="$STATE/bin/context-guru-proxy"
mkdir -p "$HOME/.bob/plugins" && rm -rf "$DEST" && cp -R "$HERE/plugin" "$DEST" &&
  mkdir -p -m 700 "$STATE" && mkdir -p "$STATE/bin" && cp "$BIN_SRC" "$BIN" && chmod 0755 "$BIN"
if [ $? -ne 0 ]; then fail "copy files"; printf '%s' "$ROWS"; rollback; fi
# A downloaded zip marks every file. Gatekeeper then blocks the proxy when launchd starts it.
xattr -dr com.apple.quarantine "$DEST" "$BIN" 2>/dev/null
log "copied plugin and binary"
# Bob runs a chat with no folder open in ~/.bob/playground but never creates it. Without it every
# shell command in that chat fails, our skills too. The folder is Bob's: uninstall keeps it.
if [ -d "$HOME/.bob/playground" ]; then
  log "playground=present"
elif mkdir -p "$HOME/.bob/playground"; then
  log "playground=created"
else
  log "playground=failed"
fi

# Check the Bob gateway here: setup would otherwise try to download a proxy from GitHub.
if ! "$BIN" --help 2>&1 | grep -q -- '-bob-upstream'; then
  fail "binary (this proxy build has no Bob support)"
  printf '%s' "$ROWS"
  rollback
fi

# CONTEXT_GURU_BIN: use our copy, never another context-guru-proxy on PATH.
(cd "$HOME" && CONTEXT_GURU_BIN="$BIN" python3 -I -B "$DEST/scripts/bob_plugin.py" setup \
   --i-consent-to-traffic-interception) > "$PLAN" 2>&1
log "setup result=$(fact result "$PLAN") rc_probe=$(fact rc_probe "$PLAN") statusbar=$(fact statusbar "$PLAN")"

# ---------------------------------------------------------------- 6. verify every step
if [ "$(fact result "$PLAN")" = installed ] && [ "$(fact rc_probe "$PLAN")" = ok ]; then
  row "✓" "setup"
else
  fail "setup"
  sed 's/^/  /' "$PLAN"
fi
PORT=$(fact port "$PLAN")
if [ -n "$PORT" ] && [ "$(curl -fsS --max-time 3 "http://127.0.0.1:$PORT/healthz" 2>/dev/null)" = ok ]; then
  row "✓" "proxy /healthz"
else
  fail "proxy /healthz"
fi
STATUSBAR=$(fact statusbar "$PLAN")
if [ -z "$FAILED" ]; then
  (cd "$HOME" && python3 -I -B "$DEST/scripts/bob_plugin.py" doctor) > "$PLAN" 2>&1
  if grep -q '=fail$' "$PLAN"; then fail "doctor"; grep -A1 '=fail$' "$PLAN" | sed 's/^/  /'
  else row "✓" "doctor"; fi
  (cd "$HOME" && python3 -I -B "$DEST/scripts/bob_plugin.py" status) > "$PLAN" 2>&1
  if grep -qx 'routed=true' "$PLAN" && grep -qx 'proxy_up=true' "$PLAN"; then row "✓" "status"
  else fail "status"; fi
  if [ "$STATUSBAR" = installed ]; then row "✓" "status bar"
  elif [ "$HAVE_VSIX" = 0 ]; then row "–" "status bar: not in this package. Bob works without it."
  else fail "status bar"; fi
fi
say ""
printf '%s' "$ROWS"
[ -z "$FAILED" ] || rollback

# ---------------------------------------------------------------- 8. done
trap - HUP INT TERM
log "result=installed port=$PORT"
say ""
say "Done. context-guru is installed for IBM Bob."
say "Open IBM Bob from the Dock. Bob asks you to log in once — this is expected."
say "To remove it, double-click 'Uninstall context-guru for Bob.command' in this folder."
exit 0
