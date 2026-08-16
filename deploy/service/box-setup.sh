#!/usr/bin/env bash
# Set up IBM Box as context-guru's cold storage.
#
# The OAuth step needs a browser, and this box is headless. So this script does every
# part that can be automated and stops with exact instructions for the one part that
# cannot. Run it, follow what it prints, run it again.
#
#   ./box-setup.sh check     what is installed, what is configured, what is missing
#   ./box-setup.sh install   install rclone (needs sudo)
#   ./box-setup.sh token     print the exact commands for the browser step
#   ./box-setup.sh paste     write a token you obtained elsewhere into the config
#   ./box-setup.sh verify    prove the remote works end to end
#
# Nothing here touches a secret except `paste`, which reads the token from stdin so it
# never lands in shell history, and writes it 0600.

set -euo pipefail

REMOTE="${REMOTE:-box}"
BASE="${BASE:-${REMOTE}:context-guru}"
RCLONE_CONFIG_PATH="${RCLONE_CONFIG_PATH:-$HOME/.config/rclone/rclone.conf}"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
no()  { printf '  \033[31m✗\033[0m %s\n' "$*"; }

cmd_check() {
  say "context-guru Box cold storage — status"
  if command -v rclone >/dev/null 2>&1; then
    ok "rclone installed: $(rclone version | head -1)"
  else
    no "rclone is NOT installed  →  run: $0 install"
    return 0
  fi
  if [ -f "$RCLONE_CONFIG_PATH" ]; then
    ok "config file: $RCLONE_CONFIG_PATH"
  else
    no "no config file at $RCLONE_CONFIG_PATH"
  fi
  if rclone listremotes 2>/dev/null | grep -qx "${REMOTE}:"; then
    ok "remote '${REMOTE}:' is configured"
  else
    no "remote '${REMOTE}:' is NOT configured  →  run: $0 token"
    return 0
  fi
  if rclone --config "$RCLONE_CONFIG_PATH" lsd "${REMOTE}:" >/dev/null 2>&1; then
    ok "remote responds (token is valid)"
  else
    no "remote does not respond — the token may have expired"
    echo "      →  rclone config reconnect ${REMOTE}:"
    echo "         (or re-run: $0 token)"
  fi
}

cmd_install() {
  say "Installing rclone"
  # RHEL 9 has rclone in EPEL, not the base repos. The official install script is the
  # dependency-free route and is what rclone.org documents; it needs no EPEL enablement.
  if command -v dnf >/dev/null 2>&1 && dnf -y install rclone 2>/dev/null; then
    ok "installed from the distribution repositories"
  else
    echo "  Not in the enabled repos. Using the official installer (needs sudo):"
    echo "    sudo -v ; curl https://rclone.org/install.sh | sudo bash"
    echo
    echo "  Or, without root, a user-local install:"
    echo "    mkdir -p ~/.local/bin && cd /tmp \\"
    echo "      && curl -fsSLO https://downloads.rclone.org/rclone-current-linux-amd64.zip \\"
    echo "      && unzip -oq rclone-current-linux-amd64.zip \\"
    echo "      && install -m0755 rclone-*-linux-amd64/rclone ~/.local/bin/rclone"
    return 1
  fi
}

cmd_token() {
  say "Authorizing Box — the one step that needs a browser"
  cat <<'INSTRUCTIONS'
This host is headless, so rclone cannot open the IBM SSO page itself. Pick ONE:

  ── Option A (recommended): authorize on your laptop, paste the token here ──

  1. On YOUR MACHINE (the one with a browser), install rclone:
         macOS:   brew install rclone
         Linux:   sudo dnf install rclone   (or curl https://rclone.org/install.sh | sudo bash)
         Windows: winget install Rclone.Rclone

  2. On YOUR MACHINE, run exactly this — note the empty client id and secret,
     which is what makes rclone use its shared Box app:

         rclone authorize "box" --client-id "" --client-secret ""

  3. Your browser opens. Choose "Use Single Sign on (SSO)", complete the IBM
     login, and grant rclone access.

  4. rclone prints a JSON token between two "---" lines, like:
         {"access_token":"...","token_type":"bearer","refresh_token":"...","expiry":"..."}

  5. Back HERE, paste it into this command (it reads stdin, so the token never
     enters your shell history):

         ./box-setup.sh paste

  ── Option B: forward the callback port from this host to your laptop ──

  1. On YOUR MACHINE, open an SSH tunnel for rclone's callback port:
         ssh -L 53682:localhost:53682 <this-host>
     (In VS Code Remote, add port 53682 in the PORTS panel instead.)

  2. HERE, in that session, run:
         rclone config create box box client_id="" client_secret=""

  3. It prints a http://127.0.0.1:53682/auth?state=... URL. Open it in your
     laptop's browser and complete the IBM SSO. The command finishes on its own.
INSTRUCTIONS
}

cmd_paste() {
  say "Writing the Box token into $RCLONE_CONFIG_PATH"
  command -v rclone >/dev/null 2>&1 || { no "install rclone first: $0 install"; exit 1; }
  echo "Paste the JSON token from 'rclone authorize \"box\"', then press Ctrl-D:"
  TOKEN="$(cat)"
  # A cheap sanity check beats writing junk into the config and getting an opaque
  # failure from the first transfer.
  case "$TOKEN" in
    *access_token*refresh_token*) ;;
    *) no "that does not look like an rclone token (no access_token/refresh_token)"; exit 1 ;;
  esac
  mkdir -p "$(dirname "$RCLONE_CONFIG_PATH")"
  chmod 700 "$(dirname "$RCLONE_CONFIG_PATH")"
  # --non-interactive is load-bearing. Without it `config create` starts its OWN OAuth
  # flow even when handed a complete token, so pasting a token you obtained elsewhere
  # still demands a browser — which is the exact thing this path exists to avoid.
  rclone --config "$RCLONE_CONFIG_PATH" config create "$REMOTE" box \
      --non-interactive \
      client_id="" client_secret="" token="$TOKEN" box_sub_type=user >/dev/null
  chmod 600 "$RCLONE_CONFIG_PATH"
  unset TOKEN
  ok "remote '${REMOTE}:' written, config mode 600"
  cmd_verify
}

cmd_verify() {
  say "Verifying the remote end to end"
  command -v rclone >/dev/null 2>&1 || { no "rclone is not installed"; exit 1; }
  local probe="context-guru/.setup-probe"
  local payload="context-guru cold storage probe $(date -u +%FT%TZ)"

  printf '%s' "$payload" | rclone --config "$RCLONE_CONFIG_PATH" rcat "${REMOTE}:${probe}"
  ok "wrote  ${REMOTE}:${probe}"
  local got
  got="$(rclone --config "$RCLONE_CONFIG_PATH" cat "${REMOTE}:${probe}")"
  [ "$got" = "$payload" ] || { no "read back does not match what was written"; exit 1; }
  ok "read back, byte-identical"
  rclone --config "$RCLONE_CONFIG_PATH" deletefile "${REMOTE}:${probe}"
  ok "deleted the probe"

  say "Cold storage is ready. Point the service at it:"
  cat <<EOF
  ARCHIVE_REMOTE=${BASE}
  RCLONE_CONFIG=${RCLONE_CONFIG_PATH}

Add those to the systemd unit (deploy/service/context-guru.service) and restart:
  sudo systemctl restart context-guru

Then confirm the proxy agrees, in its log:
  journalctl -u context-guru | grep 'cold storage'
EOF
}

case "${1:-check}" in
  check)   cmd_check ;;
  install) cmd_install ;;
  token)   cmd_token ;;
  paste)   cmd_paste ;;
  verify)  cmd_verify ;;
  *) echo "usage: $0 {check|install|token|paste|verify}" >&2; exit 2 ;;
esac
