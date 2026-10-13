package controller

import (
	"context"
	"strings"

	"shanyangcode.com/pilot/internal/ai/multiagent"
)

// saveMultiAgentResult persists synchronous multi-agent results and approval state.
func (c *ChatController) saveMultiAgentResult(ctx context.Context, interactionID string, req *ChatRequest, result *multiagent.Result) bool {
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["interaction_id"] = interactionID
	if status, _ := result.Metadata["status"].(string); status == "interrupted" {
		c.recordMultiAgentInteraction(ctx, interactionID, req, *result, "interrupted")
		return true
	}
	c.persistTurn(ctx, req.UserID, req.SessionID, req.Query, result.Answer, persistTurnOptions{
		SaveShortTermUser: true, SaveShortTermAssistant: true, SaveHistory: true,
	})
	status := "completed"
	if result.Verification != nil {
		switch strings.ToLower(result.Verification.Decision) {
		case "abstain", "rejected":
			status = strings.ToLower(result.Verification.Decision)
		}
	}
	c.recordMultiAgentInteraction(ctx, interactionID, req, *result, status)
	return false
}
