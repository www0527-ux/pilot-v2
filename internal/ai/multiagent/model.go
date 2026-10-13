package multiagent

import "time"

// Role 表示事件分析流程中的职责角色。
type Role string

const (
	RoleTriage      Role = "triage"
	RoleEvidence    Role = "evidence"
	RoleKnowledge   Role = "knowledge"
	RoleSynthesis   Role = "synthesis"
	StatusCompleted      = "completed"
	StatusFailed         = "failed"
	StatusSkipped        = "skipped"
)

// Step 记录角色的可观测执行结果，不暴露提示词或模型推理过程。
type Step struct {
	Role       Role           `json:"role"`
	Status     string         `json:"status"`
	Summary    string         `json:"summary,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	DurationMS int64          `json:"duration_ms"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// Limitation 表示最终结果中的数据缺口或单角色失败。
type Limitation struct {
	Role    Role   `json:"role"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Plan 是确定性路由结果，供各角色 Agent 共用。
type Plan struct {
	Intent        string         `json:"intent"`
	Service       string         `json:"service"`
	SelectedRoles []Role         `json:"selected_roles"`
	SkippedRoles  []Role         `json:"skipped_roles"`
	RoutingReason string         `json:"routing_reason"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// Finding 是单个角色基于证据产出的结论。
type Finding struct {
	Role     Role           `json:"role"`
	Summary  string         `json:"summary"`
	Evidence []EvidenceItem `json:"evidence,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// EvidenceItem 是提供给综合角色的精简、可溯源证据项。
type EvidenceItem struct {
	ID       string         `json:"id"`
	Source   string         `json:"source"`
	Content  string         `json:"content"`
	Score    *float64       `json:"score,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Result 是多 Agent 接口返回值，可安全持久化为聊天回复。
type Result struct {
	Answer        string              `json:"answer"`
	Plan          Plan                `json:"plan"`
	Steps         []Step              `json:"steps"`
	Findings      []Finding           `json:"findings"`
	Limitations   []Limitation        `json:"limitations,omitempty"`
	EvidenceGraph *EvidenceGraph      `json:"evidence_graph,omitempty"`
	Verification  *VerificationReport `json:"verification,omitempty"`
	Metadata      map[string]any      `json:"metadata,omitempty"`
}

func roleOrder() []Role {
	return []Role{RoleTriage, RoleEvidence, RoleKnowledge, RoleSynthesis}
}
