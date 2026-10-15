# Markdown 技术指南入库与父子检索

本轮只支持 UTF-8 `.md` / `.markdown` 文件，以及原有 JSON 接口粘贴的 Markdown 正文。不解析 PDF、Word 或扫描件，不通过模型猜测文档结构。

## 为什么保留 ES，复用 Eino

ES 负责全文关键词、精确标识符和向量检索；Eino 提供文档转换与父子检索的组合能力。项目配置 ES 8.12.0，现有链路自行生成向量后写入 `text + dense_vector`，本轮不升级到 `semantic_text`。

- `goldmark v1.7.13`：解析真实 Markdown 标题、段落、列表、代码块；源位置适配器记录原文边界，不重新渲染或裁剪代码缩进。管道表格在 CommonMark 中作为段落，由薄适配层识别分隔行并保护整个块。
- Eino 官方 `recursive.NewSplitter`：只对过长普通段落递归切分，配置 Unicode rune 长度、中文标点、保留分隔符；原字符簇工具保护组合字符和 emoji。
- Eino v0.9.13 `parent.NewRetriever`：接收已经完成混合召回和重排的子片段，收集并去重章节 ID，再调用 ES 章节读取函数。不会再执行一次召回。
- 保留项目的双路错误降级、精确匹配权重、可解释得分和重排。没有直接引入依赖较新 ES SDK 的官方 ES8 适配器，也没有重写父级 ID 收集流程。

## 入库流程

`POST /api/v1/knowledge/upload` 支持两种请求：

1. 原有 JSON：`title`、`content`、`category`、可选 `tags` / `metadata`。
2. multipart：`file` 为一个 Markdown 文件，可选 `title` / `category`；省略时采用文件名和 `runbook`。

文件最多 256 KiB，请求最多 512 KiB，正文沿用 50000 rune 限制。检查扩展名、UTF-8、NUL 和内容类型，拒绝伪装成 `.md` 的 PDF/二进制文件。页面“知识库 → 参考文档库 · 上传与检索”中可选择文件或粘贴正文。

链路：`Upload → IndexDocument → MarkdownChunks → Embed → BulkIndexDocuments`。

每个真实顶层标题划分一个章节，保存其标题层级路径。章节里的相邻短块在目标大小内合并；过长普通段落使用 Eino 递归切分。代码、列表、管道表格保持完整，单个结构块最多 8000 rune，超过时明确要求拆成多个块，不静默破坏结构。800 rune 是普通块的目标，不是所有结构块的硬上限。

子片段携带 `section_id`、`section_path`、`splitter_version` 和原文 rune 范围。向量输入包含章节路径和片段正文。原文范围不包含人为补入的路径。父章节以 `record_type=section` 保存，未生成向量；BM25 显式排除父记录，向量检索在 kNN 预过滤中要求存在向量并排除父记录。未保存 `record_type` 的旧向量片段仍可参与。

## HNSW 向量召回

`searchVector()` 使用 ES 8.12 的顶层 `knn` 查询，替代原来 `script_score + cosineSimilarity` 对所有匹配向量的逐条评分。现有 `dense_vector` 映射已经配置 `index=true` 和 `similarity=cosine`，ES 8.12 默认由 Lucene 构建和维护 HNSW；项目不自行实现图算法。

- `size=k=limit`：沿用向量路已有的召回数量，即 `max(vector_top_k, poolLimit)`，不是最终传给 Agent 的章节数量。默认配置下为 15。
- `num_candidates=min(10000,max(100,10*k))`：每分片候选数量，默认配置下为 150；这是起始启发式，没有经过业务数据集调优。它不是相似度计算次数上限，也不是最终结果条数。
- ES 8.12 要求 `k < num_candidates <= 10000`；项目在调用 ES 前拒绝不在 `[1,9999]` 的向量召回数量，不静默缩小配置。
- `knn.filter` 在近邻检索期间排除 section，不采用可能导致最终不足 k 条的事后过滤。
- 原脚本分数为 `1+cosine`；原生 cosine kNN 分数为 `(1+cosine)/2`，所以配置 `boost=2.0` 保持旧分数尺度，避免同时改变纯向量模式下词法重排的相对权重。RRF 仍按两路排名融合。
- 查询失败继续沿用已有策略：hybrid 下 BM25 成功且允许 fallback 时降级；vector 单路或关闭 fallback 时返回错误。不额外静默退回全量脚本扫描。

HNSW 将向量组织成分层邻接图，上层快速定位大致区域，底层维护候选并细化搜索；通常减少查询时的向量比较，但近似搜索可能漏掉真实最近邻。建图、维护和内存占用也有成本。ES 在少量过滤结果等情况下可能自行选择暴力搜索，因此不能承诺每次都只走图或固定比较次数。

面试可以这样说明：原实现的精确向量召回需要比较所有符合条件的 chunk；这次复用 ES 已提供的 HNSW 索引，用候选搜索换取更低的潜在查询成本，保留 Embedding、RRF、重排、章节扩展和错误降级。项目实现的是检索策略与参数接入，图算法由 ES/Lucene 提供。不能在没有测试数据时声称延迟降低了某个比例或召回率保持不变。

上线前先读取实际索引的 `GET /<knowledge_index>/_mapping`，确认向量维度、`index=true`、cosine 和 HNSW 索引类型；代码中的 Mapping 不能证明现有索引状态。已符合的索引通常无需重新上传，不符合时按 `RAG_MIGRATION.md` 创建版本化索引并重新入库。在相同向量、同一数据快照和问题集上，将旧脚本结果作为向量近邻基线，对比 kNN 的 Recall@k、ES `took` 和客户端 p50/p95 延迟，再调候选数。近邻 Recall@k 不等于业务相关性，需要同时检查命中章节与答案依据。

依据：[ES 8.12 kNN](https://www.elastic.co/guide/en/elasticsearch/reference/8.12/knn-search.html)、[dense_vector](https://www.elastic.co/guide/en/elasticsearch/reference/8.12/dense-vector.html)、[Search API 参数](https://www.elastic.co/guide/en/elasticsearch/reference/8.12/search-search.html)。单元测试验证查询契约、结果转换、空结果、参数边界、错误与 BM25 降级，不等于已经完成真实 ES 性能验证。

## 检索和返回

`SearchDetailed` 仍返回带分数的子片段用于诊断。`Search` 在这批结果上执行 Eino Parent Retriever，供页面普通搜索和 `search_knowledge` 使用：

1. 按相关性选择章节，同章节不重复占据 `FinalTopK` / 每文档名额，但保留入选章节在候选池中的全部命中片段。因此 `SearchDetailed` 的片段数可能超过 `FinalTopK`，`Search` 的最终章节/无章节片段项数仍受该限制。两路召回与重排顺序不变。
2. 章节不超过 8000 rune 时，返回完整章节，标记 `context_complete=true`。
3. 长章节按全部命中的 `StartRune` / `EndRune`（整篇原文的 Unicode rune 坐标，右端不包含）定位。先合并相邻/重叠范围并保留所有命中；命中本身超过预算时，按已有相关性顺序保留，必要时截断。仅在所有命中均能保留时，每轮将剩余预算均分给当前各窗口的左右两侧，按原文顺序扩展并立即合并相邻窗口。到达章节边界或合并后释放的预算在下一轮重新分配；余量不足一个完整轮次时按原文顺序尝试单 rune 扩展，直到预算耗尽或无法再扩展。直接截取已保存的章节原文，不额外查询邻居 chunk，也不依赖其存在。
4. 不连续窗口之间插入 `[…省略…]`，标识和换行也计入每项 8000 rune 预算。窗口返回 `context_complete=false`，`context_note` 区分窗口与命中超预算。新增 `context_ranges` 元数据为原文绝对范围的 JSON 数组，例如 `[[100,400],[1200,1600]]`；`StartRune/EndRune` 是最外侧范围边界，不意味着整个包围区间连续返回。
5. 父章节缺失、空正文或 ES 查询失败时，尽可能保留该章节全部已命中片段，按原文顺序合并位置可靠的重叠片段；缺口或位置未知的片段之间标记省略，超预算按相关性保留。无法确定合成内容的连续范围时 `StartRune/EndRune` 置零。长章节命中偏移越界、为空或与父原文不一致时，仅降级该章节，不影响其他正常章节。完整章节保持 `context_complete=true`，降级原因保存在 `context_note`；取消/超时仍向上传递。
6. 工具结果采用 16000 rune 总预算，优先保留完整返回项；超预算项明确说明未返回。不再每条截断为 1200 rune。

这不保证模型每次读取整个文件。不同问题的章节不应默认全部注入。标题路径和完整性标记帮助模型明确所读范围。

动态扩展使用字符位置，不保证窗口一定停在完整段落、代码围栏或字符簇边界；也不对未召回区域重新评估语义相关性。候选池外的命中仍无法参与聚合。每章节预算与工具总预算是两层独立限制，工具仍可能跳过完整返回项。

## 兼容与验证

旧索引记录继续参与检索，没有章节关系的结果按原片段返回。不会删除旧数据或自动重建索引。重新上传 Markdown 后启用结构切分；自动生成的文档 ID 纳入切分版本和大小参数，避免不同策略写到同一组片段 ID。旧文档与新版本可能同时存在，可以在确认新版本后通过既有删除接口移除旧文档 ID。

新增覆盖：代码中伪标题、代码缩进、Setext 标题、表格完整性、Unicode/source 范围、父子入库、父章节去重、长章节窗口、文件验证、工具预算和取消。测试不调用远端模型。

这验证了机制正确性，尚未证明真实排障指南的召回率提高。后续使用相同指南和问题集对比命中章节、步骤完整性、模型引用与入库成本。

## 阅读顺序

1. `internal/ai/retriever/knowledge_retriever.go`：入库和检索的总编排。
2. `internal/ai/retriever/markdown_chunker.go`：结构边界与 Eino 递归切分。
3. `internal/ai/retriever/section_retriever.go`：Eino Parent Retriever 配置及 ES 章节适配。
4. `internal/ai/tools/knowledge_tools.go`：章节结果如何交给模型。
5. `internal/controller/knowledge_controller.go`：文件上传边界。
