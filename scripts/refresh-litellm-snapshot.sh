#!/usr/bin/env bash
# Refresh internal/modelinfo/testdata/litellm_prices.json.gz from the public LiteLLM map.
# Run from any directory. Then run `go test ./internal/modelinfo` and review the diff of
# every test whose expected value changes. The URL is modelinfo.LiteLLMPricesURL.
set -euo pipefail
url="https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
dest="$(cd "$(dirname "$0")/.." && pwd)/internal/modelinfo/testdata/litellm_prices.json.gz"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp"
python3 -I -c 'import json,sys; assert len(json.load(open(sys.argv[1]))) > 1000' "$tmp"
gzip -9 -n -c "$tmp" > "$dest"
echo "wrote $dest ($(date -u +%Y-%m-%d))"
