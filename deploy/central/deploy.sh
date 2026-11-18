#!/usr/bin/env bash
# Deploy and manage the central metrics stack on a RHEL 9 server, from your laptop, over ssh.
#
#   ./deploy.sh [--dry-run] [--install-deps] USER@HOST deploy      validate, copy, install, status
#   ./deploy.sh USER@HOST stage                                    only copy the folder and move `current` (no install)
#   ./deploy.sh USER@HOST nginx                                    TLS front end (needs CENTRAL_HOST, TLS_CERT, TLS_KEY)
#   ./deploy.sh USER@HOST status                                   containers, targets, rules, who pushed
#   ./deploy.sh USER@HOST check                                    from HERE: does https://CENTRAL_HOST accept an empty push (expect 200)?
#   ./deploy.sh USER@HOST logs [collector|prometheus|alertmanager|grafana]
#   ./deploy.sh USER@HOST remove                                   drop the containers, KEEP the data
#
# What it does on the server: install.sh does the work (see its header). This script only copies the
# folder to $HOME/central-metrics-src/releases/<time>, points `current` at it, and runs install.sh with sudo.
# It never deletes data. A release folder holds config and scripts only.
#
# Settings (environment), all optional except for `nginx` and `check`:
#   CENTRAL_HOST   the name proxies use in their URL, for example metrics.example.com
#   TLS_CERT       certificate chain ON THE SERVER, leaf first   (default /etc/ssl/central/fullchain.pem)
#   TLS_KEY        private key ON THE SERVER                      (default /etc/ssl/central/privkey.pem)
#   BIND, GRAFANA_PORT, PROM_RETENTION, PROM_RETENTION_SIZE, *_IMAGE   passed to install.sh when set
#
# --dry-run prints every remote command and runs none of them.
# --install-deps lets `deploy` run `sudo dnf -y install podman` when no container runtime is found.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRY=0; DEPS=0
# Expanded by the SERVER's shell, so it is the server user's home, not this machine's.
# shellcheck disable=SC2016  # $HOME is meant to stay literal
SRC_ROOT='$HOME/central-metrics-src'

die()  { printf '%s\n' "$*" >&2; exit 1; }
say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1; shift ;;
    --install-deps) DEPS=1; shift ;;
    -h|--help) usage ;;
    *) break ;;
  esac
done
[ $# -ge 2 ] || usage
TARGET="$1"; CMD="$2"; shift 2
[[ "$TARGET" =~ ^[A-Za-z0-9._@-]+$ ]] || die "'$TARGET' is not a user@host"

# Environment passed to install.sh. Only names from this list, and only when set, so a stray
# variable on the laptop never reaches the server.
ENV_PASS=(CENTRAL_HOST TLS_CERT TLS_KEY BIND GRAFANA_PORT CORTEX_PORT CG_PORT PROM_RETENTION PROM_RETENTION_SIZE
          OTELCOL_IMAGE PROM_IMAGE ALERTMANAGER_IMAGE GRAFANA_IMAGE)
env_args() {
  local v out=()
  for v in "${ENV_PASS[@]}"; do
    if [ -n "${!v:-}" ]; then out+=("$(printf '%s=%q' "$v" "${!v}")"); fi
  done
  printf '%s ' "${out[@]+"${out[@]}"}"
}

# run_remote COMMAND: one command line for the server's shell. -t gives sudo a terminal for its password.
run_remote() {
  if [ "$DRY" = 1 ]; then printf '  [dry-run] ssh -t %s %s\n' "$TARGET" "$1"; return 0; fi
  ssh -t -o ServerAliveInterval=30 "$TARGET" "$1"
}

# install_sh ARGS...: run install.sh from the current release, as root, with the settings above.
install_sh() {
  local a; a=$(printf '%q ' "$@")
  run_remote "cd $SRC_ROOT/current && sudo env $(env_args)./install.sh $a"
}

validate_local() {
  say "Checking the folder here"
  bash -n "$HERE/install.sh" && ok "install.sh parses"
  if command -v shellcheck >/dev/null 2>&1; then shellcheck -x "$HERE/install.sh" && ok "shellcheck is clean"; fi
  if [ -f "$HERE/grafana/gen_dashboards.py" ]; then
    python3 "$HERE/grafana/gen_dashboards.py" --check >/dev/null || die "the dashboard JSON is out of date: run grafana/gen_dashboards.py and commit"
    ok "dashboards match their generator"
  fi
  if command -v promtool >/dev/null 2>&1; then
    promtool check rules "$HERE"/prometheus/rules/*.yml >/dev/null && promtool test rules "$HERE/prometheus/tests/alerts_test.yml" >/dev/null && ok "promtool: rules and tests pass"
  else
    printf '  - promtool not found here: the rule tests were not run\n'
  fi
}

preflight_remote() {
  say "Checking the server"
  run_remote "set -e; . /etc/os-release; echo \"os: \$PRETTY_NAME\"; case \"\$ID \$ID_LIKE\" in *rhel*|*fedora*|*centos*) ;; *) echo 'not a RHEL family system' >&2; exit 1;; esac; sudo -n true 2>/dev/null || echo 'sudo will ask for a password'; if command -v podman >/dev/null || command -v docker >/dev/null; then echo 'container runtime: found'; else echo 'container runtime: MISSING'; exit 3; fi" || {
    local rc=$?
    if [ "$rc" = 3 ] && [ "$DEPS" = 1 ]; then
      run_remote "sudo dnf -y install podman"
    elif [ "$rc" = 3 ]; then
      die "no podman or docker on the server. Run again with --install-deps, or: ssh $TARGET sudo dnf -y install podman"
    else
      die "the server check failed (exit $rc)"
    fi
  }
}

stage_release() {
  say "Copying the folder"
  local ts; ts=$(date +%Y%m%d-%H%M%S)
  local rel="$SRC_ROOT/releases/$ts"
  if [ "$DRY" = 1 ]; then
    printf '  [dry-run] tar (%s, without compose.yaml and the tests) | ssh %s "mkdir -p %s && tar -xzf - -C %s"\n' "$HERE" "$TARGET" "$rel" "$rel"
  else
    # shellcheck disable=SC2029  # on purpose: $rel is built here and expanded there
    tar -C "$HERE" --exclude=./compose.yaml --exclude=./prometheus/tests --exclude='*.pyc' --exclude=__pycache__ -czf - . \
      | ssh "$TARGET" "mkdir -p $rel && tar -xzf - -C $rel"
  fi
  run_remote "ln -sfn $rel $SRC_ROOT/current && cd $SRC_ROOT/releases && ls -1d ./* | sort -r | tail -n +6 | xargs -r rm -rf"
  ok "release $ts is current (the last 5 are kept)"
}

case "$CMD" in
  deploy)
    validate_local
    preflight_remote
    stage_release
    say "Installing"
    install_sh install
    say "Status"
    install_sh status
    if [ -z "${CENTRAL_HOST:-}" ]; then
      printf '\nNext: set CENTRAL_HOST, TLS_CERT and TLS_KEY, then run:  %s %s nginx\n' "$0" "$TARGET"
    fi
    ;;
  nginx)
    [ -n "${CENTRAL_HOST:-}" ] || die "set CENTRAL_HOST (and TLS_CERT, TLS_KEY: paths on the server)"
    install_sh nginx
    ;;
  stage) validate_local; stage_release ;;
  status) install_sh status ;;
  remove) install_sh remove ;;
  token) die "token commands are off: the collector does not check tokens today (see README.md)" ;;
  logs)
    c="${1:-collector}"
    case "$c" in collector|prometheus|alertmanager|grafana) ;; *) die "logs: one of collector, prometheus, alertmanager, grafana" ;; esac
    run_remote "sudo \$(command -v podman || command -v docker) logs --tail 100 cm-$c"
    ;;
  check)
    [ -n "${CENTRAL_HOST:-}" ] || die "set CENTRAL_HOST"
    say "From this machine to https://$CENTRAL_HOST"
    for p in cortex context-guru; do
      # An empty OTLP request carries no data, so it changes nothing; the collector answers 200.
      code=$(curl -sS -o /dev/null -w '%{http_code}' -m 10 -X POST -H 'content-type: application/json' -d '{}' "https://$CENTRAL_HOST/$p/v1/metrics" 2>&1) || { printf '  ✗ %s: %s\n' "$p" "$code"; continue; }
      if [ "$code" = 200 ]; then ok "/$p accepts a push over TLS (answered 200)"; else printf '  ✗ /%s answered %s (expected 200)\n' "$p" "$code"; fi
    done
    ;;
  *) usage ;;
esac
