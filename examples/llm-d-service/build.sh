#!/usr/bin/env bash
# Build the context-guru compaction service (bin/context-guru-proxy).
#
# Requires Go 1.26 and a C toolchain: CGO_ENABLED=1 is set below because the
# `skeleton` component links tree-sitter via cgo. bifrost is an ordinary published
# dependency, so this repo builds standalone — no sibling checkout needed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

CGO_ENABLED=1 go build -o bin/context-guru-proxy ./cmd/context-guru-proxy
echo "built: $repo_root/bin/context-guru-proxy"
