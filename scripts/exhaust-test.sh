#!/usr/bin/env bash
set -euo pipefail

URL="http://localhost:8080/v1/chat/completions"
RESULTS=$(mktemp)
trap "rm -f $RESULTS" EXIT

fire_wave() {
  local wave=$1 count=$2 tokens=$3
  for i in $(seq 1 "$count"); do
    curl -s -o /dev/null -w "%{http_code}\n" -X POST "$URL" \
      -H "Host: api.openai.com" \
      -H "x-genops-workload-id: example-agent" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"gpt-4\",\"max_tokens\":$tokens}" >> "$RESULTS" &
  done
  wait
  echo "=== Wave $wave: $count requests, max_tokens=$tokens ==="
  sort "$RESULTS" | uniq -c | sort -rn
  echo "---"
  : > "$RESULTS"
}

fire_wave 1 50 1000
fire_wave 2 50 1000
fire_wave 3 50 1000

echo "=== Post-burst health check ==="
curl -s localhost:8080/healthz
echo ""
