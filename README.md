# Pilot

Pilot 是用 Go、Gin 和 Eino v0.9.13 构建的排障助手。当前主链路使用 **ADK ChatModelAgent** 自主选择只读工具，查询 Prometheus、Elasticsearch 和 Jaeger，最后流式输出结论、依据与限制。独立的知识整理 Agent 将已结束的排障记录转换为账号私有的经验草稿，经过人工审核发布后，可在新会话中检索。

## 当前能力

- 同步 Chat、SSE Chat；执行过程、工具调用和最终回答分别展示，支持 Run 取消与事件续接。
- 会话原始历史保存在 MySQL，Redis 缓存用于界面历史；模型工作上下文单独持久化，包括工具调用与结果。
- 复用 Eino ADK 官方 `summarization` 中间件，按上下文容量触发摘要，保留整个会话脉络、方案取舍及当前请求。压缩不删除原始历史。
- Run 结束后增量更新问题摘要与证据归属；同会话多个交错问题独立跟踪。确认解决后自动生成草稿，也支持按钮或 `organize_experience` 主动整理。SQL 任务具有幂等、重试、租约与重启恢复。
- 经验管理中心：筛选、审核抽屉、编辑保存、发布、拒绝、创建修订、归档、查看版本和来源证据。
- 审核、验证、生命周期分别管理；仅当前账号已发布、已审核通过且未归档的版本供 `search_experience` 检索。
- 原有 Chunk RAG 作为独立的参考文档库保留：Embedding、BM25、向量召回、RRF 与重排。它不是上述私有经验库。

技术指南现支持 Markdown 文件上传、按结构切分和 Eino 父子检索补回章节上下文，详见 [Markdown RAG 实现与阅读指南](docs/MARKDOWN_RAG_GUIDE.md)。旧文档继续可检索，重新上传后使用新切分策略。
- 原有日志/指标/链路查询、跨信号关联、OpenTelemetry 和 Prometheus 监控保留。

旧固定多 Agent 排障路径及其 HTTP 入口已从主程序移除；相关包暂保留用于历史回归测试。旧情景记忆不再参与主 Agent 上下文。反馈按钮只记录评价，不触发经验收录。旧实现说明见 [归档文档](docs/LEGACY_ARCHITECTURE.md)，请勿将其路由和验证门禁视为当前 ADK 的能力。

## 本地启动

环境要求：Go 1.24.10+、Docker Desktop、模型及 Embedding 服务。数据库表在启动时自动迁移，不需要新增消息队列或向量服务。

当前 Windows 开发环境已配置 DeepSeek **Flash** 与本地 Embedding；在 `pilot` 目录运行：

```powershell
powershell -ExecutionPolicy Bypass -File artifacts/startup/start.ps1
```

该本地脚本与运行配置在忽略提交的 `artifacts` 下。首次在其他机器安装，请复制 `.env.example` 为 `.env`，填写所选模型/Embedding 凭据，用 `manifest/docker/docker-compose.yml` 启动依赖，再启动 Go 服务：

```powershell
docker compose --env-file .env -f manifest/docker/docker-compose.yml up -d
go run ./cmd -config manifest/config/config.yaml
```

默认 manifest 使用 DashScope；当前 DeepSeek 开发环境使用 `artifacts/startup/config.deepseek.yaml`，两者不要混用。配置优先级为 `-config`、`PILOT_CONFIG`、默认 manifest；读取配置前加载根目录 `.env`。不要提交密钥。

打开 http://localhost:8080/，在“智能对话”完成排障；展开“本次问题”查看、修正归属或整理会话经验。新设备默认开启“确认解决后自动生成草稿”，原设备保存的设置继续生效。草稿可以稍后处理，在知识库管理中心继续审核。

```yaml
agent:
  max_iterations: 25
  context_tokens: 24000
  timeout: 300
```

`context_tokens` 使用包含消息及工具 Schema 的保守字符估算，控制压缩触发，并非精确计费 Token 数；小于等于零时主 Agent 使用 24000。摘要输出上限为 1800 Token。

## 账号和数据边界

控制台使用 HttpOnly 随机凭据 Cookie 建立设备账号，后端依据凭据确定所有者，忽略调用方伪造的 `user_id`。同一浏览器刷新、服务重启不会更换账号；`localhost` 与 `127.0.0.1` 的 Cookie 不共享，请固定使用一个地址。

这是本地演示的设备账号方案，没有密码登录、跨设备恢复和组织权限。清除 Cookie 后会获得新账号；旧数据不会自动转移。旧版由用户手填 `user_id` 的历史也不会被自动归属到新账号。

会话、工作上下文和经验存储在本地 MySQL Docker 卷中；模型调用仍会将需要分析的内容发送给当前配置的模型服务。参考文档库沿用原来的共享存储，本次私有隔离针对 Chat 和排障经验。

## API

所有响应沿用项目统一 JSON envelope。Chat 与经验接口要求账号 Cookie；浏览器启动时自动建立会话。CLI 可先保存 Cookie，再调用：

```powershell
curl.exe -c pilot.cookies -X POST http://localhost:8080/api/v1/account/session
curl.exe -b pilot.cookies -H 'Content-Type: application/json' -d '{"session_id":"demo","query":"检查 pilot-api 最近的错误"}' http://localhost:8080/api/v1/chat
```

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/api/v1/account/session` | 建立或读取设备账号 |
| POST | `/api/v1/chat` | 同步回答 |
| POST | `/api/v1/chat/stream` | 任务事件 SSE 与回答正文 |
| GET | `/api/v1/chat/runs` | 当前账号会话的执行历史 |
| GET | `/api/v1/chat/runs/:run_id/events` | 回放/续接执行事件 |
| POST | `/api/v1/chat/runs/:run_id/cancel` | 取消任务 |
| GET / DELETE | `/api/v1/chat/history` | 查询/清理当前账号会话 |
| GET / POST | `/api/v1/experiences/settings` | 自动整理开关 |
| GET | `/api/v1/experiences/issues?session_id=...` | 当前会话的问题、状态、证据归属与关联经验 |
| GET | `/api/v1/experiences/unassigned?session_id=...` | 暂时无法归属的事件，展示预算最多 100 项 |
| POST | `/api/v1/experiences/issues/:issue_id` | `rename/status/merge/split/move/assign`，携带 Revision |
| POST | `/api/v1/experiences/sessions/:session_id/organize` | 主动整理会话中尚有新增内容的问题 |
| POST | `/api/v1/experiences/sessions/:session_id/close` | 结束时检查遗漏 Run 并安排整理 |
| GET / POST | `/api/v1/experiences/jobs/:run_id` | 查询/创建任务，失败后重试 |
| GET | `/api/v1/experiences` | 筛选列表、分页与分类计数 |
| GET | `/api/v1/experiences/:entry_id` | 详情，支持 `?version=N` |
| GET | `/api/v1/experiences/:entry_id/versions` | 历史版本 |
| POST | `/api/v1/experiences/:entry_id` | `save/publish/reject/revise/archive`，携带 `revision` |
| GET | `/api/v1/health/ready` | 核心依赖就绪状态 |
| GET | `/metrics` | Prometheus 指标 |

经验接口的筛选参数包括 `tab/query/service/type/review/verification/from/to/run_id/page`。详情与版本携带不可编辑的来源证据。正文 SSE 使用 `kind=answer,status=delta`，以 `kind=run` 终态和 `[DONE]` 结束；完整基础协议见 [AGENT_CHAT_UI.md](docs/AGENT_CHAT_UI.md)。

## 学习与验证

先读 [ADK 主链路与经验库实现指南](docs/ADK_EXPERIENCE_GUIDE.md)，再依次看 `full_agent.go`、`session_context.go`、`answer_stream.go` 和 `internal/knowledge`。该文档区分了框架职责、业务职责以及当前实现限制。

```powershell
go test ./...
node --test web/app.test.mjs web/agent-session.test.mjs
```

存储测试使用 SQLite，需要 `CGO_ENABLED=1` 和 C 编译器；无编译器的 Windows 可使用 Linux 容器执行。当前实际模型与浏览器验收脚本是 `scripts/issue_experience_smoke.py`，需已启动服务及 Python Playwright，会创建独立测试账号并产生模型调用费用。设计动机与增量流程见 [Issue 与知识更新指南](docs/ISSUE_KNOWLEDGE_GUIDE.md)。

当前实现故障案例 MVP，包括 Issue 增量记忆、相似案例关联及修订提案。向量召回、用户偏好、长期画像、版本差异视图和完整登录系统属于后续迭代。模型的归属与相似判断可能出错，需要界面修正和人工审核。
