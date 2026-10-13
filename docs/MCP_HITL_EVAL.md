# MCP / HITL Evaluation

## MCP

**Status:** implemented as an optional standard read-only MCP server (`cmd/mcpserver`).

- Enable with `mcp.enabled=true` (forced `read_only=true` in config defaults).
- Exposes: `query_alerts`, `query_logs`, `query_metrics`, `query_traces`, `search_knowledge`, `correlate_signals`.
- Protocol: `initialize`/capabilities, deterministic `tools/list` with `inputSchema`, validated `tools/call`, notifications/initialized, ping, and JSON-RPC errors. The registry structurally rejects names outside the six-tool allowlist.
- `query_logs` shares the HTTP/Agent log-level vocabulary, time-window bounds, and rune-safe summary contract.
- Auth: shared `PILOT_API_KEY` via Bearer / X-API-Key when configured.
- Verify: `make docker-module MODULE=mcp`.

**Still deferred:** consuming third-party MCP servers as inbound tool marketplace.

## HITL

**Status:** implemented behind `agent.multi_agent.hitl.enabled` (default false).

- Gate: `ThresholdGate` interrupts when `duration_minutes` ≥ `wide_window_minutes` or query contains `correlate_approve`.
- Store: `redis` (default) or `memory`; status transitions use CAS to prevent concurrent double resume.
- API: multi-agent may return `metadata.status=interrupted` + `checkpoint_id`; resume via `POST /api/v1/chat/multi-agent/resume`.
- Checkpoint: persists original user/session/query, prepared plan, completed steps, findings, next stage, attempts, and final result across restart. States are pending/processing/completed/rejected/failed; failed runs have bounded retry.
- Console: persistent accessible approve/reject component; reject is a normal business result and resumed output is persisted to the original session.
- Verify: `make docker-module MODULE=hitl`.
