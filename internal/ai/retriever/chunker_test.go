package retriever

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkTextPreservesCJKEmojiAndCombiningSequences(t *testing.T) {
	content := "中文e\u0301🙂👩‍💻🇨🇳结束" + strings.Repeat("界", 20)
	chunks, err := ChunkText(content, 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks: %+v", chunks)
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk.Content) || chunk.StartRune >= chunk.EndRune {
			t.Fatalf("invalid chunk: %+v", chunk)
		}
		if strings.HasPrefix(chunk.Content, "\u0301") || strings.HasSuffix(chunk.Content, "\u200d") {
			t.Fatalf("split combining/ZWJ sequence: %q", chunk.Content)
		}
	}
	for _, sequence := range []string{"e\u0301", "👩‍💻", "🇨🇳"} {
		found := false
		for _, chunk := range chunks {
			if strings.Contains(chunk.Content, sequence) {
				found = true
			}
		}
		if !found {
			t.Fatalf("sequence %q was split across every chunk: %+v", sequence, chunks)
		}
	}
}

func TestChunkTextOverlapAndCoverage(t *testing.T) {
	chunks, err := ChunkText("abcdefghijklmnop", 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || chunks[0].Content != "abcdefgh" || chunks[1].Content != "fghijklm" || chunks[2].Content != "klmnop" {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
	if chunks[1].StartRune != 5 || chunks[1].EndRune != 13 {
		t.Fatalf("unexpected offsets: %+v", chunks[1])
	}
}
