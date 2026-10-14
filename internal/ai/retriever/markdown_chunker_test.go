package retriever

import (
	"context"
	"errors"
	"strings"
	"testing"

	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/types"
)

func TestMarkdownSectionsPreserveStructureAndSource(t *testing.T) {
	code := "```python\n# not a heading\nif ready:\n    check_pool()\n```\n"
	table := "| Signal | Meaning |\n| --- | --- |\n| Busy | Inspect pool |\n"
	content := "# Database\n\n## Pool\n\n" + strings.Repeat("Check active connections. ", 8) + "\n\n" + code + "\n" + table + "\n## Slow queries\n\nInspect query duration.\n"
	sections, err := MarkdownChunks(context.Background(), content, 70, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 3 || sections[1].Path != "Database > Pool" || sections[2].Path != "Database > Slow queries" {
		t.Fatalf("sections=%+v", sections)
	}
	var foundCode, foundTable bool
	raw := []rune(content)
	for _, section := range sections {
		if section.Content != string(raw[section.StartRune:section.EndRune]) {
			t.Fatal("section lost raw source")
		}
		for _, chunk := range section.Chunks {
			if chunk.Content != string(raw[chunk.StartRune:chunk.EndRune]) || chunk.StartRune < section.StartRune || chunk.EndRune > section.EndRune {
				t.Fatalf("invalid raw chunk %+v", chunk)
			}
			foundCode = foundCode || strings.Contains(chunk.Content, code)
			foundTable = foundTable || strings.Contains(chunk.Content, table)
		}
	}
	if !foundCode || !foundTable {
		t.Fatalf("code=%v table=%v", foundCode, foundTable)
	}
}

func TestMarkdownSetextAndUnicode(t *testing.T) {
	content := "Database\n========\n\n" + strings.Repeat("中文 e\u0301 👩‍💻 🇨🇳 ", 15) + "\n"
	sections, err := MarkdownChunks(context.Background(), content, 12, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Path != "Database" {
		t.Fatalf("sections=%+v", sections)
	}
	for _, chunk := range sections[0].Chunks {
		if strings.HasPrefix(chunk.Content, "\u0301") || strings.HasPrefix(chunk.Content, "\u200d") || strings.HasSuffix(chunk.Content, "\u200d") {
			t.Fatalf("broken grapheme %q", chunk.Content)
		}
	}
}

func TestMarkdownIndexStoresParentsSeparately(t *testing.T) {
	store := &fakeKnowledgeStore{}
	cfg := testRetrieverConfig()
	cfg.RAG.ChunkSize = 60
	cfg.RAG.ChunkOverlap = 10
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc := &types.KnowledgeDocument{Title: "Guide", Content: "# Guide\n\n## Pool\n\n" + strings.Repeat("inspect connections ", 12) + "\n\n```sql\nSELECT 1;\n```\n", Metadata: map[string]string{"format": "markdown"}}
	if err = r.IndexDocument(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	children, parents := 0, 0
	for _, record := range store.bulk {
		item := record.Document.(types.KnowledgeDocument)
		if item.RecordType == "section" {
			parents++
			if len(item.Vector) > 0 || item.SectionID != record.ID {
				t.Fatal("parent must not participate in vector recall")
			}
			continue
		}
		children++
		if item.SectionID == "" || item.SplitterVersion != markdownSplitterVersion || len(item.Vector) != 2 {
			t.Fatalf("child relation missing %+v", item)
		}
	}
	if children != doc.ChunkCount || parents != 2 {
		t.Fatalf("children=%d parents=%d count=%d", children, parents, doc.ChunkCount)
	}
}

func TestParentRetrieverReturnsOneCompleteSection(t *testing.T) {
	parentSource := map[string]interface{}{"id": "d", "doc_id": "d", "chunk_id": "s", "section_id": "s", "record_type": "section", "section_path": "Guide > Pool", "content": "symptom\ncheck\nfix\nverify\n"}
	store := &fakeKnowledgeStore{searchResults: [][]elasticsearch.SearchHit{{{ID: "s", Source: parentSource}}}}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	children := []RetrievedDocument{{Document: types.KnowledgeDocument{DocID: "d", ChunkID: "c1", SectionID: "s", Content: "check"}}, {Document: types.KnowledgeDocument{DocID: "d", ChunkID: "c2", SectionID: "s", Content: "fix"}}}
	docs, err := r.expandSections(context.Background(), children)
	if err != nil || len(docs) != 1 || docs[0].Content != parentSource["content"] || docs[0].Metadata["context_complete"] != "true" {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	if len(store.searchQueries) != 1 {
		t.Fatal("parent flow must not run child recall again")
	}
}

func TestLongParentUsesSourceWindowAndReportsIncomplete(t *testing.T) {
	body := strings.Repeat("x", 9000)
	store := &fakeKnowledgeStore{}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := r.sectionWindow(context.Background(), types.KnowledgeDocument{SectionID: "s", Content: body, StartRune: 100, EndRune: 9100, Metadata: map[string]string{}}, []types.KnowledgeDocument{{SectionID: "s", ChunkIndex: 3, Content: strings.Repeat("x", 200), StartRune: 5000, EndRune: 5200}})
	if err != nil || len([]rune(doc.Content)) != maxSectionContextRunes || doc.StartRune > 5000 || doc.EndRune < 5200 || doc.Metadata["context_complete"] != "false" {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	if len(store.searchQueries) != 0 {
		t.Fatal("source windows must not depend on missing neighboring ES chunks")
	}
}

func TestParentFetchFailureKeepsChildAndCancellationPropagates(t *testing.T) {
	store := &fakeKnowledgeStore{searchErrs: []error{errors.New("ES unavailable")}}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	results := []RetrievedDocument{{Document: types.KnowledgeDocument{DocID: "d", ChunkID: "c", SectionID: "s", Content: "known evidence"}}}
	docs, err := r.expandSections(context.Background(), results)
	if err != nil || len(docs) != 1 || docs[0].Content != "known evidence" || docs[0].Metadata["context_complete"] != "false" {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.expandSections(ctx, results)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
}

func TestProtectedMarkdownBlockSizeIsExplicit(t *testing.T) {
	_, err := MarkdownChunks(context.Background(), "```\n"+strings.Repeat("x", maxProtectedBlockRunes+1)+"\n```\n", 800, 120)
	if err == nil || !strings.Contains(err.Error(), "拆成多个完整内容块") {
		t.Fatalf("oversized block was silently split: %v", err)
	}
}
