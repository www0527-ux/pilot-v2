#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/verify/_common.sh
source "${SCRIPT_DIR}/_common.sh"

MODULE="eval"
require_env_file
mkdir -p "${ROOT_DIR}/artifacts/eval/offline" "${ROOT_DIR}/artifacts/eval/live"

# Offline canaries always run (no LLM).
(
  cd "${ROOT_DIR}"
  go run ./cmd/agenteval -mode offline -cases testdata/eval/canaries.json -out artifacts/eval/offline
)

# Live canaries when stack is up and key present.
if grep -q 'DASHSCOPE_API_KEY=.\+' "${COMPOSE_ENV_FILE}" 2>/dev/null; then
  compose --profile app up -d --build
  wait_http "${PILOT_BASE_URL}/api/v1/health/ready" 90
  (
    cd "${ROOT_DIR}"
    go run ./cmd/agenteval -mode live -base-url "${PILOT_BASE_URL}" -cases testdata/eval/canaries.json -out artifacts/eval/live
  )
fi

write_summary "${MODULE}" "ok" "offline/live eval reports written to separate artifacts/eval directories"
echo "[${MODULE}] OK"
