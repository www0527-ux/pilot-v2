package config

import (
	"strings"
	"testing"
)

const minimalYAML = `
redis:
  addr: "localhost:6379"
elasticsearch:
  addresses:
    - "http://localhost:9200"
database:
  host: "127.0.0.1"
  name: "pilot"
observability:
  prometheus:
    url: "http://localhost:9090"
  log:
    url: "http://localhost:9200"
  trace:
    url: "http://localhost:16686"
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.Server.Address != ":8080" {
		t.Fatalf("server.address default = %q", cfg.Server.Address)
	}
	if cfg.Redis.Memory.MaxMessages != 20 || cfg.Redis.Memory.ChunkSize != 10 {
		t.Fatalf("redis.memory defaults = %+v", cfg.Redis.Memory)
	}
	if cfg.RAG.RetrievalMode != "hybrid" || cfg.RAG.RRFK != 60 {
		t.Fatalf("rag defaults = %+v", cfg.RAG)
	}
	if cfg.RAG.ChunkSize != 800 || cfg.RAG.ChunkOverlap != 0 || cfg.RAG.MaxChunksPerDoc != 2 || cfg.RAG.RerankerMode != "lexical" {
		t.Fatalf("rag chunk/reranker defaults = %+v", cfg.RAG)
	}
	if cfg.Agent.MaxIterations != 25 || cfg.Agent.TimeoutSeconds != 300 {
		t.Fatalf("agent defaults = %+v", cfg.Agent)
	}
	if cfg.Agent.MultiAgent.EvidenceMode != "deterministic" || cfg.Agent.MultiAgent.ReactMaxSteps != 8 {
		t.Fatalf("multi-agent defaults = %+v", cfg.Agent.MultiAgent)
	}
	if cfg.Agent.MultiAgent.Planner.Mode != "rule" || cfg.Agent.MultiAgent.Planner.MinConfidence != 0.6 {
		t.Fatalf("planner defaults = %+v", cfg.Agent.MultiAgent.Planner)
	}
	if cfg.Agent.MultiAgent.Verification.Mode != "evidence_graph" ||
		cfg.Agent.MultiAgent.Verification.MinConfidence != 0.65 ||
		cfg.Agent.MultiAgent.Verification.MinIndependentSources != 2 ||
		cfg.Agent.MultiAgent.Verification.ReplanWindowMultiplier != 2 {
		t.Fatalf("verification defaults = %+v", cfg.Agent.MultiAgent.Verification)
	}
	if cfg.Agent.MultiAgent.EvidenceWindowMinutes != 30 || cfg.Agent.MultiAgent.MaxReplanRounds != 0 {
		// max_replan_rounds default 0 unless set; config.yaml sets 1 but minimalYAML leaves 0
		if cfg.Agent.MultiAgent.EvidenceWindowMinutes != 30 {
			t.Fatalf("evidence window = %d", cfg.Agent.MultiAgent.EvidenceWindowMinutes)
		}
	}
	if cfg.Observability.ServiceName != "pilot-api" {
		t.Fatalf("observability.service_name default = %q", cfg.Observability.ServiceName)
	}
}

func TestParseExpandsEnvRefs(t *testing.T) {
	t.Setenv("PILOT_TEST_REDIS_PASSWORD", "secret-value")
	yaml := minimalYAML + `
llm:
  main_model:
    api_key: "${PILOT_TEST_REDIS_PASSWORD}"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.LLM.MainModel.APIKey != "secret-value" {
		t.Fatalf("api_key = %q, want expanded env value", cfg.LLM.MainModel.APIKey)
	}
}

func TestParseExpandsMissingEnvToEmpty(t *testing.T) {
	yaml := minimalYAML + `
llm:
  main_model:
    api_key: "${PILOT_TEST_DEFINITELY_MISSING_ENV}"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.LLM.MainModel.APIKey != "" {
		t.Fatalf("api_key = %q, want empty for missing env", cfg.LLM.MainModel.APIKey)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	yaml := minimalYAML + `
user_profile:
  enabled: true
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected unknown key to be rejected")
	}
}

func TestParseRejectsMissingRequiredKeys(t *testing.T) {
	_, err := Parse([]byte("server:\n  address: \":8080\"\n"))
	if err == nil {
		t.Fatal("expected missing required keys to fail validation")
	}
	if !strings.Contains(err.Error(), "redis.addr") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestParseRejectsInvalidRetrievalMode(t *testing.T) {
	yaml := minimalYAML + `
rag:
  retrieval_mode: "magic"
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected invalid retrieval mode to be rejected")
	}
}

func TestParseRejectsInvalidChunkAndRerankerConfig(t *testing.T) {
	yaml := minimalYAML + `
rag:
  chunk_size: 64
  chunk_overlap: 64
  final_top_k: 2
  max_chunks_per_doc: 3
  reranker_mode: "magic"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected invalid chunk/reranker config")
	}
	for _, field := range []string{"chunk_overlap", "max_chunks_per_doc", "reranker_mode"} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("validation error missing %s: %v", field, err)
		}
	}
}

func TestParseRejectsInvalidEvidenceMode(t *testing.T) {
	yaml := minimalYAML + `
agent:
  multi_agent:
    evidence_mode: "magic"
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected invalid evidence mode to be rejected")
	}
}

func TestParseRejectsNonPositiveReactMaxSteps(t *testing.T) {
	yaml := minimalYAML + `
agent:
  multi_agent:
    react_max_steps: -1
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected non-positive react max steps to be rejected")
	}
}

func TestParseRejectsInvalidEvidenceGraphPolicy(t *testing.T) {
	yaml := minimalYAML + `
agent:
  multi_agent:
    verification:
      mode: "magic"
      min_confidence: -0.2
      min_independent_sources: -1
      replan_window_multiplier: -2
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected invalid evidence graph policy")
	}
	for _, field := range []string{"verification.mode", "verification.min_confidence", "verification.min_independent_sources", "verification.replan_window_multiplier"} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("validation error missing %s: %v", field, err)
		}
	}
}

func TestParseRejectsAuthEnabledWithoutAPIKey(t *testing.T) {
	yaml := minimalYAML + `
security:
  auth:
    enabled: true
    api_key: ""
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected auth enabled without api_key to be rejected")
	}
}

func TestParseRejectsMixedCORSWildcard(t *testing.T) {
	yaml := minimalYAML + `
security:
  cors:
    allowed_origins:
      - "*"
      - "https://ops.example.com"
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected mixed CORS wildcard to be rejected")
	}
}

func TestParseRejectsInvalidRateLimit(t *testing.T) {
	yaml := minimalYAML + `
security:
  rate_limit:
    enabled: true
    requests_per_minute: 0
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected invalid rate limit to be rejected")
	}
}

func TestResolvePathPrecedence(t *testing.T) {
	if got := ResolvePath("explicit.yaml"); got != "explicit.yaml" {
		t.Fatalf("explicit path = %q", got)
	}
	t.Setenv(EnvPathKey, "/tmp/from-env.yaml")
	if got := ResolvePath(""); got != "/tmp/from-env.yaml" {
		t.Fatalf("env path = %q", got)
	}
	t.Setenv(EnvPathKey, "")
	if got := ResolvePath(""); got != DefaultPath {
		t.Fatalf("default path = %q", got)
	}
}
