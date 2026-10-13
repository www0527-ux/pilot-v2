package prompts

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed prompts.yaml
var embeddedDefault []byte

type Catalog struct {
	FullPilotAgentSystem      string `yaml:"full_pilot_agent_system"`
	MultiAgentSynthesisSystem string `yaml:"multi_agent_synthesis_system"`
	MultiAgentSynthesisUser   string `yaml:"multi_agent_synthesis_user"`
	IntentClassifierSystem    string `yaml:"intent_classifier_system"`
	IntentClassifierUser      string `yaml:"intent_classifier_user"`
	ReactEvidenceSystem       string `yaml:"react_evidence_system"`
	ReactEvidenceUser         string `yaml:"react_evidence_user"`
	RAGRerankerSystem         string `yaml:"rag_reranker_system"`
	RAGRerankerUser           string `yaml:"rag_reranker_user"`
	ContinueInstruction       string `yaml:"continue_instruction"`

	compiled map[string]*template.Template
}

var specs = map[string]struct {
	value func(*Catalog) string
	vars  []string
}{
	"full_pilot_agent_system":      {func(c *Catalog) string { return c.FullPilotAgentSystem }, []string{"date"}},
	"multi_agent_synthesis_system": {func(c *Catalog) string { return c.MultiAgentSynthesisSystem }, nil},
	"multi_agent_synthesis_user":   {func(c *Catalog) string { return c.MultiAgentSynthesisUser }, []string{"query", "intent", "service", "assessment", "findings", "limitations"}},
	"intent_classifier_system":     {func(c *Catalog) string { return c.IntentClassifierSystem }, nil},
	"intent_classifier_user":       {func(c *Catalog) string { return c.IntentClassifierUser }, []string{"query"}},
	"react_evidence_system":        {func(c *Catalog) string { return c.ReactEvidenceSystem }, nil},
	"react_evidence_user":          {func(c *Catalog) string { return c.ReactEvidenceUser }, []string{"service", "query"}},
	"rag_reranker_system":          {func(c *Catalog) string { return c.RAGRerankerSystem }, nil},
	"rag_reranker_user":            {func(c *Catalog) string { return c.RAGRerankerUser }, []string{"query", "candidates"}},
	"continue_instruction":         {func(c *Catalog) string { return c.ContinueInstruction }, nil},
}

func Load(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read prompts %s failed: %w", path, err)
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Catalog, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	catalog := &Catalog{}
	if err := decoder.Decode(catalog); err != nil {
		return nil, fmt.Errorf("parse prompts failed: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse prompts failed: multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("parse prompts failed: %w", err)
	}
	catalog.compiled = make(map[string]*template.Template, len(specs))
	for key, spec := range specs {
		value := strings.TrimSpace(spec.value(catalog))
		if value == "" {
			return nil, fmt.Errorf("prompt %s is required", key)
		}
		tmpl, err := template.New(key).Option("missingkey=error").Parse(value)
		if err != nil {
			return nil, fmt.Errorf("parse prompt %s: %w", key, err)
		}
		dummy := make(map[string]any, len(spec.vars))
		for _, variable := range spec.vars {
			dummy[variable] = "test"
		}
		if err := tmpl.Execute(io.Discard, dummy); err != nil {
			return nil, fmt.Errorf("validate prompt %s variables: %w", key, err)
		}
		catalog.compiled[key] = tmpl
	}
	return catalog, nil
}

func (c *Catalog) Render(key string, values map[string]any) (string, error) {
	if c == nil || c.compiled == nil {
		return "", fmt.Errorf("prompt catalog is not initialized")
	}
	tmpl, ok := c.compiled[key]
	if !ok {
		return "", fmt.Errorf("unknown prompt key %q", key)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, values); err != nil {
		return "", fmt.Errorf("render prompt %s: %w", key, err)
	}
	return rendered.String(), nil
}

var (
	defaultMu      sync.RWMutex
	defaultCatalog *Catalog
	defaultErr     error
	defaultOnce    sync.Once
)

func Default() (*Catalog, error) {
	defaultMu.RLock()
	configured := defaultCatalog
	defaultMu.RUnlock()
	if configured != nil {
		return configured, nil
	}
	defaultOnce.Do(func() { defaultCatalog, defaultErr = Parse(embeddedDefault) })
	return defaultCatalog, defaultErr
}

func Configure(catalog *Catalog) error {
	if catalog == nil || catalog.compiled == nil {
		return fmt.Errorf("cannot configure an unvalidated prompt catalog")
	}
	defaultMu.Lock()
	defaultCatalog = catalog
	defaultErr = nil
	defaultMu.Unlock()
	return nil
}
