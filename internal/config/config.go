package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath 是仓库内默认配置文件位置（相对进程工作目录）。
const DefaultPath = "manifest/config/config.yaml"

// EnvPathKey 允许通过环境变量覆盖配置文件路径。
const EnvPathKey = "PILOT_CONFIG"

// Config 是全量应用配置。所有键都必须有真实消费方；
// 加载时启用严格模式，配置文件里出现未知键会直接报错。
type Config struct {
	Server        Server        `yaml:"server"`
	Logger        Logger        `yaml:"logger"`
	Security      Security      `yaml:"security"`
	LLM           LLM           `yaml:"llm"`
	Embedding     Embedding     `yaml:"embedding"`
	Redis         Redis         `yaml:"redis"`
	Elasticsearch Elasticsearch `yaml:"elasticsearch"`
	Database      Database      `yaml:"database"`
	Observability Observability `yaml:"observability"`
	Agent         Agent         `yaml:"agent"`
	Memory        Memory        `yaml:"memory"`
	RAG           RAG           `yaml:"rag"`
	MCP           MCP           `yaml:"mcp"`
	Prompts       Prompts       `yaml:"prompts"`
}

type Server struct {
	Address       string `yaml:"address"`
	Env           string `yaml:"env"`
	BaseURL       string `yaml:"base_url"`
	AllowDegraded bool   `yaml:"allow_degraded"` // true：OTLP/运行日志等可选能力失败时降级启动，不中止进程
}

type Logger struct {
	Level string `yaml:"level"`
}

// Security 定义进程内生产边界。主体是共享服务 API Key（非多租户）；
// 本地默认关闭鉴权与限流；CORS 仅在显式配置来源时生效。
type Security struct {
	Auth      Auth      `yaml:"auth"`
	CORS      CORS      `yaml:"cors"`
	RateLimit RateLimit `yaml:"rate_limit"`
}

type Auth struct {
	Enabled bool   `yaml:"enabled"`
	APIKey  string `yaml:"api_key"`
}

type CORS struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type RateLimit struct {
	Enabled           bool `yaml:"enabled"`
	RequestsPerMinute int  `yaml:"requests_per_minute"`
}

type LLM struct {
	MainModel     Model `yaml:"main_model"`
	EvidenceModel Model `yaml:"evidence_model"` // optional; empty api_key falls back to main_model
}

type Model struct {
	APIKey      string  `yaml:"api_key"`
	BaseURL     string  `yaml:"base_url"`
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
}

type Embedding struct {
	APIKey    string `yaml:"api_key"`
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	Dimension int    `yaml:"dimension"`
}

type Redis struct {
	Addr     string      `yaml:"addr"`
	Password string      `yaml:"password"`
	DB       int         `yaml:"db"`
	PoolSize int         `yaml:"pool_size"`
	Memory   RedisMemory `yaml:"memory"`
}

type RedisMemory struct {
	TTLSeconds  int `yaml:"ttl"`
	MaxMessages int `yaml:"max_messages"`
	ChunkSize   int `yaml:"chunk_size"`
}

type Elasticsearch struct {
	Addresses      []string `yaml:"addresses"`
	Username       string   `yaml:"username"`
	Password       string   `yaml:"password"`
	KnowledgeIndex string   `yaml:"knowledge_index"`
	MaxResults     int      `yaml:"max_results"`
}

type Database struct {
	Host                   string `yaml:"host"`
	Port                   int    `yaml:"port"`
	User                   string `yaml:"user"`
	Password               string `yaml:"password"`
	Name                   string `yaml:"name"`
	MaxIdleConns           int    `yaml:"max_idle_conns"`
	MaxOpenConns           int    `yaml:"max_open_conns"`
	ConnMaxLifetimeSeconds int    `yaml:"conn_max_lifetime"`
}

type Observability struct {
	ServiceName string     `yaml:"service_name"`
	Prometheus  Prometheus `yaml:"prometheus"`
	Log         LogSource  `yaml:"log"`
	Trace       Trace      `yaml:"trace"`
}

type Prometheus struct {
	URL            string `yaml:"url"`
	TimeoutSeconds int    `yaml:"timeout"`
}

type LogSource struct {
	URL           string `yaml:"url"`
	Index         string `yaml:"index"`
	SlowRequestMS int64  `yaml:"slow_request_ms"`
}

type Trace struct {
	URL              string  `yaml:"url"`
	OTLPHTTPEndpoint string  `yaml:"otlp_http_endpoint"`
	SampleRatio      float64 `yaml:"sample_ratio"`
}

type Agent struct {
	MaxIterations  int        `yaml:"max_iterations"`
	ContextTokens  int        `yaml:"context_tokens"`
	TimeoutSeconds int        `yaml:"timeout"`
	MultiAgent     MultiAgent `yaml:"multi_agent"`
}

type MultiAgent struct {
	Enabled               bool               `yaml:"enabled"`
	EvidenceMode          string             `yaml:"evidence_mode"`
	ReactMaxSteps         int                `yaml:"react_max_steps"`
	Planner               PlannerConfig      `yaml:"planner"`
	EvidenceWindowMinutes int                `yaml:"evidence_window_minutes"`
	BranchTimeoutSeconds  int                `yaml:"branch_timeout_seconds"`
	MaxReplanRounds       int                `yaml:"max_replan_rounds"`
	Runtime               string             `yaml:"runtime"` // legacy | adk
	CorrelationEnabled    bool               `yaml:"correlation_enabled"`
	Verification          VerificationConfig `yaml:"verification"`
	HITL                  HITLConfig         `yaml:"hitl"`
}

type PlannerConfig struct {
	Mode          string  `yaml:"mode"`
	MinConfidence float64 `yaml:"min_confidence"`
	TimeoutMS     int     `yaml:"timeout_ms"`
}

// VerificationConfig controls the deterministic evidence graph and its
// bounded acquire-assess-replan loop.
type VerificationConfig struct {
	Mode                   string  `yaml:"mode"` // evidence_graph | off
	MinConfidence          float64 `yaml:"min_confidence"`
	MinIndependentSources  int     `yaml:"min_independent_sources"`
	ReplanWindowMultiplier int     `yaml:"replan_window_multiplier"`
}

// HITLConfig gates wide-window / high-cost tool approval. Default disabled (auto-approve).
type HITLConfig struct {
	Enabled           bool   `yaml:"enabled"`
	WideWindowMinutes int    `yaml:"wide_window_minutes"`
	Store             string `yaml:"store"` // redis | memory
}

// Memory holds layered memory settings beyond Redis short-term / MySQL history.
type Memory struct {
	Episodic EpisodicMemoryConfig `yaml:"episodic"`
}

type EpisodicMemoryConfig struct {
	Mode  string `yaml:"mode"`  // sql | hybrid
	Index string `yaml:"index"` // ES index for hybrid vectors
	TopK  int    `yaml:"top_k"`
}

// MCP exposes read-only tools over the Model Context Protocol (optional side process).
type MCP struct {
	Enabled  bool   `yaml:"enabled"`
	Listen   string `yaml:"listen"`
	ReadOnly bool   `yaml:"read_only"`
}

type Prompts struct {
	Path string `yaml:"path"`
}

type RAG struct {
	RetrievalMode       string `yaml:"retrieval_mode"`
	BM25TopK            int    `yaml:"bm25_top_k"`
	VectorTopK          int    `yaml:"vector_top_k"`
	FinalTopK           int    `yaml:"final_top_k"`
	RRFK                int    `yaml:"rrf_k"`
	FallbackToBM25      bool   `yaml:"fallback_to_bm25"`
	ChunkSize           int    `yaml:"chunk_size"`
	ChunkOverlap        int    `yaml:"chunk_overlap"`
	CandidateMultiplier int    `yaml:"candidate_multiplier"`
	MaxChunksPerDoc     int    `yaml:"max_chunks_per_doc"`
	RerankerMode        string `yaml:"reranker_mode"` // lexical | model
}

// ResolvePath 返回配置文件路径：显式参数 > PILOT_CONFIG 环境变量 > 默认路径。
func ResolvePath(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if fromEnv := strings.TrimSpace(os.Getenv(EnvPathKey)); fromEnv != "" {
		return fromEnv
	}
	return DefaultPath
}

// Load 读取并解析配置文件：展开 ${ENV_VAR} 引用、拒绝未知键、填默认值并校验。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s failed: %w", path, err)
	}
	return Parse(raw)
}

// Parse 从原始 YAML 内容加载配置，行为与 Load 一致。
func Parse(raw []byte) (*Config, error) {
	expanded := expandEnvRefs(raw)

	cfg := &Config{}
	decoder := yaml.NewDecoder(bytes.NewReader(expanded))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config failed: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRefs 将 ${VAR} 替换为对应环境变量值；未设置的变量替换为空字符串。
func expandEnvRefs(raw []byte) []byte {
	return envRefPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := envRefPattern.FindSubmatch(match)[1]
		return []byte(os.Getenv(string(name)))
	})
}

func (c *Config) applyDefaults() {
	if c.Server.Address == "" {
		c.Server.Address = ":8080"
	}
	if c.Server.Env == "" {
		c.Server.Env = "local"
	}
	if c.Server.BaseURL == "" {
		c.Server.BaseURL = "http://localhost:8080"
	}
	if c.Logger.Level == "" {
		c.Logger.Level = "info"
	}
	if c.Embedding.Dimension <= 0 {
		c.Embedding.Dimension = 1024
	}
	if c.Redis.PoolSize <= 0 {
		c.Redis.PoolSize = 10
	}
	if c.Redis.Memory.TTLSeconds <= 0 {
		c.Redis.Memory.TTLSeconds = 3600
	}
	if c.Redis.Memory.MaxMessages <= 0 {
		c.Redis.Memory.MaxMessages = 20
	}
	if c.Redis.Memory.ChunkSize <= 0 {
		c.Redis.Memory.ChunkSize = 10
	}
	if c.Elasticsearch.KnowledgeIndex == "" {
		c.Elasticsearch.KnowledgeIndex = "pilot_knowledge"
	}
	if c.Elasticsearch.MaxResults <= 0 {
		c.Elasticsearch.MaxResults = 5
	}
	if c.Database.Port <= 0 {
		c.Database.Port = 3306
	}
	if c.Database.MaxIdleConns <= 0 {
		c.Database.MaxIdleConns = 10
	}
	if c.Database.MaxOpenConns <= 0 {
		c.Database.MaxOpenConns = 100
	}
	if c.Database.ConnMaxLifetimeSeconds <= 0 {
		c.Database.ConnMaxLifetimeSeconds = 3600
	}
	if c.Observability.ServiceName == "" {
		c.Observability.ServiceName = "pilot-api"
	}
	if c.Observability.Prometheus.TimeoutSeconds <= 0 {
		c.Observability.Prometheus.TimeoutSeconds = 30
	}
	if c.Observability.Log.Index == "" {
		c.Observability.Log.Index = "pilot_logs"
	}
	if c.Observability.Log.SlowRequestMS <= 0 {
		c.Observability.Log.SlowRequestMS = 1000
	}
	if c.Observability.Trace.OTLPHTTPEndpoint == "" {
		c.Observability.Trace.OTLPHTTPEndpoint = "http://localhost:4318"
	}
	if c.Observability.Trace.SampleRatio <= 0 || c.Observability.Trace.SampleRatio > 1 {
		c.Observability.Trace.SampleRatio = 1.0
	}
	if c.Agent.MaxIterations <= 0 {
		c.Agent.MaxIterations = 25
	}
	if c.Agent.TimeoutSeconds <= 0 {
		c.Agent.TimeoutSeconds = 300
	}
	c.Agent.MultiAgent.EvidenceMode = strings.ToLower(strings.TrimSpace(c.Agent.MultiAgent.EvidenceMode))
	if c.Agent.MultiAgent.EvidenceMode == "" {
		c.Agent.MultiAgent.EvidenceMode = "deterministic"
	}
	if c.Agent.MultiAgent.ReactMaxSteps == 0 {
		c.Agent.MultiAgent.ReactMaxSteps = 8
	}
	c.Agent.MultiAgent.Planner.Mode = strings.ToLower(strings.TrimSpace(c.Agent.MultiAgent.Planner.Mode))
	if c.Agent.MultiAgent.Planner.Mode == "" {
		c.Agent.MultiAgent.Planner.Mode = "rule"
	}
	if c.Agent.MultiAgent.Planner.MinConfidence <= 0 {
		c.Agent.MultiAgent.Planner.MinConfidence = 0.6
	}
	if c.Agent.MultiAgent.Planner.TimeoutMS <= 0 {
		c.Agent.MultiAgent.Planner.TimeoutMS = 3000
	}
	if c.Agent.MultiAgent.EvidenceWindowMinutes <= 0 {
		c.Agent.MultiAgent.EvidenceWindowMinutes = 30
	}
	if c.Agent.MultiAgent.BranchTimeoutSeconds < 0 {
		c.Agent.MultiAgent.BranchTimeoutSeconds = 0
	}
	if c.Agent.MultiAgent.MaxReplanRounds < 0 {
		c.Agent.MultiAgent.MaxReplanRounds = 0
	}
	if c.Agent.MultiAgent.MaxReplanRounds > 1 {
		c.Agent.MultiAgent.MaxReplanRounds = 1
	}
	c.Agent.MultiAgent.Runtime = strings.ToLower(strings.TrimSpace(c.Agent.MultiAgent.Runtime))
	if c.Agent.MultiAgent.Runtime == "" {
		c.Agent.MultiAgent.Runtime = "legacy"
	}
	c.Agent.MultiAgent.Verification.Mode = strings.ToLower(strings.TrimSpace(c.Agent.MultiAgent.Verification.Mode))
	if c.Agent.MultiAgent.Verification.Mode == "" {
		c.Agent.MultiAgent.Verification.Mode = "evidence_graph"
	}
	if c.Agent.MultiAgent.Verification.MinConfidence == 0 {
		c.Agent.MultiAgent.Verification.MinConfidence = 0.65
	}
	if c.Agent.MultiAgent.Verification.MinIndependentSources == 0 {
		c.Agent.MultiAgent.Verification.MinIndependentSources = 2
	}
	if c.Agent.MultiAgent.Verification.ReplanWindowMultiplier == 0 {
		c.Agent.MultiAgent.Verification.ReplanWindowMultiplier = 2
	}
	if c.Agent.MultiAgent.HITL.WideWindowMinutes <= 0 {
		c.Agent.MultiAgent.HITL.WideWindowMinutes = 120
	}
	c.Agent.MultiAgent.HITL.Store = strings.ToLower(strings.TrimSpace(c.Agent.MultiAgent.HITL.Store))
	if c.Agent.MultiAgent.HITL.Store == "" {
		c.Agent.MultiAgent.HITL.Store = "redis"
	}
	c.Memory.Episodic.Mode = strings.ToLower(strings.TrimSpace(c.Memory.Episodic.Mode))
	if c.Memory.Episodic.Mode == "" {
		c.Memory.Episodic.Mode = "sql"
	}
	if c.Memory.Episodic.Index == "" {
		c.Memory.Episodic.Index = "pilot_episodic"
	}
	if c.Memory.Episodic.TopK <= 0 {
		c.Memory.Episodic.TopK = 3
	}
	if c.MCP.Listen == "" {
		c.MCP.Listen = ":8081"
	}
	// MCP is read-only by product boundary; force true even if misconfigured.
	c.MCP.ReadOnly = true
	if strings.TrimSpace(c.Prompts.Path) == "" {
		c.Prompts.Path = "internal/prompts/prompts.yaml"
	}
	if c.RAG.RetrievalMode == "" {
		c.RAG.RetrievalMode = "hybrid"
	}
	if c.RAG.BM25TopK <= 0 {
		c.RAG.BM25TopK = c.Elasticsearch.MaxResults
	}
	if c.RAG.VectorTopK <= 0 {
		c.RAG.VectorTopK = c.Elasticsearch.MaxResults
	}
	if c.RAG.FinalTopK <= 0 {
		c.RAG.FinalTopK = c.Elasticsearch.MaxResults
	}
	if c.RAG.RRFK <= 0 {
		c.RAG.RRFK = 60
	}
	if c.RAG.ChunkSize <= 0 {
		c.RAG.ChunkSize = 800
	}
	if c.RAG.ChunkOverlap < 0 {
		c.RAG.ChunkOverlap = 0
	}
	if c.RAG.CandidateMultiplier <= 0 {
		c.RAG.CandidateMultiplier = 3
	}
	if c.RAG.MaxChunksPerDoc <= 0 {
		c.RAG.MaxChunksPerDoc = 2
		if c.RAG.FinalTopK < c.RAG.MaxChunksPerDoc {
			c.RAG.MaxChunksPerDoc = c.RAG.FinalTopK
		}
	}
	c.RAG.RerankerMode = strings.ToLower(strings.TrimSpace(c.RAG.RerankerMode))
	if c.RAG.RerankerMode == "" {
		c.RAG.RerankerMode = "lexical"
	}
}

// Validate 校验运行所需的结构性配置；密钥类配置由具体消费方（如模型构造）校验。
func (c *Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Redis.Addr) == "" {
		problems = append(problems, "redis.addr 不能为空")
	}
	if len(c.Elasticsearch.Addresses) == 0 {
		problems = append(problems, "elasticsearch.addresses 不能为空")
	}
	if strings.TrimSpace(c.Database.Host) == "" {
		problems = append(problems, "database.host 不能为空")
	}
	if strings.TrimSpace(c.Database.Name) == "" {
		problems = append(problems, "database.name 不能为空")
	}
	if strings.TrimSpace(c.Observability.Prometheus.URL) == "" {
		problems = append(problems, "observability.prometheus.url 不能为空")
	}
	if strings.TrimSpace(c.Observability.Log.URL) == "" {
		problems = append(problems, "observability.log.url 不能为空")
	}
	if strings.TrimSpace(c.Observability.Trace.URL) == "" {
		problems = append(problems, "observability.trace.url 不能为空")
	}
	switch c.RAG.RetrievalMode {
	case "bm25", "vector", "hybrid":
	default:
		problems = append(problems, fmt.Sprintf("rag.retrieval_mode 不支持 %q（可选 bm25、vector、hybrid）", c.RAG.RetrievalMode))
	}
	if c.RAG.ChunkSize < 64 || c.RAG.ChunkSize > 32000 {
		problems = append(problems, "rag.chunk_size 必须在 [64,32000]")
	}
	if c.RAG.ChunkOverlap < 0 || c.RAG.ChunkOverlap >= c.RAG.ChunkSize {
		problems = append(problems, "rag.chunk_overlap 必须大于等于 0 且小于 chunk_size")
	}
	if c.RAG.CandidateMultiplier < 1 || c.RAG.CandidateMultiplier > 20 {
		problems = append(problems, "rag.candidate_multiplier 必须在 [1,20]")
	}
	if c.RAG.MaxChunksPerDoc < 1 || c.RAG.MaxChunksPerDoc > c.RAG.FinalTopK {
		problems = append(problems, "rag.max_chunks_per_doc 必须在 [1,final_top_k]")
	}
	switch c.RAG.RerankerMode {
	case "lexical", "model":
	default:
		problems = append(problems, fmt.Sprintf("rag.reranker_mode 不支持 %q（可选 lexical、model）", c.RAG.RerankerMode))
	}
	switch c.Agent.MultiAgent.EvidenceMode {
	case "deterministic", "react":
	default:
		problems = append(problems, fmt.Sprintf("agent.multi_agent.evidence_mode 不支持 %q（可选 deterministic、react）", c.Agent.MultiAgent.EvidenceMode))
	}
	switch c.Agent.MultiAgent.Planner.Mode {
	case "rule", "hybrid":
	default:
		problems = append(problems, fmt.Sprintf("agent.multi_agent.planner.mode 不支持 %q（可选 rule、hybrid）", c.Agent.MultiAgent.Planner.Mode))
	}
	switch c.Agent.MultiAgent.Runtime {
	case "legacy", "adk":
	default:
		problems = append(problems, fmt.Sprintf("agent.multi_agent.runtime 不支持 %q（可选 legacy、adk）", c.Agent.MultiAgent.Runtime))
	}
	switch c.Agent.MultiAgent.Verification.Mode {
	case "evidence_graph", "off":
	default:
		problems = append(problems, fmt.Sprintf("agent.multi_agent.verification.mode 不支持 %q（可选 evidence_graph、off）", c.Agent.MultiAgent.Verification.Mode))
	}
	switch c.Agent.MultiAgent.HITL.Store {
	case "redis", "memory":
	default:
		problems = append(problems, fmt.Sprintf("agent.multi_agent.hitl.store 不支持 %q（可选 redis、memory）", c.Agent.MultiAgent.HITL.Store))
	}
	switch c.Memory.Episodic.Mode {
	case "sql", "hybrid":
	default:
		problems = append(problems, fmt.Sprintf("memory.episodic.mode 不支持 %q（可选 sql、hybrid）", c.Memory.Episodic.Mode))
	}
	if c.Agent.MultiAgent.Planner.MinConfidence <= 0 || c.Agent.MultiAgent.Planner.MinConfidence > 1 {
		problems = append(problems, "agent.multi_agent.planner.min_confidence 必须在 (0,1]")
	}
	if c.Agent.MultiAgent.Verification.MinConfidence <= 0 || c.Agent.MultiAgent.Verification.MinConfidence > 1 {
		problems = append(problems, "agent.multi_agent.verification.min_confidence 必须在 (0,1]")
	}
	if c.Agent.MultiAgent.Verification.MinIndependentSources < 1 || c.Agent.MultiAgent.Verification.MinIndependentSources > 8 {
		problems = append(problems, "agent.multi_agent.verification.min_independent_sources 必须在 [1,8]")
	}
	if c.Agent.MultiAgent.Verification.ReplanWindowMultiplier < 1 || c.Agent.MultiAgent.Verification.ReplanWindowMultiplier > 8 {
		problems = append(problems, "agent.multi_agent.verification.replan_window_multiplier 必须在 [1,8]")
	}
	if c.Agent.MultiAgent.ReactMaxSteps < 1 {
		problems = append(problems, "agent.multi_agent.react_max_steps 必须大于 0")
	}
	if c.Security.Auth.Enabled && strings.TrimSpace(c.Security.Auth.APIKey) == "" {
		problems = append(problems, "security.auth.enabled=true 时 security.auth.api_key 不能为空")
	}
	if c.Security.RateLimit.Enabled && c.Security.RateLimit.RequestsPerMinute < 1 {
		problems = append(problems, "security.rate_limit.enabled=true 时 requests_per_minute 必须大于 0")
	}
	hasStar, hasOther := false, false
	for _, origin := range c.Security.CORS.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			hasStar = true
		} else {
			hasOther = true
		}
	}
	if hasStar && hasOther {
		problems = append(problems, `security.cors.allowed_origins 不能同时包含 "*" 与具体来源`)
	}
	if len(problems) > 0 {
		return fmt.Errorf("config invalid: %s", strings.Join(problems, "; "))
	}
	return nil
}
