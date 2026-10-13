package controller

import (
	"context"
	"log/slog"

	"shanyangcode.com/pilot/pkg/types"
)

// persistTurnOptions 控制一轮对话的记忆写入范围。
type persistTurnOptions struct {
	RunID string
	// SaveShortTermUser 写入短期记忆中的用户消息。
	SaveShortTermUser bool
	// SaveShortTermAssistant 写入短期记忆中的助手消息。
	SaveShortTermAssistant bool
	// SaveHistory 写入 MySQL 长期历史（用户 + 助手）。
	SaveHistory bool
}

// persistTurn 统一 Chat / Stream / MultiAgent 的记忆写入。
// 记忆失败不影响主响应，但必须打 Warn 日志，避免静默丢弃。
func (c *ChatController) persistTurn(ctx context.Context, userID, sessionID, query, answer string, opts persistTurnOptions) {
	if opts.SaveShortTermUser {
		if err := c.shortTermMemory.SaveMessage(ctx, userID, sessionID, types.ChatMessage{
			Role:    "user",
			Content: query,
			RunID:   opts.RunID,
		}); err != nil {
			slog.WarnContext(ctx, "save user message to short-term memory failed",
				"error", err, "user_id", userID, "session_id", sessionID)
		}
	}
	if opts.SaveShortTermAssistant && answer != "" {
		if err := c.shortTermMemory.SaveMessage(ctx, userID, sessionID, types.ChatMessage{
			Role:    "assistant",
			Content: answer,
			RunID:   opts.RunID,
		}); err != nil {
			slog.WarnContext(ctx, "save assistant message to short-term memory failed",
				"error", err, "user_id", userID, "session_id", sessionID)
		}
	}
	if !opts.SaveHistory {
		return
	}
	save := func(role, content string) error {
		if store, ok := c.historyManager.(interface {
			SaveMessageForRun(context.Context, string, string, string, string, string) error
		}); ok && opts.RunID != "" {
			return store.SaveMessageForRun(ctx, userID, sessionID, role, content, opts.RunID)
		}
		return c.historyManager.SaveMessage(ctx, userID, sessionID, role, content)
	}
	if err := save("user", query); err != nil {
		slog.WarnContext(ctx, "save user message to mysql failed",
			"error", err, "user_id", userID, "session_id", sessionID)
	}
	if answer != "" {
		if err := save("assistant", answer); err != nil {
			slog.WarnContext(ctx, "save assistant message to mysql failed",
				"error", err, "user_id", userID, "session_id", sessionID)
		}
	}
}
