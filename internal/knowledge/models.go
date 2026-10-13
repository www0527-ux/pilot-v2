// Package knowledge manages account-private incident experience. Ordinary RAG
// documents and model working context deliberately use separate stores.
package knowledge

import (
	"encoding/json"
	"shanyangcode.com/pilot/internal/memory"
	"time"
)

type Entry struct {
	ID               string    `gorm:"primaryKey;size:64" json:"id"`
	OwnerID          string    `gorm:"size:128;index;not null" json:"-"`
	JobID            string    `gorm:"size:64;uniqueIndex:job_candidate" json:"job_id"`
	Ordinal          int       `gorm:"uniqueIndex:job_candidate" json:"-"`
	Type             string    `gorm:"size:32;index" json:"type"`
	SourceRunID      string    `gorm:"size:64;index" json:"source_run_id"`
	SessionID        string    `gorm:"size:128" json:"session_id"`
	Lifecycle        string    `gorm:"size:32;index" json:"lifecycle"`
	Revision         uint64    `json:"revision"`
	LatestVersion    int       `json:"latest_version"`
	PublishedVersion int       `json:"published_version"`
	ArchiveReason    string    `gorm:"type:text" json:"archive_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (Entry) TableName() string { return "experience_entries" }

// Reviews live with immutable published content; draft edits use Entry.Revision.
type Version struct {
	ID           uint       `gorm:"primaryKey" json:"-"`
	EntryID      string     `gorm:"size:64;uniqueIndex:entry_version" json:"entry_id"`
	Number       int        `gorm:"uniqueIndex:entry_version" json:"number"`
	Title        string     `gorm:"size:240" json:"title"`
	Service      string     `gorm:"size:128;index" json:"service"`
	TagsJSON     string     `gorm:"type:text" json:"-"`
	Phenomenon   string     `gorm:"type:text" json:"phenomenon"`
	RootCause    string     `gorm:"type:text" json:"root_cause"`
	CauseStatus  string     `gorm:"size:32" json:"cause_status"`
	Solution     string     `gorm:"type:text" json:"solution"`
	Validation   string     `gorm:"type:text" json:"validation"`
	Conditions   string     `gorm:"type:text" json:"conditions"`
	Verification string     `gorm:"size:32;index" json:"verification"`
	ReviewStatus string     `gorm:"size:32;index" json:"review_status"`
	ReviewerID   string     `gorm:"size:128" json:"reviewer_id,omitempty"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	ReviewNote   string     `gorm:"type:text" json:"review_note,omitempty"`
	EvidenceJSON string     `gorm:"type:longtext" json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (Version) TableName() string { return "experience_versions" }

type Evidence struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	Sequence   uint64          `json:"sequence"`
	Tool       string          `json:"tool"`
	ObservedAt time.Time       `json:"observed_at"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result"`
}
type Candidate struct {
	Action         string   `json:"action,omitempty" jsonschema:"description=知识更新决策,enum=CREATE,enum=LINK,enum=ENRICH,enum=NOOP"`
	IssueID        string   `json:"issue_id,omitempty"`
	TargetID       string   `json:"target_id,omitempty"`
	TargetRevision uint64   `json:"target_revision,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Title          string   `json:"title"`
	Service        string   `json:"service"`
	Tags           []string `json:"tags"`
	Phenomenon     string   `json:"phenomenon"`
	RootCause      string   `json:"root_cause"`
	CauseStatus    string   `json:"cause_status" jsonschema:"description=根因状态；模型草稿应采用hypothesis或unknown,enum=hypothesis,enum=unknown,enum=confirmed"`
	Solution       string   `json:"solution"`
	Validation     string   `json:"validation"`
	Conditions     string   `json:"conditions"`
	Verification   string   `json:"verification" jsonschema:"description=模型提交必须为unverified，人工审核独立修改,enum=unverified"`
	EvidenceIDs    []string `json:"evidence_ids"`
}
type Detail struct {
	Entry    Entry            `json:"entry"`
	Version  Version          `json:"version"`
	Tags     []string         `json:"tags"`
	Evidence []Evidence       `json:"evidence"`
	Sources  []IssueKnowledge `json:"sources"`
}
type Source struct {
	Run          memory.AgentRun              `json:"run"`
	Conversation []memory.ConversationHistory `json:"conversation"`
	Evidence     []Evidence                   `json:"evidence"`
	Limitations  []string                     `json:"limitations"`
	Issues       []Issue                      `json:"issues,omitempty"`
	Existing     []Detail                     `json:"existing,omitempty"`
	Unassigned   []IssueAssociation           `json:"unassigned,omitempty"`
	PriorEvents  []map[string]any             `json:"prior_events,omitempty"`
}
type Job struct {
	ID            string     `gorm:"primaryKey;size:64" json:"id"`
	OwnerID       string     `gorm:"size:128;uniqueIndex:owner_run" json:"-"`
	RunID         string     `gorm:"size:64;uniqueIndex:owner_run" json:"run_id"`
	SessionID     string     `gorm:"size:128;index" json:"session_id"`
	SourceAt      time.Time  `gorm:"index" json:"-"`
	Manual        bool       `json:"manual"`
	Tracked       bool       `json:"tracked"`
	IssueIDsJSON  string     `gorm:"type:text" json:"-"`
	Status        string     `gorm:"size:32;index" json:"status"`
	SnapshotJSON  string     `gorm:"type:longtext" json:"-"`
	Attempts      int        `json:"attempts"`
	LeaseToken    string     `gorm:"size:64" json:"-"`
	LeaseUntil    *time.Time `json:"-"`
	NextAttemptAt time.Time  `json:"-"`
	Error         string     `gorm:"type:text" json:"error,omitempty"`
	DraftCount    int        `json:"draft_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (Job) TableName() string { return "experience_jobs" }

type Settings struct {
	OwnerID   string    `gorm:"primaryKey;size:128" json:"-"`
	Automatic bool      `json:"automatic"`
	EnabledAt time.Time `json:"enabled_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Settings) TableName() string { return "experience_settings" }

type Reference struct {
	ID        uint      `gorm:"primaryKey" json:"-"`
	OwnerID   string    `gorm:"size:128;uniqueIndex:run_knowledge" json:"-"`
	RunID     string    `gorm:"size:64;uniqueIndex:run_knowledge" json:"run_id"`
	EntryID   string    `gorm:"size:64;uniqueIndex:run_knowledge" json:"entry_id"`
	Version   int       `gorm:"uniqueIndex:run_knowledge" json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func (Reference) TableName() string { return "experience_references" }
