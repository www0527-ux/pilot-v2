# Pilot Agent 深度评估

> 对照 `Agent 深度评估与优化规划` 的基线记录。产品边界：证据绑定的只读运维助手；不承诺代码修复、自治写操作或未接线能力。

## 1. 模块成熟度

| 模块 | 成熟度 (1–5) | 说明 |
|------|-------------:|------|
| Multi-Agent Orchestrator | 4 | legacy/ADK 并行分支、limitations、受限扩窗 replan、证据图门禁 |
| RulePlanner | 3 | 可预期、可测；关键词脆，易漏开 Evidence |
| HybridPlanner + Intent（规划落地后） | — | 规则兜底 + LLM 分类；默认 `planner.mode=rule` |
| Chat FullPilotAgent | 4 | 按需 RAG、记忆与失败可见化，已有独立单测 |
| Tools / Observability | 4 | 真数据源；可配置时间窗；MAD/lead-lag 关联；topology 为弱先验 |
| RAG Retriever | 4 | BM25+向量+RRF，可降级 BM25 |
| Memory | 4 | Redis + MySQL 历史 + episodic sql/hybrid（ES 向量召回，可降级） |
| Evidence Graph / Verifier | 4 | provenance DAG、来源 quorum、反证、去重、严格 ID 引用与弃权；待真实事故校准 |
| Observe (LLM/tool metrics) | 4 | OTel GenAI agent/model/tool/embedding spans + Prometheus；默认不采内容 |
| Eino ADK 采用度 | 4 | `runtime=adk` 使用 ADK ParallelAgent 且与 legacy 共用可信门禁；默认 legacy |

## 2. Planner 问法集基线（规则）

金丝雀集见 `internal/ai/multiagent/testdata/intent_cases.json`。规则基线预期：

| Intent | 典型漏检风险 |
|--------|----------------|
| `incident_triage` | 改写句无「故障/排查」等词时落到 `general_analysis` 且跳过 Evidence |
| `observability_query` | 口语「怎么回事」无监控关键词 |
| `knowledge_query` | 「怎么配」类问法可能误进 general |
| service 抽取 | 仅 `service:=` / `service=`；自然语言服务名常丢失 |

优化后关注指标：**漏开 Evidence 率**、intent 准确率、service 命中率（相对本金丝雀集）。

## 3. 与社区模式差距

| 模式 | Pilot 差距 | 采纳结论 |
|------|-------------|---------|
| Magentic-One Ledger / Replan | 已有结构化执行账本 | 按证据充分度最多 1 轮扩窗重规划 |
| Context Engineering | 双重 RAG + 硬截断 | 按需工具检索 + 摘要压缩 |
| Verifier-Critic | 无 claim-level NLI | 已有证据 DAG、引用完整性、反证与 selective abstention；语义蕴含仍待评测 |
| 分层 Memory | 无跨会话事故记忆 | 情景记忆最小集 |
| Eino ADK Plan-Execute / HITL | ParallelAgent + checkpoint HITL 已落地；未采用通用 planexecute | 当前角色计划更可审计；动态长任务再评估 planexecute |
| MCP | 只读 facade 已落地 | 保持只读；第三方 MCP 入站市场仍延后 |
| CodeAct / Swarm / Debate | — | **不做** |

## 4. 不做清单

- CodeAct、Computer Use、写操作/代码执行工具
- Swarm / Blackboard / Debate / Voting
- 独立意图微服务或 Chat 前置意图 HTTP
- 伪多租户（共享 API Key 扩成租户 IAM）
- 将意图分类结果当作 Findings 证据

## 5. 落地对照

| Phase | 内容 | 文档/代码落点 |
|-------|------|----------------|
| 0 | 本评估 | `docs/AGENT_EVAL.md` |
| 1 | Intent / Replan / Verifier / RAG / 卫生 | `internal/ai/multiagent/*`、`agent/`、`config` |
| 2 | Eino spike、情景记忆、双模型 | `docs/EINO_ADK_SPIKE.md`、`memory/episodic*` |
| 3 | MCP / HITL 落地 | `docs/MCP_HITL_EVAL.md`、`internal/mcp`、`multiagent/hitl.go` |
| 4 | Epistemic evidence graph / adaptive replan / GenAI telemetry | `docs/EVIDENCE_GRAPH.md`、`multiagent/evidence_graph.go` |
