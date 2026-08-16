#!/usr/bin/env bash
# Install context-guru as a hosted service. Idempotent — safe to re-run after changing
# a credential, adding an upstream, or rebuilding the binary.
#
#   sudo ./install.sh preflight   what is missing, and nothing else
#   sudo ./install.sh install     create the user, dirs, units; does NOT start
#   sudo ./install.sh start       preflight, then enable --now
#   sudo ./install.sh nginx       install the TLS front end (needs nginx present)
#   sudo ./install.sh status      what is running, and where to look if it is not
#   sudo ./install.sh grafana     Prometheus + Grafana over /metrics, loopback only
#   sudo ./install.sh grafana-status    containers, scrape health, provisioned dashboards
#   sudo ./install.sh grafana-remove    drop the containers, keep the metrics history
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
    ok "site config $out (left alone)"
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
  chmod 0644 "$out"
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
  install -m0644 "$SVC_DIR/nginx.conf" /etc/nginx/conf.d/context-guru.conf
  ok "installed /etc/nginx/conf.d/context-guru.conf"
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
# shared box's LAN interface. Reach them with `ssh -L 3000:127.0.0.1:3000`.
OBS_ETC="$ETC/observability"
OBS_STATE="$STATE/observability"
PROM_IMAGE="${PROM_IMAGE:-docker.io/prom/prometheus:v3.2.1}"
GRAFANA_IMAGE="${GRAFANA_IMAGE:-docker.io/grafana/grafana:11.6.0}"
PROM_RETENTION="${PROM_RETENTION:-90d}"

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
  ok "$OBS_STATE/{prometheus,grafana}"

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

  say "Grafana"
  # A password is only needed the FIRST time — after that it lives in Grafana's own
  # database, and re-running the installer must not demand a secret it does not need.
  local pw_args=() generated=""
  if [ -n "${GRAFANA_ADMIN_PASSWORD:-}" ]; then
    pw_args=(-e "GF_SECURITY_ADMIN_PASSWORD=$GRAFANA_ADMIN_PASSWORD")
  elif [ ! -f "$OBS_STATE/grafana/grafana.db" ]; then
    generated=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
    pw_args=(-e "GF_SECURITY_ADMIN_PASSWORD=$generated")
  fi
  $oci rm -f cg-grafana >/dev/null 2>&1 || true
  # The dashboards mount INSIDE the data volume: the provider yml points at
  # /var/lib/grafana/dashboards/context-guru, and nested binds resolve outermost-first, so
  # a read-only dashboard directory survives inside the writable data directory.
  $oci run -d --name cg-grafana --network=host --restart=unless-stopped \
    -e GF_SERVER_HTTP_ADDR=127.0.0.1 -e GF_SERVER_HTTP_PORT=3000 \
    -e GF_AUTH_ANONYMOUS_ENABLED=false -e GF_ANALYTICS_REPORTING_ENABLED=false \
    "${pw_args[@]}" \
    -v "$OBS_ETC/grafana/provisioning:/etc/grafana/provisioning:ro,Z" \
    -v "$OBS_ETC/grafana/dashboards:/var/lib/grafana/dashboards/context-guru:ro,Z" \
    -v "$OBS_STATE/grafana:/var/lib/grafana:Z" \
    "$GRAFANA_IMAGE" >/dev/null
  ok "cg-grafana on 127.0.0.1:3000"
  if [ -n "$generated" ]; then
    warn "generated the admin password below. It is NOT written anywhere — save it now:"
    printf '\n      admin / %s\n\n' "$generated"
    warn "lost it, or rotating it? There is no recovery, only a reset — see"
    warn "  deploy/grafana/README.md, 'Rotating the admin password'"
  fi

  say "Next"
  echo "  sudo $0 grafana-status          # scrape health + provisioned dashboards"
  echo "  ssh -L 3000:127.0.0.1:3000 $(hostname -f 2>/dev/null || hostname)"
  echo "  then: http://127.0.0.1:3000/d/context-guru/context-guru"
}

cmd_grafana_status() {
  local oci
  oci=$(oci_runtime) || { no "no container runtime"; exit 1; }
  say "Containers"
  $oci ps -a --filter name=cg-prometheus --filter name=cg-grafana \
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

  say "Grafana"
  if curl -sf --max-time 5 http://127.0.0.1:3000/api/health >/dev/null 2>&1; then
    ok "answering on 127.0.0.1:3000"
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
    echo "  dashboards (needs the admin password):"
    echo "    curl -s -u admin:PASSWORD 'http://127.0.0.1:3000/api/search?type=dash-db'"
  else
    no "Grafana is not answering on 127.0.0.1:3000"
  fi
}

cmd_grafana_remove() {
  need_root
  local oci
  oci=$(oci_runtime) || { no "no container runtime"; exit 1; }
  $oci rm -f cg-prometheus cg-grafana >/dev/null 2>&1 || true
  ok "removed cg-prometheus and cg-grafana"
  warn "kept $OBS_STATE — the metrics history and Grafana's database. Delete it by hand if"
  warn "you actually want the history gone."
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
