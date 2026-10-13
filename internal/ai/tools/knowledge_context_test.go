package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"shanyangcode.com/pilot/pkg/types"
)

type contextKnowledgeSource func(context.Context, string) ([]types.KnowledgeDocument, error)

func (s contextKnowledgeSource) Search(ctx context.Context, q string) ([]types.KnowledgeDocument, error) {
	return s(ctx, q)
}

func TestKnowledgeToolPreservesExpandedSectionAndLimitsTotal(t *testing.T) {
	text := strings.Repeat("step\n", 400) + "verification must survive\n"
	source := contextKnowledgeSource(func(context.Context, string) ([]types.KnowledgeDocument, error) {
		return []types.KnowledgeDocument{{DocID: "d", SectionPath: "Guide > Pool", Content: text}, {DocID: "large", Content: strings.Repeat("x", maxKnowledgeToolResultRunes)}}, nil
	})
	tool, err := NewSearchKnowledgeTool(source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.InvokableRun(context.Background(), `{"query":"pool"}`)
	if err != nil {
		t.Fatal(err)
	}
	var output SearchKnowledgeOutput
	if err = json.Unmarshal([]byte(result), &output); err != nil {
		t.Fatal(err)
	}
	if output.Count != 1 || !strings.Contains(output.Results, text) || !strings.Contains(output.Message, "预算") || len([]rune(output.Results)) > maxKnowledgeToolResultRunes {
		t.Fatalf("output=%+v", output)
	}
}

func TestKnowledgeToolDoesNotSwallowCancellation(t *testing.T) {
	source := contextKnowledgeSource(func(context.Context, string) ([]types.KnowledgeDocument, error) { return nil, context.Canceled })
	tool, err := NewSearchKnowledgeTool(source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.InvokableRun(context.Background(), `{"query":"pool"}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
