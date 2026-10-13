# Pilot consistency repair acceptance

Date: 2026-08-26. This report maps the audited discrepancy to the production change and evidence. Detailed four-state requirements are maintained in `docs/IMPLEMENTATION_AUDIT.md`.

| Original discrepancy | Modification location | Verification command / result |
| --- | --- | --- |
| Log levels differed across HTTP, tools, MCP and Web; no `all`, complete fatal/alias sync, or output-budget summary | `internal/observability/validation.go`, `log_summary.go`, controller, Agent Tool, MCP, logging and `web/*` | `go test ./internal/observability ./internal/logging ./internal/ai/tools ./internal/mcp ./internal/controller`; `make web-test` — passed |
| Web had duplicate design variables/responsive rules, browser confirm, weak status semantics and no feedback/log-summary UI | `web/index.html`, `web/app.js`, `web/styles.css`, `web/app.test.mjs` | `node --check web/app.js`; `make web-test` — 3/3 passed; in-app Browser 1280px/390px smoke — no horizontal overflow or console errors |
| Knowledge indexed and returned whole documents with one vector and one ES ID | `chunker.go`, `knowledge_retriever.go`, `pkg/types/types.go`, strict RAG config | Chunk CJK/emoji/combining/overlap, independent embedding and “whole document not indexed” tests — passed |
| Multi-Chunk writes/deletes lacked stable logical identity, rollback, count and idempotency | `pkg/elasticsearch/elasticsearch.go`, retriever and knowledge controller | stable doc_id, partial-bulk rollback, DeleteByQuery count and second-delete-zero tests — passed |
| Hybrid recall fused whole documents and lacked exact boosts, reranking, diversity, explicit degradation and observability | retriever recall/reranker/telemetry files plus `cmd/main.go` model injection | RRF, exact ID, lexical/model/fallback, per-document limit and degradation tests — passed; `pilot_rag_*` metrics and OTel spans build-tested |
| Old vector conflicts silently changed index name | retriever mapping failure and `docs/RAG_MIGRATION.md` | incompatible-dimension migration-instruction test — passed |
| Prompts were hard-coded in several Go packages | `internal/prompts/prompts.yaml`, strict loader, startup/Docker wiring, all consumers | missing/unknown/illegal-variable/load/render and consumer tests — passed |
| Full Chat lacked a real ContextPreFetcher and bounded MySQL/episodic/static context | `context_prefetcher.go`, Full Agent Eino Graph, layered memory wiring | parallel/timeout/failure isolation/no-RAG/MySQL-cold tests — passed |
| Short memory nested generated summaries; Multi-Agent read only Redis | independent Redis summary state and `LayeredSessionContext` | non-nesting/injectable fallback, Redis+MySQL parallel and cold recovery tests — passed |
| Episodic “hybrid” lacked independent keyword/vector recall, Recent fallback and source provenance | `internal/memory/episodic.go` | SQLite Recent fallback and HTTP-backed keyword+vector `recall_sources` tests — passed |
| Every Multi-Agent completion auto-promoted success memory; no interaction feedback lifecycle | interaction/feedback MySQL models, Chat API/header/metadata, Web controls; promotion removed from SessionOrchestrator | SQLite idempotent vote update, Up-only eligibility, interrupted/rejected/abstain exclusion and promotion-failure retention tests — passed |
| Hybrid classifier failure could omit Evidence; service parsing was narrow | hybrid planner and routing regex set | failure/low-confidence widening and service `:`, `=`, `:=`, Chinese forms — passed |
| ADK ignored branch timeout and parity was not proven | ADK timeout context, shared prepared plan/gates | ADK timeout and legacy/ADK plan/verification/limitation parity tests — passed |
| Time-window semantics stopped at some boundaries; HITL parsed query text separately | HTTP `duration_minutes` → request context → plan → evidence/correlation/HITL; Tool/MCP shared bounds | controller propagation, MCP invalid bounds and HITL checkpoint tests — passed |
| HITL resume was non-atomic and used a synthetic session; Console used confirm | checkpoint ledger, memory/Redis CAS, prepared-plan reuse, original identity/result, persistent Web component | concurrent resume, wrapper restart, reject, original-session and Web tests — passed |
| MCP was a minimal facade without initialize/schema/validation | standard fixed six-tool server and enhanced module verifier | initialize/capabilities, stable inputSchema, allowlist, illegal params and shared log summary tests — passed |
| Eval live failures were swallowed and offline/live reports overwrote each other | `cmd/agenteval` schema v2 and `scripts/verify/eval.sh` | `go run ./cmd/agenteval -mode offline` — 3/3 passed in `artifacts/eval/offline`; `bash -n` passed; live no longer uses `|| true` |
| Team development Skills were absent | `.claude/skills/pilot-*` (six scoped Skills) | skill-creator `quick_validate.py` — all six valid |

## Deterministic gates

| Gate | Result |
| --- | --- |
| `gofmt` / `git diff --check` | passed |
| `go vet ./...` | passed |
| `go test ./...` | passed |
| `node --check web/app.js` | passed |
| `make web-test` | passed, 3/3 |
| `go run ./cmd/agenteval -mode offline` | passed, 3/3, schema `pilot.agent_eval.v2` |
| `make ci` | passed, including race/build/tidy; macOS linker emitted the known non-fatal LC_DYSYMTAB warning |

## External checks not passed or not run

- `make verify` was executed and failed because Redis `6379`, Elasticsearch `9200`, MySQL `3306`, and Jaeger `16686` were not running; the configured Prometheus endpoint did not expose the expected API. Configuration and model-key checks passed.
- Docker daemon was unavailable, so `make e2e`, `make docker-e2e`, and `make docker-module MODULE=corr|episodic|eval|hitl|mcp` were not run.
- Live Agent eval was not run because the Pilot service and its real dependency stack were unavailable. Its script is fail-fast and writes only to `artifacts/eval/live` when executed.
- No credentials or prompt/log/memory bodies were printed during these probes.
