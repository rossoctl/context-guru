#!/usr/bin/env bash
# Install context-guru as a hosted service. Idempotent — safe to re-run after changing
# a credential, adding an upstream, or rebuilding the binary.
#
#   sudo ./install.sh preflight   what is missing, and nothing else
#   sudo ./install.sh install     create the user, dirs, units; does NOT start
#   sudo ./install.sh start       preflight, then enable --now
#   sudo ./install.sh nginx       install the TLS front end (needs nginx present)
#   sudo ./install.sh status      what is running, and where to look if it is not
#   sudo ./install.sh grafana     the observability stack, loopback only:
#                                 Prometheus + Grafana over /metrics, and
#                                 Loki + promtail over the JSON log sink
#   sudo ./install.sh grafana-status    containers, scrape health, log ingest, dashboards
#   sudo ./install.sh grafana-remove    drop the containers, keep the metrics and logs
#
# It deliberately does NOT start the service until preflight passes. A unit with
# Restart=always and a missing prerequisite is a crash loop that respawns every two
# seconds and buries the real cause in the journal — which is exactly what happens if
# you copy the unit file in and enable it before the user, the binary, the directories
# and the credentials exist.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SVC_DIR="$REPO_DIR/deploy/service"
BIN_SRC="$REPO_DIR/bin/context-guru-proxy"
BIN_DST=/usr/local/bin/context-guru-proxy
ETC=/etc/context-guru
CREDS="$ETC/credentials"
STATE=/var/lib/context-guru
DROPIN=/etc/systemd/system/context-guru.service.d
SVC_USER=cg
# The rclone config to hand the service user. Defaults to the invoking user's, which is
# where box-setup.sh puts it.
RCLONE_SRC="${RCLONE_SRC:-${SUDO_USER:+/home/$SUDO_USER}/.config/rclone/rclone.conf}"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
# Every ✗ increments FAILS. A subcommand that prints ✗ and exits 0 is worse than one that
# says nothing: `install.sh nginx && echo deployed` then reports success on a box whose
# certificate is missing its intermediate and whose port 80 is open, and the operator has
# a written all-clear for a state that leaks tokens in cleartext. Subcommands end with
# `return_fails` so the exit status matches what was printed.
FAILS=0
no()  { printf '  \033[31m✗\033[0m %s\n' "$*"; FAILS=$((FAILS+1)); }
return_fails() {
  [ "$FAILS" -eq 0 ] && return 0
  printf '\n\033[31m%d check(s) failed above.\033[0m\n' "$FAILS" >&2
  return 1
}
warn(){ printf '  \033[33m!\033[0m %s\n' "$*"; }

need_root() {
  [ "$(id -u)" = 0 ] || { echo "run this with sudo" >&2; exit 1; }
}

# ── install ─────────────────────────────────────────────────────────────────
cmd_install() {
  need_root
  say "Service account"
  # The GROUP first, and explicitly. `useradd --system` without --gid falls back to the
  # distribution default (on RHEL that is `users`), and then every `install -g cg` in this
  # script fails with "invalid group" and aborts a half-finished install. Owning a
  # dedicated group also means 0700 on the state directory actually means "only this
  # service", rather than "anyone in users".
  if getent group "$SVC_USER" >/dev/null 2>&1; then
    ok "group $SVC_USER exists"
  else
    groupadd --system "$SVC_USER"
    ok "created system group $SVC_USER"
  fi
  if id "$SVC_USER" >/dev/null 2>&1; then
    ok "user $SVC_USER exists"
    # Align the primary group if the account was created before this script existed.
    if [ "$(id -gn "$SVC_USER")" != "$SVC_USER" ]; then
      usermod --gid "$SVC_USER" "$SVC_USER"
      warn "moved $SVC_USER's primary group from $(getent group "$(id -g "$SVC_USER")" | cut -d: -f1) to $SVC_USER"
    fi
  else
    # A system account with no shell and no home: it runs one binary and owns one
    # directory, and should not be a login.
    useradd --system --gid "$SVC_USER" --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
    ok "created system user $SVC_USER"
  fi

  say "Directories"
  install -d -m0755 "$ETC"
  install -d -m0700 "$CREDS"
  install -d -m0700 "$ETC/tls"
  # StateDirectory= would create this at start, but the rclone config and the databases
  # have to be in place BEFORE the first start, so it is created here.
  install -d -m0700 -o "$SVC_USER" -g "$SVC_USER" "$STATE"
  ok "$ETC, $CREDS, $ETC/tls, $STATE"

  say "Binary"
  if [ -x "$BIN_SRC" ]; then
    install -m0755 "$BIN_SRC" "$BIN_DST"
    ok "installed $BIN_DST ($("$BIN_DST" --version 2>/dev/null || echo 'version unknown'))"
  elif [ -x "$BIN_DST" ]; then
    warn "no fresh build at $BIN_SRC; keeping the installed $BIN_DST"
  else
    no "no binary. Run 'make build' in $REPO_DIR first."
    exit 1
  fi

  say "Upstream allow-list"
  if [ -f "$ETC/upstreams.yaml" ]; then
    ok "$ETC/upstreams.yaml exists (left alone)"
  else
    install -m0644 "$SVC_DIR/upstreams.example.yaml" "$ETC/upstreams.yaml"
    warn "installed the EXAMPLE allow-list — edit $ETC/upstreams.yaml and set real hosts"
  fi

  say "rclone config for cold storage"
  if [ -f "$STATE/rclone.conf" ]; then
    ok "$STATE/rclone.conf exists (left alone)"
  elif [ -n "$RCLONE_SRC" ] && [ -f "$RCLONE_SRC" ]; then
    install -m0600 -o "$SVC_USER" -g "$SVC_USER" "$RCLONE_SRC" "$STATE/rclone.conf"
    ok "copied $RCLONE_SRC -> $STATE/rclone.conf (0600, $SVC_USER)"
  else
    warn "no rclone config found; cold storage will be disabled until $STATE/rclone.conf exists"
    warn "run $SVC_DIR/box-setup.sh as your own user first, then re-run this installer"
  fi

  say "systemd units"
  # The shipped unit carries defaults and is REPLACED on every install, so a fix to it
  # actually lands. Site settings therefore must not live in it — see write_local_dropin.
  install -m0644 "$SVC_DIR/context-guru.service" /etc/systemd/system/
  install -m0644 "$SVC_DIR/context-guru-backup.service" /etc/systemd/system/
  install -m0644 "$SVC_DIR/context-guru-backup.timer" /etc/systemd/system/
  install -m0755 "$SVC_DIR/context-guru-start.sh" /usr/local/bin/context-guru-start
  install -m0755 "$SVC_DIR/context-guru-backup.sh" /usr/local/bin/
  install -m0755 "$SVC_DIR/box-setup.sh" /usr/local/bin/
  ok "units + helper scripts installed"

  write_credential_dropin
  write_local_dropin
  systemctl daemon-reload
  ok "daemon-reload done"

  say "Next"
  echo "  1. Edit $ETC/upstreams.yaml (real hosts). No key_env: each caller's own"
  echo "     provider key is forwarded, so no credential file is needed."
  echo "  2. Set MANAGER_EMAIL in $DROPIN/20-local.conf (NOT in the unit — the unit is"
  echo "     replaced on every install; the drop-in never is)."
  echo "  3. sudo $0 start"
  echo
  echo "  Gateway deployments ONLY (agents hold a placeholder key): add key_env: NAME to"
  echo "  an entry, put the key in $CREDS/<name>, chmod 0400, and re-run install."
}

# write_credential_dropin generates one LoadCredential line per file in the credentials
# directory. Generated rather than hand-written because a LoadCredential naming a file
# that does not exist fails the whole unit, and the error does not say which file.
write_credential_dropin() {
  install -d -m0755 "$DROPIN"
  local out="$DROPIN/10-credentials.conf"
  {
    echo "# GENERATED by deploy/service/install.sh — do not edit."
    echo "# One line per file in $CREDS. Re-run the installer after adding a key."
    echo "[Service]"
    local n=0
    if [ -d "$CREDS" ]; then
      for f in "$CREDS"/*; do
        [ -f "$f" ] || continue
        echo "LoadCredential=$(basename "$f"):$f"
        n=$((n + 1))
      done
    fi
    if [ "$n" = 0 ]; then
      echo "# (no credential files found)"
    fi
  } > "$out"
  chmod 0644 "$out"
  local count
  count=$(grep -c '^LoadCredential=' "$out" || true)
  if [ "$count" = 0 ]; then
    ok "no server-held upstream credentials — callers' own provider keys are forwarded"
  else
    ok "credential drop-in written ($count upstream$([ "$count" = 1 ] || echo s))"
  fi
}

# write_local_dropin creates the SITE configuration drop-in, once, and never touches it
# again.
#
# This exists because of a mistake worth not repeating: settings like MANAGER_EMAIL used
# to be edited directly in the shipped unit, and since installing is idempotent and gets
# re-run (to regenerate the credential drop-in), every re-run silently reverted the
# operator's edits. Anything a human is expected to change belongs in a file the
# installer only ever creates.
write_local_dropin() {
  install -d -m0755 "$DROPIN"
  local out="$DROPIN/20-local.conf"
  if [ -f "$out" ]; then
    # CONTENTS left alone; the mode is not content. An install predating the 0600 below
    # left this file 0644 with METRICS_TOKEN documented in it, and only a re-run can
    # correct that — skipping the chmod here would mean the fix never reaches the boxes
    # that actually have the problem.
    chmod 0600 "$out"
    ok "site config $out (left alone, mode 0600)"
    return
  fi
  cat > "$out" <<'LOCAL'
# Site configuration for context-guru. Created ONCE by install.sh and never overwritten,
# so it is safe to edit and safe to re-run the installer.
#
# Values here override the shipped unit. After editing:
#   sudo systemctl daemon-reload && sudo systemctl restart context-guru
[Service]
# The account registered with this address becomes the manager (sees and edits every
# tenant). Until one exists, nobody can administer anyone.
Environment=MANAGER_EMAIL=

# Who may self-register, by email domain. Empty means anyone who can reach the port.
Environment=REGISTER_DOMAINS=ibm.com

# Listen address. Keep this on loopback until nginx terminates TLS in front of it: every
# user's token crosses the network on every request.
Environment=LISTEN_ADDR=127.0.0.1:4000

# Per-tenant limits on the shared box. 0 disables a bound.
Environment=TENANT_RPM=120
Environment=TENANT_CONCURRENT=16

# Uncomment to let a Prometheus on another host scrape /metrics (loopback needs nothing).
#Environment=METRICS_TOKEN=
LOCAL
  # 0600, not 0644: this file is where METRICS_TOKEN goes, and that token reads a
  # service-wide view carrying every tenant's month-to-date cost. systemd reads drop-ins as
  # root, so nothing needs the group or other bits. The sibling drop-ins stay 0644 because
  # they hold no secrets — 10-credentials.conf names credential FILES, and those are 0640
  # root-only themselves.
  chmod 0600 "$out"
  warn "created $out — set MANAGER_EMAIL in it, then: systemctl daemon-reload"
}

# ── preflight ───────────────────────────────────────────────────────────────
cmd_preflight() {
  local bad=0
  # Preflight needs root, and not for the sake of ceremony: the credentials directory is
  # 0700 root and the state directory is 0700 cg, so an unprivileged run cannot stat
  # either and reports both as MISSING. False negatives in a diagnostic are worse than no
  # diagnostic — someone chases a credential they already installed.
  if [ "$(id -u)" != 0 ]; then
    echo "preflight needs root to read /etc/context-guru/credentials and $STATE" >&2
    echo "  sudo $0 preflight" >&2
    exit 1
  fi
  say "Preflight"

  if id "$SVC_USER" >/dev/null 2>&1; then
    if [ "$(id -gn "$SVC_USER")" = "$SVC_USER" ]; then
      ok "service user $SVC_USER (group $SVC_USER)"
    else
      no "user $SVC_USER has primary group $(id -gn "$SVC_USER"), not $SVC_USER — run: sudo $0 install"
      bad=1
    fi
  else
    no "user $SVC_USER missing"
    bad=1
  fi
  [ -x "$BIN_DST" ] && ok "binary $BIN_DST" || { no "binary $BIN_DST missing"; bad=1; }
  [ -x /usr/local/bin/context-guru-start ] && ok "start wrapper" || { no "/usr/local/bin/context-guru-start missing"; bad=1; }
  [ -d "$STATE" ] && ok "state dir $STATE" || { no "state dir $STATE missing"; bad=1; }
  [ -f "$ETC/upstreams.yaml" ] && ok "allow-list $ETC/upstreams.yaml" || { no "$ETC/upstreams.yaml missing"; bad=1; }

  # The example ships placeholder hosts. Starting with those means every request 502s.
  if grep -q 'REPLACE-WITH' "$ETC/upstreams.yaml" 2>/dev/null; then
    no "$ETC/upstreams.yaml still has REPLACE-WITH placeholders — edit it"
    bad=1
  fi

  # Every key_env the allow-list names must have a matching credential file, or the
  # proxy refuses to boot. Naming none is the normal case — the caller's own provider
  # key is forwarded — so this loop usually finds nothing to check.
  local missing=0
  while read -r env; do
    [ -n "$env" ] || continue
    # UPSTREAM_IBM_LITELLM_KEY -> ibm-litellm (or ibm_litellm). The wrapper maps both
    # dashes and underscores to '_', so both spellings work at runtime — preflight has to
    # accept both too, or it reports a file as missing that the service would happily use.
    local base dashed under found=""
    base=$(echo "$env" | sed -e 's/^UPSTREAM_//' -e 's/_KEY$//')
    dashed=$(echo "$base" | tr 'A-Z_' 'a-z-')
    under=$(echo "$base" | tr 'A-Z-' 'a-z_')
    for cand in "$dashed" "$under"; do
      [ -s "$CREDS/$cand" ] && { found="$cand"; break; }
    done
    if [ -n "$found" ]; then
      ok "credential for $env ($CREDS/$found)"
    else
      no "credential for $env expected at $CREDS/$dashed — missing or empty"
      missing=1
    fi
  done < <(grep -oE 'key_env:[[:space:]]*[A-Z0-9_]+' "$ETC/upstreams.yaml" 2>/dev/null | awk '{print $2}' | sort -u)
  [ "$missing" = 0 ] || bad=1

  if [ -f "$DROPIN/10-credentials.conf" ]; then
    ok "credential drop-in present"
  else
    no "no credential drop-in — run: sudo $0 install"
    bad=1
  fi

  # Read the EFFECTIVE value: it can come from the shipped unit or the site drop-in, and
  # checking only one of them is how "I set it and it still says empty" happens.
  if systemctl show context-guru -p Environment 2>/dev/null | grep -qE 'MANAGER_EMAIL=[^[:space:]]+'; then
    ok "MANAGER_EMAIL is set"
  elif grep -qhE '^Environment=MANAGER_EMAIL=.+' /etc/systemd/system/context-guru.service \
        "$DROPIN"/*.conf 2>/dev/null; then
    ok "MANAGER_EMAIL is set (pending daemon-reload)"
  else
    # Not fatal: the service runs fine, but nobody can administer other accounts, and
    # that is discovered later and awkwardly.
    warn "MANAGER_EMAIL is empty in the unit — no account will be able to administer others"
  fi

  if [ -f "$STATE/rclone.conf" ]; then
    ok "rclone config for cold storage"
  else
    warn "no $STATE/rclone.conf — cold storage disabled, so eviction DELETES instead of archiving"
  fi

  say $([ "$bad" = 0 ] && echo "Preflight PASSED" || echo "Preflight FAILED")
  return "$bad"
}

# ── start ───────────────────────────────────────────────────────────────────
cmd_start() {
  need_root
  cmd_preflight || { echo; echo "refusing to start with failures above" >&2; exit 1; }
  say "Starting"
  systemctl daemon-reload
  systemctl enable --now context-guru
  systemctl enable --now context-guru-backup.timer
  sleep 2
  if systemctl is-active --quiet context-guru; then
    ok "context-guru is running"
    local addr
    addr=$(grep -oE '^Environment=LISTEN_ADDR=.*' /etc/systemd/system/context-guru.service | cut -d= -f3-)
    echo "  listening on ${addr:-:4000}; dashboard at http://${addr:-127.0.0.1:4000}/dashboard/"
  else
    no "context-guru did not start"
    journalctl -u context-guru -n 20 --no-pager
    exit 1
  fi
}

cmd_nginx() {
  need_root
  if ! command -v nginx >/dev/null 2>&1; then
    no "nginx is not installed"
    echo "    sudo dnf -y install nginx"
    echo
    echo "  Until then the proxy is reachable only on its own LISTEN_ADDR, and if that is"
    echo "  loopback nobody else can use it. Do NOT open it to the LAN without TLS: every"
    echo "  user's token crosses the network on every request."
    exit 1
  fi
  install -d -m0755 /etc/nginx/conf.d
  # Validate BEFORE committing to the live file, and put the old one back if the new one does
  # not pass. A straight `install` over the live conf with `nginx -t` only further down
  # leaves a broken file in place on failure: nginx keeps serving the old config from memory,
  # so nothing looks wrong until the next reload or reboot takes the whole front end down —
  # and by then the file that worked is gone. `nginx -t` reads the on-disk tree, so the swap
  # has to happen first; the rollback is what makes that safe.
  #
  # Backups go to /etc/nginx/backups, NOT conf.d: nginx includes conf.d/*.conf, and a backup
  # name that ever matches that glob defines the whole server block twice and nginx refuses
  # to start.
  local live=/etc/nginx/conf.d/context-guru.conf prev=""
  if [ -f "$live" ]; then
    install -d -m0755 /etc/nginx/backups
    prev="/etc/nginx/backups/context-guru.conf.$(date +%Y%m%d-%H%M%S)"
    cp -a "$live" "$prev"
  fi
  install -m0644 "$SVC_DIR/nginx.conf" "$live"
  if ! nginx -t >/dev/null 2>&1; then
    no "the new nginx config does not pass nginx -t — the live config is left as it was:"
    nginx -t 2>&1 | sed 's/^/    /'
    if [ -n "$prev" ]; then cp -a "$prev" "$live"; else rm -f "$live"; fi
    exit 1
  fi
  ok "installed /etc/nginx/conf.d/context-guru.conf${prev:+ (previous kept at $prev)}"
  if [ ! -f "$ETC/tls/fullchain.pem" ] || [ ! -f "$ETC/tls/privkey.pem" ]; then
    no "no TLS certificate at $ETC/tls/{fullchain,privkey}.pem — nginx will not start"
    echo "    Put the real certificate there, or for a first test:"
    echo "      sudo openssl req -x509 -newkey rsa:2048 -nodes -days 365 \\"
    echo "        -subj \"/CN=\$(hostname -f)\" \\"
    echo "        -keyout $ETC/tls/privkey.pem -out $ETC/tls/fullchain.pem"
    echo "    A self-signed certificate means every agent needs to trust it, so it is a"
    echo "    test convenience, not a deployment."
    exit 1
  fi
  # Verify the certificate actually chains and matches its key, rather than only that
  # two files exist. A fullchain missing its intermediate is the classic failure: it
  # works in a browser that cached the intermediate from another site and fails for
  # every agent, which is a miserable thing to debug from the client side.
  if ! openssl x509 -in "$ETC/tls/fullchain.pem" -noout -checkend 0 >/dev/null 2>&1; then
    no "$ETC/tls/fullchain.pem is not a valid certificate, or it has expired"
    exit 1
  fi
  crt=$(openssl x509 -in "$ETC/tls/fullchain.pem" -noout -modulus 2>/dev/null | openssl sha256)
  key=$(openssl rsa -in "$ETC/tls/privkey.pem" -noout -modulus 2>/dev/null | openssl sha256)
  if [ -n "$crt" ] && [ "$crt" = "$key" ]; then
    ok "certificate and private key match"
  else
    no "the certificate and private key do NOT match — nginx will serve a cert it cannot prove"
    exit 1
  fi
  if [ "$(grep -c 'BEGIN CERTIFICATE' "$ETC/tls/fullchain.pem")" -lt 2 ]; then
    no "fullchain.pem holds only ONE certificate — the intermediate is missing"
    echo "    Clients trusting only the root CA will fail to verify. Concatenate leaf then"
    echo "    intermediate:  cat leaf.pem intermediate.pem > $ETC/tls/fullchain.pem"
  else
    ok "fullchain.pem includes the intermediate"
  fi
  if openssl x509 -in "$ETC/tls/fullchain.pem" -noout -checkend $((21*86400)) >/dev/null 2>&1; then
    ok "certificate valid > 21 days"
  else
    no "certificate expires within 21 days — reissue it now ($(openssl x509 -in "$ETC/tls/fullchain.pem" -noout -enddate | cut -d= -f2))"
  fi

  nginx -t && systemctl reload nginx && ok "nginx reloaded"

  # Our own config deliberately has no port-80 block (see nginx.conf), but the distro
  # nginx.conf ships a default one, and a package upgrade can put it back. A cleartext
  # port on this service is a credential leak waiting for one mistyped URL, so report
  # it rather than let it reappear silently.
  if ss -ltn 2>/dev/null | grep -qE '(^|[^0-9.]):80\s'; then
    no "something is listening on port 80"
    echo "    Every request here carries a cg_live_ token in a header, and a 301 to https"
    echo "    cannot retract a credential the client has already sent in cleartext. The"
    echo "    granted firewall exception is 443 only. Usually this is the distro default"
    echo "    server in /etc/nginx/nginx.conf — comment that block out and reload."
  else
    ok "nothing listening on port 80"
  fi
  return_fails
}

# ── grafana ─────────────────────────────────────────────────────────────────
# Prometheus + Grafana in containers rather than packages: Prometheus is not in any RHEL 9
# repository, so the alternative is a packaged Grafana beside a tarball Prometheus with two
# unrelated sets of paths to keep straight. Two `docker run` lines is the smaller thing.
#
# Both bind LOOPBACK ONLY. /metrics is a service-wide view carrying every tenant's spend,
# and Grafana's session cookie is as good as the admin password — neither belongs on a
# shared box's LAN interface. Reach them with `ssh -L 3000:127.0.0.1:3000`, or through the
# nginx front end's /grafana/, which is gated to context-guru MANAGERS by auth_request and
# signs them in from that same gate (see deploy/service/nginx.conf). Prometheus and Loki are
# not published at all.
OBS_ETC="$ETC/observability"
OBS_STATE="$STATE/observability"
PROM_IMAGE="${PROM_IMAGE:-docker.io/prom/prometheus:v3.2.1}"
GRAFANA_IMAGE="${GRAFANA_IMAGE:-docker.io/grafana/grafana:11.6.0}"
PROM_RETENTION="${PROM_RETENTION:-90d}"
# The public URL Grafana is reached at, which is the nginx front end's /grafana/ — not the
# loopback port. Grafana generates its redirects and asset URLs from this, so it has to be
# the address the BROWSER used. Override it if the front end answers to another name.
GRAFANA_ROOT_URL="${GRAFANA_ROOT_URL:-https://$(hostname -f 2>/dev/null || hostname)/grafana/}"
# Loki holds the LOGS; Prometheus holds the numbers. Both are read through the same
# Grafana, which is the whole reason Loki won over the alternatives — see
# deploy/grafana/README.md, "Why Loki".
# node_exporter is the HOST's own metrics — CPU, memory, disk, filesystem fill, network,
# load. The proxy's /metrics answers "is context-guru working"; nothing answered "is this
# box healthy", which is the question behind every report that starts "it felt slow".
# Writing our own /proc reader was the alternative and it is strictly worse: this is the
# standard exporter, the queries are the ones every runbook already uses, and it is one
# more container beside four.
NODE_EXPORTER_IMAGE="${NODE_EXPORTER_IMAGE:-docker.io/prom/node-exporter:v1.9.0}"
LOKI_IMAGE="${LOKI_IMAGE:-docker.io/grafana/loki:3.4.2}"
PROMTAIL_IMAGE="${PROMTAIL_IMAGE:-docker.io/grafana/promtail:3.4.2}"
# Where the proxy writes its JSON log sink (CG_LOG_FILE in the unit) and promtail
# tails it from. Same path inside the promtail container, so promtail.yml needs no
# templating.
LOG_DIR="${LOG_DIR:-/var/log/context-guru}"

# oci_runtime picks whichever of podman/docker is present. Both accept every flag used
# below, so the rest of this section does not care which one it got.
oci_runtime() {
  if command -v podman >/dev/null 2>&1; then echo podman
  elif command -v docker >/dev/null 2>&1; then echo docker
  else return 1; fi
}

cmd_grafana() {
  need_root
  local oci
  if ! oci=$(oci_runtime); then
    no "no container runtime — install one:"
    echo "    sudo dnf -y install podman"
    exit 1
  fi
  say "Container runtime"
  ok "$oci ($($oci --version 2>/dev/null | head -1))"

  say "Configuration"
  # The empty plugins/ and alerting/ directories are not decoration: Grafana logs a
  # level=error line for each provisioning directory it cannot open, and an operator
  # following the README should not have to decide whether those two errors matter.
  install -d -m0755 "$OBS_ETC" "$OBS_ETC/grafana/dashboards" \
    "$OBS_ETC/grafana/provisioning/datasources" \
    "$OBS_ETC/grafana/provisioning/dashboards" \
    "$OBS_ETC/grafana/provisioning/plugins" \
    "$OBS_ETC/grafana/provisioning/alerting"
  # install -m0644, not cp: root's umask leaves cp output 0640, and a container that cannot
  # read its own config crash-loops on "permission denied" with nothing else to go on.
  install -m0644 "$REPO_DIR/deploy/grafana/prometheus-scrape.yml" "$OBS_ETC/prometheus.yml"
  install -m0644 "$REPO_DIR/deploy/grafana/loki.yml" "$OBS_ETC/loki.yml"
  install -m0644 "$REPO_DIR/deploy/grafana/promtail.yml" "$OBS_ETC/promtail.yml"
  install -m0644 "$REPO_DIR"/deploy/grafana/provisioning/datasources/*.yml \
    "$OBS_ETC/grafana/provisioning/datasources/"
  install -m0644 "$REPO_DIR"/deploy/grafana/provisioning/dashboards/*.yml \
    "$OBS_ETC/grafana/provisioning/dashboards/"
  install -m0644 "$REPO_DIR"/deploy/grafana/dashboards/*.json "$OBS_ETC/grafana/dashboards/"
  ok "$OBS_ETC ($(ls "$OBS_ETC/grafana/dashboards" | wc -l) dashboard(s))"

  say "State"
  install -d -m0755 "$OBS_STATE"
  # The images run as fixed unprivileged uids and neither will chown its own volume.
  install -d -m0755 -o 65534 -g 65534 "$OBS_STATE/prometheus"
  install -d -m0755 -o 472 -g 0 "$OBS_STATE/grafana"
  # Loki and promtail both run as uid 10001 in their images and neither chowns its volume.
  install -d -m0755 -o 10001 -g 10001 "$OBS_STATE/loki" "$OBS_STATE/promtail"
  ok "$OBS_STATE/{prometheus,grafana,loki,promtail}"

  # The log directory. systemd's LogsDirectory= creates it when the service starts, but
  # promtail must be able to open it whether or not the service has run yet — and a
  # promtail that cannot see its target says nothing at all about why.
  say "Log sink"
  if [ -d "$LOG_DIR" ]; then
    ok "$LOG_DIR exists ($(ls "$LOG_DIR" 2>/dev/null | wc -l) file(s))"
  else
    install -d -m0750 -o "$SVC_USER" -g "$SVC_USER" "$LOG_DIR" 2>/dev/null \
      && ok "created $LOG_DIR" \
      || warn "could not create $LOG_DIR (is the $SVC_USER user installed?); \
promtail will find nothing until the service starts"
  fi
  install -m0644 "$REPO_DIR/deploy/grafana/context-guru.logrotate" /etc/logrotate.d/context-guru
  ok "/etc/logrotate.d/context-guru (daily, 7 kept, copytruncate)"

  say "Prometheus"
  $oci rm -f cg-prometheus >/dev/null 2>&1 || true
  # --network=host so 127.0.0.1:4000 in the scrape config means the proxy. That also means
  # 9090 would land on every interface, hence the explicit --web.listen-address.
  $oci run -d --name cg-prometheus --network=host --restart=unless-stopped \
    -v "$OBS_ETC/prometheus.yml:/etc/prometheus/prometheus.yml:ro,Z" \
    -v "$OBS_STATE/prometheus:/prometheus:Z" \
    "$PROM_IMAGE" \
    --config.file=/etc/prometheus/prometheus.yml \
    --storage.tsdb.path=/prometheus \
    --storage.tsdb.retention.time="$PROM_RETENTION" \
    --web.listen-address=127.0.0.1:9090 >/dev/null
  ok "cg-prometheus on 127.0.0.1:9090, retention $PROM_RETENTION"

  say "node_exporter"
  $oci rm -f cg-node-exporter >/dev/null 2>&1 || true
  # The host's /proc, /sys and / are mounted read-only and the exporter is pointed at them,
  # which is the documented way to make a containerised node_exporter report the HOST rather
  # than the container. --path.rootfs makes its filesystem metrics carry the host's
  # mountpoints, so a full /var shows up as /var and not as /host/var.
  #
  # Loopback only, like everything else here: host metrics name every mountpoint and every
  # process count on a box that serves several tenants.
  #
  # NOT --collector.systemd. It would report the context-guru unit's own state, which is the
  # one thing here the proxy's /metrics cannot answer during a crash loop — but it needs
  # /run/systemd/private mounted WRITABLE, and that socket is full control of systemd on this
  # host. One panel is not worth handing that to a metrics exporter; `up{job="context-guru"}`
  # on the SLO dashboard covers the same question from outside.
  $oci run -d --name cg-node-exporter --network=host --restart=unless-stopped \
    --pid=host \
    -v /proc:/host/proc:ro,rslave \
    -v /sys:/host/sys:ro,rslave \
    -v /:/rootfs:ro,rslave \
    "$NODE_EXPORTER_IMAGE" \
    --path.procfs=/host/proc --path.sysfs=/host/sys --path.rootfs=/rootfs \
    --web.listen-address=127.0.0.1:9100 \
    --collector.filesystem.mount-points-exclude='^/(dev|proc|sys|var/lib/docker/.+|var/lib/containers/.+)($|/)' >/dev/null
  ok "cg-node-exporter on 127.0.0.1:9100 (host cpu/memory/disk/network)"

  say "Loki"
  $oci rm -f cg-loki >/dev/null 2>&1 || true
  # Loopback only, same reasoning as the other two: log lines carry tenant ids, routes
  # and upstream hosts. loki.yml pins http_listen_address, which host networking would
  # otherwise leave on every interface.
  $oci run -d --name cg-loki --network=host --restart=unless-stopped \
    -v "$OBS_ETC/loki.yml:/etc/loki/local-config.yaml:ro,Z" \
    -v "$OBS_STATE/loki:/loki:Z" \
    "$LOKI_IMAGE" -config.file=/etc/loki/local-config.yaml >/dev/null
  ok "cg-loki on 127.0.0.1:3100, retention 30d"

  say "promtail"
  $oci rm -f cg-promtail >/dev/null 2>&1 || true
  # The log directory read-only, and its own state directory writable for the positions
  # file — without which a restarted promtail re-ships the whole log.
  $oci run -d --name cg-promtail --network=host --restart=unless-stopped \
    -v "$OBS_ETC/promtail.yml:/etc/promtail/config.yml:ro,Z" \
    -v "$OBS_STATE/promtail:/var/lib/promtail:Z" \
    -v "$LOG_DIR:/var/log/context-guru:ro,Z" \
    "$PROMTAIL_IMAGE" -config.file=/etc/promtail/config.yml >/dev/null
  ok "cg-promtail tailing $LOG_DIR/*.jsonl -> Loki"

  say "Grafana"
  # NOBODY NEEDS A GRAFANA PASSWORD HERE. A manager reaches /grafana/ through the gate and
  # is signed in by it (auth-proxy, below), so the built-in `admin` account is break-glass
  # only — for the day the proxy is down and you are on the loopback port.
  #
  # It still must not be left at Grafana's default `admin`, which is what an unset
  # GF_SECURITY_ADMIN_PASSWORD means on a first install. So a first install seeds it with a
  # random value that is NEVER printed and never written anywhere: unguessable, and unknown
  # even to us, which is the correct state for a credential no workflow uses. Whoever wants
  # break-glass sets their own afterwards — see deploy/grafana/README.md, "The admin
  # password". GRAFANA_ADMIN_PASSWORD in the environment still wins if you want to choose it
  # now. After the first install nothing is set at all: it lives in Grafana's database.
  local pw="${GRAFANA_ADMIN_PASSWORD:-}"
  if [ -z "$pw" ] && [ ! -f "$OBS_STATE/grafana/grafana.db" ]; then
    pw=$(head -c 32 /dev/urandom | base64 | tr -d '/+=')
  fi

  # Every setting goes through an env-FILE rather than -e flags, and that is about the one
  # secret among them. `docker run -e GF_SECURITY_ADMIN_PASSWORD=…` puts the password in
  # this process's argv, i.e. /proc/<pid>/cmdline, which is world-readable: any local user
  # running `ps auxww` during the install reads Grafana's admin password, and Grafana's
  # admin sees every tenant's spend. The file is 0600 root-owned, lives on the /run tmpfs
  # so the value never reaches a disk, and is removed as soon as the container exists —
  # the runtime has copied the variables into the container's own config by then.
  local envfile
  envfile=$(mktemp /run/cg-grafana-env.XXXXXX)
  chmod 0600 "$envfile"
  {
    echo "GF_SERVER_HTTP_ADDR=127.0.0.1"
    echo "GF_SERVER_HTTP_PORT=3000"
    # Sub-path support, for the manager-gated /grafana/ on the nginx front end. Grafana
    # builds its own redirects and asset URLs from ROOT_URL, so without BOTH of these every
    # asset 404s and the login redirect escapes the sub-path. Harmless when nothing is
    # proxying: the loopback port then serves the same UI under /grafana/.
    echo "GF_SERVER_ROOT_URL=$GRAFANA_ROOT_URL"
    echo "GF_SERVER_SERVE_FROM_SUB_PATH=true"
    # Off, deliberately. /grafana/ is reachable from the network now, and the manager gate
    # in front of it is what decides who gets in.
    echo "GF_AUTH_ANONYMOUS_ENABLED=false"
    echo "GF_ANALYTICS_REPORTING_ENABLED=false"
    # SINGLE SIGN-ON, from the gate that is already there. nginx's auth_request asks the
    # proxy who this is and copies the answer into X-Cg-Grafana-User; Grafana's auth-proxy
    # trusts that header, creates the account on first visit and signs the manager in. One
    # login, one notion of who an administrator is, and no second password to lose.
    #
    # The header is an authentication, so the whitelist is the control that makes it safe on
    # this side: only the loopback peer — i.e. this nginx — may present it. Anything else
    # that reaches port 3000 gets Grafana's login form instead. nginx's own three controls
    # are in deploy/service/nginx.conf; both halves are required.
    echo "GF_AUTH_PROXY_ENABLED=true"
    echo "GF_AUTH_PROXY_HEADER_NAME=X-Cg-Grafana-User"
    echo "GF_AUTH_PROXY_HEADER_PROPERTY=email"
    echo "GF_AUTH_PROXY_AUTO_SIGN_UP=true"
    echo "GF_AUTH_PROXY_WHITELIST=127.0.0.1,::1"
    # No Grafana session cookie: every request re-presents the header and is re-authorized
    # by the gate. A cookie would outlive the gate's decision, so a manager who is disabled
    # in context-guru would keep their Grafana session until it expired.
    echo "GF_AUTH_PROXY_ENABLE_LOGIN_TOKEN=false"
    # Admin, because the gate already decided. Everyone who can reach Grafana at all is a
    # context-guru MANAGER — the same role that can read every tenant's spend on the
    # dashboard — so a Viewer role here would only mean a manager cannot use Explore on
    # numbers they are already entitled to. Signing up through Grafana itself stays off:
    # accounts come from the gate, or not at all.
    echo "GF_USERS_AUTO_ASSIGN_ORG_ROLE=Admin"
    echo "GF_USERS_ALLOW_SIGN_UP=false"
    if [ -n "$pw" ]; then echo "GF_SECURITY_ADMIN_PASSWORD=$pw"; fi
  } > "$envfile"

  $oci rm -f cg-grafana >/dev/null 2>&1 || true
  # The dashboards mount INSIDE the data volume: the provider yml points at
  # /var/lib/grafana/dashboards/context-guru, and nested binds resolve outermost-first, so
  # a read-only dashboard directory survives inside the writable data directory.
  if ! $oci run -d --name cg-grafana --network=host --restart=unless-stopped \
    --env-file "$envfile" \
    -v "$OBS_ETC/grafana/provisioning:/etc/grafana/provisioning:ro,Z" \
    -v "$OBS_ETC/grafana/dashboards:/var/lib/grafana/dashboards/context-guru:ro,Z" \
    -v "$OBS_STATE/grafana:/var/lib/grafana:Z" \
    "$GRAFANA_IMAGE" >/dev/null; then
    rm -f "$envfile"
    no "cg-grafana failed to start — see: $oci logs cg-grafana"
    exit 1
  fi
  rm -f "$envfile"
  ok "cg-grafana on 127.0.0.1:3000, root_url $GRAFANA_ROOT_URL"
  ok "managers sign in through the gate (auth-proxy on X-Cg-Grafana-User), as Admin"
  if [ -z "${GRAFANA_ADMIN_PASSWORD:-}" ] && [ -n "$pw" ]; then
    warn "the built-in 'admin' account was seeded with a random password that was NOT saved."
    warn "Nothing needs it; if you want break-glass on the loopback port, choose one:"
    warn "  deploy/grafana/README.md, 'The admin password'"
  fi

  say "Next"
  echo "  sudo $0 grafana-status          # scrape health + provisioned dashboards"
  echo "  ${GRAFANA_ROOT_URL}d/context-guru/context-guru        # the numbers (manager only)"
  echo "  ${GRAFANA_ROOT_URL}d/context-guru-logs/context-guru-logs   # the logs"
  echo "  install.sh nginx publishes those; until then, or to bypass the gate:"
  echo "    ssh -L 3000:127.0.0.1:3000 $(hostname -f 2>/dev/null || hostname)"
  echo "    http://127.0.0.1:3000/grafana/d/context-guru/context-guru"
}

cmd_grafana_status() {
  local oci
  oci=$(oci_runtime) || { no "no container runtime"; exit 1; }
  say "Containers"
  $oci ps -a --filter name=cg-prometheus --filter name=cg-grafana \
    --filter name=cg-loki --filter name=cg-promtail --filter name=cg-node-exporter \
    --format '{{.Names}}  {{.Status}}' 2>&1 | sed 's/^/  /' || true

  say "Prometheus scrape target"
  # The distinction that matters: a target Prometheus has never scraped reports health
  # "unknown", which is not the same failure as "up" with an empty result.
  if ! curl -sf --max-time 5 http://127.0.0.1:9090/api/v1/targets 2>/dev/null \
      | python3 -c 'import json,sys
ts = json.load(sys.stdin)["data"]["activeTargets"]
for t in ts:
    print("  %-8s %s  %s" % (t["health"], t["scrapeUrl"], t["lastError"] or ""))
if not ts:
    print("  no targets yet — normal for the first few seconds after a restart")' 2>/dev/null; then
    no "Prometheus is not answering on 127.0.0.1:9090"
  fi

  say "A series with a value in it"
  if ! curl -sfG --max-time 5 http://127.0.0.1:9090/api/v1/query \
      --data-urlencode 'query=cg_requests_total' 2>/dev/null \
      | python3 -c 'import json,sys
r=json.load(sys.stdin)["data"]["result"]
print("  cg_requests_total = %s" % r[0]["value"][1] if r else "  EMPTY — target up but the exposition changed")' 2>/dev/null; then
    no "query failed"
  fi

  # Every panel of the HOST dashboard, asked whether it would actually draw anything.
  #
  # This check exists because a panel that queries a series this kernel or this exporter does
  # not produce renders EMPTY, which looks exactly like a healthy idle box — and PromQL that
  # parses is no evidence at all. Two of the eighteen were caught this way on first install:
  # load-per-core divided a labelled series by a bare aggregate (no labels, so vector matching
  # matched nothing), and the pressure-stall panels needed /proc/pressure, which this kernel
  # does not expose.
  say "Host dashboard panels with no data"
  if ! python3 - "$OBS_ETC/grafana/dashboards/context-guru-host.json" <<'PY' 2>/dev/null; then
import json, sys, urllib.parse, urllib.request
try:
    d = json.load(open(sys.argv[1]))
except OSError as e:
    print("  cannot read the dashboard: %s" % e)
    raise SystemExit(0)
bad = []
for p in d.get("panels", []):
    for t in p.get("targets", []):
        e = t.get("expr")
        if not e:
            continue
        try:
            u = "http://127.0.0.1:9090/api/v1/query?" + urllib.parse.urlencode({"query": e})
            with urllib.request.urlopen(u, timeout=10) as r:
                out = json.load(r)
        except Exception as err:
            bad.append((p.get("title"), "query failed: %s" % err))
            continue
        if out.get("status") != "success":
            bad.append((p.get("title"), "rejected: %s" % out.get("error")))
        elif not out["data"]["result"]:
            bad.append((p.get("title"), "no data"))
if not bad:
    print("  all panels return data")
for title, why in bad:
    print("  EMPTY  %-42s %s" % (title, why))
PY
    no "could not check the host panels"
  fi

  say "Loki"
  # `ready` is the one that matters: Loki answers /metrics while its ingester is still
  # coming up, so a health check that only tests the port reports green for a minute
  # during which every push is rejected.
  if curl -sf --max-time 5 http://127.0.0.1:3100/ready 2>/dev/null | grep -q ready; then
    ok "ready on 127.0.0.1:3100"
  else
    no "not ready on 127.0.0.1:3100: $(curl -s --max-time 5 http://127.0.0.1:3100/ready 2>/dev/null | head -1)"
  fi

  say "Log ingest"
  # The failure this catches, and the reason it is not just "is promtail running": the
  # container can be perfectly healthy while the log directory it mounted is empty
  # (CG_LOG_FILE unset in the unit), and then Grafana shows an empty logs dashboard with
  # no error anywhere. So check the FILE, then check that Loki actually has the lines.
  if compgen -G "$LOG_DIR/*.jsonl" >/dev/null; then
    ok "$(ls "$LOG_DIR"/*.jsonl | wc -l) file(s) in $LOG_DIR ($(du -sh "$LOG_DIR" 2>/dev/null | cut -f1))"
  else
    no "nothing in $LOG_DIR — is CG_LOG_FILE set in the unit? (systemctl show -p Environment context-guru)"
  fi
  if ! curl -sfG --max-time 10 http://127.0.0.1:3100/loki/api/v1/query \
      --data-urlencode 'query=sum(count_over_time({job="context-guru"}[10m]))' 2>/dev/null \
      | python3 -c 'import json,sys
r=json.load(sys.stdin)["data"]["result"]
n=r[0]["value"][1] if r else "0"
print("  lines ingested in the last 10 minutes: %s" % n)
print("  EMPTY — promtail is not shipping. `docker logs cg-promtail`") if n in ("0","") else None' 2>/dev/null; then
    no "Loki query failed"
  fi
  echo "  tenants Loki has seen:"
  curl -sf --max-time 5 'http://127.0.0.1:3100/loki/api/v1/label/tenant/values' 2>/dev/null \
    | python3 -c 'import json,sys
v=json.load(sys.stdin).get("data") or []
print("    " + (", ".join(v) if v else "none yet"))' 2>/dev/null || echo "    (query failed)"

  say "Grafana"
  # /grafana/api/health, not /api/health: SERVE_FROM_SUB_PATH moves the whole application
  # and the bare path answers 301, which `curl -sf` reports as a failure — a healthy
  # Grafana would read as down.
  if curl -sf --max-time 5 http://127.0.0.1:3000/grafana/api/health >/dev/null 2>&1; then
    ok "answering on 127.0.0.1:3000/grafana/"
    # /api/search needs credentials; the provisioning log does not, and it is the thing
    # that actually goes wrong (a dashboard JSON that fails to parse is logged, not shown).
    if $oci logs cg-grafana 2>&1 | grep -iE 'provisioning.*(error|fail)' \
        | grep -v 'provisioning/plugins\|provisioning/alerting' | tail -5 | grep -q .; then
      no "dashboard provisioning errors:"
      $oci logs cg-grafana 2>&1 | grep -iE 'provisioning.*(error|fail)' \
        | grep -v 'provisioning/plugins\|provisioning/alerting' | tail -5 | sed 's/^/    /'
    else
      ok "no dashboard provisioning errors in the log"
    fi
    # /api/search needs an identity, and the auth-proxy header is the one this deployment
    # actually uses — the same header nginx sends. Use an address that is ALREADY a manager:
    # auto_sign_up would otherwise create a Grafana account for a typo.
    echo "  dashboards:"
    echo "    curl -s -H 'X-Cg-Grafana-User: <a manager email>' \\"
    echo "      'http://127.0.0.1:3000/grafana/api/search?type=dash-db'"
  else
    no "Grafana is not answering on 127.0.0.1:3000/grafana/"
  fi
}

cmd_grafana_remove() {
  need_root
  local oci
  oci=$(oci_runtime) || { no "no container runtime"; exit 1; }
  $oci rm -f cg-prometheus cg-grafana cg-loki cg-promtail cg-node-exporter >/dev/null 2>&1 || true
  ok "removed cg-prometheus, cg-grafana, cg-loki, cg-promtail and cg-node-exporter"
  warn "kept $OBS_STATE — the metrics history, the log store and Grafana's database, and"
  warn "kept $LOG_DIR. Delete them by hand if you actually want the history gone."
}

cmd_status() {
  say "context-guru"
  systemctl status context-guru --no-pager 2>&1 | head -12 || true
  say "Recent log"
  journalctl -u context-guru -n 15 --no-pager 2>&1 || true
  say "Backup timer"
  systemctl list-timers context-guru-backup.timer --no-pager 2>&1 | head -4 || true
}

case "${1:-preflight}" in
  install)   cmd_install ;;
  preflight) cmd_preflight ;;
  start)     cmd_start ;;
  nginx)     cmd_nginx ;;
  status)    cmd_status ;;
  grafana)        cmd_grafana ;;
  grafana-status) cmd_grafana_status ;;
  grafana-remove) cmd_grafana_remove ;;
  *) echo "usage: $0 {install|preflight|start|nginx|status|grafana|grafana-status|grafana-remove}" >&2; exit 2 ;;
esac
