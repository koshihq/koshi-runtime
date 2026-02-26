#!/usr/bin/env bash
set -euo pipefail

# GenOps Spec Version Validation
#
# Verifies that genops.spec.version is emitted on every event and exposed
# in /status. Runs against a live Koshi instance.
#
# Requires:
#   - A running Koshi instance at KOSHI_URL (default localhost:8080)
#   - curl and jq installed
#   - Log access via Docker container or log file
#
# Usage:
#   ./scripts/validate-spec-version.sh                          # Docker default
#   LOG_SOURCE=/tmp/koshi.log ./scripts/validate-spec-version.sh  # file mode

KOSHI_URL="${KOSHI_URL:-http://localhost:8080}"
WORKLOAD_ID="${WORKLOAD_ID:-customer-support-agent}"
LOG_SOURCE="${LOG_SOURCE:-docker:koshi}"
EXPECTED_VERSION="${EXPECTED_VERSION:-0.1.0}"
IDENTITY_HEADER="${IDENTITY_HEADER:-x-genops-workload-id}"

LOG_FILE=$(mktemp)
trap 'rm -f "$LOG_FILE"' EXIT

# --- Preflight ---

if ! command -v jq &>/dev/null; then
  echo "ERROR: jq is required but not installed." >&2
  exit 1
fi

if ! command -v curl &>/dev/null; then
  echo "ERROR: curl is required but not installed." >&2
  exit 1
fi

if ! curl -sf -o /dev/null "${KOSHI_URL}/healthz"; then
  echo "ERROR: Koshi not reachable at ${KOSHI_URL}/healthz" >&2
  exit 1
fi

if [[ "$LOG_SOURCE" == docker:* ]]; then
  CONTAINER="${LOG_SOURCE#docker:}"
  if ! command -v docker &>/dev/null; then
    echo "ERROR: docker is required for LOG_SOURCE=docker:*" >&2
    exit 1
  fi
  if ! docker inspect "$CONTAINER" &>/dev/null; then
    echo "ERROR: Docker container '$CONTAINER' not found." >&2
    exit 1
  fi
else
  if [ ! -f "$LOG_SOURCE" ]; then
    echo "ERROR: Log file '$LOG_SOURCE' not found." >&2
    exit 1
  fi
fi

echo "========================================"
echo "GENOPS SPEC VERSION VALIDATION"
echo "========================================"
echo "Expected version: ${EXPECTED_VERSION}"
echo "Log source:       ${LOG_SOURCE}"
echo "Target:           ${KOSHI_URL}"
echo "Workload:         ${WORKLOAD_ID}"
echo "========================================"
echo ""

FAILED=0

# --- Check 1: /status endpoint ---

echo "--- /status endpoint ---"
STATUS_JSON=$(curl -sf "${KOSHI_URL}/status")
SPEC_VERSION=$(echo "$STATUS_JSON" | jq -r '.genops_spec_version // empty')

if [ "$SPEC_VERSION" = "$EXPECTED_VERSION" ]; then
  echo "  genops_spec_version: ${SPEC_VERSION} ... PASS"
else
  echo "  FAIL: genops_spec_version = \"${SPEC_VERSION}\", expected \"${EXPECTED_VERSION}\""
  FAILED=1
fi
echo ""

# --- Check 2: Trigger events ---

# Record timestamp before sending requests (for docker --since).
TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

echo "--- Triggered requests ---"

# Request A: allowed path (request_allowed + possibly budget_reconciled).
CODE_A=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${KOSHI_URL}/v1/chat/completions" \
  -H "Host: api.openai.com" \
  -H "${IDENTITY_HEADER}: ${WORKLOAD_ID}" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","max_tokens":50}')
echo "  Request A (allow):    ${CODE_A}"

# Request B: guard rejection (max_tokens exceeds 4096 guard).
CODE_B=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${KOSHI_URL}/v1/chat/completions" \
  -H "Host: api.openai.com" \
  -H "${IDENTITY_HEADER}: ${WORKLOAD_ID}" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","max_tokens":50000}')
echo "  Request B (guard):    ${CODE_B}"

# Request C: identity rejection (no identity header).
CODE_C=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${KOSHI_URL}/v1/chat/completions" \
  -H "Host: api.openai.com" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","max_tokens":50}')
echo "  Request C (identity): ${CODE_C}"
echo ""

# Wait for async emit buffer to drain.
sleep 2

# --- Check 3: Capture and parse logs ---

echo "--- Event inspection ---"

if [[ "$LOG_SOURCE" == docker:* ]]; then
  docker logs "$CONTAINER" --since="$TIMESTAMP" 2>&1 | grep '"msg":"koshi event"' > "$LOG_FILE" || true
else
  grep '"msg":"koshi event"' "$LOG_SOURCE" > "$LOG_FILE" || true
fi

TOTAL_EVENTS=$(wc -l < "$LOG_FILE" | tr -d ' ')

if [ "$TOTAL_EVENTS" -eq 0 ]; then
  echo "  FAIL: No koshi event log lines found."
  FAILED=1
  echo ""
  echo "========================================"
  echo "RESULT: FAIL"
  exit 1
fi

# Count events with and without genops.spec.version.
EVENTS_WITH=0
EVENTS_MISSING=0
MISSING_TYPES=""

while IFS= read -r line; do
  HAS_FIELD=$(echo "$line" | jq -r 'if ."genops.spec.version" then "yes" else "no" end' 2>/dev/null || echo "no")
  if [ "$HAS_FIELD" = "yes" ]; then
    EVENTS_WITH=$((EVENTS_WITH + 1))
  else
    EVENTS_MISSING=$((EVENTS_MISSING + 1))
    ET=$(echo "$line" | jq -r '.event_type // "unknown"' 2>/dev/null || echo "unknown")
    MISSING_TYPES="${MISSING_TYPES}  ${ET}\n"
  fi
done < "$LOG_FILE"

echo "  Total events:          ${TOTAL_EVENTS}"
echo "  With spec version:     ${EVENTS_WITH}"
echo "  Missing spec version:  ${EVENTS_MISSING}"

if [ "$EVENTS_MISSING" -ne 0 ]; then
  echo "  FAIL: Events missing genops.spec.version:"
  printf "%b" "$MISSING_TYPES"
  FAILED=1
fi
echo ""

# --- Check 4: Per-event-type verification ---

echo "--- Event types verified ---"
VERIFIED=0

for event_type in request_allowed guard_rejected identity_rejected budget_reconciled enforcement; do
  MATCH=$(jq -r "select(.event_type == \"$event_type\") | .\"genops.spec.version\"" "$LOG_FILE" 2>/dev/null | head -1)
  if [ "$MATCH" = "$EXPECTED_VERSION" ]; then
    echo "  PASS: ${event_type} = ${MATCH}"
    VERIFIED=$((VERIFIED + 1))
  fi
done

echo "  Verified: ${VERIFIED}/3 required"

if [ "$VERIFIED" -lt 3 ]; then
  echo "  FAIL: Only ${VERIFIED} event types verified (need >= 3)"
  FAILED=1
fi
echo ""

# --- Result ---

echo "========================================"
if [ "$FAILED" -eq 0 ]; then
  echo "RESULT: PASS"
  exit 0
else
  echo "RESULT: FAIL"
  exit 1
fi
