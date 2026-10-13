#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/verify/_common.sh
source "${SCRIPT_DIR}/_common.sh"

MODULE="episodic"
require_env_file

compose --profile app up -d --build
wait_http "${PILOT_BASE_URL}/api/v1/health/ready" 90

sid="episodic-$(date +%s)"
body1="$(printf '{"user_id":"episodic","session_id":"%s","query":"pilot-api 故障排查 service:=pilot-api"}' "$sid")"
curl -fsS -X POST "${PILOT_BASE_URL}/api/v1/chat/multi-agent" -H 'Content-Type: application/json' -d "${body1}" >/tmp/pilot-episodic-1.json
sleep 2
body2="$(printf '{"user_id":"episodic","session_id":"%s-2","query":"pilot-api 类似故障怎么处理 service:=pilot-api"}' "$sid")"
resp="$(curl -fsS -X POST "${PILOT_BASE_URL}/api/v1/chat/multi-agent" -H 'Content-Type: application/json' -d "${body2}")"

if ! echo "${resp}" | grep -Eq 'episodic:|episodic_memory|session_context_parts|episodic_memory_findings'; then
  write_summary "${MODULE}" "fail" "expected episodic memory markers: ${resp}"
  exit 1
fi

write_summary "${MODULE}" "ok" "episodic recall markers present"
echo "[${MODULE}] OK"
