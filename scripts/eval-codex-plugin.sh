#!/usr/bin/env bash
# End-to-end eval-box test for the Codex marketplace. Run only in a fresh shipped checkout.
set -euo pipefail

SRC=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
RUN_ROOT="$HOME/cg-codex-plugin-eval-47c2"
CODEX_REAL_HOME="${CODEX_HOME:-$HOME/.codex}"
CODEX_SEED_CONFIG="${CODEX_SEED_CONFIG:-}"
export PATH="/usr/local/go/bin:$SRC/bin:$PATH"
if [ -s "$HOME/.nvm/nvm.sh" ]; then
  # shellcheck disable=SC1090
  . "$HOME/.nvm/nvm.sh"
fi
command -v go >/dev/null

# This path belongs to this test only. Never use /tmp: the eval-box cleaner can remove a live run.
rm -rf "$RUN_ROOT"
mkdir -p "$RUN_ROOT/codex" "$RUN_ROOT/state" "$RUN_ROOT/work" "$RUN_ROOT/tmp"
cleanup() {
  kill "${SERVE_PID:-0}" 2>/dev/null || true
  rm -f "$RUN_ROOT/codex/config.toml" "$RUN_ROOT/codex/auth.json"
}
trap cleanup EXIT
export TMPDIR="$RUN_ROOT/tmp"
# Exclude /usr/local/bin: the shared box has a deployed context-guru-proxy there, and installer
# tests whose premise is "no binary installed" otherwise test the box instead of the fixture.
export PATH="${NVM_BIN:+$NVM_BIN:}/usr/local/go/bin:/usr/bin:/bin"
if ! command -v codex >/dev/null 2>&1; then
  npm install --prefix "$RUN_ROOT/codex-cli" @openai/codex@0.159.3
  export PATH="$RUN_ROOT/codex-cli/node_modules/.bin:$PATH"
fi
command -v codex >/dev/null
export CODEX_HOME="$RUN_ROOT/codex"
export XDG_STATE_HOME="$RUN_ROOT/state"

# Reuse the box's Codex login/config without reading or printing either file.
if [ -f "$CODEX_REAL_HOME/auth.json" ]; then
  cp -p "$CODEX_REAL_HOME/auth.json" "$CODEX_HOME/auth.json"
fi
if [ -n "$CODEX_SEED_CONFIG" ]; then
  cp -p "$CODEX_SEED_CONFIG" "$CODEX_HOME/config.toml"
elif [ -f "$CODEX_REAL_HOME/config.toml" ]; then
  cp -p "$CODEX_REAL_HOME/config.toml" "$CODEX_HOME/config.toml"
fi

echo "phase=go_test"
cd "$SRC"
CGO_ENABLED=1 go test ./...
CGO_ENABLED=0 go build -o "$SRC/bin/context-guru-proxy" ./cmd/context-guru-proxy
export PATH="$SRC/bin:$PATH"

echo "phase=plugin_install"
codex plugin marketplace add "$SRC/codex-marketplace" --json
codex plugin add context-guru@context-guru --json
codex plugin list --json > "$RUN_ROOT/plugin-list.json"

echo "phase=sandboxed_setup"
codex exec -C "$RUN_ROOT/work" -s workspace-write -c sandbox_workspace_write.network_access=true \
  --skip-git-repo-check --add-dir "$CODEX_HOME" --add-dir "$XDG_STATE_HOME" \
  --dangerously-bypass-hook-trust \
  "Use the context-guru setup skill. I explicitly consent to routing this test session's model traffic through the loopback context-guru proxy. Perform setup and report its facts." \
  > "$RUN_ROOT/setup.out" 2> "$RUN_ROOT/setup.err"
test -f "$CODEX_HOME/context-guru.config.toml"
test -x "$XDG_STATE_HOME/context-guru-codex/context-guru-reset"

# Codex CLI 0.159.3 does not dispatch installed SessionStart hooks from `codex exec`.
# Exercise the same foreground service entrypoint from the outer test harness; the routed Codex
# process below remains fully workspace-write sandboxed.
PLUGIN="$SRC/codex-marketplace/plugins/context-guru/scripts/codex_plugin.py"
python3 "$PLUGIN" serve &
SERVE_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  python3 "$PLUGIN" status >/dev/null 2>&1 && break
  sleep 1
done
python3 "$PLUGIN" status >/dev/null

echo "phase=sandboxed_routed_turn"
codex exec -p context-guru -C "$RUN_ROOT/work" -s workspace-write -c sandbox_workspace_write.network_access=true \
  --skip-git-repo-check --add-dir "$XDG_STATE_HOME" \
  --dangerously-bypass-hook-trust \
  "Create a file named codex-e2e-proof.txt containing exactly: context-guru codex e2e ok" \
  > "$RUN_ROOT/routed.out" 2> "$RUN_ROOT/routed.err"
grep -qx 'context-guru codex e2e ok' "$RUN_ROOT/work/codex-e2e-proof.txt"

python3 "$PLUGIN" status > "$RUN_ROOT/status.out"
grep -q '^proxy_up=true$' "$RUN_ROOT/status.out"
grep -q 'cg.request' "$XDG_STATE_HOME/context-guru-codex/proxy.log"

echo "phase=update_check"
python3 "$PLUGIN" update --check > "$RUN_ROOT/update.out"
grep -q '^update_available=' "$RUN_ROOT/update.out"

echo "phase=escape_hatch"
RESET="$XDG_STATE_HOME/context-guru-codex/context-guru-reset"
"$RESET" --dry-run > "$RUN_ROOT/reset-dry.out"
test -f "$CODEX_HOME/context-guru.config.toml"
"$RESET" --yes > "$RUN_ROOT/reset.out"
test ! -e "$CODEX_HOME/context-guru.config.toml"
test -d "$XDG_STATE_HOME/context-guru-codex/recovery"

echo "result=passed"
echo "artifacts=$RUN_ROOT"
