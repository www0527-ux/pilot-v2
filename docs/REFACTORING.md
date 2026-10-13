# Pilot 重构：进展与待办

> 当前分支：main
>
> 目标：将同一套产品收敛为无虚假能力、配置诚实、API 契约统一、依赖可注入、错误可预期、带 CI 门禁的 Pilot；Chat、RAG、Multi-agent 与真实可观测性数据源路径持续可用且可测。

## 已完成的增量

### C1 321b167：删除虚假能力与死代码

- 删除只返回“功能开发中”的 analyze_code 与 generate_code，不再在系统提示词中承诺代码修复、用户画像或 IDE 能力。
- 移除未调用 mock 数据源、SimplePilotAgent、PromptOptimizer、用户画像和未接线的配置块。
- agent.max_iterations 实际接入 ReAct 的 MaxStep。

### C2 279374d：类型化配置、依赖注入与 slog

- 新增 internal/config，严格 YAML、环境变量展开、默认值和启动期校验。
- 配置路径固定为 -config > PILOT_CONFIG > manifest/config/config.yaml。
- Redis、Elasticsearch、MySQL 改为可注入客户端；控制器依赖收敛为窄接口，装配集中在 cmd/main.go。
- 全链路使用 context，日志统一为 slog，logger.level 已真实生效。

### C3 c8c4d73：GoFrame 迁移到 Gin 与 API 契约统一

- 移除 GoFrame，路由和中间件迁移到 Gin。
- 普通 API 使用 success、code、message、data envelope；错误码映射到稳定 HTTP 状态。
- SSE 规范为数据分片、流中错误事件和 [DONE] 结束标记。
- 增加 panic 恢复、API 404 envelope、优雅退出和历史 Chat 尾斜杠兼容。
- 控制器契约测试使用 Gin httptest 和假依赖，不依赖外部服务。

### C4：Eino Agent 能力与可观测性

C4 由三个独立提交组成：2a8e2fd、d020691、d0cbf8d。

- 工具构造函数由具体 DataCollector 改为窄数据源接口，构造失败改为返回错误并在 Agent 组装处聚合，消除 panic 路径。
- 工具支持服务过滤条件，告警、日志、指标、trace、拓扑和知识源均可注入测试替身。
- 新增 internal/ai/observe：Eino 模型与工具 callbacks 写入 OpenTelemetry 子 span 和 Prometheus 指标，注册过程为 once；不记录 prompt 或工具载荷。
- 多 Agent 综合由 Eino compose.Chain 执行，使模型调用进入 callbacks。
- 新增受限 ReAct 证据执行器。它只注册 query_alerts、query_logs、query_metrics，按单次执行记录真实工具回调；没有受限工具调用的模型文本不能作为证据。
- React 失败时回退确定性证据执行器，返回 metadata 中标明 deterministic_fallback 和截断后的失败摘要。配置新增 evidence_mode 与 react_max_steps，默认仍是确定性模式。
- React、工具依赖注入、回退、上限和 callback 可观测性都有单元测试。

### C5 31c436e：CI 门禁与本地真实验证

- 新增 GitHub Actions，push 和 pull request 执行格式检查、go vet、全包构建、race 测试和 go mod tidy -diff。
- Makefile 新增 fmt、fmt-check、vet、build-check、test-race、tidy-check 和 ci；ci 与 GitHub Actions 使用同一质量门禁。
- make start 与两个启动脚本不再假设固定 sleep，而是调用 checkdeps 轮询真实 Redis、Elasticsearch、MySQL、Prometheus、Jaeger 和模型密钥状态。
- Windows 脚本不覆盖外部 PILOT_CONFIG，且 Docker Compose 启动失败会立即结束。
- Compose 中 Prometheus 与 Grafana 不再使用 latest 标签。
- E2E 检查不再只看 HTTP 2xx：健康检查验证 envelope，知识上传验证 data.id，Chat、检索和历史验证 OK code，Redis/MySQL 至少两条对话记录，Jaeger 至少一个 span，并使用配置中的 service_name 查询日志。
- E2E probe 的 envelope 校验有独立单元测试。

### C6：README 与状态文档对齐

- README 已改为 Go + Gin + Eino，说明真实依赖、配置优先级、统一 JSON/SSE 契约、错误码、多 Agent 的受限 ReAct 行为与质量门禁。
- 移除了已失实的 GoFrame、固定睡眠、缺失 Windows 脚本和未提供 Swagger/OpenAPI 的表述。
- 明确标注本地 Compose、鉴权和真实环境验收的边界，避免将规划中的能力写成当前能力。

### C10：正确性债收敛（告警过滤 / 客户端拆分 / 记忆写入 / Embedding span）

- 修复 Prometheus GetAlerts 过滤：labels 与 status 统一由 `matchAlertFilters` 判定，消除失败分支中冗余且语义不清的 status 检查；补充表驱动与 httptest 单测。
- 将原 `clients.go` 拆为 `prometheus_client.go` / `log_client.go` / `trace_client.go` + `client_helpers.go`。
- Chat / Stream / MultiAgent 记忆写入收敛到 `persistTurn`；短期记忆失败改为 Warn 日志，不再 `_ =` 静默丢弃；FullPilotAgent 同步改为可观测的最佳努力写入。
- `embedder.Embed` 增加 OpenTelemetry 子 span（模型名与维度，不记录原文），补齐 C9 中可本地完成的观测缺口。

### C11：生产边界、就绪探测与 Embedding 指标

产品边界已显式编码，而不是“默认全开假装安全”：

- **鉴权**：`security.auth` 共享服务 API Key（Bearer / X-API-Key）；本地默认关闭；健康与 `/metrics` 豁免。
- **CORS**：仅当 `allowed_origins` 非空时注入；空列表表示同域部署。
- **限流**：按客户端 IP 的每分钟固定窗口；默认关闭。
- **降级启动**：`server.allow_degraded` 仅允许 OTLP 导出与运行日志写入失败时继续；Redis/MySQL/ES/模型仍必须可用。
- **探测分离**：`/health/live` 存活、`/health/ready` 核心依赖就绪（失败 503）、`/health` 兼容摘要（ok/degraded/not_ready）。
- **Embedding 指标**：`pilot_embedding_requests_total{model,status}` 与 `pilot_embedding_request_duration_seconds{model}`，与 LLM/tool 同级登记。

## 本轮验证记录

已在本机执行并通过：

~~~text
go test ./internal/security/ ./internal/health/ ./internal/observability/ ./internal/controller/ ./internal/config/ ./internal/httpapi/ ./internal/ai/observe/ ./cmd/e2echeck -count=1
make ci
go mod tidy -diff
bash -n scripts/start.sh
MYSQL_ROOT_PASSWORD=check-only docker compose -f manifest/docker/docker-compose.yml config -q
~~~

C10/C11 相关包测试已覆盖。make ci 包含格式检查、go vet、go build、go test -race 和 tidy 检查。macOS race 链接阶段会输出 Command Line Tools 的 LC_DYSYMTAB 警告，但所有包测试通过；这是当前宿主工具链警告，未观察到 Go 测试失败。

## 尚未伪装为完成的验证

以下验证需要真实 Docker 服务和 DashScope 凭据，本次未在共享开发环境运行：

1. 启动 make up，执行 make verify。
2. 启动 API 后执行 make e2e，确认真实写入 Redis、MySQL、Elasticsearch、Prometheus 和 Jaeger。
3. 在浏览器中人工回归对话、可观测性和知识库三个页面。
4. 检查 Grafana provisioning 面板与新增加的 Eino 指标展示。

## 后续工作与风险

### 多租户与网关边界

当前鉴权是共享服务 API Key，不是用户/租户 IAM。若需要按组织隔离知识库与会话，应在网关或独立认证模块定义主体与租户模型后再接入，不能把单密钥扩成伪多租户。

### 更细粒度降级

Chat/知识库路径仍要求核心依赖启动成功。若未来要对 Prometheus/Jaeger 查询做启动期探测并局部关闭 observability API，需要为对应路由返回稳定的 FEATURE_DISABLED / DEPENDENCY_UNAVAILABLE，并更新前端能力开关。

### 真实环境验收

- E2E 会写入带时间戳的文档和会话，适合隔离环境；后续可把清理流程和并发唯一标识纳入 probe。
- 浏览器和真实依赖链路没有被 CI 覆盖，CI 的职责仅限不依赖外部服务的回归门禁。
- 本地 Compose 的固定端口和 container_name 使其不适合多实例或生产环境，应由后续部署清单取代。

## 合入原则

后续变更继续遵循小模块、可独立评审、每个模块先测试再提交的方式。对任何新增配置、API 或 Agent 工具，必须同时补齐真实消费方、契约测试和失败语义；没有真实数据源或实现路径的能力不进入接口、前端或文档。

## Next-Gen Wave（M1–M8）

- **M1**：`Dockerfile` + Compose `pilot`/`e2e` profiles + `config.docker.yaml` + `scripts/verify/*` + `make docker-*`；Prometheus 双目标（host + compose）。
- **M2**：`internal/observability/correlate` 启发式共现引擎；工具 `correlate_signals`；API `POST /api/v1/observability/correlate`；证据合并进 multi-agent。
- **M3**：情景记忆 `sql|hybrid`（ES 向量召回，失败降级 SQL）；Session 注入标记 `memory/not_runtime_evidence`。
- **M4**：`cmd/agenteval` + `testdata/eval/canaries.json`；`make docker-eval`。
- **M5**：`agent.multi_agent.runtime=adk` 走 Eino ADK `ParallelAgent`（Evidence∥Knowledge），默认 `legacy` 可回滚。
- **M6**：HITL `ThresholdGate` + Redis/Memory CAS checkpoint；`/chat/multi-agent/resume` 从准备好的 plan 继续；Console 持久审批组件。
- **M7**：标准只读 `cmd/mcpserver`（initialize/capabilities、稳定 tools/list inputSchema、参数校验、tools/call）。
- **M8**：Epistemic evidence graph：MAD/lead-lag 时序假设、反证边、独立来源 quorum、扩窗 replan、严格引用审计、selective abstention；legacy/ADK 同门禁；OTel GenAI agent spans。

## C12：Agent 深度评估落地（Intent / Replan / Context / Memory）

对照 `docs/AGENT_EVAL.md`：

- Multi-Agent：`intent/` + `HybridPlanner`（`planner.mode=rule|hybrid`，默认 rule）；单轮 Progress 重规划；证据 ID Verifier；分支超时；部分证据失败升 Limitation。
- Chat：去掉 Graph 预取双重 RAG，仅 `search_knowledge` 按需检索；`full_agent` 单测；检索失败可见化辅助保留。
- 工具：`duration_minutes` 可配置；topology/anomaly 描述诚实化。
- 记忆：短期溢出摘要；`episodic_incidents` 情景记忆；SessionOrchestrator 注入会话摘要。
- 双模型：`llm.evidence_model`（可选）；Eino ADK / MCP-HITL 见 `docs/EINO_ADK_SPIKE.md`、`docs/MCP_HITL_EVAL.md`。

## C13：可信证据图与自适应排障闭环

- 新增 `internal/ai/multiagent/evidence_graph.go`：不复制证据正文的 provenance nodes/edges；显式表示 `supported_by`、`contradicted_by`、`same_trace`、`duplicates`。
- 证据充分度不采用 LLM 自报 confidence；按独立运行来源、可靠性先验、去重密度、派生假设和反证计算。情景记忆与 correlation 不冒充独立 runtime source。
- legacy 与 Eino ADK 共享同一 acquire→assess→replan→synthesize→audit 状态机；不足时按配置扩大时间窗，预算耗尽返回低置信限制并弃权。
- correlation 升级为三源并发采集、MAD robust anomaly、lead/lag、健康 availability 反证；静态拓扑只增强已有观测节点，不再凭空传播出依赖假设。
- 引用 Verifier 要求方括号内精确 evidence ID；缺引用或幻觉 ID 均回退确定性摘要。
- Session context 改为 request context 传递，消除共享 Orchestrator 在并发用户间串摘要的竞态。
- OpenTelemetry 增加 `invoke_agent`、`plan`、角色、`chat`、`execute_tool`、`embeddings` GenAI 语义 span；不采集 prompt/工具载荷/日志正文。
- `cmd/agenteval` 报告新增 confidence、decision、stop reason、contradictions、citation audit 和 evidence graph nodes；详细设计见 `docs/EVIDENCE_GRAPH.md`。
