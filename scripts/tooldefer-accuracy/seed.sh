#!/bin/bash
# seed.sh: build snap/dash.db, the seeded history for the s3 and s3b arms. Four tiny real runs, about $0.50.
# Needs the tap on 18970 (tap.py, upstream = plain gateway). The runs use Bash only, so every deferred tool is "unused".
D=${TD871_ROOT:-$(cd "$(dirname "$0")" && pwd)}; cd "$D" || exit 1
rm -f snap/dash.db snap/dash.db-wal snap/dash.db-shm snap/context-guru-tooldefer.db
./startproxy.sh seed 18972 ultrahigh "$D/snap/dash.db"
for m in claude-sonnet-5-5 claude-opus-5-5 claude-sonnet-5-5 claude-opus-5-5; do
  ./runcc.sh seed-$m-$RANDOM.jsonl seed seed 18972 $m "$D/mcp.json" CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1 -- "Run ls with the Bash tool, then reply with the single word done. nonce $(uuidgen)"
  sleep 3
done
curl -s http://127.0.0.1:18972/stats | python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin).get('tool_defer'),indent=0))"
./stopproxy.sh seed
sqlite3 snap/dash.db "select count(distinct session_id) from tool_defer_history" # expect 4
