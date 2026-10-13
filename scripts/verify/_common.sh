#!/usr/bin/env bash
# Shared helpers for module Docker verification scripts.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-${ROOT_DIR}/manifest/docker/docker-compose.yml}"
COMPOSE_ARM_FILE="${COMPOSE_ARM_FILE:-${ROOT_DIR}/manifest/docker/docker-compose.arm.yml}"
COMPOSE_ENV_FILE="${COMPOSE_ENV_FILE:-${ROOT_DIR}/.env}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-${ROOT_DIR}/artifacts/verify}"
PILOT_BASE_URL="${PILOT_BASE_URL:-http://localhost:8080}"

mkdir -p "${ARTIFACTS_DIR}"

compose() {
  local files=(-f "${COMPOSE_FILE}")
  if [[ "${USE_ARM_OVERLAY:-1}" == "1" ]] && [[ -f "${COMPOSE_ARM_FILE}" ]]; then
    files+=(-f "${COMPOSE_ARM_FILE}")
  fi
  docker compose --env-file "${COMPOSE_ENV_FILE}" "${files[@]}" "$@"
}

require_env_file() {
  if [[ ! -f "${COMPOSE_ENV_FILE}" ]]; then
    echo "missing ${COMPOSE_ENV_FILE}; copy .env.example to .env" >&2
    exit 1
  fi
}

write_summary() {
  local module="$1"
  local status="$2"
  local detail="${3:-}"
  local out="${ARTIFACTS_DIR}/${module}.json"
  cat >"${out}" <<EOF
{"module":"${module}","status":"${status}","detail":$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "${detail}"),"checked_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
  echo "wrote ${out}"
}

wait_http() {
  local url="$1"
  local attempts="${2:-60}"
  local i=1
  while [[ $i -le $attempts ]]; do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    i=$((i + 1))
  done
  echo "timeout waiting for ${url}" >&2
  return 1
}

json_get() {
  # Usage: json_get <file> <python-expr-on-obj>
  local file="$1"
  local expr="$2"
  python3 - "$file" "$expr" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1]))
print(eval(sys.argv[2], {"obj": obj}))
PY
}
