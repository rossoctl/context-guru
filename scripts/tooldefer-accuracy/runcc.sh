#!/bin/bash
# usage: runcc.sh <out> <cfgname> <workname> <port> <model> <mcpjson> <extra-env|-> -- <claude args...>
D=${TD871_ROOT:-$(cd "$(dirname "$0")" && pwd)}; out=$D/runs/$1; cfg=$D/cfg/$2; wd=$D/work/$3; port=$4; model=$5; mcp=$6; extra=$7; shift 7; [ "$1" == "--" ] && shift
mkdir -p "$cfg" "$wd"; [ -f "$cfg/settings.json" ] || cp $D/settings.json "$cfg/settings.json"
cd "$wd"
[ "$extra" == "-" ] && extra=CG_NOOP=1
env -i HOME="$HOME" PATH="$PATH" TERM=xterm $extra ANTHROPIC_BASE_URL=http://127.0.0.1:$port/anthropic ANTHROPIC_AUTH_TOKEN="$TD871_API_KEY" ANTHROPIC_CUSTOM_HEADERS= ANTHROPIC_MODEL=$model ANTHROPIC_SMALL_FAST_MODEL=claude-haiku-4-5-20251001 ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-4-5-20251001 CLAUDE_CONFIG_DIR="$cfg" timeout 300 claude -p "$@" --output-format stream-json --verbose --permission-mode bypassPermissions --mcp-config $mcp > "$out" 2>&1 < /dev/null
echo "rc=$? sid=$(head -1 "$out" | python3 -c 'import json,sys
try: print(json.loads(sys.stdin.readline())["session_id"])
except Exception: print("?")')"
