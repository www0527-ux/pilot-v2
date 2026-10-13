package retriever

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino-ext/components/document/transformer/splitter/recursive"
	"github.com/cloudwego/eino/schema"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const markdownSplitterVersion = "markdown-structure-v1"
const maxProtectedBlockRunes = 8000

type MarkdownInputError struct{ Message string }

func (e *MarkdownInputError) Error() string { return e.Message }

type MarkdownSection struct {
	TextChunk
	Path   string
	Chunks []TextChunk
}

// sourceParser delegates Markdown syntax to goldmark and records raw source
// positions. In particular, code indentation and fences are never re-rendered.
type sourceParser struct{ parser.BlockParser }

func (p sourceParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil {
		start := segment.Start
		if node.Lines().Len() > 0 && node.Lines().At(0).Start < start {
			start = node.Lines().At(0).Start // Setext headings reuse the previous paragraph.
		}
		source := reader.Source()
		for start > 0 && source[start-1] != '\n' {
			start--
		}
		node.SetAttributeString("pilot_source_start", start)
	}
	return node, state
}

// MarkdownChunks splits at real top-level heading/block boundaries. Paragraphs
// exceeding the target use Eino's recursive splitter; code/list/table blocks stay whole.
func MarkdownChunks(ctx context.Context, content string, size, overlap int) ([]MarkdownSection, error) {
	if size <= 0 || overlap < 0 || overlap >= size {
		return nil, fmt.Errorf("invalid Markdown chunk size/overlap")
	}
	blocks := parser.DefaultBlockParsers()
	for i := range blocks {
		blocks[i].Value = sourceParser{blocks[i].Value.(parser.BlockParser)}
	}
	p := parser.NewParser(parser.WithBlockParsers(blocks...), parser.WithInlineParsers(parser.DefaultInlineParsers()...), parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...))
	source := []byte(content)
	root := p.Parse(text.NewReader(source))
	type block struct {
		start int
		node  ast.Node
	}
	var nodes []block
	for node := root.FirstChild(); node != nil; node = node.NextSibling() {
		if value, ok := node.AttributeString("pilot_source_start"); ok {
			nodes = append(nodes, block{value.(int), node})
		}
	}
	if len(nodes) == 0 && strings.TrimSpace(content) != "" {
		return nil, fmt.Errorf("Markdown contains no readable blocks")
	}
	type sectionRange struct {
		start, end int
		path       string
	}
	var sections []sectionRange
	start, path := 0, ""
	headings := make([]string, 6)
	for _, b := range nodes {
		if h, ok := b.node.(*ast.Heading); ok {
			if b.start > start && strings.TrimSpace(content[start:b.start]) != "" {
				sections = append(sections, sectionRange{start, b.start, path})
			}
			for i := h.Level - 1; i < len(headings); i++ {
				headings[i] = ""
			}
			headings[h.Level-1] = string(h.Text(source))
			var active []string
			for _, title := range headings {
				if title != "" {
					active = append(active, title)
				}
			}
			start, path = b.start, strings.Join(active, " > ")
		}
	}
	if strings.TrimSpace(content[start:]) != "" {
		sections = append(sections, sectionRange{start, len(source), path})
	}
	splitter, err := recursive.NewSplitter(ctx, &recursive.Config{ChunkSize: size, OverlapSize: overlap, Separators: []string{"\n\n", "\n", "。", "！", "？", ". ", "; ", " ", ""}, LenFunc: utf8.RuneCountInString, KeepType: recursive.KeepTypeEnd})
	if err != nil {
		return nil, err
	}
	var result []MarkdownSection
	runes := []rune(content)
	clusters := splitGraphemeLikeClusters(runes)
	starts, ends := make([]int, len(runes)), make([]int, len(runes))
	for _, cluster := range clusters {
		for i := cluster.start; i < cluster.end; i++ {
			starts[i], ends[i] = cluster.start, cluster.end
		}
	}
	for _, s := range sections {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		section := MarkdownSection{TextChunk: TextChunk{Content: content[s.start:s.end], StartRune: utf8.RuneCount(source[:s.start]), EndRune: utf8.RuneCount(source[:s.end])}, Path: s.path}
		// Each unit includes its original whitespace up to the next block.
		var units []TextChunk
		for i, b := range nodes {
			if b.start < s.start || b.start >= s.end {
				continue
			}
			end := s.end
			if i+1 < len(nodes) && nodes[i+1].start < end {
				end = nodes[i+1].start
			}
			raw := content[b.start:end]
			if strings.TrimSpace(raw) == "" {
				continue
			}
			base := utf8.RuneCount(source[:b.start])
			protected := b.node.Kind() != ast.KindParagraph || looksLikeMarkdownTable(raw)
			if protected {
				if utf8.RuneCountInString(raw) > maxProtectedBlockRunes {
					return nil, &MarkdownInputError{Message: fmt.Sprintf("Markdown 单个代码块、列表或表格超过 %d 个字符，请拆成多个完整内容块后上传", maxProtectedBlockRunes)}
				}
				units = append(units, TextChunk{raw, base, utf8.RuneCount(source[:end])})
				continue
			}
			docs, splitErr := splitter.Transform(ctx, []*schema.Document{{Content: raw}})
			if splitErr != nil {
				return nil, splitErr
			}
			cursor := 0
			for _, doc := range docs {
				if doc.Content == "" {
					continue
				}
				pos := strings.Index(raw[cursor:], doc.Content)
				if pos < 0 {
					return nil, fmt.Errorf("recursive chunk could not be mapped to Markdown source")
				}
				pos += cursor
				from := base + utf8.RuneCountInString(raw[:pos])
				units = append(units, TextChunk{doc.Content, from, from + utf8.RuneCountInString(doc.Content)})
				// Overlap may put the next start inside this chunk, but must advance.
				_, width := utf8.DecodeRuneInString(raw[pos:])
				cursor = pos + width
			}
		}
		// Merge adjacent small blocks within the same section, preserving raw text.
		for _, unit := range units {
			if unit.StartRune < len(starts) && unit.EndRune > 0 {
				unit.StartRune = starts[unit.StartRune]
				unit.EndRune = ends[unit.EndRune-1]
				unit.Content = string(runes[unit.StartRune:unit.EndRune])
			}
			if len(section.Chunks) > 0 {
				last := &section.Chunks[len(section.Chunks)-1]
				if unit.StartRune >= last.EndRune && unit.EndRune-last.StartRune <= size {
					last.EndRune = unit.EndRune
					last.Content = string(runes[last.StartRune:last.EndRune])
					continue
				}
			}
			section.Chunks = append(section.Chunks, unit)
		}
		if len(section.Chunks) > 0 {
			result = append(result, section)
		}
	}
	return result, nil
}

// CommonMark treats pipe tables as paragraphs; protect the complete paragraph
// rather than rewriting its syntax or cell spacing.
func looksLikeMarkdownTable(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "|") && strings.Contains(line, "---") && len(bytes.Trim([]byte(line), " |:-\t\r")) == 0 {
			return true
		}
	}
	return false
}
