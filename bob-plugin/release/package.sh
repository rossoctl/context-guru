#!/bin/sh
# Assemble the Bob release zip from files that are already built. `make bob-release` calls it.
#
# usage: package.sh VERSION OUT_DIR ARM64_BINARY AMD64_BINARY PLUGIN_DIR
# Prints the zip path. The zip holds one folder, context-guru-bob-VERSION/, with both .command
# files, README.md, bin/darwin-{arm64,amd64}/context-guru-proxy, plugin/ and SHA256SUMS.
set -eu

[ "$#" = 5 ] || { echo "usage: package.sh VERSION OUT_DIR ARM64_BINARY AMD64_BINARY PLUGIN_DIR" >&2; exit 2; }
VERSION=$1 OUT=$2 ARM64=$3 AMD64=$4 PLUGIN=$5
HERE=$(cd "$(dirname "$0")" && pwd)
NAME="context-guru-bob-$VERSION"

for need in "$ARM64" "$AMD64" "$PLUGIN/scripts/bob_plugin.py" "$PLUGIN/statusbar/context-guru-bob.vsix"; do
  [ -f "$need" ] || { echo "package.sh: missing $need (run make bob-statusbar for the vsix)" >&2; exit 1; }
done

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/cg-bob-release.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
ROOT="$WORK/$NAME"
mkdir -p "$ROOT/bin/darwin-arm64" "$ROOT/bin/darwin-amd64"

cp "$HERE/Install context-guru for Bob.command" "$HERE/Uninstall context-guru for Bob.command" \
   "$HERE/README.md" "$ROOT/"
cp "$ARM64" "$ROOT/bin/darwin-arm64/context-guru-proxy"
cp "$AMD64" "$ROOT/bin/darwin-amd64/context-guru-proxy"
# The plugin without tests, caches or git files: they are not needed on the user's Mac.
cp -R "$PLUGIN" "$ROOT/plugin"
find "$ROOT/plugin" \( -name '__pycache__' -o -name 'test_*.py' -o -name '.gitignore' \
  -o -name '.DS_Store' \) -prune -exec rm -rf {} +
chmod 0755 "$ROOT"/*.command "$ROOT"/bin/*/context-guru-proxy

(cd "$ROOT" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort |
  while IFS= read -r file; do shasum -a 256 "$file"; done > SHA256SUMS)

rm -f "$OUT/$NAME.zip"
# -X: no extra macOS attributes. zip keeps the Unix mode, so the .command files stay executable.
(cd "$WORK" && zip -q -r -X "$OUT/$NAME.zip" "$NAME")
echo "$OUT/$NAME.zip"
