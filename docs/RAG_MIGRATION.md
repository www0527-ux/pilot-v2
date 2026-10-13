# Knowledge index migration to chunk schema

Pilot now stores one Elasticsearch document per knowledge Chunk. `elasticsearch.knowledge_index` must point to an index whose vector dimension matches `embedding.dimension` and whose mapping includes `doc_id`, `chunk_id`, chunk offsets, and `vector`.

The service adds compatible fields to an existing index. If Elasticsearch reports an incompatible dense-vector dimension, startup fails with an explicit migration error; Pilot no longer silently switches to an undiscoverable index.

For an incompatible or whole-document legacy index:

1. Keep the old index read-only as the rollback source.
2. Choose an explicit versioned name such as `pilot_knowledge_chunks_v1` in both local and Docker config.
3. Start Pilot once to create and validate the new mapping.
4. Re-upload the authoritative source documents through `POST /api/v1/knowledge/upload`. The response must contain stable `doc_id` and `chunk_count`.
5. Search through both compatibility and detailed endpoints. Confirm that returned `content` is a relevant Chunk, `chunk_id` is present, per-route/rerank scores are exposed, and no result reconstructs the whole document.
6. Delete one test document twice by `doc_id`; the first response must report its Chunk count and the second `deleted_count: 0`.
7. Only after source-count and search checks pass, retire the old index using the operator's normal recoverable retention process.

Deterministic code checks:

```bash
go test ./internal/ai/retriever ./pkg/elasticsearch ./internal/controller -count=1
```

Real Elasticsearch verification additionally requires `make verify` and `make e2e`; it must not be reported as passed when the service or credentials are unavailable.
