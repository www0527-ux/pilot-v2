# Pilot implementation consistency audit

This matrix is the working source of truth for the repository-wide consistency repair. Status values are limited to `verified`, `partial`, `missing`, and `incorrect`. An item may become `verified` only after its production call path and deterministic test evidence are both linked here.

| Group | Requirement | Status | Current evidence / gap |
| --- | --- | --- | --- |
| A | Shared log-level semantics (`debug/info/warn/error/fatal`, `warning`, `all`) | verified | Production: `internal/observability/validation.go`, `internal/logging/logging.go`, HTTP/Tool/MCP and `web/index.html`. Evidence: `log_summary_test.go`, `logging_test.go`, `server_test.go`, `web/app.test.mjs`. |
| A | Rune/output-budget-safe log summarization and aggregates | verified | Production: `internal/observability/log_summary.go` consumed by HTTP, Agent Tool and MCP. Evidence: `TestSummarizeLogsIsRuneSafeAndReportsTruncation`, MCP shared-summary test, Web summary contract test. |
| A | Accessible responsive Web UI, explicit async states, restrained glass/status design | verified | Production: single variable source and degradable glass/status/focus/HITL/feedback/live-region rules in `web/*`. Evidence: `make web-test`; in-app Browser smoke at 1280px and 390px found no horizontal overflow or console errors. |
| B | Rune-safe configurable chunk indexing with complete chunk metadata | verified | Production: `chunker.go`, `KnowledgeRetriever.IndexDocument`, expanded `KnowledgeDocument`. Evidence: CJK/emoji/combining/overlap and independent-Chunk indexing tests. |
| B | Stable `doc_id`, atomic/traceable multi-chunk upload, bulk delete semantics | verified | Production: ES bulk + rollback and DeleteByQuery, controller count contract. Evidence: stable ID, partial rollback, multi-Chunk/idempotent delete tests. |
| B | Concurrent BM25/vector chunk recall, RRF, explicit degradation | verified | Production: `KnowledgeRetriever.recall`, `candidateIdentity` uses chunk_id, degradation fields. Evidence: RRF tests and vector-fallback test. |
| B | Exact/phrase boosts, reranking, per-document diversity limits | verified | Production: exact identifier BM25 boosts, lexical/model rerank, diversification. Evidence: exact-query, lexical/model/fallback and max-per-document tests. |
| B | Detailed score/provenance contract and RAG telemetry | verified | Production: BM25/vector/RRF/rerank/degradation JSON and `pilot_rag_*` metrics plus OTel spans. Evidence: retriever score/fallback tests and all-package build. |
| B | Old-index compatibility or verified migration | verified | Compatible fields are added; vector conflicts fail with explicit action. Evidence: migration-error test and `docs/RAG_MIGRATION.md`. |
| C | Authoritative typed `prompts.yaml` with strict validation | verified | Production consumers all render `internal/prompts/prompts.yaml`; configured path loads at startup and Docker copies it. Evidence: missing/unknown/illegal-variable/load/render tests plus consumer package tests. |
| C | Real wired `ContextPreFetcher` Eino node with bounded parallel safe context | verified | Production: named graph node in `full_agent.go`, bounded Redis/MySQL/episodic/static loader and OTel span. Evidence: parallel, timeout, failure-isolation, cold-context and no-RAG-prefetch tests. |
| C | Knowledge remains on-demand through `search_knowledge` only | verified | Graph has no retriever node or retriever dependency in ContextPreFetcher; only `newAgentTools` registers `search_knowledge`. Evidence: ContextPreFetcher no-RAG test and Full Agent tests. |
| D | Chat and Multi-Agent bounded Redis + MySQL context with cold recovery | verified | Production: Full Agent ContextPreFetcher and Multi-Agent `LayeredSessionContext`. Evidence: MySQL cold recovery, parallel and single-source failure tests. |
| D | Non-nesting short-term summary with injectable summarizer/fallback | verified | Production: independent Redis `:summary` state and recent raw messages, injectable summarizer, deterministic fallback/migration. Evidence: non-nesting summary test and memory package tests. |
| D | Episodic hybrid lexical/vector/recent recall with provenance and fallback | verified | Production: concurrent keyword/vector channels, RRF-style merge, SQL/Recent fallback and `recall_sources`. Evidence: SQLite Recent and HTTP-backed dual-channel provenance tests. |
| D | Stable interaction ID, idempotent feedback API/UI, Up-only promotion | verified | Production: interaction/feedback MySQL models, response header/metadata, API and Web controls. Evidence: SQLite idempotency/eligibility/failure-retention tests and controller/Web contract tests. |
| D | Remove automatic success-memory promotion | verified | Session orchestrator has no recorder path; feedback manager is the sole promoter and enforces Up/success/nonempty status. Evidence: session policy and feedback eligibility tests. |
| E | Rule-default/hybrid fallback and forced Evidence+Knowledge expansion | verified | Production: hybrid low confidence and classifier errors widen roles. Evidence: `hybrid_planner_test.go`. |
| E | Service extraction variants and Chinese expressions | verified | Production: ordered English/Chinese patterns. Evidence: routing table covers `service:`, `service=`, `service:=`, Chinese prefix and suffix forms. |
| E | Parallel evidence/knowledge, bounded replan, limitations, citations | verified | Production shared acquire/assess/replan/audit pipeline. Evidence: orchestrator, evidence graph, partial failure and citation tests. |
| E | Legacy/ADK contract parity including branch timeout | verified | ADK applies the configured branch timeout and shared prepared-plan/evidence gate. Evidence: explicit timeout and legacy/ADK contract-parity tests. |
| E | Static topology is weak prior only; no unobserved propagation | verified | Runtime and tool descriptions now state observed-node-only prior; correlate engine enforces it. Evidence: correlate engine topology tests and evidence-graph tests. |
| E | Unified bounded time window across HTTP/Tool/Multi-Agent/MCP/HITL | verified | `duration_minutes` flows through HTTP context → prepared plan → evidence/correlation/HITL; Tool/MCP use the same 1–1440 bounds. Evidence: controller, MCP, HITL and Multi-Agent tests. |
| F | True checkpoint continuation with CAS state machine and original session | verified | Production: full checkpoint ledger, Redis/memory CAS, prepared-plan reuse, final-result persistence and original-session controller writeback. Evidence: concurrent resume, wrapper restart, reject and prepared-plan tests. |
| F | Accessible persistent Web approval component | verified | `#hitlApproval` replaces browser dialogs and exposes approve/reject buttons/live status. Evidence: Web test and Browser smoke. |
| F | Standard MCP initialize/capabilities/schema/validation/interoperability | verified | Production: initialize, notification, ping, stable schema list, call, invalid params and protocol errors. Evidence: MCP handshake/schema/invalid-param tests and enhanced module script. |
| F | Structural six-tool read-only MCP allowlist and unified log summary/window | verified | Registry panics on names outside the fixed six, always lists all six, and annotates read-only/non-destructive. Evidence: allowlist/schema/log-summary tests. |
| F | Expanded offline/live Agent eval and separate artifacts | verified | Schema v2 reports intent/evidence/corr/citation/fallback/latency/parity; script uses separate offline/live paths and does not swallow live failures. Evidence: offline 3/3 report and `bash -n scripts/verify/eval.sh`. |
| F | Shared `.claude/skills` development standards | verified | Six scoped development Skills include required `pilot-add-react-tool`. Evidence: every folder passed the skill-creator quick validator. |
| F | Required deterministic and Web smoke tests | verified | Acceptance suites, Browser smoke, `go test ./...`, offline eval and `make ci` passed. External Docker/module checks are accurately listed as not run in `docs/FINAL_ACCEPTANCE.md`. |

## Baseline verification record

| Command | Result |
| --- | --- |
| `git status --short --branch` | clean `main...origin/main` at audit start |
| `go test ./...` | passed on 2026-08-26 before implementation changes |
| `node --check web/app.js` | passed on 2026-08-26 before implementation changes |

Passing these baseline commands is not completion evidence for any row above.

## Final deterministic verification record

| Command | Result |
| --- | --- |
| `gofmt` / `git diff --check` | passed |
| `go vet ./...` / `go test ./...` | passed |
| `node --check web/app.js` / `make web-test` | passed; Web tests 3/3 |
| `go run ./cmd/agenteval -mode offline` | passed 3/3; report schema v2 in `artifacts/eval/offline` |
| `make ci` | passed including race/build/tidy |
| `make verify` | external dependency failure; Redis/ES/MySQL/Jaeger unavailable |
