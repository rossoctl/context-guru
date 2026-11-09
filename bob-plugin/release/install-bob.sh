#!/bin/bash
# Install or uninstall context-guru for IBM Bob from the latest GitHub release:
#
#   curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-bob.sh | bash
#   curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-bob.sh | bash -s uninstall
#
# It downloads the release zip, checks it against the release's SHA-256, and runs the installer or
# the uninstaller in it. A file curl downloads has no quarantine mark, so macOS does not block the
# unsigned installer. The SHA-256 is the only integrity check, so a mismatch or a missing sum stops
# before anything runs.
#
# Everything is inside main, called on the last line: a download cut short runs nothing.
#
# CG_BOB_RELEASE_URL: the release folder to download from (a pinned version, or a test server).
# CG_BOB_TTY: where the installer reads the user's answers. bash reads this script on stdin, so the
# answers come from the terminal.

main() {
  set -u
  local base="${CG_BOB_RELEASE_URL:-https://github.com/rossoctl/context-guru/releases/latest/download}"
  local tty="${CG_BOB_TTY:-/dev/tty}"
  local sums="context-guru-bob-SHA256SUMS"
  local script="Install context-guru for Bob.command"
  case "${1:-install}" in
    install) ;;
    uninstall) script="Uninstall context-guru for Bob.command" ;;
    *) printf 'usage: install-bob.sh [install|uninstall]\n' >&2; exit 2 ;;
  esac

  stop() { printf 'context-guru: %s\n' "$1" >&2; exit 1; }

  [ "$(uname -s)" = Darwin ] || stop "this installer is for macOS only."
  [ -r "$tty" ] || stop "no terminal to ask you. Download the zip from the release page instead."

  local work
  work=$(mktemp -d "${TMPDIR:-/tmp}/cg-bob-install.XXXXXX") || stop "cannot make a temp folder."
  trap 'rm -rf "$work"' EXIT

  printf 'Downloading context-guru for IBM Bob…\n'
  curl -fsSL "$base/$sums" -o "$work/$sums" || stop "cannot download $base/$sums."
  # One line: "<sha256>  context-guru-bob-<version>.zip". The name is taken only from that list.
  local digest name
  read -r digest name < "$work/$sums" || stop "$sums is empty."
  case "$name" in
    context-guru-bob-*.zip) ;;
    *) stop "$sums does not name a context-guru-bob zip." ;;
  esac
  case "$name" in
    */* | *..*) stop "$sums names an unsafe file: $name" ;;
  esac
  curl -fsSL "$base/$name" -o "$work/$name" || stop "cannot download $base/$name."
  (cd "$work" && printf '%s  %s\n' "$digest" "$name" | shasum -a 256 -c -s) ||
    stop "the download does not match its SHA-256. Nothing was installed. Try again later."
  printf '✓ download matches SHA-256\n'

  /usr/bin/unzip -q "$work/$name" -d "$work/unzipped" || stop "cannot unzip $name."
  local found
  found=$(find "$work/unzipped" -mindepth 2 -maxdepth 2 -name "$script" -type f)
  [ -n "$found" ] || stop "the zip has no $script."

  # Not exec: the EXIT trap must still remove the temp folder.
  /bin/bash "$found" < "$tty"
}

main "$@"
