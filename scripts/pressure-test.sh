#!/usr/bin/env bash
set -euo pipefail

URL="http://localhost:8080/v1/chat/completions"
RESULTS=$(mktemp)
trap "rm -f $RESULTS" EXIT

for i in $(seq 1 80); do
  curl -s -o /dev/null -w "%{http_code}\n" -X POST "$URL" \
    -H "Host: api.openai.com" \
    -H "x-genops-workload-id: example-agent" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","max_tokens":3000}' >> "$RESULTS" &
done

wait

echo "=== Pressure Test: 80 parallel requests, max_tokens=3000 ==="
sort "$RESULTS" | uniq -c | sort -rn
echo "Total: $(wc -l < "$RESULTS" | tr -d ' ')"

echo ""
echo "=== Post-burst health check ==="
curl -s localhost:8080/healthz
echo ""
