#!/usr/bin/env bash
set -euo pipefail

# Bootstrap local dev environment up to "build base template".
# Idempotent behavior:
# - downloads are skipped when artifacts already exist
# - infra/services are skipped when already healthy

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

LOG_DIR="$ROOT_DIR/tmp/local-dev-logs"
PID_DIR="$ROOT_DIR/tmp/local-dev-pids"
mkdir -p "$LOG_DIR" "$PID_DIR"

STARTUP_TIMEOUT_SECONDS="${STARTUP_TIMEOUT_SECONDS:-180}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-2}"

print() {
  echo "[$(date +'%H:%M:%S')] $*"
}

can_use_sudo_non_interactive() {
  sudo -n true >/dev/null 2>&1
}

need_sudo_or_exit() {
  local reason="$1"
  if ! can_use_sudo_non_interactive; then
    print "ERROR: sudo is required for ${reason}, but non-interactive sudo is not available."
    print "Please run this script in an environment where sudo works (or pre-configure the prerequisite manually)."
    exit 1
  fi
}

is_port_open() {
  local host="$1"
  local port="$2"
  (echo >/dev/tcp/"$host"/"$port") >/dev/null 2>&1
}

wait_for_http_200() {
  local name="$1"
  local url="$2"
  local timeout="${3:-$STARTUP_TIMEOUT_SECONDS}"
  local elapsed=0

  print "Waiting for ${name} at ${url} (timeout ${timeout}s)"
  while (( elapsed < timeout )); do
    local code
    code="$(curl -s -o /dev/null -w "%{http_code}" "$url" || true)"
    if [[ "$code" == "200" ]]; then
      print "${name} is healthy"
      return 0
    fi
    sleep "$POLL_INTERVAL_SECONDS"
    elapsed=$((elapsed + POLL_INTERVAL_SECONDS))
  done

  print "ERROR: ${name} is not healthy in time"
  return 1
}

wait_for_port() {
  local name="$1"
  local host="$2"
  local port="$3"
  local timeout="${4:-$STARTUP_TIMEOUT_SECONDS}"
  local elapsed=0

  print "Waiting for ${name} on ${host}:${port} (timeout ${timeout}s)"
  while (( elapsed < timeout )); do
    if is_port_open "$host" "$port"; then
      print "${name} is reachable"
      return 0
    fi
    sleep "$POLL_INTERVAL_SECONDS"
    elapsed=$((elapsed + POLL_INTERVAL_SECONDS))
  done

  print "ERROR: ${name} is not reachable in time"
  return 1
}

ensure_last_used_env_marker() {
  if [[ ! -f "$ROOT_DIR/.last_used_env" ]]; then
    print "Creating .last_used_env marker (local)"
    echo "local" > "$ROOT_DIR/.last_used_env"
  fi
}

ensure_nbd_and_hugepages() {
  print "Checking kernel prerequisites (nbd, hugepages)"

  if lsmod | awk '{print $1}' | grep -q "^nbd$"; then
    print "nbd module already loaded"
  else
    need_sudo_or_exit "loading nbd module"
    print "Loading nbd module"
    sudo modprobe nbd nbds_max=64
  fi

  local current_hugepages
  current_hugepages="$(cat /proc/sys/vm/nr_hugepages)"
  if [[ "$current_hugepages" -ge 2048 ]]; then
    print "Hugepages already set (${current_hugepages})"
  else
    need_sudo_or_exit "setting hugepages"
    print "Setting hugepages to 2048 (current ${current_hugepages})"
    sudo sysctl -w vm.nr_hugepages=2048
  fi
}

ensure_kernels_downloaded() {
  if find "$ROOT_DIR/packages/fc-kernels" -type f -name "vmlinux.bin" 2>/dev/null | grep -q .; then
    print "Kernels already downloaded, skipping"
  else
    print "Downloading kernels"
    make download-public-kernels
  fi
}

ensure_firecrackers_downloaded() {
  if find "$ROOT_DIR/packages/fc-versions/builds" -type f -name "firecracker" 2>/dev/null | grep -q .; then
    print "Firecrackers already downloaded, skipping"
  else
    print "Downloading firecrackers"
    make download-public-firecrackers
  fi
}

ensure_local_infra() {
  local compose_file="$ROOT_DIR/packages/local-dev/docker-compose.yaml"
  local clickhouse_cfg="$ROOT_DIR/packages/local-dev/clickhouse-config-generated.xml"

  # Keep local-dev behavior aligned with `make -C packages/local-dev local-infra`:
  # generate ClickHouse config (contains remote_servers/cluster) before compose up.
  print "Generating ClickHouse local config"
  make -C packages/local-dev clickhouse-config-generated.xml

  if ! grep -q "<cluster>" "$clickhouse_cfg" 2>/dev/null; then
    print "ERROR: ClickHouse config was generated, but cluster definition is missing: $clickhouse_cfg"
    return 1
  fi

  print "Ensuring local infra is running (docker compose up -d)"
  if docker compose -f "$compose_file" up -d >/dev/null 2>&1; then
    docker compose -f "$compose_file" up -d
  elif can_use_sudo_non_interactive; then
    print "Docker requires sudo in this environment"
    sudo docker compose -f "$compose_file" up -d
  else
    print "ERROR: cannot run docker compose without sudo, and sudo is unavailable."
    print "Fix by adding your user to docker group or enabling sudo."
    exit 1
  fi

  wait_for_port "Postgres" "127.0.0.1" "5432" 120
  wait_for_port "Redis" "127.0.0.1" "6379" 120
  wait_for_port "ClickHouse" "127.0.0.1" "8123" 120
}

prepare_local_env() {
  print "Running DB and service preparation"
  make -C packages/db migrate-local
  make -C packages/clickhouse migrate-local
  make -C packages/envd build
  make -C packages/local-dev seed-database
}

start_api_if_needed() {
  if is_port_open "127.0.0.1" "3000"; then
    print "API already running, skipping start"
    return 0
  fi

  print "Starting API in background (detached)"
  nohup bash -lc "cd '$ROOT_DIR/packages/api' && make LOCAL_BUILD_TARGET=build run-local" \
    >"$LOG_DIR/api.log" 2>&1 &
  local api_pid=$!
  echo "$api_pid" > "$PID_DIR/api.pid"

  local elapsed=0
  print "Waiting for API port 3000 (timeout 180s)"
  while (( elapsed < 180 )); do
    if is_port_open "127.0.0.1" "3000"; then
      print "API port is reachable"
      return 0
    fi

    if ! kill -0 "$api_pid" >/dev/null 2>&1; then
      print "ERROR: API process exited unexpectedly. Last logs:"
      tail -n 80 "$LOG_DIR/api.log" || true
      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
    elapsed=$((elapsed + POLL_INTERVAL_SECONDS))
  done

  print "ERROR: API did not open port 3000 in time"
  tail -n 80 "$LOG_DIR/api.log" || true
  return 1
}

start_orchestrator_if_needed() {
  if is_port_open "127.0.0.1" "5008"; then
    print "Orchestrator already running, skipping start"
    return 0
  fi

  print "Building orchestrator (non-debug) before start"
  make -C packages/orchestrator build-local

  need_sudo_or_exit "starting orchestrator"
  print "Starting orchestrator in background (detached, sudo)"
  sudo -E nohup bash -lc "cd '$ROOT_DIR/packages/orchestrator' && make run-local" \
    >"$LOG_DIR/orchestrator.log" 2>&1 &
  local orchestrator_pid=$!
  echo "$orchestrator_pid" > "$PID_DIR/orchestrator.pid"

  local elapsed=0
  print "Waiting for Orchestrator port 5008 (timeout 240s)"
  while (( elapsed < 240 )); do
    if is_port_open "127.0.0.1" "5008"; then
      print "Orchestrator port is reachable"
      return 0
    fi

    if ! kill -0 "$orchestrator_pid" >/dev/null 2>&1; then
      print "ERROR: Orchestrator process exited unexpectedly. Last logs:"
      tail -n 80 "$LOG_DIR/orchestrator.log" || true
      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
    elapsed=$((elapsed + POLL_INTERVAL_SECONDS))
  done

  print "ERROR: Orchestrator did not open port 5008 in time"
  tail -n 80 "$LOG_DIR/orchestrator.log" || true
  return 1
}

start_client_proxy_if_needed() {
  if curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:3003" 2>/dev/null | grep -q "^200$"; then
    print "Client-proxy already running, skipping start"
    return 0
  fi

  print "Starting client-proxy in background (detached)"
  nohup bash -lc "cd '$ROOT_DIR/packages/client-proxy' && make LOCAL_BUILD_TARGET=build run-local" \
    >"$LOG_DIR/client-proxy.log" 2>&1 &
  local client_proxy_pid=$!
  echo "$client_proxy_pid" > "$PID_DIR/client-proxy.pid"

  wait_for_http_200 "Client-proxy health" "http://127.0.0.1:3003" 180
}

build_base_template() {
  print "Building base template"
  make -C packages/shared/scripts local-build-base-template
}

main() {
  print "Starting local bootstrap (up to base template build)"
  ensure_last_used_env_marker
  ensure_nbd_and_hugepages
  ensure_kernels_downloaded
  ensure_firecrackers_downloaded
  ensure_local_infra
  prepare_local_env
  start_api_if_needed
  start_orchestrator_if_needed
  start_client_proxy_if_needed
  build_base_template

  print "Done."
  print "Logs: $LOG_DIR"
  print "PIDs: $PID_DIR"
}

main "$@"
