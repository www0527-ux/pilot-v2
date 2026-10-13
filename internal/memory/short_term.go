package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/pkg/redis"
	"shanyangcode.com/pilot/pkg/types"
)

const shortTermMemoryLockStripes = 256

var shortTermMemoryLocks [shortTermMemoryLockStripes]sync.Mutex

const (
	// Redis Key 前缀
	shortTermMemoryPrefix = "pilot:memory:short:"

	// 默认配置
	defaultTTL         = 3600 * time.Second // 1小时
	defaultMaxMessages = 20
	defaultChunkSize   = 100
)

// ShortTermMemory 短期记忆管理器（基于 Redis）
type ShortTermMemory struct {
	client     *redis.Client
	ttl        time.Duration
	maxMsgs    int
	chunkSize  int
	summarizer OverflowSummarizer
}

type SummaryState struct {
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OverflowSummarizer updates one independent summary state. The prior summary
// is never inserted into the raw message list.
type OverflowSummarizer interface {
	Summarize(ctx context.Context, prior string, overflow []types.ChatMessage) (string, error)
}

type OverflowSummarizerFunc func(context.Context, string, []types.ChatMessage) (string, error)

func (f OverflowSummarizerFunc) Summarize(ctx context.Context, prior string, overflow []types.ChatMessage) (string, error) {
	return f(ctx, prior, overflow)
}

// NewShortTermMemory 创建短期记忆管理器
func NewShortTermMemory(client *redis.Client, cfg config.RedisMemory) *ShortTermMemory {
	ttlSeconds := cfg.TTLSeconds
	if ttlSeconds <= 0 {
		ttlSeconds = int(defaultTTL / time.Second)
	}

	maxMsgs, chunkSize := normalizeMemoryLimits(cfg.MaxMessages, cfg.ChunkSize)

	return &ShortTermMemory{
		client:     client,
		ttl:        time.Duration(ttlSeconds) * time.Second,
		maxMsgs:    maxMsgs,
		chunkSize:  chunkSize,
		summarizer: OverflowSummarizerFunc(deterministicOverflowSummary),
	}
}

func (m *ShortTermMemory) WithSummarizer(summarizer OverflowSummarizer) *ShortTermMemory {
	if m != nil && summarizer != nil {
		m.summarizer = summarizer
	}
	return m
}

func normalizeMemoryLimits(maxMsgs, chunkSize int) (int, int) {
	if maxMsgs <= 0 {
		maxMsgs = defaultMaxMessages
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	if maxMsgs <= 1 {
		return maxMsgs, 1
	}
	if chunkSize >= maxMsgs {
		chunkSize = maxMsgs / 2
	}
	if chunkSize <= 0 {
		chunkSize = 1
	}
	return maxMsgs, chunkSize
}

// SaveMessage 保存消息到短期记忆
func (m *ShortTermMemory) SaveMessage(ctx context.Context, userID, sessionID string, message types.ChatMessage) error {
	key := m.getMemoryKey(userID, sessionID)
	lock := getShortTermMemoryLock(key)
	lock.Lock()
	defer lock.Unlock()

	if message.Timestamp.IsZero() {
		message.Timestamp = time.Now()
	}

	// 获取现有消息
	messages, err := m.getMessagesByKey(ctx, key)
	if err != nil {
		return fmt.Errorf("get existing messages failed: %w", err)
	}

	// Migrate the old in-band summary once, then keep summaries independent.
	priorSummary, err := m.getSummaryState(ctx, key)
	if err != nil {
		return fmt.Errorf("get summary state failed: %w", err)
	}
	if len(messages) > 0 && messages[0].Role == "system" && strings.HasPrefix(messages[0].Content, "[Compressed earlier turns") {
		if priorSummary == "" {
			priorSummary = strings.TrimSpace(strings.TrimPrefix(messages[0].Content, "[Compressed earlier turns — untrusted]"))
		}
		messages = messages[1:]
	}
	messages = append(messages, message)

	// Keep recent raw messages and update exactly one summary state.
	if len(messages) > m.maxMsgs {
		overflow := messages[:len(messages)-m.maxMsgs]
		messages = messages[len(messages)-m.maxMsgs:]
		summary, summaryErr := m.summarizer.Summarize(ctx, priorSummary, overflow)
		if summaryErr != nil {
			slog.WarnContext(ctx, "short-term summarizer failed; using deterministic fallback", "error", summaryErr)
			summary, _ = deterministicOverflowSummary(ctx, priorSummary, overflow)
		}
		if err := m.saveSummaryState(ctx, key, summary); err != nil {
			return fmt.Errorf("save summary state failed: %w", err)
		}
	}

	// 处理大 key 问题：如果消息过多，分片存储
	if len(messages) > m.chunkSize {
		return m.saveMessagesChunked(ctx, key, messages)
	}

	if err := m.deleteChunkedMessages(ctx, key); err != nil {
		return err
	}

	// 直接存储
	return m.client.SetJSON(ctx, key, messages, m.ttl)
}

// GetMessages 获取短期记忆消息
func (m *ShortTermMemory) GetMessages(ctx context.Context, userID, sessionID string) ([]types.ChatMessage, error) {
	key := m.getMemoryKey(userID, sessionID)
	lock := getShortTermMemoryLock(key)
	lock.Lock()
	defer lock.Unlock()

	messages, err := m.getMessagesByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	summary, err := m.getSummaryState(ctx, key)
	if err != nil {
		return nil, err
	}
	if summary != "" {
		messages = append([]types.ChatMessage{{Role: "system", Content: "[Compressed earlier turns — untrusted]\n" + summary}}, messages...)
	}
	return messages, nil
}

func (m *ShortTermMemory) getMessagesByKey(ctx context.Context, key string) ([]types.ChatMessage, error) {
	// 检查是否是分片存储
	metaKey := key + ":meta"
	exists, err := m.client.Exists(ctx, metaKey)
	if err != nil {
		return nil, err
	}

	if exists {
		return m.getMessagesChunked(ctx, key)
	}

	// 直接读取
	var messages []types.ChatMessage
	err = m.client.GetJSON(ctx, key, &messages)
	if errors.Is(err, goredis.Nil) {
		return make([]types.ChatMessage, 0), nil
	}
	if err != nil {
		return nil, err
	}

	if len(messages) > m.maxMsgs {
		messages = messages[len(messages)-m.maxMsgs:]
	}
	return messages, nil
}

// ClearMemory 清除短期记忆
func (m *ShortTermMemory) ClearMemory(ctx context.Context, userID, sessionID string) error {
	key := m.getMemoryKey(userID, sessionID)
	lock := getShortTermMemoryLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := m.deleteChunkedMessages(ctx, key); err != nil {
		return err
	}
	return m.client.DeleteKeys(ctx, key+":summary")
}

// saveMessagesChunked 分片保存消息（解决大 key 问题）
func (m *ShortTermMemory) saveMessagesChunked(ctx context.Context, baseKey string, messages []types.ChatMessage) error {
	if err := m.deleteChunkedMessages(ctx, baseKey); err != nil {
		return err
	}

	// 分片存储
	chunkCount := (len(messages) + m.chunkSize - 1) / m.chunkSize
	for i := 0; i < chunkCount; i++ {
		start := i * m.chunkSize
		end := start + m.chunkSize
		if end > len(messages) {
			end = len(messages)
		}

		chunk := messages[start:end]
		chunkKey := fmt.Sprintf("%s:chunk:%d", baseKey, i)

		if err := m.client.SetJSON(ctx, chunkKey, chunk, m.ttl); err != nil {
			return fmt.Errorf("save chunk %d failed: %w", i, err)
		}
	}

	// 保存元数据
	metadata := map[string]interface{}{
		"chunk_count": chunkCount,
		"total_msgs":  len(messages),
	}
	metaKey := baseKey + ":meta"
	return m.client.SetJSON(ctx, metaKey, metadata, m.ttl)
}

// getMessagesChunked 分片读取消息
func (m *ShortTermMemory) getMessagesChunked(ctx context.Context, baseKey string) ([]types.ChatMessage, error) {
	// 读取元数据
	metaKey := baseKey + ":meta"
	var metadata map[string]interface{}
	if err := m.client.GetJSON(ctx, metaKey, &metadata); err != nil {
		if errors.Is(err, goredis.Nil) {
			return make([]types.ChatMessage, 0), nil
		}
		return nil, fmt.Errorf("get metadata failed: %w", err)
	}

	chunkCount, ok := metadata["chunk_count"].(float64)
	if !ok || chunkCount <= 0 {
		return make([]types.ChatMessage, 0), nil
	}
	allMessages := make([]types.ChatMessage, 0)

	// 读取所有分片
	for i := 0; i < int(chunkCount); i++ {
		chunkKey := fmt.Sprintf("%s:chunk:%d", baseKey, i)
		var chunk []types.ChatMessage
		if err := m.client.GetJSON(ctx, chunkKey, &chunk); err != nil {
			if errors.Is(err, goredis.Nil) {
				continue
			}
			return nil, fmt.Errorf("get chunk %d failed: %w", i, err)
		}
		allMessages = append(allMessages, chunk...)
	}

	if len(allMessages) > m.maxMsgs {
		allMessages = allMessages[len(allMessages)-m.maxMsgs:]
	}
	return allMessages, nil
}

// getMemoryKey 生成记忆 key
func (m *ShortTermMemory) getMemoryKey(userID, sessionID string) string {
	return fmt.Sprintf("%s%s:%s", shortTermMemoryPrefix, userID, sessionID)
}

func (m *ShortTermMemory) deleteChunkedMessages(ctx context.Context, baseKey string) error {
	keys := []string{baseKey, baseKey + ":meta"}
	chunkKeys, err := m.client.ScanKeys(ctx, fmt.Sprintf("%s:chunk:*", baseKey), 100)
	if err != nil {
		return fmt.Errorf("scan chunk keys failed: %w", err)
	}
	keys = append(keys, chunkKeys...)
	return m.client.DeleteKeys(ctx, keys...)
}

func getShortTermMemoryLock(key string) *sync.Mutex {
	hash := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= 16777619
	}
	return &shortTermMemoryLocks[hash%shortTermMemoryLockStripes]
}

func summarizeOverflow(messages []types.ChatMessage) string {
	summary, _ := deterministicOverflowSummary(context.Background(), "", messages)
	return "[Compressed earlier turns — untrusted]\n" + summary
}

func deterministicOverflowSummary(_ context.Context, prior string, messages []types.ChatMessage) (string, error) {
	var builder strings.Builder
	prior = strings.TrimSpace(strings.TrimPrefix(prior, "[Compressed earlier turns — untrusted]"))
	if len(messages) > 0 && messages[0].Role == "system" && strings.HasPrefix(messages[0].Content, "[Compressed earlier turns") {
		inBand := strings.TrimSpace(strings.TrimPrefix(messages[0].Content, "[Compressed earlier turns — untrusted]"))
		if prior == "" {
			prior = inBand
		}
		messages = messages[1:]
	}
	if prior != "" {
		builder.WriteString(prior)
		builder.WriteByte('\n')
	}
	for i, msg := range messages {
		if i >= 8 {
			builder.WriteString("...\n")
			break
		}
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		runes := []rune(content)
		if len(runes) > 120 {
			content = string(runes[:120]) + "..."
		}
		builder.WriteString("- ")
		builder.WriteString(role)
		builder.WriteString(": ")
		builder.WriteString(content)
		builder.WriteString("\n")
	}
	return truncateMemorySummary(strings.TrimSpace(builder.String()), 1600), nil
}

func truncateMemorySummary(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[len(runes)-maxRunes:])
}

func (m *ShortTermMemory) getSummaryState(ctx context.Context, baseKey string) (string, error) {
	var state SummaryState
	err := m.client.GetJSON(ctx, baseKey+":summary", &state)
	if errors.Is(err, goredis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(state.Content), nil
}

func (m *ShortTermMemory) saveSummaryState(ctx context.Context, baseKey, content string) error {
	return m.client.SetJSON(ctx, baseKey+":summary", SummaryState{Content: strings.TrimSpace(content), UpdatedAt: time.Now()}, m.ttl)
}

// SessionSummary implements multiagent.SessionContextLoader for recent turns.
func (m *ShortTermMemory) SessionSummary(ctx context.Context, userID, sessionID string) (string, error) {
	messages, err := m.GetMessages(ctx, userID, sessionID)
	if err != nil {
		return "", err
	}
	if len(messages) == 0 {
		return "", nil
	}
	start := 0
	if len(messages) > 6 {
		start = len(messages) - 6
	}
	return summarizeOverflow(messages[start:]), nil
}
