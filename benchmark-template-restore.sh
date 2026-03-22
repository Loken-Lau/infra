#!/usr/bin/env bash
set -euo pipefail

export E2B_API_KEY=e2b_53ae1fed82754c17ad8077fbc8bcdd90
export E2B_ACCESS_TOKEN=sk_e2b_89215020937a4c989cde33d7bc647715
export E2B_API_URL=http://localhost:3000
export E2B_SANDBOX_URL=http://localhost:3002

# Benchmark template restore/start performance via API sandbox creation.
# Measures end-to-end latency from POST /sandboxes to sandbox state=running.

ITERATIONS=20
TEMPLATE_ID="base"
API_URL="${E2B_API_URL:-http://localhost:3000}"
API_KEY="${E2B_API_KEY:-}"
TIMEOUT_SECONDS=120
POLL_INTERVAL_SECONDS=0.20
SANDBOX_TIMEOUT_SECONDS=300
OUTPUT_CSV=""

usage() {
  cat <<'EOF'
Usage:
  ./benchmark-template-restore.sh [options]

Options:
  -n, --iterations <N>         Number of benchmark rounds (default: 20)
  -t, --template <ID|alias>    Template ID/alias to restore from (default: base)
  -u, --api-url <URL>          E2B API URL (default: $E2B_API_URL or http://localhost:3000)
  -k, --api-key <KEY>          E2B API key (default: $E2B_API_KEY)
  -s, --sandbox-timeout <sec>  Sandbox timeout sent to API (default: 300)
  --timeout <sec>              Max wait for sandbox to reach running (default: 120)
  --poll <sec>                 Poll interval for sandbox state (default: 0.20)
  -o, --output <path>          Output CSV file path (default: ./tmp/benchmark-template-restore-<ts>.csv)
  -h, --help                   Show help

Examples:
  E2B_API_URL=https://api.example.com E2B_API_KEY=e2b_xxx ./benchmark-template-restore.sh -n 50 -t base
  ./benchmark-template-restore.sh -k e2b_xxx -u http://localhost:3000 -n 30 -t team-slug/base
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--iterations)
      ITERATIONS="$2"; shift 2;;
    -t|--template)
      TEMPLATE_ID="$2"; shift 2;;
    -u|--api-url)
      API_URL="$2"; shift 2;;
    -k|--api-key)
      API_KEY="$2"; shift 2;;
    -s|--sandbox-timeout)
      SANDBOX_TIMEOUT_SECONDS="$2"; shift 2;;
    --timeout)
      TIMEOUT_SECONDS="$2"; shift 2;;
    --poll)
      POLL_INTERVAL_SECONDS="$2"; shift 2;;
    -o|--output)
      OUTPUT_CSV="$2"; shift 2;;
    -h|--help)
      usage; exit 0;;
    *)
      echo "Unknown option: $1" >&2
      usage
      exit 1;;
  esac
done

if [[ -z "$API_KEY" ]]; then
  echo "E2B API key missing. Set E2B_API_KEY or pass -k." >&2
  exit 1
fi

if ! [[ "$ITERATIONS" =~ ^[0-9]+$ ]] || [[ "$ITERATIONS" -le 0 ]]; then
  echo "iterations must be a positive integer" >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 1
fi

mkdir -p ./tmp
if [[ -z "$OUTPUT_CSV" ]]; then
  OUTPUT_CSV="./tmp/benchmark-template-restore-$(date +%Y%m%d-%H%M%S).csv"
fi

now_ms() {
  date +%s%3N
}

cleanup_sandbox() {
  local sandbox_id="$1"
  curl -sS -o /dev/null -X DELETE \
    -H "X-API-Key: ${API_KEY}" \
    "${API_URL}/sandboxes/${sandbox_id}" || true
}

require_api_ready() {
  local code
  code="$(curl -s -o /dev/null -w "%{http_code}" "${API_URL}/health" || true)"
  if [[ "$code" != "200" ]]; then
    echo "API is not ready: ${API_URL}/health returned ${code}" >&2
    exit 1
  fi
}

require_api_auth() {
  # API-key-friendly auth sanity check (unlike /nodes which may require admin auth).
  local raw code body
  raw="$(curl -sS -w '\n%{http_code}' \
    -H "X-API-Key: ${API_KEY}" \
    "${API_URL}/sandboxes?limit=1")"
  code="$(echo "$raw" | tail -n1)"
  body="$(echo "$raw" | sed '$d')"

  if [[ "$code" != "200" ]]; then
    echo "API key/auth check failed: GET /sandboxes?limit=1 returned ${code}" >&2
    echo "$body" >&2
    exit 1
  fi
}

percentile_from_file() {
  local p="$1"
  local file="$2"
  local n
  n=$(wc -l < "$file")
  if [[ "$n" -eq 0 ]]; then
    echo "NaN"
    return 0
  fi
  local idx
  idx=$(awk -v n="$n" -v p="$p" 'BEGIN {
    i = int((p/100.0)*n + 0.999999);
    if (i < 1) i = 1;
    if (i > n) i = n;
    print i;
  }')
  sed -n "${idx}p" "$file"
}

echo "round,sandbox_id,create_http_ms,ready_ms,total_ms,status" > "$OUTPUT_CSV"

durations_file="$(mktemp)"
create_http_file="$(mktemp)"
ready_file="$(mktemp)"
trap 'rm -f "$durations_file" "$create_http_file" "$ready_file"' EXIT

success=0
failure=0

echo "Benchmarking template restore/start latency"
echo "API:        ${API_URL}"
echo "Template:   ${TEMPLATE_ID}"
echo "Iterations: ${ITERATIONS}"
echo "CSV:        ${OUTPUT_CSV}"
echo

require_api_ready
require_api_auth

printed_error_body=0
for i in $(seq 1 "$ITERATIONS"); do
  payload=$(jq -cn \
    --arg templateID "$TEMPLATE_ID" \
    --argjson timeout "$SANDBOX_TIMEOUT_SECONDS" \
    '{templateID:$templateID, timeout:$timeout}')

  create_started_ms="$(now_ms)"
  create_raw="$(curl -sS -w '\n%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    -H "X-API-Key: ${API_KEY}" \
    -d "$payload" \
    "${API_URL}/sandboxes")"
  create_finished_ms="$(now_ms)"

  create_http_ms=$((create_finished_ms - create_started_ms))
  create_code="$(echo "$create_raw" | tail -n1)"
  create_body="$(echo "$create_raw" | sed '$d')"

  if [[ "$create_code" != "201" ]]; then
    failure=$((failure + 1))
    echo "${i},,${create_http_ms},,,$create_code" >> "$OUTPUT_CSV"
    echo "[$i/$ITERATIONS] create failed: http=$create_code"
    if [[ "$printed_error_body" -eq 0 ]]; then
      printed_error_body=1
      echo "First create error body:" >&2
      echo "$create_body" >&2
    fi
    continue
  fi

  sandbox_id="$(echo "$create_body" | jq -r '.sandboxID // empty')"
  if [[ -z "$sandbox_id" ]]; then
    failure=$((failure + 1))
    echo "${i},,${create_http_ms},,,$create_code" >> "$OUTPUT_CSV"
    echo "[$i/$ITERATIONS] create failed: missing sandboxID"
    continue
  fi

  deadline_ms=$((create_started_ms + TIMEOUT_SECONDS * 1000))
  ready_ms=""
  status=""
  while true; do
    now="$(now_ms)"
    if [[ "$now" -ge "$deadline_ms" ]]; then
      status="timeout"
      break
    fi

    get_raw="$(curl -sS -w '\n%{http_code}' \
      -H "X-API-Key: ${API_KEY}" \
      "${API_URL}/sandboxes/${sandbox_id}")"
    get_code="$(echo "$get_raw" | tail -n1)"
    get_body="$(echo "$get_raw" | sed '$d')"

    if [[ "$get_code" != "200" ]]; then
      status="get_${get_code}"
      break
    fi

    current_state="$(echo "$get_body" | jq -r '.state // empty')"
    if [[ "$current_state" == "running" ]]; then
      ready_ms=$((now - create_finished_ms))
      status="ok"
      break
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done

  total_ms=$(( $(now_ms) - create_started_ms ))
  cleanup_sandbox "$sandbox_id"

  if [[ "$status" == "ok" ]]; then
    success=$((success + 1))
    echo "${create_http_ms}" >> "$create_http_file"
    echo "${ready_ms}" >> "$ready_file"
    echo "${total_ms}" >> "$durations_file"
    echo "${i},${sandbox_id},${create_http_ms},${ready_ms},${total_ms},ok" >> "$OUTPUT_CSV"
    echo "[$i/$ITERATIONS] ok sandbox=${sandbox_id} total=${total_ms}ms (create=${create_http_ms}ms, ready=${ready_ms}ms)"
  else
    failure=$((failure + 1))
    echo "${i},${sandbox_id},${create_http_ms},,${total_ms},${status}" >> "$OUTPUT_CSV"
    echo "[$i/$ITERATIONS] failed sandbox=${sandbox_id} status=${status} total=${total_ms}ms"
  fi
done

echo
echo "Done. success=${success}, failure=${failure}"
echo "CSV: ${OUTPUT_CSV}"

if [[ "$success" -gt 0 ]]; then
  sort -n "$create_http_file" -o "$create_http_file"
  sort -n "$ready_file" -o "$ready_file"
  sort -n "$durations_file" -o "$durations_file"

  c_p50="$(percentile_from_file 50 "$create_http_file")"
  c_p95="$(percentile_from_file 95 "$create_http_file")"
  c_p99="$(percentile_from_file 99 "$create_http_file")"

  r_p50="$(percentile_from_file 50 "$ready_file")"
  r_p95="$(percentile_from_file 95 "$ready_file")"
  r_p99="$(percentile_from_file 99 "$ready_file")"

  t_p50="$(percentile_from_file 50 "$durations_file")"
  t_p95="$(percentile_from_file 95 "$durations_file")"
  t_p99="$(percentile_from_file 99 "$durations_file")"

  echo
  echo "Latency summary (ms)"
  echo "create_http: p50=${c_p50}, p95=${c_p95}, p99=${c_p99}"
  echo "ready_wait : p50=${r_p50}, p95=${r_p95}, p99=${r_p99}"
  echo "total      : p50=${t_p50}, p95=${t_p95}, p99=${t_p99}"
fi
