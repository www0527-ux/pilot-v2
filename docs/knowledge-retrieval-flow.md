# 技术文档知识库召回链路：代码复盘与面试讲解

核对日期：2026-10-14。业务代码基线：`7acdb18`（多命中、预算约束的章节窗口扩展）。本文只描述当前实现，不修改业务代码。配置数字取自仓库的 `manifest/config/config.yaml`，不代表已经启动服务、连接真实 ES 或验证模型效果；运行配置可以不同。

本文的知识库是 **ES 技术文档库 `search_knowledge`**，不包括 MySQL 的个人历史排障经验 `search_experience`，也不包括运行日志 `query_logs`。

## 1. 全局流程 Mermaid 图

蓝色为正常路径，黄色为降级/失败观察，红色为 Go 错误返回或执行终止，菱形为判断。红色错误是否被上层转换为观察，要继续看其后分支。图中“召回失败”同时包括查询向量生成失败与 ES 请求失败；详细条件见第 6 节。

```mermaid
flowchart LR
    subgraph ENTRY[业务入口与工具选择]
        direction TB
        A["Chat / StreamChat<br/>Generate / Stream"] --> B["Graph 准备上下文<br/>ChatTemplate → ADK"]
        B --> C{"模型选择<br/>search_knowledge？"}
        C -->|是| T["Eino ToolsNode<br/>工具回调检查 ctx"]
        T --> V{"query 非空且<br/>至多 1000 rune？"}
        V -->|是| Q["有效 query<br/>调用 Search"]
        V -->|否| BAD["success=false<br/>Go error=nil<br/>作为工具观察回填 ADK"]
        C -->|否| OTHER["不执行本次 RAG<br/>其它工具或已有上下文回答"]
        T -.->|ctx 已终止| STOP0["返回错误，终止执行"]
    end

    subgraph RECALL[chunk召回、重排与筛选]
        direction TB
        R["SearchDetailed → recall<br/>BM25 / vector / hybrid"] --> RF{"召回路失败？"}
        RF -->|否| S["fuseRRF<br/>或单路转换"]
        RF -->|是| AL{"符合单路<br/>降级条件？"}
        AL -->|是| FB["成功单路转换<br/>markDegraded"]
        S --> RK["rerank + diversifyChunks<br/>入选章节全部命中<br/>[]RetrievedDocument"]
        FB --> RK
        AL -->|否| ERR["Go error 向工具层返回"]
        ERR --> CANCEL{"取消或超时？"}
        CANCEL -->|是| STOP1["直接返回错误<br/>Agent / Run 终止"]
        CANCEL -->|否| OBS1["工具转换失败 JSON<br/>success=false，error=nil<br/>回填 ADK，可修正或报告缺口"]
    end

    subgraph RETURN[工具返回与 Agent 使用]
        direction TB
        EX["expandSections：见图 2<br/>全章 / 动态窗口 / 命中回退<br/>[]KnowledgeDocument"] --> EE{"Search 有错误？"}
        EE -->|否| FMT["空结果正常返回 count=0<br/>非空：格式化来源与正文<br/>16000 rune，超预算整项跳过"]
        FMT --> TM
        TM["JSON → ToolMessage<br/>进入 ADK 工作上下文<br/>可继续其它工具或再次检索"] -->|调查结束| FINAL["separatedADKStream<br/>无工具最终模型调用"]
        EE -->|是| CANCEL2{"取消或超时？"}
        CANCEL2 -->|是| STOP2["直接返回错误<br/>Agent / Run 终止"]
        CANCEL2 -->|否| OBS2["失败工具 JSON<br/>success=false，error=nil"]
        OBS2 --> TM
    end

    ENTRY -->|仅有效工具 query：Search → SearchDetailed| RECALL
    RECALL -->|仅正常或已降级的召回结果：Search 继续扩展| RETURN

    classDef normal fill:#eaf3ff,stroke:#356ca5,color:#152b43;
    classDef fallback fill:#fff3d6,stroke:#ba7d18,color:#533900;
    classDef error fill:#ffe6e6,stroke:#b93d3d,color:#6e1717;
    classDef decision fill:#f4f4f4,stroke:#666,color:#222;
    class A,B,T,Q,R,S,RK,EX,FMT,TM,FINAL,OTHER normal;
    class FB,BAD,OBS1,OBS2 fallback;
    class ERR,STOP0,STOP1,STOP2 error;
    class C,V,RF,AL,EE,CANCEL,CANCEL2 decision;
```

图 1 用三个并排模块减少长连线：模块间箭头只表示校验通过、召回可返回时的数据流，不代表黄色失败观察也继续进入 ES/章节扩展。标明“回填 ADK”的分支与 RETURN 中的 ToolMessage 具有相同的模型观察语义；若模型再次检索，重新进入入口，图中不重复画跨模块回环。

`AL` 的真实规则不是“任意一条路成功就可以”：单路模式失败没有跨路回退；hybrid 的向量失败需要 BM25 成功且开启 `fallback_to_bm25`；BM25 失败需要向量结果非空。“取消或超时”在工具层同时检查 ctx.Err 和错误的 Canceled/DeadlineExceeded 类型。框架参数解码等回调外错误由 `recoverableToolErrors` 处理，普通错误转为失败观察，取消/超时继续抛出。

## 2. 章节扩展与动态窗口详细 Mermaid 图

这张图描述一次 `expandSections()` 调用。对各章节的处理重复执行，但最终输出顺序取自原 `results`，不是 ES 父章节返回顺序或 map 遍历顺序。

```mermaid
flowchart TD
    START["expandSections<br/>输入：results，类型为 []RetrievedDocument"] --> CTX{"ctx.Err 非空？"}
    CTX -->|是| STOPSTART["返回 context 错误"]
    CTX -->|否| GROUP["扫描全部 results<br/>有 SectionID 的 chunk 按身份去重<br/>seeds：section_id → 命中片段切片<br/>children：Eino Document，元数据含 section_id"]
    GROUP --> HAS{"children 非空？"}
    HAS -->|否| ORDER
    HAS -->|是| BUILD["parent.NewRetriever<br/>Retriever=children<br/>ParentIDKey=section_id<br/>OrigDocGetter=项目回调"]
    BUILD --> BUILDOK{"创建成功？"}
    BUILDOK -->|否| DIRECT["直接返回创建错误<br/>不进入读取失败回退"]
    BUILDOK -->|是| FLOW["flow.Retrieve：query 为空字符串<br/>rankedChildren.Retrieve 返回已有 children<br/>Eino 提取并去重父 ID，再调用 OrigDocGetter"]
    FLOW --> ES["ES SearchHits：ids.values=父 ID<br/>同一知识索引，size=ID 数<br/>转换后仅保留 RecordType=section"]
    ES --> ESOK{"父章节批量查询成功？"}
    ESOK -->|否| FLOWERR
    ESOK -->|是| EACH["按请求的章节 ID 处理<br/>parents[id]：父章节<br/>seeds[id]：该章节全部命中"]
    EACH --> PERCTX{"ctx.Err 非空？"}
    PERCTX -->|是| STOPPARENT["返回 context 错误"]
    PERCTX -->|否| PARENT{"父章节存在且<br/>Content 非空？"}
    PARENT -->|否| LOCAL["仅该章节 fallbackSection<br/>预算内保留多命中正文<br/>context_complete=false"]
    PARENT -->|是| SIZE{"章节长度不超过<br/>8000 rune？"}
    SIZE -->|是| FULL["返回完整章节<br/>context_complete=true<br/>清理旧 context_note / context_ranges"]
    SIZE -->|否| WINDOW["sectionWindow<br/>整篇原文坐标转换为章节内坐标"]

    %% 长章节的范围规划与预算分配
        WINDOW --> OFF{"章节起点非负，且全部命中<br/>范围有效、同章节、正文匹配原文？"}
        OFF -->|否| INVALID["仅该章节 fallbackSection<br/>保留预算内命中正文<br/>注明偏移无效或原文不一致"]
        OFF -->|是| SEED{"命中范围非空？"}
        SEED -->|否| WERR["返回 no hits 错误<br/>正常聚合链路通常不会进入"]
        SEED -->|是| MERGE["selectHitRanges + mergeRuneRanges<br/>按源位置合并相邻或重叠范围<br/>成本=唯一正文 rune + 省略标识"]
        MERGE --> FIT{"全部命中范围及标识<br/>可放入 8000 rune？"}
        FIT -->|否| CUT["按输入相关性顺序逐个接纳<br/>必要时缩短当前命中的右端<br/>不驱逐已选命中，不扩展周边"]
        FIT -->|是| ROUND["剩余预算按当前窗口两侧均分<br/>step 至少为 1 rune<br/>按源位置依次尝试左右扩展"]
        ROUND --> WCTX{"ctx.Err 非空？"}
        WCTX -->|是| STOPWIN["返回 context 错误"]
        WCTX -->|否| GROW["限制在章节边界内<br/>每次尝试后合并范围<br/>仅当含标识总长度不超预算才接纳"]
        GROW --> CHANGE{"本轮有范围增长？"}
        CHANGE -->|是| ROUND
        CHANGE -->|否| SLICE
        CUT --> SLICE["按源位置截取父章节原文<br/>窗口之间加入省略标识<br/>context_complete=false<br/>保存 context_ranges 和原因"]
    WINDOW -.->|进入时或输出前 ctx 终止| STOPWIN
    WERR --> FLOWERR["OrigDocGetter 错误<br/>传到 flow.Retrieve"]
    FLOWERR --> FCANCEL{"ctx.Err 非空？"}
    FCANCEL -->|是| STOPFLOW["返回 context 错误"]
    FCANCEL -->|否| ALL["发出 warning 进度事件<br/>该次所有有章节关系的结果<br/>均用 fallbackSection 回退"]
    FULL --> PACK["包装 Eino Document<br/>ID=章节 ID，MetaData.record=业务文档"]
    LOCAL --> PACK
    INVALID --> PACK
    SLICE --> PACK
    PACK --> MORE{"还有章节 ID？"}
    MORE -->|是| EACH
    MORE -->|否| MAP["flow 成功返回<br/>拆出 MetaData.record<br/>expanded：章节 ID → 业务文档"]
    MAP --> ORDER["重新遍历原 results<br/>维持章节首次命中的相关性顺序"]
    ALL --> ORDER
    ORDER --> ANY{"还有下一条原 results？"}
    ANY -->|是| REL{"原 chunk 有 SectionID？"}
    ANY -->|否| LAST
    REL -->|是| DUP{"章节已输出？"}
    DUP -->|是| SKIP["跳过重复章节"]
    DUP -->|否| TAKE["标记 seen<br/>取 expanded[SectionID]"]
    REL -->|否| LEGACY["原 chunk 按身份去重<br/>保留原文，必要时截至 8000 rune<br/>context_complete=false"]
    TAKE --> TEXT{"正文去空白后非空？"}
    LEGACY --> TEXT
    TEXT -->|是| APPEND["追加最终 KnowledgeDocument 切片"]
    TEXT -->|否| SKIP
    APPEND --> ORDER
    SKIP --> ORDER
    LAST{"最终 ctx.Err 非空？"}
    LAST -->|是| STOPLAST["返回 context 错误"]
    LAST -->|否| RETURN["返回 docs，error=nil<br/>可能完整、局部、混合或为空"]

    classDef normal fill:#eaf3ff,stroke:#356ca5,color:#152b43;
    classDef fallback fill:#fff3d6,stroke:#ba7d18,color:#533900;
    classDef error fill:#ffe6e6,stroke:#b93d3d,color:#6e1717;
    classDef decision fill:#f4f4f4,stroke:#666,color:#222;
    class START,GROUP,BUILD,FLOW,ES,EACH,WINDOW,FULL,MERGE,ROUND,GROW,SLICE,PACK,MAP,ORDER,TAKE,APPEND,RETURN,SKIP normal;
    class LOCAL,INVALID,CUT,ALL,LEGACY fallback;
    class STOPSTART,STOPPARENT,STOPWIN,STOPFLOW,STOPLAST,DIRECT,FLOWERR,WERR error;
    class CTX,HAS,BUILDOK,ESOK,PERCTX,PARENT,SIZE,OFF,SEED,FIT,WCTX,CHANGE,FCANCEL,MORE,ANY,REL,DUP,TEXT,LAST decision;
```

图 2 的 `fallbackSection` 不会恢复缺失的原文：位置可靠的已命中正文按源顺序去重、合并；缺口用省略标识隔开。位置未知的片段在内部安排临时不连续位置，放到位置已知的内容之后；相同未知正文去重。临时位置不作为原文位置输出，合成结果的 `StartRune/EndRune` 置零，移除 `context_ranges`，通过 `context_note` 说明限制。

`PACK` 不是修改 ES：它仅在内存里将业务文档放入 Eino 的 `MetaData["record"]`。图中的全局读取错误回退只检查 `ctx.Err()`；它没有额外对这个错误做 `errors.Is` 判断。真正请求上下文取消时会终止；若某个内部调用返回超时类错误但请求 ctx 尚有效，这一层可能仍将它转换为命中片段回退。

## 3. 入口、数量、预算与数据结构

### 3.1 谁触发检索，Query 从哪里来

主链入口是 `/api/v1/chat` 和 `/api/v1/chat/stream`。同步调用 `ChatController.Chat → FullPilotAgent.Generate`；流式调用 `StreamChat → startRun → executeRun → FullPilotAgent.Stream`。Graph 依次准备工作会话上下文、ChatTemplate、ADK Agent。`ContextPreFetcher` 组装 SQL 会话工作上下文与静态背景，**不预取文档 RAG 或历史经验**。

用户 Query 首先是用户消息。模型根据本轮问题和已有证据决定是否调用 `search_knowledge`，通过工具调用参数中的 `query` 指定检索词；它可以与原用户问题不同。项目没有额外的“LLM Query 改写器”。`prepareAgentQuery` 仅对“继续”等少量输入替换会话继续提示，不能等同于 RAG 查询改写。

工具 schema 只有 `query`。模型不能在这个工具中指定 ES 索引、召回模式、TopK 或窗口预算。索引由构造检索器时的 `cfg.Elasticsearch.KnowledgeIndex` 决定。

其他真实入口与主链的区别：

| 入口 | 实际调用 | 返回及差异 |
|---|---|---|
| `/api/v1/knowledge/search` | `KnowledgeController.Search → Search` | 直接返回扩展后的文档列表，没有工具层 16000 rune 的格式化预算，也不自动进入 Agent |
| `/api/v1/knowledge/search/detailed` | `KnowledgeController.SearchDetailed → SearchDetailed` | 返回带分数的命中 chunk；不调用 `expandSections` |
| 独立 `cmd/mcpserver` 的 `search_knowledge` | `Deps.Knowledge → Search` | 返回扩展文档，由 MCP 包装；不是主 Agent 工具的 `SearchKnowledgeOutput` 格式与预算 |
| `newRetrieverLambda` / `formatKnowledgeDocumentsForPrompt` | `SearchDetailed` 后格式化 | 当前保留为测试辅助，未接入主 Graph；单条 1200/总 5000 的旧格式化限制不是主 Agent 文档工具限制 |
| `internal/ai/multiagent` 的知识适配 | `SearchDetailed` | 历史工作流仍在仓库；当前 `cmd/main.go` 未启用该多 Agent executor，不能画成主链 |

### 3.2 参数与限制的实际位置

| 参数 / 预算 | 代码位置 | 仓库配置或常量 | 真实含义 |
|---|---|---|---|
| Agent 用户 Query | `controller/chat_helpers.go` | 8000 rune | 对话输入限制，和文档工具 Query 不同 |
| 文档工具 Query | `tools/knowledge_tools.go` | 1000 rune | TrimSpace 后空或超长，返回 `success=false` JSON，而非 Go error |
| HTTP 文档搜索 Query | `normalizeKnowledgeSearch` | 1000 rune | 参数错误走 HTTP 错误响应 |
| `knowledge_index` | `newKnowledgeRetriever` | `pilot_knowledge` | 由服务端配置选择，三类 ES 查询复用同一索引 |
| `retrieval_mode` | `recall` | `hybrid` | 支持 `bm25`、`vector`、`hybrid` |
| BM25 / vector TopK | `recall` | 各为 10 | 实际请求 size 为 `max(该路 TopK, poolLimit)` |
| `candidate_multiplier` | `recall` | 3 | `poolLimit=max(FinalTopK*倍率, FinalTopK)`，当前为 15；融合/单路转换后截取候选池 |
| `rrf_k` | `fuseRRF` | 60 | 排名融合常数，不是返回数量 |
| `FinalTopK` | `diversifyChunks` | 5 | 限制入选章节或无章节 chunk 的输出名额；入选章节多个命中不重复占名额 |
| `max_chunks_per_doc` | `diversifyChunks` | 2 | 当前实际限制每篇文档入选的章节/无章节 chunk 名额；不是每章节只允许两个 seed |
| `reranker_mode` | `rerank` / `cmd/main.go` | `lexical` | 可配置 `model`；当前默认不调用重排 LLM |
| 章节/片段内容预算 | `section_retriever.go` | 8000 rune | 单项预算，窗口间省略标识及换行也计入；不等于整个文件或所有章节的总预算 |
| 工具 `Results` 预算 | `knowledge_tools.go` | 16000 rune | 计入前导说明、标题/ID/分类、章节路径、完整性说明及正文；整项超预算就跳过 |
| Agent 工作上下文压缩 | `newADKAgentLambda` / `session_context.go` | 24000 的保守估算阈值 | 由官方 summarization 压缩，估算器实际统计序列化输入的 rune；不是精确 tokenizer，也不是 RAG 的单项预算 |

因此当前两路的 ES `size` 都是 15，而不是直接等于配置的 10。`SearchDetailed` 保留入选章节在这个候选池中的全部命中，返回的 chunk 数可以超过 5；`Search` 的最终章节/无章节片段名额最多为 5，工具再按总预算筛选。

### 3.3 数据在各层如何转换

```go
// ES 适配层
type SearchHit struct {
    ID     string                 // ES _id
    Score  float64                // ES _score
    Source map[string]interface{} // ES _source
}

// 检索、融合、重排阶段；仅列关键字段
type RetrievedDocument struct {
    Document          types.KnowledgeDocument
    Score             float64
    BM25Score         *float64
    VectorScore       *float64
    FusedScore        *float64
    RerankScore       *float64
    RerankMode        string
    RecallMethods     []string
    Degraded          bool
    DegradationReason string
}

// Search 最终返回 []types.KnowledgeDocument；列出阅读链路需要的字段
// 这个类型既用于 chunk，也用于 section，并不代表每次都是整篇文章。
type KnowledgeDocument struct {
    ID, DocID, ChunkID          string
    ChunkIndex, ChunkCount     int
    SectionID, SectionPath     string
    RecordType, SplitterVersion string
    StartRune, EndRune         int
    Title, Content, Category   string
    Tags                       []string
    Metadata                   map[string]string
    // 原类型还有 Vector、CreatedAt、UpdatedAt；召回转换未填充这些字段。
}

// Agent 工具返回的 JSON；Results 是格式化字符串，不是文档数组
type SearchKnowledgeOutput struct {
    Success bool
    Count   int
    Results string
    Message string
}
```

数据主线：`ES hits → []recallCandidate → []RetrievedDocument → []KnowledgeDocument → SearchKnowledgeOutput JSON 字符串 → schema.ToolMessage → ADK 工作消息`。

`SearchDetailed` 保留原始路分数、融合/重排分数及降级原因；`Search` 扩展后只返回 `KnowledgeDocument`，**没有分数字段，也没有把这些分数和路降级原因统一拷贝进 Metadata**。保留的是结果优先顺序。父章节读取产生的 `_score` 不参与排序。

## 4. ES 召回、融合、重排与章节窗口的实现依据

### 4.1 BM25 与精确匹配

`searchBM25` 使用 ES 全文查询评分；项目没有自行实现 BM25 公式。Go 发送完整 Query，ES 根据字段 mapping 处理。`title/content/section_path` 为 `standard` 分析器的 text；`tags/doc_id/chunk_id` 为 keyword。不能说所有字段使用同一种分词方式，也不能说当前已安装中文 IK 分词器。

| 查询子句 | 字段 / 权重 | 用途 |
|---|---|---|
| `multi_match`，`best_fields` | `title^3, section_path^3, content, tags^2` | 多字段关键词匹配，偏重标题与章节路径 |
| `multi_match`，`phrase`，外层 boost=3 | `title^5, content^2` | 提高短语匹配权重 |
| 每个 `exactIdentifiers` 结果的 `term` | `doc_id`、`chunk_id`，各 boost=12 | 精确定位 ID |
| 每个标识符的 `match_phrase` | `content`，boost=8 | 提升标识符在正文中的短语命中 |

外层 `bool.should` 至少命中一个子句；`must_not` 排除 `record_type=section`。boost 是评分权重，不是固定加 12 分。`exactIdentifiers` 的正则为 `[A-Za-z0-9][A-Za-z0-9_-]{3,}`，按忽略大小写去重；它也会提取一般英文词，不是专门的 ID 识别模型。所有子句组成一次 ES 请求，没有每个词单独请求 ES。

### 4.2 向量与 ES 返回

`embedder.Embed(ctx, query)` 通过 Eino OpenAI-compatible embedding 组件生成 Query 向量；结果为空是错误。随后 `searchVector` 使用：

```text
knn
  field: vector
  size = k = max(vector_top_k, poolLimit)
  num_candidates = min(10000, max(100, 10*k))
  filter: exists(vector) AND record_type != section
  boost: 2.0
```

这是 ES 8.12 的近似 kNN 召回，使用已开启 `index=true` 的 dense_vector 默认 HNSW 索引。图由 ES/Lucene 构建和维护。预过滤要求存在向量并排除父章节，同时兼容没有 `record_type` 的旧片段。`k` 不在 `[1,9999]` 时直接报错；默认配置 k=15、每分片候选数 150，候选数不是相似度计算次数上限。原生 cosine kNN 得分为 `(1+cosine)/2`，通过 boost=2 保持原脚本 `1+cosine` 的分数尺度；RRF 仍按排名融合。近似召回可能遗漏真实最近邻，尚未完成真实 ES 的延迟与召回率对比。实际索引必须支持已索引的向量；不兼容时报错，再由已有 hybrid 策略决定是否降级 BM25，不自动切回全量扫描。

`Client.SearchHits` 将 map 编码成 JSON，通过 ES SDK 携带请求 context、索引、body、`track_total_hits=true`。HTTP 非成功、解码失败、顶层 hits 结构不正确返回错误。`extractSearchHits` 按返回数组顺序提取 `_id/_score/_source`；单条记录结构异常或缺少 `_source` 会跳过，缺失数值 `_score` 则保留默认零值。适配层没有再排序，也不把 ES total 返回给调用方。

这两类查询没有显式 `sort`，依赖 ES 的相关性排序。项目转换 `_source` 为业务结构，并在 `chunk_id` 缺失时用 ES `_id` 补齐，`DocID/ID` 也做兼容补齐。它没有再次校验每条 chunk 的正文和偏移；偏移问题在长章节窗口时处理。

### 4.3 RRF、重排、选择章节

hybrid 中 BM25 在 goroutine 执行，主路径生成向量并查询向量路，最后收集两路结果。正常双路成功时按候选身份聚合，优先以 ChunkID 作为身份；RRF 使用：

```text
融合分数 = 各召回路的 1 / (rrfK + 从 1 开始的排名) 之和
```

RRF 融合排名，不直接相加 BM25 与 cosine 的原始分数；原始分数另外保留。融合后排序并截到候选池。单路模式/回退使用 `singleRouteResults` 去重、保留路顺序并截取候选池。

之后始终先执行 `lexicalRerank`：项目 `tokenSet` 计算 Query 与标题/正文/标签的词集合覆盖，标识符匹配额外加权，再 clamp 到 `[0,1]`，加到已有 `Score`。这个 Go 侧 tokenSet 与 ES 分析器不是同一个组件。最终 `Score` 降序；同分以 `candidateIdentity` 作为确定性排序键。

配置 `model` 时才再调用 `ChatModelReranker`：只接受给定候选的 ID 和分数；部分候选未返回评分时保留其先前词法得分。已评分候选更新为 `0.75*modelScore + 0.25*此前Score`，再排序。模型不可用、响应不合法或没有有效候选分数时保留词法结果并标记 `lexical_fallback`。构造阶段的 model 配置错误则会导致启动失败，不能和运行中的重排降级混为一谈。

`diversifyChunks` 先按排序选择输出名额，对整篇 DocID 限制名额，再第二遍保留入选章节的全部候选命中。它没有额外检索候选池外的同章节 chunk。章节优先级取首次高排序命中，不是对整个章节求一个新的聚合相关性分数。

### 4.4 chunk 为什么能指向 section

Markdown 入库时先划分章节范围，再产生章节内 chunks。每条 child 保存所属 `SectionID`、章节路径及整篇原文的 `[StartRune, EndRune)`；父章节原文另存为 `record_type=section`，ES `_id=SectionID`，不生成向量。Title 仍是整篇文档标题；SectionPath 表示章节路径。

父章节保存的是其自身原文范围，不是每章都复制整篇文章。切分器以文档直接子块中的真实标题为边界，不把标题路径当成“包含所有下级章节的整棵子树”。普通文本分支没有这些父章节关系；没有标题、仅形成一个 section 的 Markdown 才可能由一个 section 覆盖全文。

Eino Parent Retriever 的职责是“调用底层 Retriever → 收集/去重元数据中的父 ID → 调用 OrigDocGetter”。项目的底层 Retriever 是已有 rankedChildren 的适配器，忽略 query，所以 `flow.Retrieve(ctx, "")` **不重新执行 BM25 或 embedding**。ES 读取、窗口预算和错误回退均由项目实现。

父章节查询是同索引的 `ids.values`，对 ES `_id` 精确读取，不是再次以用户 Query 搜索。返回后转换并过滤 section 类型，再按请求 ID 查找；请求 IDs 是期望集合，ES hits 是实际集合，二者可能不同。

### 4.5 动态窗口的精确规则与限制

长章节把全部 seeds 的绝对坐标减去 `section.StartRune`，转换为章节内 rune 坐标。检查章节起点非负、子范围不越界且非空、SectionID 相同，并要求切出的父原文与 seed.Content 完全一致。任一 seed 不符合条件，当前章节整体退回命中正文；不尝试猜测正确位置，也不只偷偷丢掉坏 seed。

先按源位置合并相邻/重叠范围，成本是唯一原文长度加上各不连续窗口间 `"\n\n[…省略…]\n\n"` 的长度。若全部命中成本超预算，依相关性顺序接纳每个命中；放不下时从右端逐字符缩短，尝试最长可放入的前缀，没有空间则跳过。已选内容不被低排序命中驱逐，且这一分支不追加周边原文。

若全部命中可保留，每轮按当前已排序窗口计算：

```text
剩余预算 = 8000 - 当前范围正文与省略标识的总 rune 数
step = max(1, 剩余预算 / (2 * 当前窗口数))   // 整数除法
```

按当前轮次的窗口快照依次尝试左侧、右侧增长。每次限制在原文章节边界内，合并候选范围，并仅在总成本仍 ≤8000 时接纳。合并后当前窗口数改变，下一轮重新计算 step。当前快照里的多个命中窗口若在本轮合成一个窗口，后续快照锚点可能继续扩展这个合并窗口；所以这是确定性、按源顺序打破平局的策略，**不是严格给每个原命中点等额分配预算**。本轮没有任何增长即结束，包括预算耗尽、到边界或当前尝试均放不下的情况。

输出按范围起点排序，从父原文直接切片，不拼接重叠 chunk。多窗口之间显式省略；`context_ranges` 是绝对 `[start,end)` 范围 JSON 数组。`StartRune/EndRune` 只包围最外侧范围，不能当作“中间全部返回”的证明。完整章节删除窗口说明与范围元数据；窗口、缺失父章节、无关系 chunk 和预算截断均标记不完整。

这段代码没有段落/代码块边界扩展、语义判断、动态 token 预算、窗口摘要或额外邻居 ES 查询。按 rune 截取能保持 UTF-8 编码，但可能切断组合字符、段落、列表或代码围栏。字段单位不是字节、token，也不是名为 StartOffset/EndOffset 的字段。

## 5. 核心函数调用链与上下文组装

### 5.1 按主链实际执行顺序定位

| 顺序 | 函数 / 组件 | 文件位置 | 输入 → 输出 / 职责 |
|---|---|---|---|
| 0，装配 | `NewKnowledgeRetriever`、`InitIndex`；`newAgentTools` | [knowledge_retriever.go](../internal/ai/retriever/knowledge_retriever.go#L59)、[full_agent.go](../internal/ai/agent/full_agent.go#L225)、[main.go](../cmd/main.go#L121) | 注入 ES、embedding、配置；注册文档工具；mapping 不兼容/依赖初始化失败则启动失败 |
| 1，入口 | `Chat`；`StreamChat → startRun → executeRun` | [chat_controller.go](../internal/controller/chat_controller.go#L147)、[chat_runs.go](../internal/controller/chat_runs.go#L231) | HTTP 用户 Query → Generate/Stream 调用；装配 Agent 超时 context |
| 2，消息 | `Generate/Stream → prepareContext → prepareAgentQuery → Graph` | [full_agent.go](../internal/ai/agent/full_agent.go#L350)、[session_context.go](../internal/ai/agent/session_context.go#L53) | 加载会话工作消息，构造 UserMessage；Graph 组装模型消息 |
| 3，决策 | `newADKAgentLambda → separatedADKStream → runner.Run` | [full_agent.go](../internal/ai/agent/full_agent.go#L182)、[answer_stream.go](../internal/ai/agent/answer_stream.go#L22) | 模型按需生成 `search_knowledge` ToolCall |
| 4，工具 | `NewSearchKnowledgeTool` 内部回调 | [knowledge_tools.go](../internal/ai/tools/knowledge_tools.go#L34)、[sources.go](../internal/ai/tools/sources.go) | SearchKnowledgeInput.Query 校验 → `KnowledgeSource.Search` |
| 5，总检索 | `Search → SearchDetailed → recall` | [knowledge_retriever.go](../internal/ai/retriever/knowledge_retriever.go#L200) | Query → 单路或混合召回候选 |
| 6，关键词路 | `searchBM25 → exactIdentifiers → Client.SearchHits` | [knowledge_retriever.go](../internal/ai/retriever/knowledge_retriever.go#L295)、[elasticsearch.go](../pkg/elasticsearch/elasticsearch.go#L175) | Query → DSL → SearchHit 切片 |
| 7，向量路 | `Embed → searchVector → Client.SearchHits` | [embedder.go](../internal/ai/embedder/embedder.go#L48)、[knowledge_retriever.go](../internal/ai/retriever/knowledge_retriever.go#L328) | Query → 向量 → HNSW kNN DSL → SearchHit 切片 |
| 8，转换融合 | `extractSearchHits → convertHitsToCandidates → fuseRRF / singleRouteResults` | [elasticsearch.go](../pkg/elasticsearch/elasticsearch.go#L316)、[recall.go](../internal/ai/retriever/recall.go#L72) | `_source/_score/_id` → 有分数/路信息的 RetrievedDocument 切片 |
| 9，重排选择 | `rerank → lexicalRerank → 可选 ModelReranker → diversifyChunks` | [knowledge_retriever.go](../internal/ai/retriever/knowledge_retriever.go#L444)、[model_reranker.go](../internal/ai/retriever/model_reranker.go#L36) | 排序候选 → 入选章节的全部 ranked hits |
| 10，章节扩展 | `expandSections → parent.NewRetriever → flow.Retrieve → OrigDocGetter` | [section_retriever.go](../internal/ai/retriever/section_retriever.go#L28) | 按 SectionID 聚合、Eino 父 ID 去重、ES ids 查询、全章或窗口/回退 |
| 11，窗口/回退 | `sectionWindow → selectHitRanges → mergeRuneRanges/rangeContextRunes`；`fallbackSection → renderHitChunks` | [section_retriever.go](../internal/ai/retriever/section_retriever.go#L187) | 多命中位置 → 预算内源窗口；或预算内已知片段正文 |
| 12，业务输出 | `expandSections` 末尾组装，`Search` 返回 | [section_retriever.go](../internal/ai/retriever/section_retriever.go#L140) | 按原相关性顺序去重、过滤空正文 → KnowledgeDocument 切片，无分数 |
| 13，工具输出 | `NewSearchKnowledgeTool` 格式化、MarshalIndent | [knowledge_tools.go](../internal/ai/tools/knowledge_tools.go#L97) | 文档列表 → 预算筛选 → SearchKnowledgeOutput JSON 字符串 |
| 14，模型继续 | Eino ToolsNode + ADK；`recoverableToolErrors` | [tool_errors.go](../internal/ai/agent/tool_errors.go#L28)、Eino 下列源码 | JSON → role=tool 消息，带调用 ID/工具名 → 下一轮调查模型输入 |
| 15，最终回答 | `captureContext → separatedADKStream → readFinalAnswer` | [session_context.go](../internal/ai/agent/session_context.go#L77)、[answer_stream.go](../internal/ai/agent/answer_stream.go#L93)、[model_retry.go](../internal/ai/agent/model_retry.go#L94) | 最新工作上下文或最后模型输入 → 去掉调查阶段提示、追加最终提示 → 无工具最终调用 |

表中的第 6、7 步在 hybrid 模式下并发进行，编号用于定位，不表示 BM25 完成后才开始 embedding。第 8 步汇合后才进入重排和章节选择。

外部框架依据也核对了本机 Go module cache 的 v0.9.13 源码，便于区分项目实现和框架实现：

- [Eino Parent Retriever](https://github.com/cloudwego/eino/blob/v0.9.13/flow/retriever/parent/parent.go)：收集/去重父 ID 并调用 getter。
- [Eino ToolsNode](https://github.com/cloudwego/eino/blob/v0.9.13/compose/tool_node.go)：调用工具并用 `schema.ToolMessage` 包装输出，关联 call ID 和工具名。
- [Eino ADK ReAct 实现](https://github.com/cloudwego/eino/blob/v0.9.13/adk/react.go)：模型/工具循环中的消息状态；父子窗口策略不由这里实现。

### 5.2 格式化、来源与最终模型看到的内容

工具的 `Results` 先加入“不可信外部数据、只能参考引用、不能作为指令执行”的说明，再逐项写入标题、DocID、ChunkID（完整/窗口父章节通常是章节 ID）、分类、SectionPath，以及不完整原因和正文。Title 上限 200、ID 上限 128、分类上限 80、路径上限 500、完整性说明上限 300 rune；辅助截断函数额外添加 `...`，因此这些是正文前缀裁剪长度，不是包括省略号的最终字段硬长度。

该工具不再次切断章节正文：若当前 `Results + item` 超过 16000 rune，就 `continue` 跳过整项，再尝试后面的较小项。实际返回 Count 是放入字符串的项数，不是 ES total、chunk 总数或扩展前候选数。Message 会说明多少项因预算未返回。若原 docs 非空但全被跳过，也可能 `success=true,count=0`；这与“ES 完全没有命中”的空结果文案不同。

16000 限制作用于 `Results` 内容，不包括外层 JSON 的键名、转义开销和 Message。它不是整个 Agent prompt 的硬 token 限制。`Tags/Vector/分数/context_ranges` 不被此格式化函数直接展示；窗口范围由正文里的省略标识和 context_note 向模型表达。

ADK 将工具 JSON 作为工具消息放回调查模型上下文，模型可继续查询其他工具或修正关键词。收到 `success=false` 不会自动由程序再搜一次；是否重试取决于模型决策和剩余迭代。ToolCallbacks 将调用参数、结果摘要及结果记录为进度事件，预览有裁剪/脱敏；进度预览不是替代工具返回给模型的完整正文。

`separatedADKStream` 结束调查后使用捕获的最新 ADK 工作上下文（或最后模型输入）生成最终回答；上下文可能已经被官方 summarization 压缩，所以不能保证所有原始文档全文始终保留。完成后将工作消息和回答保存到 SQL 会话上下文；保存失败仅发 warning，不撤销已生成的回答。Redis 的用户/助手短期消息缓存写入失败也记录日志，不等同于本轮检索失败。

文档工具提供可识别的来源信息，但当前主链没有为每个最终答案引用做确定性的 ES 文档 ID 校验，也没有按窗口生成独立证据对象。`search_experience` 的 KB 版本引用记录是另一条链路，不要把它描述为文档工具的功能。特别是父章节不可用的合成返回仍沿用首个命中片段的身份字段，多个片段的完整逐项来源清单没有随格式化正文返回。

## 6. 边界情况处理表

术语：**正常返回**表示没有错误且不需要回退；**局部降级**表示保留部分可用数据并标明限制；**直接错误**表示当前函数返回 Go error；**失败观察**表示上层将普通 Go error 转成 `success=false`、Go error=nil，Agent 可继续。HTTP 入口不会经过 Agent 工具错误转换。

| 触发条件 | 对应函数 | 当前处理策略 | 最终结果 / 错误 | 类型 |
|---|---|---|---|---|
| 模型没有选择文档工具 | ADK 模型/工具循环 | 不执行本次文档召回 | 使用已有消息/其他工具结果 | 正常，无召回 |
| 工具 query 空、nil 输入或超 1000 rune | 文档工具回调 | 返回明确校验失败 JSON | `success=false,error=nil` | 失败观察，无可用检索数据 |
| 直接调用 SearchDetailed 的空 Query | `SearchDetailed` | 返回 `query cannot be empty` | Search 传递错误；工具转换失败观察；HTTP 返回错误 | 直接错误 → 因入口而异 |
| bm25 单路 ES 失败 | `recall` | 不自动改用 vector | 返回 BM25 错误 | 直接错误 → 工具可转观察 |
| vector 单路 embedding/ES 失败 | `Embed/searchVector/recall` | 不执行 BM25 回退 | 返回 vector 错误 | 直接错误 → 工具可转观察 |
| hybrid 向量失败，BM25 成功，开关 true | `recall/markDegraded` | 保留 BM25，标记向量失败，不做双路 RRF | Detailed 有降级标记；BM25 空也允许返回空 | 局部降级，可为空 |
| 同上但回退开关 false，或 BM25 也失败 | `recall` | 返回 vector 错误 | 无成功检索输出 | 直接错误 → 工具可转观察 |
| hybrid BM25 失败，向量成功且非空 | `recall/markDegraded` | 保留 vector；这条分支不检查 fallback_to_bm25 | 返回向量命中并标记 BM25 失败 | 局部降级 |
| hybrid BM25 失败，向量成功但为空 | `recall` | 没有可用向量数据，返回 BM25 错误 | 不当作正常空结果 | 直接错误 → 工具可转观察 |
| 两路都成功但某一路为空 | `fuseRRF` | 另一条路仍按 RRF 规则参与；不标路失败 | 正常结果或两路均空 | 正常返回 |
| 全部正常无命中 | `Search → expandSections → 文档工具` | 返回空列表 | `success=true,count=0`，未找到相关文档 | 正常空结果 |
| 知识索引不存在/HTTP 非成功 | `Client.SearchHits` | 返回状态错误，可能触发召回单路回退 | 不像日志客户端那样特殊转换 404 为空 | 直接错误或单路降级 |
| ES 响应 JSON / 顶层 hits 结构异常 | `SearchHits/extractSearchHits` | 返回解码/结构错误 | 由所属召回路或父读取分支处理 | 直接错误，可能回退 |
| ES 单条 hit 非对象或缺 _source | `extractSearchHits` | 跳过坏记录，无额外 warning | 候选变少，全部跳过可变为空 | 宽容解析，未显式标降级 |
| 可选 model 重排未配置/运行失败/无有效分数 | `rerank/markRerankerFallback` | 保留先前词法重排 | `lexical_fallback`、Degraded=true | 局部降级 |
| model 仅返回部分候选的有效评分 | `rerank` | 已评分项更新，未评分项保留词法值，重新排序 | 混合得分结果，无整体错误 | 正常容错 |
| 重复 chunk 或同章多个命中 | RRF/单路转换、`diversifyChunks/expandSections` | 按身份去重；入选章节保留多个 seed；最终章节只输出一次 | 多命中参与窗口，正文不重复拼接 | 正常聚合 |
| chunk 无 SectionID | `expandSections` 末尾 | 不进 Eino 父读取；按原身份去重，必要时裁到 8000 | 原片段，complete=false，说明无章节关系 | 无法扩展的局部降级 |
| parent.NewRetriever 创建失败 | `expandSections` | 直接 return；尚未进入 flow 读取处理 | Go error | 直接错误，正常配置下是防御分支 |
| 父查询成功但某 section 缺失/类型不符/正文为空 | OrigDocGetter、`fallbackSection` | 仅该章节用已命中正文；不丢弃其他章节 | complete=false，父章节不可用说明 | 章节局部降级 |
| 父章节批量 ES 查询失败，ctx 仍有效 | `flow.Retrieve` 错误分支 | 发 warning；本次全部有章节关系的 seed 分组回退 | 多命中正文，complete=false，error=nil | 整批扩展降级 |
| 父章节长度 ≤8000 rune | OrigDocGetter | 直接返回完整章节，不调用 sectionWindow/验证 seed 偏移 | complete=true；旧窗口标记清理 | 正常完整返回 |
| 父章节长度 >8000 rune | `sectionWindow` | 合并命中，预算内动态增长，按源位置输出 | complete=false，窗口说明、context_ranges | 预算受限的局部结果 |
| 唯一命中范围及标识成本 >8000 | `selectHitRanges` | 相关性优先，缩短当前命中右端或跳过，不扩展 | 截断/省略命中并说明 | 预算降级 |
| 长章节任一 seed 越界、空范围、跨章或正文不匹配 | `sectionWindow` | 当前章节整体 fallbackSection，不猜测 offset | 保留预算内命中正文；error=nil | 章节局部降级 |
| 长章节 section.StartRune 为负 | `sectionWindow` | 同上，注明父章节偏移无效 | 命中正文，complete=false | 章节局部降级 |
| sectionWindow 没有 seed | `sectionWindow → flow` | 返回 no hits；正常聚合路径通常不可达 | ctx 有效时导致整批扩展回退 | 直接错误 → 扩展层吸收 |
| 相邻 chunk 在 ES 缺失 | `sectionWindow` | 当前不查询邻居，依靠父章节原文截取 | 父原文可用则不受邻居记录缺失影响 | 不需要回退 |
| fallback 的片段互相重叠 / 无有效位置 | `fallbackSection/renderHitChunks` | 有效范围去重合并；未知位置内容按内部不连续位置组织，不填补缺失原文 | 含省略标识，原文坐标置零；超预算按相关性截断 | 明示限制的局部降级 |
| 聚合后正文为空或纯空白 | `expandSections` 输出循环 | 不加入返回列表 | 结果变少或为空，error=nil | 正常过滤 |
| 工具总预算放不下当前整项 | 文档工具格式化 | 跳过整项，继续尝试后面项；Message 说明数量 | 返回少于 docs 的 Count，success=true | 工具预算筛选 |
| 请求上下文取消/超时 | 文档工具、ES context、expandSections、sectionWindow、ADK | 实际 context 终止时检查并抛出；中间召回/重排可能先走降级，Search 后续检查仍终止 | 工具/中间件不转普通失败观察；Run 取消或失败 | 错误终止 |
| 内部父读取返回超时类错误，但请求 ctx 未终止 | `expandSections` 的 flow 错误分支 | 这里只检查 ctx.Err，因此仍可能转为全部 seed 回退 | error=nil，扩展失败说明 | 错误被扩展层吸收 |
| 调用工具发生参数解码等普通 Go 错误 | `recoverableToolErrors` | 转为 TOOL_QUERY_FAILED JSON，保留提示 | error=nil，Agent 可继续；无自动重试保证 | 失败观察 |
| HTTP 普通/详细搜索遇到 Go error | `KnowledgeController.Search/SearchDetailed` | 返回 DependencyUnavailable；不走工具中间件 | HTTP 错误响应 | 入口直接失败 |
| 模型自身错误/最终回答失败 | `separatedADKStream/readFinalAnswer` | 可重试模型错误由现有模型策略处理；不可恢复错误向上传递 | 同步错误或流式 Run 终止；不能伪称 RAG 成功即答复成功 | 上层错误终止 |
| 工作上下文/短期缓存保存失败 | `contextRun.save/Generate/Stream` | 发 warning 或记录日志，不回滚已生成回答 | 当前回答可返回，后续会话可能回退 | 持久化层降级 |

不能把“error=nil”一律读成“完整知识召回成功”：它也可能是正常空结果、只保留片段、工具失败 JSON 或预算筛选后的更少内容。对于路降级，Detailed 的 Degraded/DegradationReason 可查看，但普通 Search/工具格式没有完整传递这组路状态；章节完整性元数据只说明返回范围，不等同于全部检索路健康。

## 7. 面试讲解版本（约 2～3 分钟）

> 项目里的技术文档检索是主 Agent 按需调用的工具。用户问题先进入 Eino ADK，模型决定是否调用 search_knowledge，并提供检索关键词；系统没有在每次对话开始前固定预取文档。
>
> 检索层用 Elasticsearch 做关键词和向量两路召回。关键词路使用多字段全文和短语查询，给标题、章节路径以及标识符匹配更高权重。向量路先生成查询向量，再通过 ES 的 HNSW 近似 kNN 检索 chunk；相比原来的逐条 cosine 脚本评分，它通常减少查询时的比较，但可能遗漏近邻，需要用真实问题集衡量速度与召回的取舍。正常双路用 RRF 融合排名，避免直接相加两种不同量纲的原始分数；之后做项目的词法重排和文档多样性筛选。ES/Lucene 提供全文评分、向量索引和图搜索，参数、RRF、筛选和降级策略由项目代码实现。
>
> 我们采用小 chunk 定位、大 section 补充上下文，因为小片段容易匹配具体问题，但技术排障往往需要完整的现象、步骤和验证条件。Markdown 入库时保存了 chunk 和它所属章节的原文，命中后用 section_id 取回章节。这里复用 Eino Parent Retriever 收集和去重父 ID，ES 读取与窗口规划则是项目自己的业务逻辑。
>
> 没有直接返回整篇文件，是因为它可能包含大量无关章节，会消耗模型上下文并干扰回答。章节不超过八千字符就完整返回；长章节聚合多个命中位置，先合并重叠范围、保留命中内容，再将剩余预算分给窗口两侧，遇到边界或合并就重新分配。远处窗口按原文顺序输出，用省略标识明确不连续。如果命中本身超预算，就优先保留排序靠前的内容，并标记截断。
>
> 可用性上，混合召回的一条路失败可以按条件退回另一条；父章节缺失或偏移不可信时，保留该章节预算内的多个已命中片段。我们区分完整章节、窗口和父读取失败，不把局部结果说成全文。普通检索失败会作为失败工具结果交还模型，让它有机会修正条件或说明数据缺口；请求取消和超时会终止执行。
>
> 最后，检索层返回扩展文档，工具附上标题、来源 ID、完整性说明和正文，按一万六千字符总预算筛选，再由 Eino 加入工具消息上下文。调查结束后另做一次无工具模型调用生成最终回答。目前窗口仍是字符范围策略，不保证完整段落或代码块，也没有确定性的最终引用校验；这是我会明确说明的实现边界。

## 8. 代码核对清单与尚未实现的能力

- 主 Graph 没有预取文档节点；辅助 newRetrieverLambda、旧多 Agent 路径和 MCP facade 均已单独区分。
- 模式/池大小/重排分数依据 `recall.go`、`knowledge_retriever.go` 和配置核对；图未将默认 lexical 描绘成必选 LLM 重排。
- seeds 当前为 `map[string][]KnowledgeDocument`，不是单命中；上游 diversity 也保留入选章节的多命中。
- 长章节不再发起固定 ±1 chunk 查询；图未画入额外邻居搜索、语义扩展或原文摘要。
- 无 SectionID 的 chunk 未进入 expanded，但仍在最终 results 遍历中保留；不是被 Eino 无条件丢弃。
- 父查询整批失败与某章 offset 失败的回退范围不同；短章节直接返回，不验证每个 seed offset，也不额外校验 section.EndRune 与正文长度一致。
- 扩展结果不携带 ES 分数；父缺失合成正文的来源字段仍来自首个 seed，没有独立的全量片段引用表。
- ES 适配只读取 hits；当前没有显式检查响应 `timed_out` 或 `_shards.failed` 来标记部分成功，因此不能宣称所有 ES 部分失败都已识别。
- 字符窗口不保证语义结构/字符簇完整；预算策略不保证全局最优区间，也不感知真实模型剩余 token。候选池外的相关命中没有被重新搜索。
- 未运行真实模型或 ES 数据集评估，不能据这些机制测试声称召回率、答案准确率已经提升。

现有回归依据：[section_retriever_test.go](../internal/ai/retriever/section_retriever_test.go)、[markdown_chunker_test.go](../internal/ai/retriever/markdown_chunker_test.go)、[knowledge_context_test.go](../internal/ai/tools/knowledge_context_test.go)、[answer_stream_test.go](../internal/ai/agent/answer_stream_test.go)、[tool_errors_test.go](../internal/ai/agent/tool_errors_test.go)。它们分别验证多命中窗口、父子关系、工具预算/取消、工具信息进入最终模型上下文和可恢复工具错误；不等同于真实环境效果评测。

本次文档验证：两张图通过 Mermaid 11.12.0 语法解析并在浏览器中实际渲染；本地源码链接及行号范围均检查可用。仓库只新增本文档，业务实现、依赖文件与配置没有修改。
