#!/usr/bin/env bash
# M1: verify Pilot ARM full-stack compose (live + ready + metrics).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/verify/_common.sh
source "${SCRIPT_DIR}/_common.sh"

MODULE="base"
require_env_file

echo "[${MODULE}] building + starting compose (profiles: app)"
compose --profile app build pilot
compose --profile app up -d

echo "[${MODULE}] waiting for live/ready"
wait_http "${PILOT_BASE_URL}/api/v1/health/live" 90
wait_http "${PILOT_BASE_URL}/api/v1/health/ready" 90

live="$(curl -fsS "${PILOT_BASE_URL}/api/v1/health/live")"
ready="$(curl -fsS "${PILOT_BASE_URL}/api/v1/health/ready")"
metrics="$(curl -fsS "${PILOT_BASE_URL}/metrics" | head -n 5 || true)"

if ! echo "${live}" | grep -q '"success":true'; then
  write_summary "${MODULE}" "fail" "live check failed: ${live}"
  exit 1
fi
if ! echo "${ready}" | grep -q '"success":true'; then
  write_summary "${MODULE}" "fail" "ready check failed: ${ready}"
  exit 1
fi
if [[ -z "${metrics}" ]]; then
  write_summary "${MODULE}" "fail" "metrics empty"
  exit 1
fi

write_summary "${MODULE}" "ok" "live+ready+metrics healthy on ${PILOT_BASE_URL}"
echo "[${MODULE}] OK"
