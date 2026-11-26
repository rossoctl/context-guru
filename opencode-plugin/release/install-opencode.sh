#!/bin/bash
# Install or uninstall context-guru for OpenCode from the latest GitHub release, on macOS or Linux:
#
#   curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-opencode.sh | bash
#   curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-opencode.sh | bash -s uninstall
#
# It downloads the release's OpenCode plugin, checks it against the release's SHA-256, unpacks it, and runs its
# installer (scripts/install_opencode.py). The installer asks before each change: install or update OpenCode,
# and the consent to route OpenCode through context-guru. It asks for no key. The SHA-256 is the only integrity
# check, so a mismatch or a missing sum stops before anything runs.
#
# Everything is inside main, called on the last line: a download cut short runs nothing.
#
# CG_OC_RELEASE_URL: the release folder to download from (a pinned version, or a test server).
# CG_OC_TTY: where the installer reads the user's answers. bash reads this script on stdin, so the answers
# come from the terminal.
# CG_OC_PYTHON: the program that runs the installer (default python3; a test sets a recorder).

main() {
  set -u
  local base="${CG_OC_RELEASE_URL:-https://github.com/rossoctl/context-guru/releases/latest/download}"
  local tty="${CG_OC_TTY:-/dev/tty}"
  local python="${CG_OC_PYTHON:-python3}"
  local sums="context-guru-opencode-SHA256SUMS"
  local action="${1:-install}"
  case "$action" in
    install | uninstall) ;;
    *) printf 'usage: install-opencode.sh [install|uninstall]\n' >&2; exit 2 ;;
  esac

  stop() { printf 'context-guru: %s\n' "$1" >&2; exit 1; }

  case "$(uname -s)" in
    Darwin | Linux) ;;
    *) stop "this installer is for macOS and Linux." ;;
  esac
  # Open it, not just test it: /dev/tty is readable by mode even when there is no controlling terminal.
  { : < "$tty"; } 2>/dev/null || stop "no terminal to ask you. Run it in a terminal window."
  command -v "$python" >/dev/null 2>&1 || stop "python3 is needed. Install Python 3, then run this again."
  local check
  if command -v shasum >/dev/null 2>&1; then check="shasum -a 256 -c -s"
  elif command -v sha256sum >/dev/null 2>&1; then check="sha256sum -c --status"
  else stop "shasum or sha256sum is needed to check the download."
  fi

  local work
  work=$(mktemp -d "${TMPDIR:-/tmp}/cg-opencode-install.XXXXXX") || stop "cannot make a temp folder."
  # The path is expanded now: the trap runs after main returns, when the local is gone. mktemp makes the
  # name, so it holds no quote.
  trap "rm -rf '$work'" EXIT

  printf 'Downloading context-guru for OpenCode…\n'
  curl -fsSL "$base/$sums" -o "$work/$sums" || stop "cannot download $base/$sums."
  # One line: "<sha256>  context-guru-opencode-<version>.tar.gz". The name is taken only from that list.
  local digest name
  read -r digest name < "$work/$sums" || stop "$sums is empty."
  case "$name" in
    context-guru-opencode-*.tar.gz) ;;
    *) stop "$sums does not name a context-guru-opencode tarball." ;;
  esac
  case "$name" in
    */* | *..*) stop "$sums names an unsafe file: $name" ;;
  esac
  curl -fsSL "$base/$name" -o "$work/$name" || stop "cannot download $base/$name."
  (cd "$work" && printf '%s  %s\n' "$digest" "$name" | $check) ||
    stop "the download does not match its SHA-256. Nothing was installed. Try again later."
  printf '✓ download matches SHA-256\n'

  mkdir "$work/unpacked" && tar -xzf "$work/$name" -C "$work/unpacked" || stop "cannot unpack $name."
  local installer="$work/unpacked/opencode-plugin/scripts/install_opencode.py"
  [ -f "$installer" ] || stop "the download has no installer."

  local code=0
  # --downloaded: the installer shows this download as step 1 of its numbered steps.
  "$python" "$installer" "$action" --source "$work/unpacked/opencode-plugin" --downloaded < "$tty" || code=$?
  rm -rf "$work"
  trap - EXIT
  return "$code"
}

main "$@"
