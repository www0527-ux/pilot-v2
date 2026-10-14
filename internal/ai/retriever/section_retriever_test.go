package retriever

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/types"
)

func hitAt(section types.KnowledgeDocument, id string, start, end int) types.KnowledgeDocument {
	return types.KnowledgeDocument{
		DocID: section.DocID, ChunkID: id, SectionID: section.SectionID, RecordType: "chunk",
		Content:   string([]rune(section.Content)[start:end]),
		StartRune: section.StartRune + start, EndRune: section.StartRune + end,
	}
}

// Verify actual source coverage, ordering, omission markers and the budget,
// rather than relying on the implementation's choice of expansion radius.
func assertWindow(t *testing.T, section, got types.KnowledgeDocument) [][2]int {
	t.Helper()
	if n := len([]rune(got.Content)); n == 0 || n > maxSectionContextRunes {
		t.Fatalf("context length=%d", n)
	}
	if got.Metadata["context_complete"] != "false" {
		t.Fatal("partial context must be marked incomplete")
	}
	var ranges [][2]int
	if err := json.Unmarshal([]byte(got.Metadata["context_ranges"]), &ranges); err != nil || len(ranges) == 0 {
		t.Fatalf("ranges=%q err=%v", got.Metadata["context_ranges"], err)
	}
	raw := []rune(section.Content)
	var parts []string
	for i, span := range ranges {
		from, to := span[0]-section.StartRune, span[1]-section.StartRune
		if from < 0 || to > len(raw) || from >= to || i > 0 && ranges[i-1][1] >= span[0] {
			t.Fatalf("invalid or unmerged ranges=%v", ranges)
		}
		parts = append(parts, string(raw[from:to]))
	}
	if got.Content != strings.Join(parts, contextOmission) {
		t.Fatal("context does not match source windows")
	}
	return ranges
}

func TestSectionWindowsAggregateHits(t *testing.T) {
	section := types.KnowledgeDocument{DocID: "d", SectionID: "s", StartRune: 100, EndRune: 30100, Content: strings.Repeat("中", 30000)}
	cases := []struct {
		name    string
		hits    [][2]int
		windows int
	}{
		{"single", [][2]int{{10000, 10200}}, 1},
		{"disjoint", [][2]int{{25000, 25100}, {1000, 1100}}, 2},
		{"overlapping", [][2]int{{5000, 5500}, {5300, 5800}}, 1},
		{"adjacent", [][2]int{{5000, 5500}, {5500, 6000}}, 1},
		{"merge while expanding", [][2]int{{5000, 5100}, {5400, 5500}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seeds []types.KnowledgeDocument
			for i, span := range tc.hits {
				seeds = append(seeds, hitAt(section, fmt.Sprint(i), span[0], span[1]))
			}
			got, err := (&KnowledgeRetriever{}).sectionWindow(context.Background(), section, seeds)
			if err != nil {
				t.Fatal(err)
			}
			ranges := assertWindow(t, section, got)
			if len(ranges) != tc.windows {
				t.Fatalf("ranges=%v", ranges)
			}
			for _, seed := range seeds {
				covered := false
				for _, span := range ranges {
					covered = covered || span[0] <= seed.StartRune && span[1] >= seed.EndRune
				}
				if !covered {
					t.Fatalf("lost hit %s", seed.ChunkID)
				}
			}
			if len([]rune(got.Content)) != maxSectionContextRunes {
				t.Fatal("unused expandable budget")
			}
			again, err := (&KnowledgeRetriever{}).sectionWindow(context.Background(), section, seeds)
			if err != nil || again.Content != got.Content || again.Metadata["context_ranges"] != got.Metadata["context_ranges"] {
				t.Fatal("window planning is not deterministic")
			}
		})
	}
}

func TestSectionHitBudgetPreservesRankBeforeSourceOrder(t *testing.T) {
	section := types.KnowledgeDocument{SectionID: "s", Content: strings.Repeat("a", 10000) + strings.Repeat("b", 10000)}
	// The later source range is higher ranked. It must survive completely.
	high := hitAt(section, "high", 14000, 18500)
	low := hitAt(section, "low", 1000, 5500)
	got, err := (&KnowledgeRetriever{}).sectionWindow(context.Background(), section, []types.KnowledgeDocument{high, low})
	if err != nil {
		t.Fatal(err)
	}
	ranges := assertWindow(t, section, got)
	if len(ranges) != 2 || ranges[1] != [2]int{14000, 18500} || ranges[0][1] >= low.EndRune {
		t.Fatalf("ranking/truncation lost: %v", ranges)
	}
	if !strings.Contains(got.Metadata["context_note"], "命中片段超过") {
		t.Fatal("missing truncation note")
	}
	if strings.Count(got.Content, "b") != 4500 || len([]rune(got.Content)) != maxSectionContextRunes {
		t.Fatal("budget or high-ranked coverage")
	}
	// One oversized hit is truncated without trying to add neighbors.
	got, err = (&KnowledgeRetriever{}).sectionWindow(context.Background(), section, []types.KnowledgeDocument{hitAt(section, "huge", 0, 9000)})
	if err != nil || len([]rune(got.Content)) != maxSectionContextRunes || got.StartRune != 0 {
		t.Fatalf("huge hit: %v", err)
	}
}

func TestParentFailureKeepsAllHitsAndDeduplicatesOverlap(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			store := &fakeKnowledgeStore{}
			if fail {
				store.searchErrs = []error{errors.New("ES unavailable")}
			}
			r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
			if err != nil {
				t.Fatal(err)
			}
			section := types.KnowledgeDocument{DocID: "d", SectionID: "s", Content: "abcdefghijklmnopqrstuvwxyz"}
			high := hitAt(section, "c1", 10, 15)
			low := hitAt(section, "c2", 0, 5)
			overlap := hitAt(section, "c3", 12, 17)
			results := []RetrievedDocument{{Document: high}, {Document: low}, {Document: overlap}, {Document: high}}
			got, err := r.expandSections(context.Background(), results)
			want := "abcde" + contextOmission + "klmnopq"
			if err != nil || len(got) != 1 || got[0].Content != want || got[0].Metadata["context_complete"] != "false" {
				t.Fatalf("got=%+v err=%v", got, err)
			}
			if len(store.searchQueries) != 1 {
				t.Fatal("extra retrieval")
			}
		})
	}
}

func TestInvalidOffsetsPreserveHitsWithoutLosingOtherSections(t *testing.T) {
	section := types.KnowledgeDocument{SectionID: "s", Content: strings.Repeat("x", 9000)}
	valid := hitAt(section, "valid", 100, 110)
	for _, invalid := range []types.KnowledgeDocument{
		{SectionID: "s", ChunkID: "invalid", Content: "known evidence", StartRune: -5, EndRune: 9},
		{SectionID: "s", ChunkID: "invalid", Content: "known evidence", StartRune: 8998, EndRune: 9012},
		{SectionID: "s", ChunkID: "invalid", Content: "known evidence", StartRune: 200, EndRune: 200},
		{SectionID: "s", ChunkID: "invalid", Content: "known evidence", StartRune: 200, EndRune: 214}, // stale content
	} {
		got, err := (&KnowledgeRetriever{}).sectionWindow(context.Background(), section, []types.KnowledgeDocument{valid, invalid})
		if err != nil || !strings.Contains(got.Content, valid.Content) || !strings.Contains(got.Content, "known evidence") || !strings.Contains(got.Metadata["context_note"], "偏移无效") {
			t.Fatalf("invalid offset lost hit: %+v %v", got, err)
		}
	}
	// A bad long section must not discard a different complete section.
	store := &fakeKnowledgeStore{searchResults: [][]elasticsearch.SearchHit{{
		{ID: "s", Source: map[string]interface{}{"section_id": "s", "record_type": "section", "content": section.Content}},
		{ID: "ok", Source: map[string]interface{}{"section_id": "ok", "record_type": "section", "content": "complete"}},
	}}}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.expandSections(context.Background(), []RetrievedDocument{
		{Document: types.KnowledgeDocument{SectionID: "s", ChunkID: "bad", Content: "evidence"}},
		{Document: types.KnowledgeDocument{SectionID: "ok", ChunkID: "good"}},
	})
	if err != nil || len(got) != 2 || got[1].Content != "complete" || got[1].Metadata["context_complete"] != "true" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestFallbackBudgetAndNoSectionChunks(t *testing.T) {
	hits := []types.KnowledgeDocument{
		{ChunkID: "high", SectionID: "s", Content: strings.Repeat("a", 5000), StartRune: 10000, EndRune: 15000},
		{ChunkID: "low", SectionID: "s", Content: strings.Repeat("b", 5000), StartRune: 0, EndRune: 5000},
	}
	got := fallbackSection(hits, "父章节不可用")
	if len([]rune(got.Content)) > maxSectionContextRunes || strings.Count(got.Content, "a") != 5000 || !strings.Contains(got.Content, "b") || !strings.Contains(got.Metadata["context_note"], "截断") {
		t.Fatal("fallback did not respect rank and budget")
	}
	r := &KnowledgeRetriever{}
	docs, err := r.expandSections(context.Background(), []RetrievedDocument{{Document: types.KnowledgeDocument{ChunkID: "legacy", Content: strings.Repeat("中", 9000)}}})
	if err != nil || len(docs) != 1 || len([]rune(docs[0].Content)) != maxSectionContextRunes {
		t.Fatal("legacy budget")
	}
	docs, err = r.expandSections(context.Background(), nil)
	if err != nil || len(docs) != 0 {
		t.Fatal("empty results")
	}
	legacy := RetrievedDocument{Document: types.KnowledgeDocument{DocID: "d", ChunkID: "same", Content: "once"}}
	docs, err = r.expandSections(context.Background(), []RetrievedDocument{legacy, legacy})
	if err != nil || len(docs) != 1 || docs[0].Content != "once" {
		t.Fatal("duplicate legacy hit")
	}
}

func TestDiversityKeepsAllRankedHitsForSelectedSections(t *testing.T) {
	candidates := []RetrievedDocument{
		{Document: types.KnowledgeDocument{DocID: "d", SectionID: "A", ChunkID: "A1"}},
		{Document: types.KnowledgeDocument{DocID: "d", SectionID: "B", ChunkID: "B1"}},
		{Document: types.KnowledgeDocument{DocID: "d", SectionID: "A", ChunkID: "A2"}},
		{Document: types.KnowledgeDocument{DocID: "other", SectionID: "C", ChunkID: "C1"}},
		{Document: types.KnowledgeDocument{DocID: "d", SectionID: "B", ChunkID: "B2"}},
	}
	got := diversifyChunks(candidates, 2, 2)
	var ids []string
	for _, item := range got {
		ids = append(ids, item.Document.ChunkID)
	}
	if strings.Join(ids, ",") != "A1,B1,A2,B2" {
		t.Fatalf("hits lost or reordered: %v", ids)
	}
	got = diversifyChunks(candidates, 3, 1)
	if len(got) != 3 || got[0].Document.ChunkID != "A1" || got[1].Document.ChunkID != "A2" || got[2].Document.ChunkID != "C1" {
		t.Fatalf("per-document limit=%+v", got)
	}
}

func TestSearchRetainsMultipleHitsThroughESRecallAndExpansion(t *testing.T) {
	raw := []rune(strings.Repeat("中", 30000))
	copy(raw[1000:], []rune("FIRST_MATCH"))
	copy(raw[25000:], []rune("SECOND_MATCH"))
	section := types.KnowledgeDocument{DocID: "d", SectionID: "s", StartRune: 100, EndRune: 30100, Content: string(raw)}
	first, second := hitAt(section, "c1", 1000, 1011), hitAt(section, "c2", 25000, 25012)
	toHit := func(doc types.KnowledgeDocument, score float64) elasticsearch.SearchHit {
		return elasticsearch.SearchHit{ID: doc.ChunkID, Score: score, Source: map[string]interface{}{
			"doc_id": doc.DocID, "chunk_id": doc.ChunkID, "section_id": doc.SectionID,
			"record_type": "chunk", "content": doc.Content, "start_rune": doc.StartRune, "end_rune": doc.EndRune,
		}}
	}
	store := &fakeKnowledgeStore{searchResults: [][]elasticsearch.SearchHit{
		{toHit(second, 3), toHit(first, 2), toHit(second, 1)},
		{{ID: "s", Source: map[string]interface{}{
			"doc_id": "d", "section_id": "s", "record_type": "section", "content": section.Content,
			"start_rune": 100, "end_rune": 30100,
		}}},
	}}
	cfg := testRetrieverConfig()
	cfg.RAG.RetrievalMode = "bm25"
	cfg.RAG.FinalTopK = 1
	cfg.RAG.MaxChunksPerDoc = 1
	cfg.RAG.CandidateMultiplier = 8
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := r.Search(context.Background(), "MATCH")
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	if strings.Count(docs[0].Content, "FIRST_MATCH") != 1 || strings.Count(docs[0].Content, "SECOND_MATCH") != 1 {
		t.Fatal("upstream diversity discarded a hit or duplicated output")
	}
	assertWindow(t, section, docs[0])
	if len(store.searchQueries) != 2 {
		t.Fatal("window expansion added ES searches")
	}
	query := store.searchQueries[1]["query"].(map[string]interface{})
	ids := query["ids"].(map[string]interface{})["values"].([]string)
	if len(ids) != 1 || ids[0] != "s" {
		t.Fatalf("parent IDs not deduplicated: %v", ids)
	}
}

func TestEmptyParentAndUnknownOffsetFallback(t *testing.T) {
	store := &fakeKnowledgeStore{searchResults: [][]elasticsearch.SearchHit{{{
		ID: "s", Source: map[string]interface{}{"section_id": "s", "record_type": "section", "content": ""},
	}}}}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	hits := []RetrievedDocument{
		{Document: types.KnowledgeDocument{SectionID: "s", ChunkID: "one", Content: "first evidence"}},
		{Document: types.KnowledgeDocument{SectionID: "s", ChunkID: "two", Content: "second evidence"}},
	}
	docs, err := r.expandSections(context.Background(), hits)
	if err != nil || len(docs) != 1 || docs[0].Content != "first evidence"+contextOmission+"second evidence" {
		t.Fatalf("empty parent lost hits: %+v %v", docs, err)
	}
	if docs[0].Metadata["context_complete"] != "false" || !strings.Contains(docs[0].Metadata["context_note"], "父章节不可用") {
		t.Fatal("empty parent must not be marked complete")
	}
	docs, err = r.expandSections(context.Background(), []RetrievedDocument{{Document: types.KnowledgeDocument{SectionID: "s", Content: ""}}})
	if err != nil || len(docs) != 0 {
		t.Fatal("empty fallback should not produce a context item")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.sectionWindow(ctx, types.KnowledgeDocument{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("window swallowed cancellation")
	}
}
