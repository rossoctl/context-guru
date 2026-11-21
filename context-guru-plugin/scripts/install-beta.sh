#!/usr/bin/env bash
# Install a beta (pre-release) of the context-guru proxy on this machine, and optionally restart
# the proxy onto it.
#
#   install-beta.sh [<tag>] [--restart] [--port <n>]
#
#   <tag>        a release tag such as v0.7.5-beta.1. Default: the newest pre-release.
#   --restart    stop the proxy on the port and start it again on the new binary. Off by default:
#                a restart cuts every session that is routed through that port.
#   --port <n>   the port to restart. Default: the port this project is installed on.
#
# Why this is not just `CONTEXT_GURU_VERSION=<tag> install.sh`:
#
#   1. The install.sh of a beta can differ from the one in the installed plugin. This script
#      fetches the plugin scripts of THE BETA'S TAG and runs those.
#   2. The installed plugin can be older than the beta. An old settings.py reads a fixed strategy
#      table and does not know new strategies. So the restart, and the steps the skill runs
#      after it, use the beta tag's scripts (`beta_scripts=`). This script never edits the
#      Claude Code plugin cache; it says when the plugin is older and how to update it.
#   3. #1254: with the update answer `always`, start-proxy.sh compares the installed version with
#      releases/latest by inequality and re-installs the older stable over the beta.
#   4. A running proxy keeps its old binary until it restarts.
#
# Every fact is printed as `key=value` on stdout. The output of the beta's install.sh is passed on
# with the prefix `install_`. The download is checked by the beta's install.sh against the
# release's checksums.txt, and it fails closed. The plugin scripts come from the tag's source
# archive over TLS; they have no checksum of their own.
#
# Env (for tests and mirrors): CONTEXT_GURU_REPO, CONTEXT_GURU_DEST, CONTEXT_GURU_STATE.
set -uo pipefail

REPO="${CONTEXT_GURU_REPO:-rossoctl/context-guru}"
DEST="${CONTEXT_GURU_DEST:-$HOME/.local/bin}"
BIN=context-guru-proxy
STATE="${CONTEXT_GURU_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/context-guru}"

emit() { printf '%s\n' "$*"; }
# RESTORE_ANSWER is set once the update answer was changed (step 4). Every die after that puts the
# old answer back, so a failed install does not leave the user's setting changed.
RESTORE_ANSWER=""
SCRIPTS=""
die() {
  if [ -n "$RESTORE_ANSWER" ] && [ -n "$SCRIPTS" ]; then
    if python3 "$SCRIPTS/settings.py" update-check answer --answer always 2>/dev/null | grep -q '^answer=auto$'; then
      emit "update_answer_restored=${RESTORE_ANSWER}"
    else
      emit "update_answer_restore_failed=true"
      emit "note=the update answer is still never. Turn it back on with: settings.py update-check answer --answer always"
    fi
  fi
  emit "result=error"; emit "reason=$1"; exit 1
}

TAG=""
RESTART=0
PORT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --restart) RESTART=1 ;;
    --port)
      case "${2:-}" in ''|*[!0-9]*) die "bad_port: --port needs a number" ;; esac
      PORT="$2"; shift ;;
    -*) die "unknown_flag: $1" ;;
    *)  [ -z "$TAG" ] || die "too_many_arguments: one tag only"
        TAG="$1" ;;
  esac
  shift
done

command -v curl >/dev/null 2>&1 || die "no_curl"
command -v python3 >/dev/null 2>&1 || die "no_python3"
command -v tar >/dev/null 2>&1 || die "no_tar"

# --- 1. which tag ------------------------------------------------------------------------
if [ -z "$TAG" ]; then
  # The newest PRE-RELEASE. `releases/latest` never returns one, so list the releases. The API
  # has no token here and is rate limited (60 per hour per IP): say so, as install.sh does.
  resp=$(curl -sSL --max-time 15 -w '\n%{http_code}' \
           "https://api.github.com/repos/${REPO}/releases?per_page=30" 2>/dev/null)
  code=$(printf '%s' "$resp" | tail -n 1)
  body=$(printf '%s' "$resp" | sed '$d')
  case "$code" in
    200) ;;
    403|429) die "github_rate_limited: GitHub's API is rate limited for this IP (60/hour, shared with everything behind the same address), so the newest beta could not be found. This says NOTHING about whether a beta exists. Wait for the window to reset, or pass the tag: install-beta.sh vX.Y.Z-beta.N" ;;
    *) die "no_release_list:${code:-unreachable}" ;;
  esac
  TAG=$(printf '%s' "$body" | python3 -I -c '
import json, sys
try:
    rels = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for r in rels if isinstance(rels, list) else []:
    if r.get("prerelease") and not r.get("draft") and r.get("tag_name"):
        print(r["tag_name"])
        break
')
  [ -n "$TAG" ] || die "no_prerelease_found: no pre-release in the last 30 releases of ${REPO}. Pass the tag: install-beta.sh vX.Y.Z-beta.N"
fi
# The tag goes into URLs and paths. Accept only a release-tag shape.
printf '%s' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
  die "bad_tag: '$TAG' is not a release tag like v0.7.5-beta.1"
emit "tag=${TAG}"

# --- 2. the plugin scripts of that tag, in a new empty directory ----------------------------
WORK=$(mktemp -d "${TMPDIR:-/tmp}/cg-beta.XXXXXX") || die "no_tmpdir"
KEEP=0
trap '[ "$KEEP" = 1 ] || rm -rf "$WORK"' EXIT
mkdir -p "$WORK/dl" "$WORK/plugin" || die "no_tmpdir"

curl -fsSL --max-time 120 -o "$WORK/dl/src.tar.gz" \
    "https://github.com/${REPO}/archive/refs/tags/${TAG}.tar.gz" 2>/dev/null ||
  die "source_unavailable: could not download the source archive of ${TAG}"
# The top directory must not be '.' or '..', and the file names must not be either, so no member
# can leave the extraction directory.
members=$(tar tzf "$WORK/dl/src.tar.gz" 2>/dev/null |
  grep -E '^[^/]+/context-guru-plugin/(scripts/[^/]+|\.claude-plugin/plugin\.json)$' |
  grep -Ev '^\.{1,2}/|/\.{1,2}$')
[ -n "$members" ] || die "plugin_not_in_tag: ${TAG} has no context-guru-plugin/scripts"
# Extract only those exact members.
# An array, so a name with a space stays one argument. No -T: bsdtar and GNU tar read it differently.
want=()
while IFS= read -r m; do want+=("$m"); done <<<"$members"
tar xzf "$WORK/dl/src.tar.gz" -C "$WORK/dl" "${want[@]}" 2>/dev/null || die "untar_failed"
top=$(printf '%s\n' "$members" | head -n 1 | cut -d/ -f1)
cp -R "$WORK/dl/$top/context-guru-plugin/." "$WORK/plugin/" || die "copy_failed"
chmod +x "$WORK"/plugin/scripts/*.sh "$WORK"/plugin/scripts/*.py 2>/dev/null
for f in install.sh settings.py start-proxy.sh; do
  [ -f "$WORK/plugin/scripts/$f" ] || die "plugin_not_in_tag: ${TAG} has no scripts/$f"
done
SCRIPTS="$WORK/plugin/scripts"

# --- 3. is the installed plugin older than the beta? -----------------------------------------
# Compared on X.Y.Z only. Reported, never fixed: the plugin cache belongs to Claude Code.
HERE="$(unset CDPATH; cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
plugin_version() { # $1 = plugin.json
  python3 -I -c '
import json, sys
try:
    print(json.load(open(sys.argv[1])).get("version", ""))
except (OSError, ValueError):
    pass
' "$1" 2>/dev/null
}
have_plugin=$(plugin_version "$HERE/../.claude-plugin/plugin.json")
beta_plugin=$(plugin_version "$WORK/plugin/.claude-plugin/plugin.json")
emit "plugin_installed=${have_plugin:-unknown}"
emit "plugin_in_beta=${beta_plugin:-unknown}"
if [ -n "$have_plugin" ] && [ -n "$beta_plugin" ]; then
  older=$(python3 -I -c '
import re, sys
def v(s):
    m = re.match(r"(\d+)\.(\d+)\.(\d+)", s)
    return tuple(int(x) for x in m.groups()) if m else None
a, b = v(sys.argv[1]), v(sys.argv[2])
print("true" if a and b and a < b else "false")
' "$have_plugin" "$beta_plugin" 2>/dev/null)
  emit "plugin_older=${older:-false}"
  if [ "$older" = true ]; then
    emit "plugin_update_command=/plugin marketplace update ${REPO}"
    emit "note=the installed plugin ${have_plugin} is older than the plugin in ${TAG} (${beta_plugin}); it may not know the beta's strategies. Update it, or run settings.py from beta_scripts"
  fi
fi

# --- 4. protect the beta from #1254 ---------------------------------------------------------
# TODO(#1254): remove this step once #1254 is fixed and released. Until then, the update answer
# `always` (shown as `answer=auto`) makes start-proxy.sh install the older `latest` over this beta
# in the background. `never` stops that. It is a user setting, so we say that we changed it and
# how to change it back.
upd=$(python3 "$SCRIPTS/settings.py" update-check show 2>/dev/null)
answer=$(printf '%s\n' "$upd" | sed -n 's/^answer=//p' | head -n 1)
# `show` prints result=ok with an answer. Anything else (file not ours, unreadable, no python)
# means we cannot tell, so say so: the guard did not run.
printf '%s\n' "$upd" | grep -q '^result=ok$' || { answer=unreadable; emit "note=the update answer could not be read, so the #1254 guard did not run. Set it yourself: settings.py update-check answer --answer never"; }
case "$answer" in
  auto|always)
    out=$(python3 "$SCRIPTS/settings.py" update-check answer --answer never 2>/dev/null)
    if printf '%s\n' "$out" | grep -q '^answer=never$'; then
      emit "update_answer_was=${answer}"
      emit "update_answer_now=never"
      RESTORE_ANSWER="$answer"
      emit "update_answer_restore=settings.py update-check answer --answer always"
      emit "note=the update answer was ${answer}. It is now never, because of #1254: auto-update would install the older stable over the beta. Turn it back on with: settings.py update-check answer --answer always"
    else
      die "update_answer_not_set: could not set the update answer to never (#1254). Nothing was installed. Run: settings.py update-check answer --answer never"
    fi ;;
  *) emit "update_answer=${answer:-none}" ;;
esac

# --- 5. install the beta, with the beta's own install.sh -------------------------------------
iout=$(CONTEXT_GURU_VERSION="$TAG" CONTEXT_GURU_DEST="$DEST" CONTEXT_GURU_STATE="$STATE" \
         bash "$SCRIPTS/install.sh" 2>/dev/null)
irc=$?
printf '%s\n' "$iout" | while IFS= read -r line; do
  case "$line" in *=*) emit "install_${line}" ;; esac
done
iresult=$(printf '%s\n' "$iout" | sed -n 's/^result=//p' | tail -n 1)
if [ "$irc" -ne 0 ] || { [ "$iresult" != installed ] && [ "$iresult" != present ]; }; then
  ireason=$(printf '%s\n' "$iout" | sed -n 's/^reason=//p' | tail -n 1)
  die "install_failed: ${ireason:-install.sh exited $irc}"
fi

# --- 6. check what is on disk ----------------------------------------------------------------
got=$("$DEST/$BIN" --version 2>/dev/null | awk '{print $2; exit}')
emit "installed_version=${got:-unknown}"
[ "$got" = "$TAG" ] || die "version_mismatch: ${DEST}/${BIN} reports '${got:-nothing}', expected ${TAG}"
first=$(command -v "$BIN" 2>/dev/null || true)
if [ -n "$first" ] && [ "$first" != "$DEST/$BIN" ]; then
  emit "path_shadow=${first}"
  emit "note=another ${BIN} comes first on PATH (${first}); a session may start that one"
fi
# The beta is on disk now. From here the answer stays `never` even if a later step fails: the
# installed beta still needs the #1254 guard.
RESTORE_ANSWER=""
KEEP=1   # the skill runs settings.py and start-proxy.sh from beta_scripts after this
emit "beta_scripts=${SCRIPTS}"

# --- 7. restart (only when asked) -----------------------------------------------------------
if [ "$RESTART" != 1 ]; then
  emit "restart=not_requested"
  emit "note=a running proxy keeps its old binary until it restarts; run again with --restart to restart it (this cuts every session routed through that port)"
  emit "result=installed"
  exit 0
fi

kv() { sed -n "s/^$1=//p" | head -n 1; }
if [ -z "$PORT" ]; then
  PORT=$(python3 "$SCRIPTS/settings.py" port install 2>/dev/null | kv port)
fi
case "$PORT" in ''|*[!0-9]*) emit "restart=skipped"; die "port_unknown: pass --port <n>; this project has no recorded port" ;; esac
emit "port=${PORT}"

# The preset: a restart without --preset resets it to off (#1244). Use the configured one; if
# none is configured, use the one the running proxy was started with.
pshow=$(python3 "$SCRIPTS/settings.py" preset show 2>/dev/null)
preset=$(printf '%s\n' "$pshow" | kv preset)
pfile=$(printf '%s\n' "$pshow" | kv file)
if [ -z "$preset" ] || [ "$pfile" = "(none)" ]; then
  fp=$(sed -n 's/^preset=\([^ ]*\).*/\1/p' "$STATE/proxy-${PORT}.fingerprint" 2>/dev/null | head -n 1)
  [ -n "$fp" ] && preset="$fp"
fi
[ -n "$preset" ] || preset=off
emit "preset=${preset}"

# Stop by pidfile, after proving the pid is our proxy on this port and owned by this user.
# Never pkill: the machine is shared with other sessions and other proxies.
pidfile="$STATE/proxy-${PORT}.pid"
pid=$(tr -dc '0-9' <"$pidfile" 2>/dev/null)
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  cmdline=$(ps -p "$pid" -o command= 2>/dev/null)
  owner=$(ps -p "$pid" -o uid= 2>/dev/null | tr -d ' ')
  case "$cmdline" in
    *"$BIN"*"--listen 127.0.0.1:${PORT}"*) ;;
    *) die "pid_not_ours: pid ${pid} in ${pidfile} is not a ${BIN} on port ${PORT} (${cmdline:-no process}); not stopped" ;;
  esac
  [ "$owner" = "$(id -u)" ] || die "pid_not_yours: pid ${pid} belongs to uid ${owner}; not stopped"
  kill "$pid" 2>/dev/null
  # The proxy drains requests for up to 25 s. Wait for the port, not only for the pid.
  i=0
  while curl -fsS --max-time 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -le 40 ] || die "stop_timeout: pid ${pid} still answers on port ${PORT} after 40 s; not forced"
    sleep 1
  done
  emit "stopped_pid=${pid}"
elif curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  die "port_in_use_not_ours: something answers on port ${PORT} and there is no live pid in ${pidfile}; not stopped"
else
  emit "stopped_pid=none"
fi

cmd=("$SCRIPTS/start-proxy.sh" --unrouted --no-update-check --port "$PORT" --preset "$preset")
up=$(python3 "$SCRIPTS/settings.py" config 2>/dev/null | kv option_upstream)
[ -n "$up" ] && cmd+=(--upstream "$up")
idle=$(python3 "$SCRIPTS/settings.py" config 2>/dev/null | kv option_idle_exit)
[ -n "$idle" ] && cmd+=(--idle-exit "$idle")
CONTEXT_GURU_BIN="$DEST/$BIN" CONTEXT_GURU_STATE="$STATE" "${cmd[@]}" 2>&1 | sed 's/^/start_proxy: /'

if curl -fsS --max-time 5 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  emit "healthz=ok"
  emit "restart=done"
  emit "result=installed"
else
  emit "healthz=fail"
  emit "restart=failed"
  emit "reason=proxy_not_answering_after_restart"
  emit "result=error"
  exit 1
fi
