#!/bin/bash
# build.sh: build the proxy from a checkout and write it to bin/proxy. Needs Go on this machine.
# usage: build.sh <repo-dir>
R=${1:?repo dir}; D=${TD871_ROOT:-$(cd "$(dirname "$0")" && pwd)}
mkdir -p "$D/bin" && (cd "$R" && CGO_ENABLED=0 go build -o "$D/bin/proxy" ./cmd/context-guru-proxy) && "$D/bin/proxy" --version
