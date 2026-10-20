#!/usr/bin/env bash
# Host entry for the PR #578 review. Builds the sandbox image from this checkout and runs a tier.
set -uo pipefail
TIER="${1:?usage: e2e/run.sh smoke|full [--keep]}"; KEEP="${2:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="$ROOT/e2e/results/$STAMP-$TIER"; mkdir -p "$OUT"
# Podman takes --ignorefile; real Docker reads Dockerfile.e2e.dockerignore automatically.
IGN=""; docker --version 2>/dev/null | grep -qi podman && IGN="--ignorefile $ROOT/e2e/build.ignore"
docker build $IGN -f "$ROOT/e2e/Dockerfile.e2e" -t cg-e2e:pr578 "$ROOT" >"$OUT/build.log" 2>&1 ||
  { echo "image build failed; see $OUT/build.log"; tail -20 "$OUT/build.log"; exit 2; }
NAME="cg-e2e-$STAMP"
RM="--rm"; [ "$KEEP" = --keep ] && RM=""
# Podman maps the bind mount to root inside the container; keep-id makes it writable by `sandbox`.
EXTRA=""; docker --version 2>/dev/null | grep -qi podman && EXTRA="--userns=keep-id"
docker run $RM $EXTRA --name "$NAME" -p 127.0.0.1:18788:8788 -v "$OUT:/results" cg-e2e:pr578 "$TIER"
rc=$?
echo "results: $OUT"
[ "$KEEP" = --keep ] && echo "container kept: $NAME (dashboard on http://127.0.0.1:18788/dashboard/)"
exit $rc
