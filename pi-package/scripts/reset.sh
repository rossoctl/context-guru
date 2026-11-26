#!/bin/sh
# context-guru-reset for Pi: the recovery path when the routed proxy cannot answer.
#
# Setup copies this file (and models_route.py) OUT of the Pi package, into the state directory, so it
# still works when Pi cannot run a command or the package moved. It needs only sh and python3.
# It removes only what context-guru owns: our models.json keys (by the routing record), our service
# definition (by its marker), and the binary we downloaded.
set -eu

DRY=0
YES=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --yes|-y) YES=1 ;;
    -h|--help) echo "usage: context-guru-reset [--dry-run] [--yes]"; exit 0 ;;
    *) echo "context-guru-reset: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

STATE_BASE=${XDG_STATE_HOME:-"$HOME/.local/state"}
STATE_DIR="$STATE_BASE/context-guru-pi"
ROUTING_STATE="$STATE_DIR/routing.json"
ROUTING_HELPER="$STATE_DIR/models_route.py"
RECORD="$STATE_DIR/install.json"
PROXY_CONFIG="$STATE_DIR/proxy.yaml"
MANAGED_BINARY="$STATE_DIR/bin/context-guru-proxy"
CONFIG_BASE=${XDG_CONFIG_HOME:-"$HOME/.config"}
SYSTEMD_UNIT="$CONFIG_BASE/systemd/user/context-guru-pi.service"
LAUNCHD_LABEL=io.rossoctl.context-guru-pi
LAUNCHD_PLIST="$HOME/Library/LaunchAgents/$LAUNCHD_LABEL.plist"
MARKER='# Managed by the context-guru Pi package.'

[ -f "$ROUTING_STATE" ] && echo "routing_record=$ROUTING_STATE" || echo "routing_record=(none)"
[ -f "$MANAGED_BINARY" ] && echo "managed_binary=$MANAGED_BINARY" || echo "managed_binary=(none)"
if [ "$DRY" = 1 ]; then echo "result=planned"; exit 0; fi
if [ "$YES" != 1 ]; then
  printf "Remove context-guru's route from Pi's models.json and stop its proxy? [y/N] "
  answer=
  read -r answer || true   # closed stdin is a no, not a silent exit under set -e
  case "$answer" in y|Y|yes|YES) ;; *) echo "result=cancelled"; exit 0 ;; esac
fi

# Routing first: Pi works again the moment the key is gone, whatever happens to the service below.
if [ -f "$ROUTING_STATE" ] && [ -f "$ROUTING_HELPER" ]; then
  python3 "$ROUTING_HELPER" restore "$ROUTING_STATE"
else
  echo "models_restore=no_record"
fi

service=gone
if [ -f "$SYSTEMD_UNIT" ]; then
  if [ "$(sed -n '1p' "$SYSTEMD_UNIT")" = "$MARKER" ]; then
    systemctl --user disable --now context-guru-pi.service >/dev/null 2>&1 || true
    rm "$SYSTEMD_UNIT"
    systemctl --user daemon-reload >/dev/null 2>&1 || true
    service=stopped
  else
    service=not_owned
  fi
elif [ -f "$LAUNCHD_PLIST" ]; then
  if python3 -c 'import plistlib,sys; sys.exit(0 if plistlib.load(open(sys.argv[1], "rb")).get("ContextGuruManaged") is True else 1)' "$LAUNCHD_PLIST" 2>/dev/null; then
    launchctl bootout "gui/$(id -u)/$LAUNCHD_LABEL" >/dev/null 2>&1 || true
    rm "$LAUNCHD_PLIST"
    service=stopped
  else
    service=not_owned
  fi
fi
echo "service=$service"

if [ -f "$PROXY_CONFIG" ] && [ "$(sed -n '1p' "$PROXY_CONFIG")" = "$MARKER" ]; then rm "$PROXY_CONFIG"; fi
if [ "$service" != not_owned ]; then
  rm -f "$RECORD"
  if [ -f "$MANAGED_BINARY" ]; then rm "$MANAGED_BINARY"; echo "binary_removed=true"; fi
fi
echo "result=removed"
