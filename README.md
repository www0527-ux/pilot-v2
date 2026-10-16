# Pilot

**基于 Go 与 Eino ADK 的智能排障与经验知识库。**

Pilot 将日志、指标、调用链查询与技术文档检索接入排障 Agent，支持多轮分析、流式回答和执行过程查看。排障记录按问题持续跟踪，由独立整理 Agent 生成经验草稿，人工审核发布后供后续会话检索。

项目重点是 Agent 的工程化：如何约束工具、组织检索上下文、管理长任务，以及将多轮排障记录沉淀为可审核的知识。当前定位为个人开发与本地演示项目，未提供生产规模或检索效果提升的量化结论。

## 核心功能

- **工具调用式排障**：按需查询 Elasticsearch 日志、Prometheus 告警与指标、Jaeger 调用链，并进行跨信号关联分析；执行过程与最终回答分开呈现。
- **技术文档 RAG**：上传 Markdown 指南，结构感知切分后进行关键词与向量混合检索，补回命中章节的相关上下文。
- **多轮对话**：原始历史与模型工作上下文分别保存，复用摘要中间件控制上下文增长。
- **任务生命周期**：持久化 AgentRun 与事件，支持取消、重复请求识别、SSE 断线后的事件回放。
- **Issue 跟踪与经验沉淀**：一个会话可跟踪多个交错问题，支持人工修正归属、手动整理和解决后自动整理；经验可编辑、审核、修订、归档与恢复发布。

## 技术栈

| 分类 | 技术与用途 |
| --- | --- |
| 后端与 Agent | Go 1.24.10+、Gin、Eino v0.9.13 ADK、Tool Calling |
| 数据存储 | MySQL：历史、工作上下文、Run、Issue、任务及经验版本；Redis：界面历史缓存 |
| 文档检索 | Elasticsearch 8.12：BM25、dense_vector / HNSW kNN；项目实现 RRF 融合、重排与上下文预算 |
| 可观测性 | Prometheus、Jaeger、OpenTelemetry、Elasticsearch 日志 |
| 交互与部署 | SSE、原生 JavaScript、Docker Compose |

**两类知识库分别管理：**技术文档库保存在 ES，使用 `search_knowledge` 检索；排障经验库保存在 MySQL，使用 `search_experience` 检索当前账号审核通过的发布版。经验库目前采用关键词及服务过滤，不使用向量召回。

## 核心技术设计

**Agent 与工具约束。** 外层 Graph 依次准备上下文、组装消息并进入 ADK。Eino 提供模型与工具接口、决策循环、事件和摘要中间件；Pilot 实现工具参数校验、查询范围及结果体积限制、账号范围传递与可恢复错误反馈。流式路径在工具调查后另做无工具模型调用，逐步输出最终回答，而非将调查中的每轮文本拼成结论。[主 Agent](internal/ai/agent/full_agent.go) · [工具实现](internal/ai/tools)

**RAG 检索与上下文补全。** 用 Goldmark 识别 Markdown 标题和块边界，保护代码、列表及表格，复用 Eino 递归切分长段落。ES 同时保存子 Chunk 与父章节记录，子 Chunk 用于 BM25 / kNN 召回，再由项目执行 RRF 融合与重排。复用 Eino Parent Retriever 收集父章节 ID，项目负责 ES 读取、多命中窗口合并、章节补全及字符预算，明确标记不完整内容，避免直接把孤立片段当成完整指南。[入库与召回](internal/ai/retriever/knowledge_retriever.go) · [章节扩展](internal/ai/retriever/section_retriever.go)

**AgentRun 与多轮上下文。** Agent 执行 Context 独立于 HTTP 连接，客户端断线不直接取消任务；持久化事件序号供重连回放，`run_id` 用于幂等识别，并提供主动取消。模型工作上下文保留消息、工具调用和摘要，通过会话串行保护、历史游标和 Revision 校验协调保存。摘要机制复用 Eino，持久化与并发控制由项目实现。[Run 管理](internal/controller/chat_runs.go) · [工作上下文](internal/ai/agent/session_context.go)

**Issue 与经验知识管理。** Session 表示会话，Run 表示一次执行，Issue 表示跨轮推进的问题，经验条目是可独立复用的知识。后台按增量记录更新 Issue，使用 MySQL 任务、租约和版本水位协调独立整理 Agent；整理结果经过程序校验后成为待审草稿，人工发布才进入经验检索。修订期间旧发布版继续可用，模型不会直接覆盖已发布知识。[后台任务](internal/knowledge/worker.go) · [Issue 整理](internal/knowledge/issue_worker.go) · [审核与版本](internal/knowledge/service.go)

## 快速启动

以下以 **PowerShell、本机 Go 服务 + Docker 依赖**为例；macOS / Linux 可使用 `cp` 复制配置并运行同样的 Docker 与 Go 命令。所有命令均在仓库根目录执行。

准备 Go 1.24.10+、Docker Compose v2，以及可用的对话模型和 Embedding 服务。默认配置使用 DashScope 的 OpenAI 兼容接口，分别调用 `qwen-plus` 和 `text-embedding-v4`（1024 维）。

### 1. 准备配置

```powershell
Copy-Item .env.example .env
go mod download
```

编辑本地 `.env`：填写 `DASHSCOPE_API_KEY`，将 `MYSQL_ROOT_PASSWORD` 改为自己的本地密码。默认 Redis 和 ES 不启用密码，其对应变量保持为空；仅填写客户端密码不会为容器自动启用认证。

默认配置为 [config.yaml](manifest/config/config.yaml)。更换模型供应商时，可将它复制为 `manifest/config/config.local.yaml`，通过环境变量引用凭据，并分别设置对话与 Embedding 服务的地址、模型和向量维度。配置优先级为 `-config`、`PILOT_CONFIG`、默认路径；启动前会加载根目录 `.env`。更换 Embedding 维度需要使用兼容的新索引，见[索引迁移说明](docs/RAG_MIGRATION.md)。

### 2. 启动依赖

现有 Compose **固定使用 ARM64**。Apple Silicon / ARM64 环境可直接执行；AMD64 环境请使用下方覆盖配置，或自行确认 Docker 已启用 ARM64 模拟。

```powershell
docker compose --env-file .env -f manifest/docker/docker-compose.yml up -d --wait redis elasticsearch mysql prometheus jaeger grafana
```

<details>
<summary>AMD64 / 常见 Windows、Linux x86-64 环境</summary>

将以下内容保存为 `manifest/docker/docker-compose.local.yml`（已忽略提交）：

```yaml
services:
  redis: {platform: linux/amd64}
  elasticsearch: {platform: linux/amd64}
  mysql: {platform: linux/amd64}
  prometheus: {platform: linux/amd64}
  jaeger: {platform: linux/amd64}
  grafana: {platform: linux/amd64}
```

然后使用这条命令替代上方启动命令；该覆盖配置仅用于本机运行 Go 服务时的六个依赖：

```powershell
docker compose --env-file .env -f manifest/docker/docker-compose.yml -f manifest/docker/docker-compose.local.yml up -d --wait redis elasticsearch mysql prometheus jaeger grafana
```

</details>

### 3. 检查依赖并启动服务

```powershell
go run ./cmd/checkdeps
go run ./cmd -config manifest/config/config.yaml
```

自定义配置需同时传递给依赖检查器（通过 `PILOT_CONFIG`）和服务，避免检查与运行使用不同配置。首次启动会自动迁移 MySQL 表并初始化 ES 索引，无需手动建表。

打开 <http://localhost:8080/>。浏览器会建立设备账号，请固定使用 `localhost` 或 `127.0.0.1` 中的一种地址，以免切换 Cookie 身份。健康检查为 `/api/v1/health/ready`，监控指标为 `/metrics`。

可先问“检查 pilot-api 最近 30 分钟的错误，区分事实和假设”，观察执行过程及流式回答；再上传自己的 Markdown 技术指南测试 RAG。新环境没有预置专业指南或真实故障数据，查询无结果并不表示服务异常。经验审核可从对话侧边面板或知识库页面进入。

完整容器运行使用 Compose 的 `app` profile 和 [config.docker.yaml](manifest/config/config.docker.yaml)。仓库中的 `artifacts/startup` 是被忽略的本地环境，不是首次安装的必要依赖。Compose 含开发用口令、关闭认证的依赖及端口映射，仅用于本地演示。

## 测试与验证

```powershell
go test ./...
node --test web/app.test.mjs web/agent-session.test.mjs
```

Go 存储测试使用 SQLite，需要 **`CGO_ENABLED=1` 和 C 编译器**；前端测试需要 Node.js。Git Bash / WSL / macOS / Linux 环境还可运行 `make ci`，检查格式、静态分析、构建、race、前端测试及依赖一致性。现有 GitHub Actions 的步骤见 [ci.yml](.github/workflows/ci.yml)，其覆盖范围不等同于完整 `make ci`。

| 验证范围 | 现有测试入口 |
| --- | --- |
| Markdown 边界、父子入库、混合召回、窗口与预算 | [retriever 测试](internal/ai/retriever)、[工具测试](internal/ai/tools) |
| ADK 调查与回答分离、摘要及上下文保存 | [agent 测试](internal/ai/agent) |
| Run 幂等、SSE 回放、取消与持久化 | [chat_runs_test.go](internal/controller/chat_runs_test.go)、[chat_events_test.go](internal/controller/chat_events_test.go) |
| Issue 归属、增量整理、任务与经验版本 | [knowledge 测试](internal/knowledge) |
| 账号隔离、历史与工作上下文存储 | [security 测试](internal/security)、[memory 测试](internal/memory) |
| 前端会话与交互契约 | `web/app.test.mjs`、`web/agent-session.test.mjs` |

这些机制测试不代表真实模型准确率或检索召回率评估。可选的 [Issue 经验验收脚本](scripts/issue_experience_smoke.py) 使用真实服务、模型和数据库，会写入测试数据并产生模型费用；当前脚本依赖 Python Playwright 及 Edge。历史模型验收未完整覆盖最新版，不能视为当前端到端全部通过。可选 Redis Markdown 样例测试在样例不存在时会跳过。

## 技术文档索引

- [ADK 主链路、上下文与经验库](docs/ADK_EXPERIENCE_GUIDE.md)
- [Issue 跟踪与知识更新](docs/ISSUE_KNOWLEDGE_GUIDE.md)
- [Markdown 切分与父子检索](docs/MARKDOWN_RAG_GUIDE.md)
- [知识召回与章节补全代码复盘](docs/knowledge-retrieval-flow.md)
- [索引迁移](docs/RAG_MIGRATION.md)
- [对话与经验审核界面](docs/UI_WORKSPACE_GUIDE.md)

[旧架构说明](docs/LEGACY_ARCHITECTURE.md)、ADK Spike、证据图和早期验收报告属于历史参考。部分旧文档及验证脚本仍描述已移除的多 Agent HTTP 路径，当前路由以 [cmd/main.go](cmd/main.go) 为准。

## 当前限制

- **事件回放不是推理恢复。** 客户端重连可以继续接收现有 Run 事件；服务进程重启后，未完成执行会标记失败并保留已有内容，不从 ADK 推理中间状态续跑。知识整理后台任务具有独立的补扫和重试机制。
- **设备账号不是完整登录系统。** 使用 HttpOnly 随机 Cookie 标识账号，没有密码登录、跨设备恢复和组织权限；清除 Cookie 后不会自动取回原账号数据。Chat 和经验按账号隔离，技术文档库目前共享。
- **模型输出仍需核实。** Issue 归属与相似判断可能出错，程序校验不等于证明结论真实；提供人工修正和经验审核，主链路没有确定性的最终结论引用审计。
- **输入及预算有边界。** 文件上传仅支持 Markdown；上下文压缩使用保守字符估算，章节窗口和工具输出预算也按字符计算，不等于精确 Token 预算。未实现用户画像或偏好记忆。
- **当前没有自主子 Agent 管理。** 主 Agent 自主选择检索工具，独立整理 Agent 由后台任务调度；旧固定多 Agent 包保留用于历史测试。调用云模型时，相关上下文仍会发送到所配置的供应商。

## 仓库文件约定

`.env`、本地覆盖配置、Cookie、数据库、日志、缓存和 `artifacts/` 不提交；`.dockerignore` 另行排除构建上下文中的敏感文件及本地产物。正式配置模板、部署文件、源码、测试和前端依赖保持可用。忽略规则不清除 Git 历史，也不清理已经产生的镜像或构建缓存。
