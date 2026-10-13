#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/verify/_common.sh
source "${SCRIPT_DIR}/_common.sh"

MODULE="mcp"
require_env_file

compose --profile app up -d --build
wait_http "${PILOT_BASE_URL}/api/v1/health/ready" 90

TMP_CFG="${ROOT_DIR}/artifacts/verify/config.mcp.yaml"
mkdir -p "$(dirname "${TMP_CFG}")"
python3 - <<PY
from pathlib import Path
src = Path("${ROOT_DIR}/manifest/config/config.docker.yaml").read_text()
src = src.replace("mcp:\n  enabled: false", "mcp:\n  enabled: true", 1)
Path("${TMP_CFG}").write_text(src)
PY

docker rm -f pilot-mcp >/dev/null 2>&1 || true
# Detect compose network name
NET="$(docker network ls --format '{{.Name}}' | grep -E 'pilot-network$' | head -n1)"
if [[ -z "${NET}" ]]; then
  write_summary "${MODULE}" "fail" "pilot-network not found"
  exit 1
fi

docker run -d --name pilot-mcp --network "${NET}" \
  --env-file "${COMPOSE_ENV_FILE}" \
  -e PILOT_CONFIG=/config.yaml \
  -v "${TMP_CFG}:/config.yaml:ro" \
  -p 8081:8081 \
  --entrypoint /app/mcpserver \
  pilot:local >/dev/null

wait_http "http://localhost:8081/health" 60 || {
  write_summary "${MODULE}" "fail" "mcp health failed"
  docker logs pilot-mcp 2>&1 | tail -n 80 || true
  docker rm -f pilot-mcp >/dev/null 2>&1 || true
  exit 1
}

initialize="$(curl -fsS -X POST http://localhost:8081/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"pilot-verify","version":"1"}}}')"
if ! echo "${initialize}" | grep -q 'protocolVersion'; then
  write_summary "${MODULE}" "fail" "initialize handshake failed: ${initialize}"
  exit 1
fi

list="$(curl -fsS -X POST http://localhost:8081/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')"
if ! echo "${list}" | grep -q 'query_alerts' || ! echo "${list}" | grep -q 'inputSchema'; then
  write_summary "${MODULE}" "fail" "tools/list missing query_alerts: ${list}"
  docker rm -f pilot-mcp >/dev/null 2>&1 || true
  exit 1
fi

invalid="$(curl -fsS -X POST http://localhost:8081/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"query_logs","arguments":{"level":"verbose"}}}')"
if ! echo "${invalid}" | grep -q -- '-32602'; then
  write_summary "${MODULE}" "fail" "invalid arguments were not rejected: ${invalid}"
  exit 1
fi

call="$(curl -fsS -X POST http://localhost:8081/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"correlate_signals","arguments":{"service":"pilot-api"}}}')"
if ! echo "${call}" | grep -q '"result"'; then
  write_summary "${MODULE}" "fail" "tools/call failed: ${call}"
  docker rm -f pilot-mcp >/dev/null 2>&1 || true
  exit 1
fi

docker rm -f pilot-mcp >/dev/null 2>&1 || true
write_summary "${MODULE}" "ok" "mcp initialize+schema+readonly call+invalid params"
echo "[${MODULE}] OK"
