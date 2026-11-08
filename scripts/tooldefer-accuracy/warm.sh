#!/bin/bash
# warm.sh <port>: one tiny no-tools request so the proxy builds its deferral state, then wait for history_live.
port=$1
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:$port/anthropic/v1/messages -H "content-type: application/json" -H "anthropic-version: 2023-06-01" -H "authorization: Bearer $TD871_API_KEY" \
 -d "{\"model\":\"claude-haiku-4-5-20251001\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"warm $(uuidgen)\"}]}"
for i in $(seq 1 50); do
  curl -s http://127.0.0.1:$port/stats | python3 -c "import json,sys; d=json.load(sys.stdin).get('tool_defer') or {}; sys.exit(0 if d.get('history_live') else 1)" && { echo live; exit 0; }
  sleep 0.1
done; echo notlive
