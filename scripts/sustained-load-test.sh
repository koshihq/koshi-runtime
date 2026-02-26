#!/usr/bin/env bash
set -euo pipefail

# Sustained Load Validation for GA Definition of Done
#
# Requires:
#   - A running Koshi instance at KOSHI_URL (default localhost:8080)
#   - A mock upstream returning valid OpenAI-format responses
#   - curl and jq installed
#
# Usage:
#   DURATION_SECONDS=60 ./scripts/sustained-load-test.sh   # quick smoke test
#   ./scripts/sustained-load-test.sh                        # full 30-min validation

DURATION_SECONDS="${DURATION_SECONDS:-1800}"
CONCURRENCY="${CONCURRENCY:-10}"
INTERVAL_SECONDS="${INTERVAL_SECONDS:-30}"
KOSHI_URL="${KOSHI_URL:-http://localhost:8080}"
WORKLOAD_ID="${WORKLOAD_ID:-example-agent}"
MAX_TOKENS="${MAX_TOKENS:-3000}"

RESULTS=$(mktemp)
HEALTH_LOG=$(mktemp)
BUDGET_LOG=$(mktemp)
trap 'rm -f "$RESULTS" "$HEALTH_LOG" "$BUDGET_LOG"; kill "$CHECKER_PID" 2>/dev/null || true' EXIT

# Preflight check.
if ! command -v jq &>/dev/null; then
  echo "ERROR: jq is required but not installed." >&2
  exit 1
fi

echo "========================================"
echo "SUSTAINED LOAD VALIDATION"
echo "========================================"
echo "Duration:    ${DURATION_SECONDS}s"
echo "Concurrency: ${CONCURRENCY}"
echo "Check interval: ${INTERVAL_SECONDS}s"
echo "Target:      ${KOSHI_URL}"
echo "Workload:    ${WORKLOAD_ID}"
echo "max_tokens:  ${MAX_TOKENS}"
echo "========================================"
echo ""

# Verify Koshi is reachable.
if ! curl -sf -o /dev/null "${KOSHI_URL}/healthz"; then
  echo "ERROR: Koshi not reachable at ${KOSHI_URL}/healthz" >&2
  exit 1
fi
echo "Koshi reachable. Starting load test..."
echo ""

# --- Invariant checker (background) ---
invariant_checker() {
  local end_time=$(($(date +%s) + DURATION_SECONDS))
  while [ "$(date +%s)" -lt "$end_time" ]; do
    sleep "$INTERVAL_SECONDS"

    # Health check.
    local health_code
    health_code=$(curl -s -o /dev/null -w "%{http_code}" "${KOSHI_URL}/healthz" || echo "000")
    echo "$health_code" >> "$HEALTH_LOG"

    # Budget floor check.
    local status_json
    status_json=$(curl -sf "${KOSHI_URL}/status" 2>/dev/null || echo "{}")
    local negative
    negative=$(echo "$status_json" | jq '[.workloads[]?.window_tokens_used // 0 | select(. < 0)] | length' 2>/dev/null || echo "0")
    echo "$negative" >> "$BUDGET_LOG"

    local dropped
    dropped=$(echo "$status_json" | jq '.dropped_events // 0' 2>/dev/null || echo "0")

    local ts
    ts=$(date +%H:%M:%S)
    if [ "$health_code" != "200" ]; then
      echo "[$ts] WARN: healthz returned $health_code"
    fi
    if [ "$negative" != "0" ]; then
      echo "[$ts] WARN: negative budget detected"
    fi
    if [ "$dropped" -gt 100 ] 2>/dev/null; then
      echo "[$ts] WARN: dropped_events=$dropped"
    fi
  done
}

invariant_checker &
CHECKER_PID=$!

# --- Traffic generator (foreground) ---
END_TIME=$(($(date +%s) + DURATION_SECONDS))
BATCH=0

while [ "$(date +%s)" -lt "$END_TIME" ]; do
  BATCH=$((BATCH + 1))
  PIDS=""
  for _ in $(seq 1 "$CONCURRENCY"); do
    (curl -s -o /dev/null -w "%{http_code}\n" -X POST "${KOSHI_URL}/v1/chat/completions" \
      -H "Host: api.openai.com" \
      -H "x-genops-workload-id: ${WORKLOAD_ID}" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"gpt-4\",\"max_tokens\":${MAX_TOKENS}}" >> "$RESULTS") &
    PIDS="$PIDS $!"
  done
  for pid in $PIDS; do wait "$pid" || true; done

  # Progress every 10 batches.
  if [ $((BATCH % 10)) -eq 0 ]; then
    echo "[$(date +%H:%M:%S)] Batch $BATCH completed ($(wc -l < "$RESULTS" | tr -d ' ') requests so far)"
  fi
done

# Wait for invariant checker to finish.
wait "$CHECKER_PID" 2>/dev/null || true

echo ""
echo "========================================"
echo "SUSTAINED LOAD VALIDATION REPORT"
echo "========================================"

# Request summary.
TOTAL=$(wc -l < "$RESULTS" | tr -d ' ')
COUNT_200=$(grep -c "^200$" "$RESULTS" 2>/dev/null || true)
COUNT_200=${COUNT_200:-0}
COUNT_429=$(grep -c "^429$" "$RESULTS" 2>/dev/null || true)
COUNT_429=${COUNT_429:-0}
COUNT_503=$(grep -c "^503$" "$RESULTS" 2>/dev/null || true)
COUNT_503=${COUNT_503:-0}
COUNT_502=$(grep -c "^502$" "$RESULTS" 2>/dev/null || true)
COUNT_502=${COUNT_502:-0}
COUNT_504=$(grep -c "^504$" "$RESULTS" 2>/dev/null || true)
COUNT_504=${COUNT_504:-0}
COUNT_OTHER=$((TOTAL - COUNT_200 - COUNT_429 - COUNT_503 - COUNT_502 - COUNT_504))

echo "Duration:         ${DURATION_SECONDS}s"
echo "Total requests:   ${TOTAL}"
echo "  200 (allowed):  ${COUNT_200}"
echo "  429 (throttled): ${COUNT_429}"
echo "  502 (no upstream): ${COUNT_502}"
echo "  503 (killed):   ${COUNT_503}"
echo "  504 (upstream err): ${COUNT_504}"
echo "  Other:          ${COUNT_OTHER}"

# Health check results.
HEALTH_TOTAL=$(wc -l < "$HEALTH_LOG" | tr -d ' ')
HEALTH_PASS=$(grep -c "^200$" "$HEALTH_LOG" 2>/dev/null || true)
HEALTH_PASS=${HEALTH_PASS:-0}
echo "Health checks:    ${HEALTH_PASS}/${HEALTH_TOTAL} passed"

# Budget floor results.
BUDGET_TOTAL=$(wc -l < "$BUDGET_LOG" | tr -d ' ')
BUDGET_VIOLATIONS=$(grep -cv "^0$" "$BUDGET_LOG" 2>/dev/null || true)
BUDGET_VIOLATIONS=${BUDGET_VIOLATIONS:-0}
BUDGET_PASS=$((BUDGET_TOTAL - BUDGET_VIOLATIONS))
echo "Budget floor:     ${BUDGET_PASS}/${BUDGET_TOTAL} passed"

# Final dropped events.
FINAL_DROPPED=$(curl -sf "${KOSHI_URL}/status" 2>/dev/null | jq '.dropped_events // 0' 2>/dev/null || echo "unknown")
echo "Dropped events:   ${FINAL_DROPPED}"

echo "========================================"

# --- Assertions ---
FAILED=0

if [ "$HEALTH_PASS" -ne "$HEALTH_TOTAL" ]; then
  echo "FAIL: Degraded transition detected — healthz returned non-200"
  FAILED=1
fi

if [ "$BUDGET_VIOLATIONS" -ne 0 ]; then
  echo "FAIL: Negative budget detected"
  FAILED=1
fi

if [ "$COUNT_OTHER" -ne 0 ]; then
  echo "FAIL: Unexpected HTTP status codes (not 200/429/502/503/504)"
  sort "$RESULTS" | grep -vE "^(200|429|502|503|504)$" | sort | uniq -c || true
  FAILED=1
fi

if [ "$COUNT_200" -eq 0 ]; then
  echo "FAIL: No requests were allowed — budget pressure not meaningful"
  FAILED=1
fi

if [ "$COUNT_429" -eq 0 ]; then
  echo "FAIL: No requests were throttled — budget pressure not achieved"
  FAILED=1
fi

if [ "$FAILED" -eq 0 ]; then
  echo "RESULT: PASS"
  exit 0
else
  echo "RESULT: FAIL"
  exit 1
fi
