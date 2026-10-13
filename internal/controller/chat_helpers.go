package controller

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"shanyangcode.com/pilot/internal/memory"
	"shanyangcode.com/pilot/pkg/types"
)

const (
	maxUserIDLength     = 128
	maxSessionIDLength  = 128
	maxChatQueryLength  = 8000
	defaultHistoryLimit = 100
	maxHistoryLimit     = 500

	maxKnowledgeTitleLength    = 200
	maxKnowledgeContentLength  = 50000
	maxKnowledgeCategoryLength = 64
	maxKnowledgeQueryLength    = 1000
	maxKnowledgeDocIDLength    = 256
)

// normalizeChatRequest 对 ChatRequest 中的字段进行规范化和验证，确保用户ID、会话ID和查询内容符合要求
func normalizeChatRequest(req *ChatRequest) error {
	var err error
	req.UserID, err = normalizeBoundedString(req.UserID, "用户ID", maxUserIDLength)
	if err != nil {
		return err
	}

	req.SessionID, err = normalizeBoundedString(req.SessionID, "会话ID", maxSessionIDLength)
	if err != nil {
		return err
	}

	req.Query, err = normalizeBoundedString(req.Query, "查询内容", maxChatQueryLength)
	return err
}

// normalizeHistoryParams 对历史记录查询的用户ID和会话ID进行规范化和验证，确保它们符合要求
func normalizeHistoryParams(userID, sessionID string) (string, string, error) {
	normalizedUserID, err := normalizeBoundedString(userID, "用户ID", maxUserIDLength)
	if err != nil {
		return "", "", err
	}

	normalizedSessionID, err := normalizeBoundedString(sessionID, "会话ID", maxSessionIDLength)
	if err != nil {
		return "", "", err
	}

	return normalizedUserID, normalizedSessionID, nil
}

// normalizeHistoryLimit 对历史记录查询的 limit 参数进行规范化，确保它在合理范围内
func normalizeHistoryLimit(raw int) int {
	if raw <= 0 {
		return defaultHistoryLimit
	}
	if raw > maxHistoryLimit {
		return maxHistoryLimit
	}
	return raw
}

// parseIntDefault 解析整数查询参数，解析失败时返回默认值。
func parseIntDefault(raw string, defaultValue int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return defaultValue
	}
	return value
}

// normalizeBoundedString 对输入字符串进行规范化，去除前后空格，并检查是否为空或超过最大长度
func normalizeBoundedString(value, field string, maxLength int) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", fmt.Errorf("%s不能为空", field)
	}
	if len([]rune(normalized)) > maxLength {
		return "", fmt.Errorf("%s长度不能超过%d个字符", field, maxLength)
	}
	return normalized, nil
}

func normalizeKnowledgeUpload(req *UploadRequest) error {
	var err error
	req.Title, err = normalizeBoundedString(req.Title, "标题", maxKnowledgeTitleLength)
	if err != nil {
		return err
	}
	// Preserve leading indentation and source offsets in Markdown/code blocks.
	if strings.TrimSpace(req.Content) == "" {
		return fmt.Errorf("内容不能为空")
	}
	if len([]rune(req.Content)) > maxKnowledgeContentLength {
		return fmt.Errorf("内容长度不能超过%d个字符", maxKnowledgeContentLength)
	}
	req.Category, err = normalizeBoundedString(req.Category, "分类", maxKnowledgeCategoryLength)
	if err != nil {
		return err
	}
	if len(req.Tags) > 32 {
		return fmt.Errorf("标签数量不能超过32个")
	}
	seenTags := make(map[string]struct{}, len(req.Tags))
	normalizedTags := make([]string, 0, len(req.Tags))
	for _, tag := range req.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len([]rune(tag)) > 64 {
			return fmt.Errorf("单个标签长度不能超过64个字符")
		}
		if _, exists := seenTags[tag]; exists {
			continue
		}
		seenTags[tag] = struct{}{}
		normalizedTags = append(normalizedTags, tag)
	}
	req.Tags = normalizedTags
	if len(req.Metadata) > 32 {
		return fmt.Errorf("metadata 字段数量不能超过32个")
	}
	for key, value := range req.Metadata {
		if strings.TrimSpace(key) == "" || len([]rune(key)) > 64 || len([]rune(value)) > 500 {
			return fmt.Errorf("metadata 键不能为空且键/值长度不能超过64/500个字符")
		}
	}
	return nil
}

func normalizeKnowledgeSearch(req *SearchRequest) error {
	var err error
	req.Query, err = normalizeBoundedString(req.Query, "查询内容", maxKnowledgeQueryLength)
	return err
}

func normalizeKnowledgeDocID(value string) (string, error) {
	docID, err := normalizeBoundedString(value, "文档ID", maxKnowledgeDocIDLength)
	if err != nil {
		return "", err
	}
	for _, r := range docID {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("文档ID只能包含字母、数字、点、下划线和短横线")
	}
	return docID, nil
}

// buildChatMessagesFromHistory 从 ConversationHistory 记录构建 ChatMessage 列表，用于生成聊天上下文
func buildChatMessagesFromHistory(records []memory.ConversationHistory) []types.ChatMessage {
	messages := make([]types.ChatMessage, 0, len(records))
	for _, item := range records {
		var metadata struct {
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal([]byte(item.Metadata), &metadata)
		messages = append(messages, types.ChatMessage{
			RunID:     metadata.RunID,
			Role:      item.Role,
			Content:   item.Content,
			Timestamp: item.CreatedAt,
		})
	}
	return messages
}
