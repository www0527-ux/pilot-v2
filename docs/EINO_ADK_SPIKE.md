# Eino ADK Spike

## Current state

Pilot depends on `github.com/cloudwego/eino v0.9.13` (latest stable checked 2026-08-08) and now optionally uses ADK:

- Chat path remains `compose.Graph` + `flow/agent/react`
- Multi-agent default runtime: `legacy` (`Orchestrator`)
- Multi-agent ADK runtime: `agent.multi_agent.runtime=adk` → `ADKOrchestrator` wraps Evidence∥Knowledge with `adk.NewParallelAgent` + `adk.NewRunner`, then reuses ModelSynthesizer + evidence-ID verifier
- Legacy 与 ADK 现在共享 provenance evidence graph、来源 quorum、反证、扩窗 replan 和严格 citation audit；切换运行时不会绕过可信门禁

## Compatibility notes

1. ADK package is present in Eino v0.9.13 (`adk`, `adk/prebuilt/planexecute`, supervisor and deep-agent patterns); Pilot deliberately keeps its incident plan deterministic and auditable.
2. Role executors are bridged via thin `adk.Agent` adapters that emit JSON findings as assistant messages.
3. If ParallelAgent construction fails, ADK path falls back to legacy branches with limitation `ADK_PARALLEL_FALLBACK`.
4. HITL checkpointing for multi-agent HTTP uses Pilot's Redis/Memory store; ADK Runner checkpoint store can be layered later for deep interrupt nesting.

## Decision

- Keep Chat on current ReAct until streaming+callback parity is measured under ADK `ChatModelAgent`.
- Default production/runtime remains `legacy`; enable `adk` behind config for eval comparison (`make docker-eval` / live canaries).

## Checklist

- [x] Prototype Evidence∥Knowledge with ADK Parallel
- [x] Feature flag `runtime=legacy|adk`
- [ ] Compare latency/token vs legacy on live canary set with DashScope credentials
- [ ] Optional: adopt `planexecute` for dynamic step lists beyond current role plan
- [x] Keep evidence verification and OpenTelemetry `invoke_agent`/`plan` spans consistent across legacy and ADK
