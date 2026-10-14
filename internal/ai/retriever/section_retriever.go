package retriever

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	einoretriever "github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/flow/retriever/parent"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/pkg/types"
)

const maxSectionContextRunes = 8000
const contextOmission = "\n\n[…省略…]\n\n"

// The existing mixed recall/rerank runs once. Eino receives its ranked child
// results and owns collecting/deduplicating parent IDs.
type rankedChildren []*schema.Document

func (r rankedChildren) Retrieve(ctx context.Context, _ string, _ ...einoretriever.Option) ([]*schema.Document, error) {
	return r, ctx.Err()
}

func (r *KnowledgeRetriever) expandSections(ctx context.Context, results []RetrievedDocument) ([]types.KnowledgeDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	children := rankedChildren{}
	seeds := map[string][]types.KnowledgeDocument{}
	seenChunks := map[string]bool{}
	//只要有片段被命中的doc，都会返回，chileren用来记录
	for _, result := range results {
		doc := result.Document
		//片段记录了所属的章节，进行章节拓展
		if doc.SectionID != "" {
			key := doc.SectionID + "\x00" + doc.ChunkID
			if doc.ChunkID == "" {
				key = fmt.Sprintf("%s\x00%d:%d:%s", doc.SectionID, doc.StartRune, doc.EndRune, doc.Content)
			}
			if seenChunks[key] {
				continue
			}
			seenChunks[key] = true
			children = append(children, &schema.Document{
				ID:      doc.ChunkID,
				Content: doc.Content,
				MetaData: map[string]any{
					"section_id": doc.SectionID,
				},
			})
			// 保留排序后的全部命中，后续预算不足时优先保留靠前的片段。
			seeds[doc.SectionID] = append(seeds[doc.SectionID], doc)
		}
	}
	expanded := map[string]types.KnowledgeDocument{}
	if len(children) > 0 {
		// children.Retrieve() 返回已有的子片段
		// 	↓
		// 读取各片段 MetaData["section_id"]
		// 	↓
		// 去重，得到 ids []string
		// 	↓
		// 调用提供的 OrigDocGetter(ctx, ids)
		flow, err := parent.NewRetriever(ctx, &parent.Config{
			Retriever:   children,
			ParentIDKey: "section_id",
			OrigDocGetter: func(
				ctx context.Context,
				ids []string,
			) ([]*schema.Document, error) {
				//得到召回的section数据集，各自保存着章节原文
				hits, err := r.es.SearchHits(ctx, r.indexName, map[string]interface{}{"size": len(ids), "query": map[string]interface{}{"ids": map[string]interface{}{"values": ids}}})
				if err != nil {
					return nil, err
				}
				parents := map[string]types.KnowledgeDocument{}
				for _, hit := range hits {
					//source转换为业务type
					doc := r.convertToDocument(hit.Source)
					if doc.RecordType == "section" {
						parents[doc.SectionID] = doc
					}
				}
				var docs []*schema.Document
				for _, id := range ids {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					doc, ok := parents[id]
					if !ok || doc.Content == "" {
						//处理部分找回section失败的情况
						doc = fallbackSection(seeds[id], "父章节不可用，返回预算内的已命中片段")
					} else {
						doc.Metadata = cloneMetadata(doc.Metadata)
						//先假定返回完整章节
						doc.Metadata["context_complete"] = "true"
						delete(doc.Metadata, "context_note")
						delete(doc.Metadata, "context_ranges")
						//长度过长就部分返回
						if len([]rune(doc.Content)) > maxSectionContextRunes {
							var windowErr error
							//依据全部命中的原文范围，合并窗口并按剩余预算扩展。
							doc, windowErr = r.sectionWindow(
								ctx,
								doc,       //原文
								seeds[id], //命中片段集合
							)
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
		//执行补全
		docs, err := flow.Retrieve(ctx, "")
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			progress.Emit(ctx, progress.Event{Kind: "notice", Status: "warning", Summary: "章节上下文读取失败，保留已召回片段"})
			for id, hits := range seeds {
				expanded[id] = fallbackSection(hits, "章节读取失败，返回预算内的已命中片段")
			}
		} else {
			for _, doc := range docs {
				expanded[doc.ID] = doc.MetaData["record"].(types.KnowledgeDocument)
			}
		}
	}
	var docs []types.KnowledgeDocument
	seen := map[string]bool{}
	//将扩展部分，放入result
	for _, result := range results {
		doc := result.Document
		if doc.SectionID != "" {
			if seen[doc.SectionID] {
				continue
			}
			//实现去重
			seen[doc.SectionID] = true
			doc = expanded[doc.SectionID]
		} else {
			key := "legacy\x00" + doc.DocID + "\x00" + doc.ChunkID
			if doc.ChunkID == "" {
				key = fmt.Sprintf("legacy\x00%s:%d:%d:%s:%s", doc.DocID, doc.StartRune, doc.EndRune, doc.Title, doc.Content)
			}
			if seenChunks[key] {
				continue
			}
			seenChunks[key] = true
			doc.Metadata = cloneMetadata(doc.Metadata)
			doc.Metadata["context_complete"] = "false"
			doc.Metadata["context_note"] = "未保存章节关系，返回原有检索内容，无法保证完整章节"
			if content := []rune(doc.Content); len(content) > maxSectionContextRunes {
				doc.Content = string(content[:maxSectionContextRunes])
				if doc.StartRune >= 0 && doc.EndRune-doc.StartRune == len(content) {
					doc.EndRune = doc.StartRune + maxSectionContextRunes
				} else {
					doc.StartRune, doc.EndRune = 0, 0
				}
				doc.Metadata["context_note"] += "；片段超过预算，已截断"
			}
		}
		if strings.TrimSpace(doc.Content) != "" {
			docs = append(docs, doc)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

// runeRange uses section-local, end-exclusive Unicode rune offsets.
type runeRange struct{ start, end int }

func mergeRuneRanges(ranges []runeRange) []runeRange {
	ranges = append([]runeRange(nil), ranges...)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := ranges[:0]
	for _, span := range ranges {
		if span.end <= span.start {
			continue
		}
		if len(merged) > 0 && span.start <= merged[len(merged)-1].end {
			last := &merged[len(merged)-1]
			last.end = maxInt(last.end, span.end)
		} else {
			merged = append(merged, span)
		}
	}
	return merged
}

func rangeContextRunes(ranges []runeRange) int {
	n := 0
	for _, span := range ranges {
		n += span.end - span.start
	}
	if len(ranges) > 1 {
		n += (len(ranges) - 1) * len([]rune(contextOmission))
	}
	return n
}

// Keep ranked hits before adding surrounding text. Overlaps cost no extra
// characters; omission markers are part of the same per-section budget.
func selectHitRanges(hits []runeRange) ([]runeRange, bool) {
	all := mergeRuneRanges(hits)
	if rangeContextRunes(all) <= maxSectionContextRunes {
		return all, false
	}
	var selected []runeRange
	for _, hit := range hits {
		candidate := mergeRuneRanges(append(append([]runeRange(nil), selected...), hit))
		if rangeContextRunes(candidate) <= maxSectionContextRunes {
			selected = candidate
			continue
		}
		// Admit as much of this ranked hit as fits; lower-ranked hits do not
		// displace content that has already been selected.
		for end := hit.end - 1; end > hit.start; end-- {
			candidate = mergeRuneRanges(append(append([]runeRange(nil), selected...), runeRange{hit.start, end}))
			if rangeContextRunes(candidate) <= maxSectionContextRunes {
				selected = candidate
				break
			}
		}
	}
	return selected, true
}

func (r *KnowledgeRetriever) sectionWindow(ctx context.Context, section types.KnowledgeDocument, seeds []types.KnowledgeDocument) (types.KnowledgeDocument, error) {
	if err := ctx.Err(); err != nil {
		return section, err
	}
	raw := []rune(section.Content)
	if section.StartRune < 0 {
		return fallbackSection(seeds, "父章节原文偏移无效，返回预算内的已命中片段"), nil
	}
	hits := make([]runeRange, 0, len(seeds))
	for _, seed := range seeds {
		from, to := seed.StartRune-section.StartRune, seed.EndRune-section.StartRune
		if from < 0 || to > len(raw) || to <= from || seed.SectionID != section.SectionID || string(raw[from:to]) != seed.Content {
			return fallbackSection(seeds, "命中片段原文偏移无效或与章节不一致，返回预算内的已命中片段"), nil
		}
		hits = append(hits, runeRange{from, to})
	}
	if len(hits) == 0 {
		return section, fmt.Errorf("no hits for section %s", section.SectionID)
	}
	ranges, truncated := selectHitRanges(hits)
	if !truncated {
		// Share the remaining budget among current window sides. Reallocate
		// boundary/merge savings next round; source order breaks small ties.
		for {
			if err := ctx.Err(); err != nil {
				return section, err
			}
			changed := false
			step := maxInt(1, (maxSectionContextRunes-rangeContextRunes(ranges))/(2*len(ranges)))
			anchors := append([]runeRange(nil), ranges...)
			for _, anchor := range anchors {
				for _, direction := range []int{-1, 1} {
					for i, span := range ranges {
						if span.start > anchor.start || span.end < anchor.end {
							continue
						}
						grown := span
						if direction < 0 {
							grown.start = maxInt(0, span.start-step)
						} else {
							grown.end = minInt(len(raw), span.end+step)
						}
						if grown != span {
							candidate := append([]runeRange(nil), ranges...)
							candidate[i] = grown
							candidate = mergeRuneRanges(candidate)
							if rangeContextRunes(candidate) <= maxSectionContextRunes {
								ranges, changed = candidate, true
							}
						}
						break
					}
				}
			}
			if !changed {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return section, err
	}
	var parts []string
	var sourceRanges [][2]int
	for _, span := range ranges {
		parts = append(parts, string(raw[span.start:span.end]))
		sourceRanges = append(sourceRanges, [2]int{section.StartRune + span.start, section.StartRune + span.end})
	}
	section.Content = strings.Join(parts, contextOmission)
	section.Metadata = cloneMetadata(section.Metadata)
	section.Metadata["context_complete"] = "false"
	section.Metadata["context_note"] = "章节过长，按多命中范围合并并在预算内向两侧扩展；非连续窗口之间标记省略，未返回完整章节"
	if truncated {
		section.Metadata["context_note"] = "命中片段超过章节预算，按相关性顺序保留并截断；非连续窗口之间标记省略，未返回完整章节"
	}
	encoded, _ := json.Marshal(sourceRanges)
	section.Metadata["context_ranges"] = string(encoded)
	section.StartRune, section.EndRune = sourceRanges[0][0], sourceRanges[len(sourceRanges)-1][1]
	return section, nil
}

// Without a reliable parent, preserve hit text itself rather than inventing
// the missing source between hits. Valid overlapping offsets are deduplicated.
func renderHitChunks(hits []types.KnowledgeDocument) string {
	hits = append([]types.KnowledgeDocument(nil), hits...)
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].StartRune < hits[j].StartRune
	})
	var out strings.Builder
	end := -1
	for _, hit := range hits {
		if hit.EndRune <= end || hit.Content == "" {
			continue
		}
		text := []rune(hit.Content)
		if hit.StartRune < end {
			text = text[end-hit.StartRune:]
		}
		if out.Len() > 0 && hit.StartRune > end {
			out.WriteString(contextOmission)
		}
		end = hit.EndRune
		out.WriteString(string(text))
	}
	return out.String()
}

func fallbackSection(hits []types.KnowledgeDocument, note string) types.KnowledgeDocument {
	if len(hits) == 0 {
		return types.KnowledgeDocument{}
	}
	doc := hits[0]
	doc.Metadata = cloneMetadata(doc.Metadata)
	doc.Metadata["context_complete"] = "false"
	doc.Metadata["context_note"] = note + "；不连续或位置未知的片段之间标记省略"
	delete(doc.Metadata, "context_ranges")
	// Invalid offsets cannot establish continuity. Assign only these snippets
	// temporary disjoint positions after the known source; never expose those
	// synthetic positions as real document offsets.
	normalized := append([]types.KnowledgeDocument(nil), hits...)
	valid := func(hit types.KnowledgeDocument) bool {
		return hit.StartRune >= 0 && hit.EndRune > hit.StartRune && hit.EndRune-hit.StartRune == len([]rune(hit.Content))
	}
	next := 0
	for _, hit := range normalized {
		if valid(hit) {
			next = maxInt(next, hit.EndRune)
		}
	}
	seenUnknown := map[string]bool{}
	var rankedRanges []runeRange
	for i := range normalized {
		hit := &normalized[i]
		if hit.Content == "" {
			continue
		}
		if !valid(*hit) {
			if seenUnknown[hit.Content] {
				hit.Content = ""
				continue
			}
			seenUnknown[hit.Content] = true
			hit.StartRune = next + 1
			hit.EndRune = hit.StartRune + len([]rune(hit.Content))
			next = hit.EndRune
		}
		rankedRanges = append(rankedRanges, runeRange{hit.StartRune, hit.EndRune})
	}
	ranges, truncated := selectHitRanges(rankedRanges)
	var selected []types.KnowledgeDocument
	for _, hit := range normalized {
		text := []rune(hit.Content)
		if len(text) == 0 {
			continue
		}
		for _, span := range ranges {
			start, end := maxInt(hit.StartRune, span.start), minInt(hit.EndRune, span.end)
			if end <= start {
				continue
			}
			part := hit
			part.Content = string(text[start-hit.StartRune : end-hit.StartRune])
			part.StartRune, part.EndRune = start, end
			selected = append(selected, part)
		}
	}
	if truncated {
		doc.Metadata["context_note"] += "；命中内容超过预算，按相关性顺序保留并截断"
	}
	doc.Content = renderHitChunks(selected)
	// A synthesized fallback is not one contiguous source span.
	doc.StartRune, doc.EndRune = 0, 0
	return doc
}

// Keep this adapter independent of Markdown rendering and Agent behavior.
var _ einoretriever.Retriever = rankedChildren(nil)
