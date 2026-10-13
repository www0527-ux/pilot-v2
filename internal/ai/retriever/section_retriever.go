package retriever

import (
	"context"
	"fmt"

	einoretriever "github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/flow/retriever/parent"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/pkg/types"
)

const maxSectionContextRunes = 8000

// The existing mixed recall/rerank runs once. Eino receives its ranked child
// results and owns collecting/deduplicating parent IDs.
type rankedChildren []*schema.Document

func (r rankedChildren) Retrieve(ctx context.Context, _ string, _ ...einoretriever.Option) ([]*schema.Document, error) {
	return r, ctx.Err()
}

func (r *KnowledgeRetriever) expandSections(ctx context.Context, results []RetrievedDocument) ([]types.KnowledgeDocument, error) {
	children := rankedChildren{}
	seeds := map[string]types.KnowledgeDocument{}
	for _, result := range results {
		doc := result.Document
		if doc.SectionID != "" {
			children = append(children, &schema.Document{ID: doc.ChunkID, Content: doc.Content, MetaData: map[string]any{"section_id": doc.SectionID}})
			if _, exists := seeds[doc.SectionID]; !exists {
				seeds[doc.SectionID] = doc
			}
		}
	}
	expanded := map[string]types.KnowledgeDocument{}
	if len(children) > 0 {
		flow, err := parent.NewRetriever(ctx, &parent.Config{Retriever: children, ParentIDKey: "section_id", OrigDocGetter: func(ctx context.Context, ids []string) ([]*schema.Document, error) {
			hits, err := r.es.SearchHits(ctx, r.indexName, map[string]interface{}{"size": len(ids), "query": map[string]interface{}{"ids": map[string]interface{}{"values": ids}}})
			if err != nil {
				return nil, err
			}
			parents := map[string]types.KnowledgeDocument{}
			for _, hit := range hits {
				doc := r.convertToDocument(hit.Source)
				if doc.RecordType == "section" {
					parents[doc.SectionID] = doc
				}
			}
			var docs []*schema.Document
			for _, id := range ids {
				doc, ok := parents[id]
				if !ok {
					doc = seeds[id]
					doc.Metadata = cloneMetadata(doc.Metadata)
					doc.Metadata["context_complete"] = "false"
					doc.Metadata["context_note"] = "父章节不可用，仅返回命中片段"
				} else {
					doc.Metadata = cloneMetadata(doc.Metadata)
					doc.Metadata["context_complete"] = "true"
					if len([]rune(doc.Content)) > maxSectionContextRunes {
						var windowErr error
						doc, windowErr = r.sectionWindow(ctx, doc, seeds[id])
						if windowErr != nil {
							return nil, windowErr
						}
					}
				}
				docs = append(docs, &schema.Document{ID: id, Content: doc.Content, MetaData: map[string]any{"record": doc}})
			}
			return docs, nil
		}})
		if err != nil {
			return nil, err
		}
		docs, err := flow.Retrieve(ctx, "")
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			progress.Emit(ctx, progress.Event{Kind: "notice", Status: "warning", Summary: "章节上下文读取失败，保留已召回片段"})
			for id, doc := range seeds {
				doc.Metadata = cloneMetadata(doc.Metadata)
				doc.Metadata["context_complete"] = "false"
				doc.Metadata["context_note"] = "章节读取失败，仅返回命中片段"
				expanded[id] = doc
			}
		} else {
			for _, doc := range docs {
				expanded[doc.ID] = doc.MetaData["record"].(types.KnowledgeDocument)
			}
		}
	}
	var docs []types.KnowledgeDocument
	seen := map[string]bool{}
	for _, result := range results {
		doc := result.Document
		if doc.SectionID != "" {
			if seen[doc.SectionID] {
				continue
			}
			seen[doc.SectionID] = true
			doc = expanded[doc.SectionID]
		} else {
			doc.Metadata = cloneMetadata(doc.Metadata)
			doc.Metadata["context_complete"] = "false"
			doc.Metadata["context_note"] = "旧索引未保存章节关系，返回原有检索内容，无法保证完整章节"
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func (r *KnowledgeRetriever) sectionWindow(ctx context.Context, section, seed types.KnowledgeDocument) (types.KnowledgeDocument, error) {
	query := map[string]interface{}{"size": 3, "sort": []interface{}{map[string]interface{}{"chunk_index": "asc"}}, "query": map[string]interface{}{"bool": map[string]interface{}{
		"filter":   []interface{}{map[string]interface{}{"term": map[string]interface{}{"section_id": section.SectionID}}, map[string]interface{}{"range": map[string]interface{}{"chunk_index": map[string]interface{}{"gte": maxInt(0, seed.ChunkIndex-1), "lte": seed.ChunkIndex + 1}}}},
		"must_not": []interface{}{map[string]interface{}{"term": map[string]interface{}{"record_type": "section"}}},
	}}}
	hits, err := r.es.SearchHits(ctx, r.indexName, query)
	if err != nil {
		return section, err
	}
	start, end := seed.StartRune, seed.EndRune
	for _, hit := range hits {
		doc := r.convertToDocument(hit.Source)
		if doc.SectionID == section.SectionID {
			start = minInt(start, doc.StartRune)
			end = maxInt(end, doc.EndRune)
		}
	}
	if end-start > maxSectionContextRunes {
		start, end = seed.StartRune, seed.EndRune
	}
	runes := []rune(section.Content)
	from, to := start-section.StartRune, end-section.StartRune
	if from < 0 || to > len(runes) || to <= from {
		return section, fmt.Errorf("invalid source range for section %s", section.SectionID)
	}
	section.Content = string(runes[from:to])
	section.StartRune, section.EndRune = start, end
	section.Metadata["context_complete"] = "false"
	section.Metadata["context_note"] = "章节过长，返回命中片段及预算内相邻片段，未返回完整章节"
	return section, nil
}

// Keep this adapter independent of Markdown rendering and Agent behavior.
var _ einoretriever.Retriever = rankedChildren(nil)
