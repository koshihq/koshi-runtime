#!/usr/bin/env bash
set -euo pipefail

URL="http://localhost:8080/v1/chat/completions"
RESULTS=$(mktemp)
trap "rm -f $RESULTS" EXIT

for i in $(seq 1 50); do
  curl -s -o /dev/null -w "%{http_code}\n" -X POST "$URL" \
    -H "Host: api.openai.com" \
    -H "x-genops-workload-id: example-agent" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","max_tokens":50}' >> "$RESULTS" &
done

wait

echo "=== Burst Test: 50 parallel requests, max_tokens=50 ==="
sort "$RESULTS" | uniq -c | sort -rn
echo "Total: $(wc -l < "$RESULTS" | tr -d ' ')"
