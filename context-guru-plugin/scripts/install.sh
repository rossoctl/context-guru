#!/usr/bin/env bash
# Put a context-guru-proxy binary on the user's PATH, without a toolchain.
#
# The binary is statically linked pure Go (scripts/gate-a-purego.sh proves it, and the release
# workflow asserts it on every run), so this is a download, a checksum, and a move. No
# compiler, no runtime dependencies, nothing to configure.
#
# Ranked by friction, and this script tries them in that order:
#
#   1. ALREADY INSTALLED — do nothing, report the version. This script is idempotent because
#      the install skill may be re-run, and re-downloading 30 MB to end up where we started is
#      not a neutral act on someone's laptop.
#   2. RELEASE TARBALL — the default path. Checksum-verified against the release's
#      checksums.txt, and on macOS the quarantine attribute is stripped, otherwise Gatekeeper
#      refuses an unsigned download with "cannot be verified" and the trial ends there.
#   3. go install — only if a Go toolchain is already present. Cheap to offer, and some people
#      would rather build.
#
# There is deliberately NO `brew` path yet: the tap repo does not exist and release signing is
# an open ownership question (see the spec). A `brew install` line that fails is worse than one
# that is absent, and adding the tap later changes nothing else here.
#
# Every fact this discovers is printed as `key=value` on stdout, so the calling skill can act
# on the outcome without parsing prose.
set -uo pipefail

REPO="${CONTEXT_GURU_REPO:-rossoctl/context-guru}"
VERSION="${CONTEXT_GURU_VERSION:-latest}"
DEST="${CONTEXT_GURU_DEST:-$HOME/.local/bin}"
BIN=context-guru-proxy

emit() { printf '%s\n' "$*"; }
die()  { emit "result=error"; emit "reason=$1"; exit 1; }

# --- 1. already there, and is it the version we want? -------------------------------------
#
# This used to return `result=present` for ANY binary on PATH regardless of version, and read
# CONTEXT_GURU_VERSION only afterwards — so on the very change that creates a release channel
# there was no way to upgrade. It also reported the version by taking `head -1` of `--help`,
# which recorded "Usage of context-guru-proxy:" as the installed version.
installed_version() { # prints e.g. v0.1.2, or "" if the binary cannot say
  "$1" --version 2>/dev/null | awk '{print $2; exit}'
}

if command -v "$BIN" >/dev/null 2>&1; then
  have_path=$(command -v "$BIN")
  have=$(installed_version "$have_path")
  emit "path=${have_path}"
  emit "version=${have:-unknown}"
  # An explicit CONTEXT_GURU_VERSION means "I want that one" — honour it even when something is
  # already installed. `latest` resolves below and is compared there.
  if [ "$VERSION" != latest ] && [ "$VERSION" = "$have" ]; then
    emit "result=present"
    exit 0
  fi
  if [ "$VERSION" = latest ] && [ -n "$have" ] && [ "${CONTEXT_GURU_UPGRADE:-}" != 1 ]; then
    # Do not silently re-download on every install run; say what is there and how to move.
    emit "result=present"
    emit "note=set CONTEXT_GURU_UPGRADE=1 to check for and install a newer release"
    exit 0
  fi
  if [ -z "$have" ]; then
    emit "note=the installed binary does not support --version; it predates the release channel"
  fi
  emit "note=upgrading from ${have:-unknown}"
fi

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *)      die "unsupported_os_$(uname -s): build from source, see docs/get-started/quickstart-proxy.md" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *)             die "unsupported_arch_$(uname -m)" ;;
esac
emit "platform=${OS}/${ARCH}"

command -v curl >/dev/null 2>&1 || die "no_curl"

# --- 2. release tarball -------------------------------------------------------------------
if [ "$VERSION" = latest ]; then
  # Resolve the tag rather than relying on /latest/download redirects, because the checksum
  # file has to come from the SAME release as the tarball. Two separate redirect follows could
  # straddle a release published between them.
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null |
            sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || die "no_release_found: no published release yet for ${REPO}; build from source or set CONTEXT_GURU_VERSION"
fi
NUM="${VERSION#v}"
TARBALL="context-guru_${NUM}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
emit "version=${VERSION}"

# try_source_build is distribution option 3 from the header comment, which the first version of
# this script documented and never implemented — on a machine that had Go 1.26.4 on PATH.
#
# It is a FALLBACK, not a path anyone is steered to: it needs a toolchain, which is the gate this
# whole change exists to remove. But when there is no downloadable asset and a toolchain is right
# there, refusing to use it is worse than using it.
try_source_build() {
  command -v go >/dev/null 2>&1 || return 1
  emit "fallback=go_install"
  # CGO off: the binary is pure Go, and requiring a C toolchain here would reintroduce the gate.
  if CGO_ENABLED=0 GOBIN="$DEST" go install "github.com/${REPO}/cmd/context-guru-proxy@${VERSION}" 2>"$TMP/go.err"; then
    emit "result=installed"
    emit "path=${DEST}/${BIN}"
    emit "built_from=source"
    return 0
  fi
  emit "go_install_failed=$(tail -1 "$TMP/go.err" 2>/dev/null | tr -d '\n')"
  return 1
}

TMP=$(mktemp -d) || die "no_tmpdir"
trap 'rm -rf "$TMP"' EXIT

# The raw curl error used to reach stdout and break this script's "every fact is a key=value
# line" contract, which the calling skill parses. Keep curl quiet and report the failure as data.
if ! curl -fsSL -o "$TMP/$TARBALL" "$BASE/$TARBALL" 2>"$TMP/curl.err"; then
  emit "download_url=$BASE/$TARBALL"
  # A published tag with no assets reaches exactly here — the release exists, the artifact does
  # not — which is what a pre-release repository looks like before the first build is attached.
  if try_source_build; then
    exit 0
  fi
  die "download_failed: $BASE/$TARBALL (no asset for this platform, and no Go toolchain to build from source)"
fi

# Checksum. The download is unsigned, there is no signature anywhere yet, and this script strips
# macOS quarantine from the file below — so this is the ONLY integrity check in the path.
#
# It is therefore fail-CLOSED, in every branch. The first version of this was fail-open: a missing
# or unfetchable checksums.txt printed one advisory line and installed anyway, which meant an
# unverified binary landed on a PATH directory and ran — a binary that then handles all of the
# user's LLM traffic and holds their API key. The comment above it said "a failure here is fatal,
# never a warning" while the code did the opposite.
#
# CONTEXT_GURU_INSECURE=1 exists for the one legitimate case (a local build served from a file
# path with no checksums file) and says what it is in its name.
verify_checksum() {
  curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" 2>/dev/null ||
    die "checksum_unavailable: could not fetch $BASE/checksums.txt, so the download cannot be verified. Set CONTEXT_GURU_INSECURE=1 to install anyway (not recommended)."
  want=$(awk -v f="$TARBALL" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt" | head -1)
  [ -n "$want" ] ||
    die "checksum_absent: $TARBALL is not listed in checksums.txt, so the download cannot be verified. Set CONTEXT_GURU_INSECURE=1 to install anyway (not recommended)."
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$TMP/$TARBALL" | awk '{print $1}')
  else
    got=$(shasum -a 256 "$TMP/$TARBALL" | awk '{print $1}')
  fi
  [ "$want" = "$got" ] || die "checksum_mismatch: expected $want got $got"
  emit "checksum=verified"
}

if [ "${CONTEXT_GURU_INSECURE:-}" = 1 ]; then
  emit "checksum=SKIPPED_BY_CONTEXT_GURU_INSECURE"
else
  verify_checksum
fi

tar xzf "$TMP/$TARBALL" -C "$TMP" || die "untar_failed"
[ -f "$TMP/$BIN" ] || die "binary_not_in_tarball"

mkdir -p "$DEST" || die "cannot_create_$DEST"
install -m 755 "$TMP/$BIN" "$DEST/$BIN" || die "install_failed_to_$DEST"

# macOS: without this, the first run dies with "cannot be verified" and the evaluator concludes
# the project is broken. Notarization would remove the need and requires a paid Apple account.
if [ "$OS" = darwin ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$DEST/$BIN" 2>/dev/null || true
  emit "quarantine=cleared"
fi

emit "result=installed"
emit "path=${DEST}/${BIN}"

# Report — do not fix — a PATH that will not find it. Editing the user's shell rc is a bigger
# intrusion than this script is entitled to, and the skill can tell them in context.
case ":${PATH}:" in
  *":${DEST}:"*) emit "on_path=true" ;;
  *)             emit "on_path=false"
                 emit "note=add ${DEST} to your PATH, or the session hook will not find the proxy" ;;
esac
