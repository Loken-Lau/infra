#!/usr/bin/env bash
set -euo pipefail

# Cleanup script for local benchmark environment.
# Stops local app services (api/orchestrator/client-proxy) and optionally local infra.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_DIR="$ROOT_DIR/tmp/local-dev-pids"
COMPOSE_FILE="$ROOT_DIR/packages/local-dev/docker-compose.yaml"

KEEP_INFRA=false
REMOVE_VOLUMES=false

usage() {
  cat <<'EOF'
Usage:
  ./scripts/local-dev-cleanup-after-benchmark.sh [options]

Options:
  --keep-infra      Keep docker compose services running (only stop app processes)
  --remove-volumes  When stopping infra, also remove compose volumes
  -h, --help        Show this help

Examples:
  ./scripts/local-dev-cleanup-after-benchmark.sh
  ./scripts/local-dev-cleanup-after-benchmark.sh --keep-infra
  ./scripts/local-dev-cleanup-after-benchmark.sh --remove-volumes
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep-infra)
      KEEP_INFRA=true
      shift
      ;;
    --remove-volumes)
      REMOVE_VOLUMES=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      exit 1
      ;;
  esac
done

print() {
  echo "[$(date +'%H:%M:%S')] $*"
}

can_use_sudo_non_interactive() {
  sudo -n true >/dev/null 2>&1
}

run_docker_compose() {
  local action="$1"
  shift

  if ! can_use_sudo_non_interactive; then
    print "WARNING: sudo is unavailable; skipping docker compose ${action}."
    return 1
  fi

  sudo docker compose -f "$COMPOSE_FILE" "$action" "$@"
}

is_pid_running() {
  local pid="$1"
  kill -0 "$pid" >/dev/null 2>&1
}

is_port_open() {
  local host="$1"
  local port="$2"
  (echo >/dev/tcp/"$host"/"$port") >/dev/null 2>&1
}

find_pid_by_port() {
  local port="$1"
  local require_sudo="${2:-false}"
  local pid=""

  if command -v lsof >/dev/null 2>&1; then
    if [[ "$require_sudo" == "true" ]] && can_use_sudo_non_interactive; then
      pid="$(sudo lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n1 || true)"
    else
      pid="$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n1 || true)"
    fi
  fi

  if [[ -z "$pid" ]] && command -v ss >/dev/null 2>&1; then
    if [[ "$require_sudo" == "true" ]] && can_use_sudo_non_interactive; then
      pid="$(sudo ss -ltnp "sport = :$port" 2>/dev/null | sed -n 's/.*pid=\([0-9]\+\).*/\1/p' | head -n1 || true)"
    else
      pid="$(ss -ltnp "sport = :$port" 2>/dev/null | sed -n 's/.*pid=\([0-9]\+\).*/\1/p' | head -n1 || true)"
    fi
  fi

  echo "$pid"
}

wait_for_port_closed() {
  local service_name="$1"
  local port="$2"
  local timeout_seconds="${3:-20}"
  local elapsed=0

  while (( elapsed < timeout_seconds )); do
    if ! is_port_open "127.0.0.1" "$port"; then
      print "${service_name} port ${port} is closed"
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done

  print "WARNING: ${service_name} port ${port} is still open after ${timeout_seconds}s"
  return 1
}

stop_pid() {
  local service_name="$1"
  local pid="$2"
  local require_sudo="$3"

  if [[ "$require_sudo" == "true" ]]; then
    if can_use_sudo_non_interactive; then
      sudo kill -TERM "$pid" >/dev/null 2>&1 || true
    else
      print "WARNING: cannot sudo-kill ${service_name} pid=${pid}; sudo unavailable"
      return 0
    fi
  else
    kill -TERM "$pid" >/dev/null 2>&1 || true
  fi

  for _ in $(seq 1 10); do
    if [[ "$require_sudo" == "true" ]]; then
      if ! sudo kill -0 "$pid" >/dev/null 2>&1; then
        print "Stopped ${service_name} (pid=${pid})"
        return 0
      fi
    else
      if ! is_pid_running "$pid"; then
        print "Stopped ${service_name} (pid=${pid})"
        return 0
      fi
    fi
    sleep 1
  done

  print "Process still alive, forcing ${service_name} (pid=${pid})"
  if [[ "$require_sudo" == "true" ]]; then
    sudo kill -KILL "$pid" >/dev/null 2>&1 || true
  else
    kill -KILL "$pid" >/dev/null 2>&1 || true
  fi
}

stop_service_from_pid_file() {
  local service_name="$1"
  local pid_file="$2"
  local require_sudo="$3"

  if [[ ! -f "$pid_file" ]]; then
    print "No pid file for ${service_name}, skipping"
    return 0
  fi

  local pid
  pid="$(cat "$pid_file" 2>/dev/null || true)"
  if [[ -z "$pid" || ! "$pid" =~ ^[0-9]+$ ]]; then
    print "Invalid pid file for ${service_name}: $pid_file"
    rm -f "$pid_file"
    return 0
  fi

  if [[ "$require_sudo" == "true" ]]; then
    if can_use_sudo_non_interactive && sudo kill -0 "$pid" >/dev/null 2>&1; then
      stop_pid "$service_name" "$pid" "true"
    else
      print "${service_name} (pid=${pid}) is not running (or not accessible), removing pid file"
    fi
  else
    if is_pid_running "$pid"; then
      stop_pid "$service_name" "$pid" "false"
    else
      print "${service_name} (pid=${pid}) is not running, removing pid file"
    fi
  fi

  rm -f "$pid_file"
}

stop_service_by_port_if_needed() {
  local service_name="$1"
  local port="$2"
  local require_sudo="$3"

  if ! is_port_open "127.0.0.1" "$port"; then
    return 0
  fi

  local pid
  pid="$(find_pid_by_port "$port" "$require_sudo")"
  if [[ -z "$pid" ]]; then
    print "WARNING: ${service_name} is still listening on ${port}, but pid could not be discovered"
    return 1
  fi

  print "Stopping ${service_name} discovered by port ${port} (pid=${pid})"
  stop_pid "$service_name" "$pid" "$require_sudo"
  wait_for_port_closed "$service_name" "$port" 20 || true
}

main() {
  print "Starting local benchmark cleanup"

  stop_service_from_pid_file "api" "$PID_DIR/api.pid" "false"
  stop_service_from_pid_file "orchestrator" "$PID_DIR/orchestrator.pid" "true"
  stop_service_from_pid_file "client-proxy" "$PID_DIR/client-proxy.pid" "false"
  stop_service_by_port_if_needed "api" "3000" "false" || true
  stop_service_by_port_if_needed "orchestrator" "5008" "true" || true
  stop_service_by_port_if_needed "client-proxy" "3003" "false" || true

  if [[ "$KEEP_INFRA" == "true" ]]; then
    print "Keeping local infra running (--keep-infra)"
  else
    print "Stopping local infra (docker compose down)"
    if [[ "$REMOVE_VOLUMES" == "true" ]]; then
      run_docker_compose down --remove-orphans -v || true
    else
      run_docker_compose down --remove-orphans || true
    fi
  fi

  print "Cleanup finished"
}

main "$@"
