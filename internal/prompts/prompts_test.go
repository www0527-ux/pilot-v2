package prompts

import (
	"strings"
	"testing"
)

func TestDefaultCatalogLoadsAndRenders(t *testing.T) {
	catalog, err := Parse(embeddedDefault)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := catalog.Render("intent_classifier_user", map[string]any{"query": "排查 api"})
	if err != nil || !strings.Contains(rendered, "排查 api") {
		t.Fatalf("rendered=%q err=%v", rendered, err)
	}
}

func TestCatalogRejectsMissingKeyUnknownKeyAndIllegalVariable(t *testing.T) {
	missing := strings.Replace(string(embeddedDefault), "continue_instruction:", "removed_instruction:", 1)
	if _, err := Parse([]byte(missing)); err == nil {
		t.Fatal("expected unknown/missing key failure")
	}
	illegal := strings.Replace(string(embeddedDefault), "{{.date}}", "{{.secret}}", 1)
	if _, err := Parse([]byte(illegal)); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected illegal variable failure, got %v", err)
	}
}
