#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/verify/_common.sh
source "${SCRIPT_DIR}/_common.sh"

MODULE="corr"
require_env_file

compose --profile app up -d --build
wait_http "${PILOT_BASE_URL}/api/v1/health/ready" 90

# Seed demo error logs so correlation has a log signal.
compose --profile app run --rm --entrypoint /app/seedlogs pilot --count 20 || true

payload='{"service":"pilot-api","duration_minutes":30,"metric_query":"up"}'
resp="$(curl -fsS -X POST "${PILOT_BASE_URL}/api/v1/observability/correlate" \
  -H 'Content-Type: application/json' -d "${payload}")"

if ! echo "${resp}" | grep -q '"hypotheses"'; then
  write_summary "${MODULE}" "fail" "missing hypotheses: ${resp}"
  exit 1
fi
if ! echo "${resp}" | grep -Eq 'corr-hyp:|corr-log:|corr-metric:|corr-alert:'; then
  write_summary "${MODULE}" "fail" "missing corr-* evidence ids: ${resp}"
  exit 1
fi
if ! echo "${resp}" | grep -q '"method":"robust_temporal_evidence_graph_v1"'; then
  write_summary "${MODULE}" "fail" "missing robust temporal confidence metadata: ${resp}"
  exit 1
fi

write_summary "${MODULE}" "ok" "correlate returned citeable robust temporal evidence graph"
echo "[${MODULE}] OK"
