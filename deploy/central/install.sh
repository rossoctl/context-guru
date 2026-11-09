#!/usr/bin/env bash
# Install the central metrics stack on a server: OpenTelemetry Collector, Prometheus, Alertmanager
# and Grafana, in containers. Idempotent: safe to re-run after a change to a config file.
#
#   sudo ./install.sh install            write the config, make the state, start the stack
#   sudo ./install.sh status             containers, scrape targets, rule health, who pushed
#   sudo ./install.sh token add NAME     make a token for one install; prints it ONCE
#   sudo ./install.sh token revoke NAME  remove that install's token (restarts the collector)
#   sudo ./install.sh token list         the names of the tokens, never the tokens
#   sudo ./install.sh remove             drop the containers, KEEP the data
#
# Proxies push to this server. Each proxy needs the collector URL and its token:
#   Cortex:       port 4318      context-guru:  port 4319
# The collector speaks plain HTTP. A proxy refuses plain http to a remote host, so put TLS in front
# of 4318 and 4319 (nginx.example.conf). Without it, no remote proxy can push.
#
# NOTHING here is published except the two collector ports. Prometheus, Alertmanager and Grafana
# sit on a private container network, and Grafana is published on loopback only. Reach it with
# `ssh -L 3000:127.0.0.1:3000 <this host>`, or put your own front end in front of it.

set -euo pipefail

CENTRAL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ETC="${CENTRAL_ETC:-/etc/central-metrics}"
STATE="${CENTRAL_STATE:-/var/lib/central-metrics}"
NET=central-metrics

OTELCOL_IMAGE="${OTELCOL_IMAGE:-docker.io/otel/opentelemetry-collector-contrib:0.165.0}"
PROM_IMAGE="${PROM_IMAGE:-docker.io/prom/prometheus:v3.2.1}"
ALERTMANAGER_IMAGE="${ALERTMANAGER_IMAGE:-docker.io/prom/alertmanager:v0.28.1}"
GRAFANA_IMAGE="${GRAFANA_IMAGE:-docker.io/grafana/grafana:11.6.0}"
PROM_RETENTION="${PROM_RETENTION:-30d}"
PROM_RETENTION_SIZE="${PROM_RETENTION_SIZE:-20GB}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
# The collector's two ports on the host. Change them if something else holds them.
CORTEX_PORT="${CORTEX_PORT:-4318}"
CG_PORT="${CG_PORT:-4319}"
# Where the collector listens on the host. Loopback by default: a TLS front end on this host
# forwards to it. Set to 0.0.0.0 only if the front end is on another machine.
BIND="${BIND:-127.0.0.1}"
# The user the collector image runs as. It must be able to read the token files.
COLLECTOR_UID="${COLLECTOR_UID:-10001}"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
FAILS=0
no()  { printf '  \033[31m✗\033[0m %s\n' "$*"; FAILS=$((FAILS+1)); }
warn(){ printf '  \033[33m!\033[0m %s\n' "$*"; }
# A subcommand that prints a cross and exits 0 is worse than one that says nothing, so every
# subcommand ends with return_fails and the exit status matches what was printed.
return_fails() {
  [ "$FAILS" -eq 0 ] && return 0
  printf '\n\033[31m%d check(s) failed above.\033[0m\n' "$FAILS" >&2
  return 1
}

need_root() { [ "$(id -u)" = 0 ] || { echo "run this with sudo" >&2; exit 1; }; }

# podman or docker. Both accept every flag used below.
oci_runtime() {
  if command -v podman >/dev/null 2>&1; then echo podman
  elif command -v docker >/dev/null 2>&1; then echo docker
  else return 1; fi
}

require_oci() {
  if ! OCI=$(oci_runtime); then
    no "no container runtime. Install one: sudo dnf -y install podman"
    exit 1
  fi
}

# A name is used in a file name and in a log line. Allow only what is safe in both.
valid_name() { [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ ]]; }

# replace_file SRC DEST MODE: put SRC at DEST so that DEST is always either the old file or the whole new
# one. `install SRC DEST` is not that: it removes DEST first, so a failure partway (a chown that is
# refused, a full disk) leaves no file at all. For a token list that means every install loses its
# token at once. Here the new file is made complete beside DEST, given its owner and mode, and only
# then renamed over DEST. A rename inside one directory is atomic.
replace_file() {
  local src="$1" dest="$2" mode="$3" new
  new=$(mktemp "$dest.XXXXXX") || return 1
  if ! { cat "$src" > "$new" && chmod "$mode" "$new" && chown "$COLLECTOR_UID:$COLLECTOR_UID" "$new"; }; then
    rm -f "$new"
    echo "could not write $dest; the old file is unchanged" >&2
    return 1
  fi
  mv -f "$new" "$dest"
}

tokens_file() {
  case "$1" in
    cortex) echo "$ETC/tokens/cortex.txt" ;;
    context-guru) echo "$ETC/tokens/context-guru.txt" ;;
    *) echo "unknown proxy '$1' (use cortex or context-guru)" >&2; return 1 ;;
  esac
}

# --- install ---------------------------------------------------------------------------------

cmd_install() {
  need_root
  require_oci
  say "Container runtime"
  ok "$OCI ($($OCI --version 2>/dev/null | head -1))"

  say "Configuration"
  # install -m0644, not cp: root's umask leaves cp output 0640, and a container that cannot read
  # its own config crash-loops on "permission denied" with nothing else to go on.
  install -d -m0755 "$ETC" "$ETC/prometheus/rules" "$ETC/alertmanager" "$ETC/grafana/dashboards" \
    "$ETC/grafana/provisioning/datasources" "$ETC/grafana/provisioning/dashboards" \
    "$ETC/grafana/provisioning/plugins" "$ETC/grafana/provisioning/alerting"
  install -m0644 "$CENTRAL_DIR/otel-collector.yml" "$ETC/otel-collector.yml"
  install -m0644 "$CENTRAL_DIR/prometheus/prometheus.yml" "$ETC/prometheus/prometheus.yml"
  install -m0644 "$CENTRAL_DIR"/prometheus/rules/*.yml "$ETC/prometheus/rules/"
  # The Alertmanager config is the operator's once they add a delivery method. Never overwrite it.
  if [ -f "$ETC/alertmanager/alertmanager.yml" ]; then
    ok "$ETC/alertmanager/alertmanager.yml kept (it is yours once you add a receiver)"
  else
    install -m0644 "$CENTRAL_DIR/alertmanager/alertmanager.yml" "$ETC/alertmanager/alertmanager.yml"
  fi
  install -m0644 "$CENTRAL_DIR"/grafana/provisioning/datasources/*.yml "$ETC/grafana/provisioning/datasources/"
  install -m0644 "$CENTRAL_DIR"/grafana/provisioning/dashboards/*.yml "$ETC/grafana/provisioning/dashboards/"
  if compgen -G "$CENTRAL_DIR/grafana/dashboards/*.json" >/dev/null; then
    install -m0644 "$CENTRAL_DIR"/grafana/dashboards/*.json "$ETC/grafana/dashboards/"
  fi
  ok "$ETC"

  say "Tokens"
  # The token files are secrets. The collector image runs as uid 10001 and must read them, so they
  # belong to that uid and to nobody else can read them: 0600, folder 0700. A world-readable token
  # file would hand every install's token to any local user. An empty file means NO push is accepted.
  install -d -m0700 -o "$COLLECTOR_UID" -g "$COLLECTOR_UID" "$ETC/tokens"
  for f in cortex.txt context-guru.txt; do
    [ -f "$ETC/tokens/$f" ] || install -m0600 -o "$COLLECTOR_UID" -g "$COLLECTOR_UID" /dev/null "$ETC/tokens/$f"
    chown "$COLLECTOR_UID:$COLLECTOR_UID" "$ETC/tokens/$f"; chmod 0600 "$ETC/tokens/$f"
  done
  for p in cortex context-guru; do
    n=$(grep -c . "$(tokens_file "$p")" || true)
    ok "$p: $n token(s)"
    [ "$n" -gt 0 ] || warn "no $p proxy can push until you run: $0 token add $p NAME"
  done

  say "State"
  # The images run as fixed unprivileged users and none will chown its own volume.
  install -d -m0755 "$STATE"
  install -d -m0755 -o 65534 -g 65534 "$STATE/prometheus" "$STATE/alertmanager"
  install -d -m0755 -o 472 -g 0 "$STATE/grafana"
  ok "$STATE/{prometheus,alertmanager,grafana}"

  say "Network"
  $OCI network exists "$NET" 2>/dev/null || $OCI network create "$NET" >/dev/null
  ok "$NET (private)"

  say "Prometheus"
  $OCI rm -f cm-prometheus >/dev/null 2>&1 || true
  $OCI run -d --name cm-prometheus --network "$NET" --network-alias prometheus --restart=unless-stopped \
    -v "$ETC/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro,Z" \
    -v "$ETC/prometheus/rules:/etc/prometheus/rules:ro,Z" \
    -v "$STATE/prometheus:/prometheus:Z" \
    "$PROM_IMAGE" \
    --config.file=/etc/prometheus/prometheus.yml \
    --storage.tsdb.path=/prometheus \
    --storage.tsdb.retention.time="$PROM_RETENTION" \
    --storage.tsdb.retention.size="$PROM_RETENTION_SIZE" \
    --web.enable-remote-write-receiver >/dev/null
  ok "cm-prometheus (private), retention $PROM_RETENTION or $PROM_RETENTION_SIZE"

  say "Alertmanager"
  $OCI rm -f cm-alertmanager >/dev/null 2>&1 || true
  $OCI run -d --name cm-alertmanager --network "$NET" --network-alias alertmanager --restart=unless-stopped \
    -v "$ETC/alertmanager/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro,Z" \
    -v "$STATE/alertmanager:/alertmanager:Z" \
    "$ALERTMANAGER_IMAGE" --config.file=/etc/alertmanager/alertmanager.yml >/dev/null
  ok "cm-alertmanager (private)"

  start_collector

  say "Grafana"
  # Nobody needs a Grafana password at install time. A first install seeds the built-in admin with
  # a random value that is never printed and never written down. The operator sets their own after
  # (see README.md, "The admin password"). It is passed in an env FILE on the /run tmpfs, not in
  # argv: `docker run -e` puts it in /proc/<pid>/cmdline, which any local user can read.
  local pw="${GRAFANA_ADMIN_PASSWORD:-}"
  if [ -z "$pw" ] && [ ! -f "$STATE/grafana/grafana.db" ]; then
    pw=$(head -c 32 /dev/urandom | base64 | tr -d '/+=')
  fi
  local envfile
  envfile=$(mktemp /run/cm-grafana-env.XXXXXX)
  chmod 0600 "$envfile"
  {
    echo "GF_SERVER_HTTP_ADDR=0.0.0.0"
    echo "GF_AUTH_ANONYMOUS_ENABLED=false"
    echo "GF_USERS_ALLOW_SIGN_UP=false"
    echo "GF_ANALYTICS_REPORTING_ENABLED=false"
    [ -z "$pw" ] || echo "GF_SECURITY_ADMIN_PASSWORD=$pw"
  } > "$envfile"
  $OCI rm -f cm-grafana >/dev/null 2>&1 || true
  if ! $OCI run -d --name cm-grafana --network "$NET" --network-alias grafana --restart=unless-stopped \
    -p "127.0.0.1:$GRAFANA_PORT:3000" --env-file "$envfile" \
    -v "$ETC/grafana/provisioning:/etc/grafana/provisioning:ro,Z" \
    -v "$ETC/grafana/dashboards:/var/lib/grafana/dashboards/central:ro,Z" \
    -v "$STATE/grafana:/var/lib/grafana:Z" \
    "$GRAFANA_IMAGE" >/dev/null; then
    rm -f "$envfile"
    no "cm-grafana failed to start: $OCI logs cm-grafana"
    return_fails
    exit 1
  fi
  rm -f "$envfile"
  ok "cm-grafana on 127.0.0.1:$GRAFANA_PORT"
  if [ -z "${GRAFANA_ADMIN_PASSWORD:-}" ] && [ -n "$pw" ]; then
    warn "the built-in 'admin' account has a random password that was NOT saved."
    warn "Set your own: $OCI exec cm-grafana grafana cli admin reset-admin-password '<new>'"
  fi

  say "Next"
  echo "  sudo $0 token add cortex <install-name>       # one token for each install"
  echo "  sudo $0 status"
  echo "  Put TLS in front of $BIND:$CORTEX_PORT and $BIND:$CG_PORT (nginx.example.conf)."
  return_fails
}

# The collector reads its token files once, at start. A change to a token file needs a restart; it takes
# about 1.5 seconds, and a push that lands in that gap is repaired by the next one (cumulative totals).
start_collector() {
  say "Collector"
  $OCI rm -f cm-collector >/dev/null 2>&1 || true
  $OCI run -d --name cm-collector --network "$NET" --network-alias otel-collector --restart=unless-stopped \
    -p "$BIND:$CORTEX_PORT:4318" -p "$BIND:$CG_PORT:4319" \
    -v "$ETC/otel-collector.yml:/etc/otelcol/config.yml:ro,Z" \
    -v "$ETC/tokens:/etc/otelcol/tokens:ro,Z" \
    "$OTELCOL_IMAGE" --config=/etc/otelcol/config.yml >/dev/null
  ok "cm-collector on $BIND:$CORTEX_PORT (Cortex) and $BIND:$CG_PORT (context-guru)"
}

# --- tokens ----------------------------------------------------------------------------------

# A token file holds one token on each line and nothing else: the collector reads the whole line, so
# a name must not be on it. The name lives in a side file, tokens/<proxy>.names, as "<name> <sha256 of the token>".
# That lets `revoke NAME` find the line without storing the token twice.
cmd_token() {
  need_root
  local action="${1:-}" proxy name
  case "$action" in
    add)
      proxy="${2:-}"; name="${3:-}"
      [ -n "$proxy" ] && [ -n "$name" ] || { echo "usage: $0 token add cortex|context-guru NAME" >&2; exit 2; }
      valid_name "$name" || { echo "NAME may hold letters, digits, dot, dash, underscore" >&2; exit 2; }
      local f; f=$(tokens_file "$proxy") || exit 2
      local names="${f%.txt}.names"
      [ -f "$f" ] || { echo "run install first" >&2; exit 1; }
      if awk -v n="$name" '$1 == n {f=1} END {exit !f}' "$names" 2>/dev/null; then
        echo "a token named '$name' exists for $proxy; revoke it first" >&2; exit 1
      fi
      local tok; tok=$(head -c 32 /dev/urandom | base64 | tr -d '/+=\n')
      # A token without a name could never be revoked, so write the name first. If the token then
      # cannot be written, drop the name again.
      printf '%s %s\n' "$name" "$(printf '%s' "$tok" | sha256sum | cut -d' ' -f1)" >> "$names"
      if ! printf '%s\n' "$tok" >> "$f"; then
        awk -v n="$name" '$1 != n' "$names" > "$names.new" && mv -f "$names.new" "$names"
        echo "could not write the token; nothing was added" >&2; exit 1
      fi
      chown "$COLLECTOR_UID:$COLLECTOR_UID" "$f"; chmod 0600 "$f" "$names"
      require_oci
      restart_if_running
      echo "$tok"          # the ONLY time it is shown
      echo "  token for $proxy install '$name'. Store it now; it is not shown again." >&2
      ;;
    revoke)
      proxy="${2:-}"; name="${3:-}"
      [ -n "$proxy" ] && [ -n "$name" ] || { echo "usage: $0 token revoke cortex|context-guru NAME" >&2; exit 2; }
      valid_name "$name" || { echo "bad NAME" >&2; exit 2; }
      local f; f=$(tokens_file "$proxy") || exit 2
      local names="${f%.txt}.names"
      local h; h=$(awk -v n="$name" '$1==n {print $2}' "$names" 2>/dev/null)
      [ -n "$h" ] || { echo "no token named '$name' for $proxy" >&2; exit 1; }
      local tmp; tmp=$(mktemp)
      while IFS= read -r line; do
        [ "$(printf '%s' "$line" | sha256sum | cut -d' ' -f1)" = "$h" ] || printf '%s\n' "$line"
      done < "$f" > "$tmp"
      # The token list first, atomically. Only when that succeeded does the name leave the names file,
      # so a failure never leaves a name without its token or a token without its name.
      replace_file "$tmp" "$f" 0600 || { rm -f "$tmp"; exit 1; }
      rm -f "$tmp"
      awk -v n="$name" '$1 != n' "$names" > "$names.new"
      mv -f "$names.new" "$names"; chmod 0600 "$names"
      require_oci
      restart_if_running
      echo "revoked '$name' for $proxy"
      ;;
    list)
      for proxy in cortex context-guru; do
        local names; names="$(tokens_file "$proxy" | sed 's/\.txt$/.names/')"
        echo "$proxy:"
        if [ -s "$names" ]; then awk '{print "  " $1}' "$names"; else echo "  (none)"; fi
      done
      ;;
    *) echo "usage: $0 token add|revoke|list ..." >&2; exit 2 ;;
  esac
}

restart_if_running() {
  if $OCI inspect cm-collector >/dev/null 2>&1; then
    $OCI restart cm-collector >/dev/null && echo "  collector restarted to load the token list" >&2
  fi
}

# --- status ----------------------------------------------------------------------------------

cmd_status() {
  need_root
  require_oci
  say "Containers"
  for c in cm-collector cm-prometheus cm-alertmanager cm-grafana; do
    if [ "$($OCI inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ]; then ok "$c running"; else no "$c is not running: $OCI logs $c"; fi
  done

  say "Prometheus"
  local out
  if out=$($OCI exec cm-prometheus wget -qO- http://localhost:9090/api/v1/targets 2>/dev/null); then
    printf '%s' "$out" | python3 -c '
import sys, json
for t in json.load(sys.stdin)["data"]["activeTargets"]:
    print("  %s  %-16s %s" % ("\033[32m✓\033[0m" if t["health"]=="up" else "\033[31m✗\033[0m", t["labels"]["job"], t["lastError"][:70]))' 2>/dev/null || warn "could not read targets"
    local rules
    rules=$($OCI exec cm-prometheus wget -qO- http://localhost:9090/api/v1/rules 2>/dev/null | python3 -c '
import sys, json
g = json.load(sys.stdin)["data"]["groups"]
n = sum(len(x["rules"]) for x in g); bad = sum(1 for x in g for r in x["rules"] if r["health"] != "ok")
print("%d rules, %d unhealthy" % (n, bad))' 2>/dev/null || echo "unknown")
    case "$rules" in *" 0 unhealthy") ok "$rules" ;; *) no "$rules" ;; esac
    local machines
    machines=$($OCI exec cm-prometheus wget -qO- 'http://localhost:9090/api/v1/query?query=count(count%20by%20(proxy,machine)%20(llm_proxy_info))' 2>/dev/null \
      | python3 -c 'import sys,json; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)' 2>/dev/null || echo 0)
    ok "$machines machine(s) are pushing"
  else
    no "Prometheus does not answer"
  fi
  return_fails
}

# --- remove ----------------------------------------------------------------------------------

cmd_remove() {
  need_root
  require_oci
  for c in cm-grafana cm-collector cm-alertmanager cm-prometheus; do $OCI rm -f "$c" >/dev/null 2>&1 || true; done
  $OCI network rm "$NET" >/dev/null 2>&1 || true
  ok "containers dropped. The data in $STATE and the config in $ETC are kept."
}

case "${1:-}" in
  install) cmd_install ;;
  status)  cmd_status ;;
  token)   shift; cmd_token "$@" ;;
  remove)  cmd_remove ;;
  *) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
