#!/bin/sh
# Build the Pi package release file and its SHA-256 list, for install-pi.sh:
#
#   package.sh <tag> <out-dir>
#
# Writes <out-dir>/context-guru-pi-<tag>.tar.gz (one top folder, pi-package/, with no tests and no
# release files) and <out-dir>/context-guru-pi-SHA256SUMS ("<sha256>  <file name>"). The release
# workflow runs it before GoReleaser publishes, so a failed build stops the tag.
set -eu
[ $# -eq 2 ] || { echo "usage: package.sh <tag> <out-dir>" >&2; exit 2; }
tag=$1
out=$2
here=$(cd "$(dirname "$0")" && pwd)
package=$(dirname "$here")
name="context-guru-pi-$tag.tar.gz"

stage=$(mktemp -d "${TMPDIR:-/tmp}/cg-pi-package.XXXXXX")
trap 'rm -rf "$stage"' EXIT
cp -R "$package" "$stage/pi-package"
# Only what a user runs: no tests, no release files, no caches, no local Node modules.
rm -rf "$stage/pi-package/release" "$stage/pi-package/tests" "$stage/pi-package/node_modules"
find "$stage/pi-package" \( -name '__pycache__' -o -name 'test_*.py' -o -name '*_test.go' -o -name '.DS_Store' \) \
  -prune -exec rm -rf {} +

mkdir -p "$out"
out=$(cd "$out" && pwd)  # tar runs in the stage folder, so the out path must be absolute
(cd "$stage" && COPYFILE_DISABLE=1 tar -czf "$out/$name" pi-package)
if command -v sha256sum >/dev/null 2>&1; then
  digest=$(sha256sum "$out/$name" | cut -d' ' -f1)
else
  digest=$(shasum -a 256 "$out/$name" | cut -d' ' -f1)
fi
printf '%s  %s\n' "$digest" "$name" > "$out/context-guru-pi-SHA256SUMS"
echo "$out/$name"
