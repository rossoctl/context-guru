#!/usr/bin/env bash
# Build Cortex with a chosen context-guru version, and optionally install it.
#
#   scripts/cortex-with-cg.sh [CG_VERSION] [--install] [--cortex-ref REF]
#
# Examples:
#   scripts/cortex-with-cg.sh                  # newest context-guru tag, build only
#   scripts/cortex-with-cg.sh v0.7.9           # build only
#   scripts/cortex-with-cg.sh v0.7.9 --install # build, install, restart the proxy
#
# What it does:
#   1. Clones Cortex to $CORTEX_DIR (default ~/git/cortex) if it is not there.
#   2. Makes a temporary git worktree at the installed Cortex version (or --cortex-ref),
#      so only the context-guru version changes. Your Cortex checkout is never touched:
#      the go.mod / go.sum edit lives in the temporary worktree and goes with it.
#   3. Runs "go get github.com/rossoctl/context-guru@CG_VERSION" and builds cortex and
#      agentop with "make cortex agentop" (profile "full", which links context-guru).
#   4. Checks with "go version -m" that the binary carries CG_VERSION.
#   5. With --install: runs the built agentop's "setup --from <bin> --restart", which is
#      how Cortex installs a source build. The restart cuts attached agent sessions for
#      a few seconds. Then it checks the proxy health and the installed binary version.
#
# The installed build reports its version as "dev". Cortex never auto-updates a dev
# build, so you stay on this context-guru version until you change it. To go back to
# a Cortex release (with its own pinned context-guru):
#   agentop update --ref=v0.10.2      # or the release you want
#
# Each build stays in ~/.cache/cortex-cg/<cortex>-cg-<version>/. Nothing removes old
# ones; delete that folder when you like.
#
# The context-guru plugin must be in ~/.cortex/config.yaml under
# pipeline.outbound.plugins, after the parsers. "agentop setup" adds it. The script
# warns when it is missing.
set -euo pipefail

CG_MODULE=github.com/rossoctl/context-guru
CG_REPO=git@github.com:rossoctl/context-guru.git
CORTEX_REPO="${CORTEX_REPO:-git@github.com:rossoctl/cortex.git}"
CORTEX_DIR="${CORTEX_DIR:-$HOME/git/cortex}"
CORTEX_CONFIG="${CORTEX_CONFIG:-$HOME/.cortex/config.yaml}"
INSTALLED_CORTEX="${INSTALLED_CORTEX:-$HOME/.local/bin/cortex}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:47604/healthz}"

cg_version=""
cortex_ref=""
install=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --install) install=1 ;;
    --cortex-ref) cortex_ref="${2:?--cortex-ref needs a value}"; shift ;;
    -h|--help) awk 'NR>1 && !/^#/ {exit} NR>1 {sub(/^# ?/, ""); print}' "$0"; exit 0 ;;
    v[0-9]*) cg_version="$1" ;;
    *) echo "unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null || die "go is not on PATH"
command -v git >/dev/null || die "git is not on PATH"
command -v make >/dev/null || die "make is not on PATH"
command -v curl >/dev/null || die "curl is not on PATH"

# The context-guru version a cortex binary carries, or "" when it has none.
cg_dep() { go version -m "$1" 2>/dev/null | awk -v m="$CG_MODULE" '$1=="dep" && $2==m {print $3}' || true; }

# The context-guru version: the newest release tag when none is given.
if [[ -z "$cg_version" ]]; then
  # "|| true": with pipefail, a failed ls-remote or no match would stop the script
  # here with no message. The check below reports it instead.
  cg_version=$(git ls-remote --tags --refs "$CG_REPO" 'v*' |
    sed 's#.*refs/tags/##' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
  [[ -n "$cg_version" ]] || die "could not read the context-guru tags"
  say "newest context-guru release: $cg_version"
fi

# The Cortex checkout.
if [[ ! -d "$CORTEX_DIR/.git" ]]; then
  say "cloning Cortex to $CORTEX_DIR"
  git clone -q "$CORTEX_REPO" "$CORTEX_DIR"
fi
git -C "$CORTEX_DIR" fetch -q --tags origin

is_commit() { [[ -n "$1" ]] && git -C "$CORTEX_DIR" rev-parse -q --verify "$1^{commit}" >/dev/null; }

# The Cortex version: the installed one, so only context-guru changes.
#   1. The last word of "cortex --version": "cortex v0.10.2" or "cortex (cpex) v0.10.2".
#   2. A build from this script says "cortex dev". Its source commit is in the binary
#      (vcs.revision), so a second run keeps the same Cortex.
#   3. Else stop: a silent fall back to main would change Cortex too.
if [[ -z "$cortex_ref" ]]; then
  [[ -x "$INSTALLED_CORTEX" ]] || die "no Cortex at $INSTALLED_CORTEX; pass --cortex-ref vX.Y.Z"
  cortex_ref=$("$INSTALLED_CORTEX" --version 2>/dev/null | awk '{print $NF}' || true)
  if ! is_commit "$cortex_ref"; then
    rev=$(go version -m "$INSTALLED_CORTEX" 2>/dev/null |
      awk '$1=="build" && $2 ~ /^vcs.revision=/ {sub(/^vcs.revision=/, "", $2); print $2}' || true)
    is_commit "$rev" || die "cannot tell which Cortex version is installed ('${cortex_ref:-none}'); pass --cortex-ref vX.Y.Z"
    cortex_ref=$(git -C "$CORTEX_DIR" describe --tags --exact-match "$rev" 2>/dev/null || echo "$rev")
  fi
fi
is_commit "$cortex_ref" || die "'$cortex_ref' is not a commit in $CORTEX_DIR"
say "Cortex version: $cortex_ref"

# A temporary worktree, so the go.mod / go.sum change never reaches the checkout.
work=$(mktemp -d "${TMPDIR:-/tmp}/cortex-cg.XXXXXX")
[[ -n "$work" && -d "$work" ]] || die "mktemp failed"
cleanup() {
  git -C "$CORTEX_DIR" worktree remove --force "$work/cortex" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
git -C "$CORTEX_DIR" worktree add -q --detach "$work/cortex" "$cortex_ref"
src="$work/cortex"

say "go get $CG_MODULE@$cg_version"
(cd "$src/cmd/cortex" && GOWORK=off go get "$CG_MODULE@$cg_version")

say "building cortex and agentop (profile full)"
make -C "$src" cortex agentop PROFILE=full >"$work/build.log" 2>&1 || {
  tail -30 "$work/build.log" >&2; die "build failed (log above)"
}

built=$(cg_dep "$src/bin/cortex")
[[ "$built" == "$cg_version" ]] || die "the built cortex has context-guru '${built:-none}', not $cg_version"
go version -m "$src/bin/cortex" | grep -q include_plugin_contextguru ||
  die "the built cortex does not link the context-guru plugin"
say "built cortex with context-guru $built"

if [[ -f "$CORTEX_CONFIG" ]] && ! grep -qE '^\s*- name: context-guru\s*$' "$CORTEX_CONFIG"; then
  cat >&2 <<'EOF'
warning: no context-guru plugin in the Cortex config. Add it under
pipeline.outbound.plugins, after the parsers:

      - name: context-guru
        config:
          engine:
            preset: xhigh
EOF
fi

name="cortex-$cortex_ref-cg-$cg_version"
out="${CORTEX_CG_OUT:-$HOME/.cache/cortex-cg}/${name//\//-}"
mkdir -p "$out"
cp "$src/bin/cortex" "$src/bin/agentop" "$out/"
say "binaries in $out"

if [[ $install -eq 0 ]]; then
  say "build only. To install it, run again with --install"
  exit 0
fi

say "installing (agentop setup --from ... --restart); agent sessions pause for a few seconds"
"$out/agentop" setup --from "$out" --yes --no-modify-path --restart

for _ in $(seq 1 30); do curl -sf "$HEALTH_URL" >/dev/null 2>&1 && break; sleep 1; done
curl -sf "$HEALTH_URL" >/dev/null 2>&1 || die "the proxy is not healthy at $HEALTH_URL"

now=$(cg_dep "$INSTALLED_CORTEX")
[[ "$now" == "$cg_version" ]] || die "installed cortex has context-guru '${now:-none}', not $cg_version"
say "done: the running Cortex uses context-guru $now"
