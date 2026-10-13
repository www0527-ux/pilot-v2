# 旧版架构文档（归档）

本文件保留迁移前的说明。旧多 Agent 路由、四路历史预取、反馈晋升和 HITL 运行时不再属于当前控制台主链路；运行方式及 API 请以根目录 README 和 docs/ADK_EXPERIENCE_GUIDE.md 为准。

# Pilot

Pilot 是一个使用 Go、Gin 和 Eino 构建的 AI 运维助手。它只暴露已接线到真实依赖的数据能力：Chat、RAG、多 Agent 排障，以及 Prometheus、Elasticsearch 和 Jaeger 的可观测性查询。它不承诺代码修复、用户画像、任意外部访问或其他未实现的能力。

## 能力与边界

- 同步 Chat 与 SSE 流式 Chat，带 Redis 短期记忆和 MySQL 会话历史。
- Chunk RAG 知识库：Unicode/emoji/组合字符安全切块，每个 Chunk 独立 Embedding；BM25 与向量并行召回、按 chunk_id RRF 融合、exact ID boost、词法/模型重排与单文档多样性限制；降级会在结果中显式返回。
- 多 Agent 排障：分诊、运行证据、知识检索和综合结论分支独立执行，失败会在 limitations 中显式返回。
- 跨信号关联（启发式）：`correlate_signals` / `POST /api/v1/observability/correlate`，产出可引用的 `corr-*` 证据（并发采集 + robust MAD + 时间桶/lead-lag + “仅已观测节点”拓扑先验，不是黑盒因果模型）。
- Epistemic 证据图：把告警、日志、指标、知识和派生假设建成 provenance DAG；按独立来源、MAD 异常、lead/lag、反证与重复依赖计算“证据充分度”（不是根因概率），不足时自动扩窗重规划，预算耗尽则明确弃权。
- 严格引用审计：零可用证据时直接跳过模型综合并弃权；有证据时回答只能引用图中真实存在的方括号证据 ID，缺引用或伪造 ID 会被 Verifier 拒绝并退回确定性摘要。`legacy` 与 `adk` 两条运行时共享同一门禁。
- 情景记忆：`memory.episodic.mode=sql|hybrid`；hybrid 使用 ES 向量+文本召回，失败降级 SQL；召回明确标记为 memory，不进入 runtime Verifier 证据集。
- 运行时双轨：核心框架已升级到 Eino `v0.9.13`；`agent.multi_agent.runtime=legacy|adk`，adk 使用 Eino ADK `ParallelAgent` 执行 Evidence∥Knowledge。
- HITL（默认关）：宽时间窗可 interrupt，checkpoint 以 CAS 状态机保存 plan/步骤/证据/下一阶段并可跨重启续跑；Console 使用持久审批组件。
- 标准只读 MCP：`cmd/mcpserver`（`mcp.enabled=true`），支持 initialize/capabilities、inputSchema、tools/list 与 tools/call，结构上仅有六类只读工具。
- 每次回答都有稳定 interaction ID；`POST /api/v1/chat/feedback` 幂等保存 Up/Down。只有 Up 且非 interrupted/rejected/abstain 的非空成功结果可晋升情景记忆。
- 可观测性数据来自 Prometheus、Elasticsearch 日志和 Jaeger，不使用 mock 数据兜底。
- Eino 的模型和工具调用会产生 OpenTelemetry 子 span 和 Prometheus 指标；Embedding 同样产出 `pilot_embedding_*` 指标。请求提示词、工具参数、工具结果和嵌入原文不会写入这些指标或 span 属性。
- Agent、规划、角色、模型、工具和 Embedding span 使用 OpenTelemetry GenAI 语义字段（`invoke_agent` / `plan` / `chat` / `execute_tool` / `embeddings`），并继续默认不采集内容载荷。
- Prometheus 额外暴露 `pilot_agent_runs_total{runtime,decision}`、`pilot_agent_evidence_confidence` 与 `pilot_agent_replans_total`，可直接观察接受、降级、弃权和重规划比例。
- Agent 的每次执行受 agent.timeout 约束；全量 Chat ReAct 使用 agent.max_iterations，多 Agent 的受限 ReAct 证据采集使用 agent.multi_agent.react_max_steps。
- ARM Mac 全栈验证：`make docker-up` / `make docker-e2e` / `make docker-module MODULE=base|corr|episodic|eval|hitl|mcp`。

核心依赖（Redis、Elasticsearch、MySQL、主模型、Embedding）缺失时启动失败。可选能力（OTLP trace 导出、运行日志写入）可在 `server.allow_degraded=true` 时降级启动；健康接口会标明 `degraded`。这不是“半残服务假装全好”，核心业务仍要求真实依赖。

生产边界（默认关闭，适合本地同域前端）：

- `security.auth`：共享服务 API Key（`Authorization: Bearer` 或 `X-API-Key`），不是多租户用户体系；健康检查与 `/metrics` 豁免。
- `security.cors`：空列表表示同域，不注入 CORS 头；跨域时显式配置来源。
- `security.rate_limit`：按客户端 IP 的每分钟固定窗口限流。

## 环境要求

- Go 1.24.10 或更高版本。
- Docker Desktop 与 Docker Compose，用于本地 Redis、Elasticsearch、MySQL、Prometheus、Jaeger 和 Grafana。
- DashScope API Key，用于主模型和 embedding。
- 可写的本地 Docker 卷空间；Elasticsearch 至少需要约 512 MB JVM 堆。

本地 Compose 是单机演示编排：端口和 container_name 固定，不适合多个并行实例或生产部署。Apple Silicon 建议配合 `manifest/docker/docker-compose.arm.yml`（`make docker-up` 默认启用），并将 ES 堆控制在约 384–512MB。

### Docker 全栈（含 Pilot 镜像）

~~~bash
make docker-up      # 构建 linux/arm64 Pilot 镜像并启动 app profile
make docker-e2e     # 容器内 e2echeck
make docker-module MODULE=corr
make docker-eval    # offline 金丝雀 + 可选 live
~~~

本机 `go run` 仍用 `make up`（仅依赖）+ `make run`；Prometheus 同时抓取 `host.docker.internal:8080` 与 `pilot:8080`。

## 快速开始

1. 创建并填写本地环境文件。

~~~bash
cp .env.example .env
~~~

至少设置 DASHSCOPE_API_KEY，并将 MYSQL_ROOT_PASSWORD 改为仅供本地开发使用的密码。

2. 启动真实依赖并等待连通性检查完成。

~~~bash
make start
~~~

make start 会运行 Docker Compose，轮询 Redis、Elasticsearch、MySQL、Prometheus、Jaeger 和模型密钥检查，全部通过后才启动 API。

Windows 原生 CMD 或 PowerShell 使用：

~~~cmd
scripts\start.bat
~~~

Git Bash 和 WSL 可以使用 make start。启动脚本保留外部提供的 PILOT_CONFIG；未提供时使用仓库内默认配置。

3. 打开服务。

- 前端：http://localhost:8080/
- 存活探测：http://localhost:8080/api/v1/health/live
- 就绪探测：http://localhost:8080/api/v1/health/ready
- 兼容健康：http://localhost:8080/api/v1/health（含 ready/degraded 摘要）
- Prometheus 指标：http://localhost:8080/metrics
- Grafana：http://localhost:3000/，默认账号 admin / admin
- Jaeger：http://localhost:16686/

## 配置

配置路径优先级为命令行 -config、环境变量 PILOT_CONFIG、默认 manifest/config/config.yaml。根目录 .env 会在读取配置前加载。

~~~bash
PILOT_CONFIG=/path/to/pilot.yaml make run
go run ./cmd/main.go -config /path/to/pilot.yaml
~~~

YAML 以严格模式解析，未知字段会使启动失败。环境变量占位符使用 &#36;{ENV_VAR}；未设置的变量会展开为空字符串，随后由实际消费方或依赖检查报错，不能将空密钥视为可用默认值。

关键配置项：

~~~yaml
server:
  address: ":8080"
  env: "local"
  base_url: "http://localhost:8080"
  allow_degraded: false

security:
  auth:
    enabled: false
    api_key: "${PILOT_API_KEY}"
  cors:
    allowed_origins: []
  rate_limit:
    enabled: false
    requests_per_minute: 120

agent:
  max_iterations: 25
  timeout: 300
  multi_agent:
    enabled: true
    evidence_mode: "deterministic" # deterministic 或 react
    react_max_steps: 8
    evidence_window_minutes: 30
    branch_timeout_seconds: 120
    max_replan_rounds: 1
    verification:
      mode: "evidence_graph" # evidence_graph 或 off
      min_confidence: 0.65   # 证据充分度阈值，不是模型自报概率
      min_independent_sources: 2
      replan_window_multiplier: 2
    planner:
      mode: "rule" # rule 或 hybrid
      min_confidence: 0.6
      timeout_ms: 3000
~~~

知识切块、候选扩张、重排和唯一 Prompt 文件也由严格配置消费：

~~~yaml
rag:
  chunk_size: 800
  chunk_overlap: 120
  candidate_multiplier: 3
  max_chunks_per_doc: 2
  reranker_mode: lexical # lexical 或 model
prompts:
  path: internal/prompts/prompts.yaml
~~~

evidence_mode 为 deterministic 时使用确定性证据采集。为 react 时，模型只能调用 query_alerts、query_logs 和 query_metrics 三个只读工具；没有受限工具调用的模型文本不会被当作运行证据。ReAct 失败时会回退到确定性采集，并在返回 metadata 中标明 deterministic_fallback 和失败原因摘要。planner.mode 为 hybrid 时使用 LLM 意图分类并与规则合并；默认 rule 保持原行为。

verification.mode 为 evidence_graph 时，事件分支需要满足独立运行数据源数量与证据分数阈值。关联假设、相同 trace 和重复内容会保留在图中，但不会被重复计算成独立来源；反证按 claim family 连接，例如健康 `up=1` 只反驳 availability 假设，不会误伤 latency/error/saturation 假设。若首轮不足，唯一一次 replan 会按 `replan_window_multiplier` 扩大时间窗和日志上限。分数只表达当前证据覆盖度，不能解释成“根因有 65% 概率正确”。

## 可信排障闭环

~~~text
Alerts ─┐
Logs ───┼─ 并发采集 ─ MAD 异常 + 时间桶/lead-lag ─ provenance evidence graph
Metrics ┘                                              │
                                                       ▼
                                      独立来源 quorum + 反证 + 去重
                                         │ sufficient      │ insufficient
                                         ▼                 ▼
                                  evidence-bound       bounded 扩窗 replan
                                    synthesis              │
                                         │                 └─ budget exhausted → abstain
                                         ▼
                              exact citation audit → accept / deterministic fallback
~~~

这里的难点不是再增加一个会说话的 Agent，而是限制“派生信息冒充独立证据”：correlation hypothesis、同 trace 记录和重复采样都有显式边；拓扑只给已经观测到的依赖节点加先验，不再把主服务分数传播到没有任何信号的依赖服务。API 的 `verification.assessment` 给出 score、band、stop_reason、runtime_sources 和 contradiction_count，`evidence_graph` 给出不重复原始日志文本的 nodes/edges。

算法、评分公式、弃权语义和已知边界见 [docs/EVIDENCE_GRAPH.md](docs/EVIDENCE_GRAPH.md)。
旧知识索引到 Chunk schema 的显式迁移步骤见 [docs/RAG_MIGRATION.md](docs/RAG_MIGRATION.md)。

## API 契约

除 SSE 已建立连接后的事件外，所有 API 使用统一 JSON envelope。

~~~json
{
  "success": true,
  "code": "OK",
  "message": "对话成功",
  "data": {}
}
~~~

失败响应不返回内部错误详情：

~~~json
{
  "success": false,
  "code": "INVALID_ARGUMENT",
  "message": "请求参数不合法"
}
~~~

| 错误码 | HTTP 状态 | 含义 |
| --- | ---: | --- |
| INVALID_ARGUMENT | 400 | 请求体、查询参数或过滤条件不合法 |
| UNAUTHENTICATED | 401 | 缺少或错误的 API Key |
| NOT_FOUND | 404 | 资源或 API 路由不存在 |
| FEATURE_DISABLED | 404 | 对应功能被配置关闭 |
| RATE_LIMITED | 429 | 触发按 IP 限流 |
| UPSTREAM_ERROR | 502 | 模型或 Agent 上游调用失败 |
| DEPENDENCY_UNAVAILABLE | 503 | Redis、Elasticsearch、MySQL 或观测数据源不可用 |
| INTERNAL | 500 | 未分类的服务端异常 |

同步 Chat 的规范路径是 POST /api/v1/chat；POST /api/v1/chat/ 只为历史客户端兼容。

~~~bash
curl -sS -X POST http://localhost:8080/api/v1/chat -H "Content-Type: application/json" -d '{
    "user_id": "user-1",
    "session_id": "session-1",
    "query": "排查 pilot-api 最近的错误"
  }'
~~~

SSE 路径是 POST /api/v1/chat/stream，统一使用任务事件协议，不需要 `event_version` 或 `include_progress`。每块包含 `event: agent`、`id: run_id:sequence` 和 JSON `data`。正文由 `kind=answer,status=delta,text=...` 传输；执行状态、工具事件、回答开始与结束分别传输。整轮终止时发送 `kind=run` 的终止状态，再发送 `data: [DONE]`；客户端必须检查终止状态，不能仅凭 `[DONE]` 判断成功。建立任务前的参数或依赖错误仍返回普通 JSON envelope。旧的纯文本 SSE 协议已删除，完整协议与断线续接见 [docs/AGENT_CHAT_UI.md](docs/AGENT_CHAT_UI.md)。

| 方法 | 路径 | data 内容 |
| --- | --- | --- |
| GET | /api/v1/health | status（ok/degraded/not_ready）、ready、checks、degraded |
| GET | /api/v1/health/live | 进程存活 |
| GET | /api/v1/health/ready | 核心依赖就绪；失败返回 503 |
| POST | /api/v1/chat | 回答文本 |
| POST | /api/v1/chat/stream | SSE 分片 |
| POST | /api/v1/chat/multi-agent | answer、plan、steps、findings、limitations、evidence_graph、verification |
| GET / DELETE | /api/v1/chat/history | 查询或清理 user_id 与 session_id 的历史 |
| POST | /api/v1/knowledge/upload | 稳定 doc_id 与 chunk_count |
| POST | /api/v1/knowledge/search | 相关 Chunk 列表（兼容 KnowledgeDocument 外形） |
| POST | /api/v1/knowledge/search/detailed | BM25/vector/RRF/rerank 分数、方法与降级状态 |
| DELETE | /api/v1/knowledge?id=... | 按 doc_id 删除全部 Chunk，返回 deleted_count/existed |
| POST | /api/v1/chat/feedback | interaction_id 的幂等 Up/Down 反馈与晋升状态 |
| POST | /api/v1/observability/alerts | 告警列表 |
| POST | /api/v1/observability/metrics | items 和 summary |
| POST | /api/v1/observability/logs | items 和 summary |
| POST | /api/v1/observability/overview | summary、alerts、metrics、logs |

可观测性查询请求示例：

~~~bash
curl -sS -X POST http://localhost:8080/api/v1/observability/logs -H "Content-Type: application/json" -d '{
    "service": "pilot-api",
    "level": "error",
    "duration_minutes": 60,
    "limit": 20
  }'
~~~

## 验证与质量门禁

无需外部服务的质量门禁：

~~~bash
make ci
~~~

该目标依次执行格式检查、go vet、全包构建、race 检测和 go mod tidy -diff。GitHub Actions 会在 push 和 pull request 上运行同一组目标。

真实依赖验证需要先启动服务：

~~~bash
make up
make verify
make run
# 在另一终端执行
make e2e
~~~

make e2e 不只是检查 HTTP 200；它会验证健康 envelope、上传后的知识文档检索、Chat 返回值、Redis 和 MySQL 至少两条对话记录、Elasticsearch 运行日志、Prometheus 请求指标以及 Jaeger trace。该检查会写入带时间戳的测试数据，因此只应对本地或隔离环境运行。

常用命令：

~~~bash
make health
make seed-logs
make ps
make logs
make down
~~~

## 项目结构

~~~text
pilot/
├── cmd/                    # API、依赖检查、E2E 与演示数据入口
├── internal/
│   ├── ai/                 # Eino Agent、RAG、工具、回调观测、多 Agent
│   ├── config/             # 严格类型化配置
│   ├── controller/         # HTTP 控制器
│   ├── health/             # liveness / readiness
│   ├── httpapi/            # 统一 JSON/SSE 契约
│   ├── security/           # API Key / CORS / 限流
│   ├── logging/            # slog 初始化
│   ├── memory/             # Redis 短期记忆与 MySQL 历史
│   └── observability/      # 指标、日志、trace 客户端与运行时
├── manifest/               # 配置和本地 Compose 编排
├── pkg/                    # 基础客户端与共享类型
├── web/                    # 同域托管的前端
└── docs/                   # 证据图、ADK、评测与重构设计记录
~~~

## 当前非目标与后续风险

共享服务 API Key / CORS / IP 限流已实现（默认关闭，见 `security.*`）；这不是多租户 IAM。Embedding 有独立 OTel span 与 `pilot_embedding_*` 指标，但不走 Eino graph callbacks。浏览器交互和完整 Docker + DashScope 链路需要在具备真实凭据的环境中执行 make e2e 后再人工验收。Multi-Agent 分诊默认 `planner.mode=rule`；可配置 `hybrid` 启用 LLM 意图分类（失败回退规则）。

