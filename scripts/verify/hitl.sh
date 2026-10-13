#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/verify/_common.sh
source "${SCRIPT_DIR}/_common.sh"

MODULE="hitl"
require_env_file

# HITL verify uses a temporary compose override enabling hitl via env is not enough
# (config is YAML). We hit the API with a wide window only when hitl.enabled=true.
# For local ARM validation: rebuild with config.docker.yaml hitl.enabled toggled by sed copy.

TMP_CFG="${ROOT_DIR}/artifacts/verify/config.hitl.yaml"
mkdir -p "$(dirname "${TMP_CFG}")"
python3 - <<PY
from pathlib import Path
src = Path("${ROOT_DIR}/manifest/config/config.docker.yaml").read_text()
src = src.replace("enabled: false\n      wide_window_minutes:", "enabled: true\n      wide_window_minutes:", 1)
Path("${TMP_CFG}").write_text(src)
print("wrote", "${TMP_CFG}")
PY

compose --profile app up -d --build
NET="$(docker inspect pilot-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' 2>/dev/null || true)"
if [[ -z "${NET}" ]]; then
  NET="$(docker network ls --format '{{.Name}}' | grep -E 'pilot-network$' | head -n1 || true)"
fi
if [[ -z "${NET}" ]]; then
  write_summary "${MODULE}" "fail" "could not resolve compose network"
  exit 1
fi

# Restart pilot with HITL config mounted
docker rm -f pilot-api-hitl >/dev/null 2>&1 || true
docker run -d --name pilot-api-hitl --network "${NET}" \
  --env-file "${COMPOSE_ENV_FILE}" \
  -e PILOT_CONFIG=/app/manifest/config/config.docker.yaml \
  -v "${TMP_CFG}:/app/manifest/config/config.docker.yaml:ro" \
  -p 18080:8080 \
  pilot:local >/dev/null

HITL_URL="${HITL_URL:-http://localhost:18080}"
wait_http "${HITL_URL}/api/v1/health/live" 90 || {
  write_summary "${MODULE}" "fail" "hitl pilot failed to become live"
  docker logs pilot-api-hitl 2>&1 | tail -n 80 || true
  exit 1
}

resp="$(curl -fsS -X POST "${HITL_URL}/api/v1/chat/multi-agent" \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"hitl","session_id":"hitl-1","query":"duration_minutes=180 correlate_approve service:=pilot-api 故障排查"}')"

cp="$(python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("data",{}).get("metadata",{}).get("checkpoint_id",""))' <<<"${resp}")"
if [[ -z "${cp}" ]]; then
  write_summary "${MODULE}" "fail" "expected interrupted checkpoint: ${resp}"
  docker rm -f pilot-api-hitl >/dev/null 2>&1 || true
  exit 1
fi

# Simulate process restart by restarting container with same volume-less redis network.
docker restart pilot-api-hitl >/dev/null
wait_http "${HITL_URL}/api/v1/health/live" 90

resume="$(curl -fsS -X POST "${HITL_URL}/api/v1/chat/multi-agent/resume" \
  -H 'Content-Type: application/json' \
  -d "$(printf '{"checkpoint_id":"%s","approve":true}' "${cp}")")"

if ! echo "${resume}" | grep -q '"success":true'; then
  write_summary "${MODULE}" "fail" "resume failed: ${resume}"
  docker rm -f pilot-api-hitl >/dev/null 2>&1 || true
  exit 1
fi

docker rm -f pilot-api-hitl >/dev/null 2>&1 || true
write_summary "${MODULE}" "ok" "interrupt+resume across restart checkpoint=${cp}"
echo "[${MODULE}] OK"
