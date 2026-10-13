package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"shanyangcode.com/pilot/internal/ai/multiagent"
)

type httpClient struct {
	baseURL string
	client  *http.Client
}

func (c *httpClient) multiAgent(ctx context.Context, query string) (multiagent.Result, error) {
	if c.client == nil {
		c.client = &http.Client{Timeout: 180 * time.Second}
	}
	body, _ := json.Marshal(map[string]string{
		"user_id":    "agenteval",
		"session_id": fmt.Sprintf("eval-%d", time.Now().UnixNano()),
		"query":      query,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/chat/multi-agent", bytes.NewReader(body))
	if err != nil {
		return multiagent.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return multiagent.Result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return multiagent.Result{}, err
	}
	var envelope struct {
		Success bool              `json:"success"`
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Data    multiagent.Result `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return multiagent.Result{}, fmt.Errorf("decode: %w body=%s", err, truncate(string(raw), 200))
	}
	if !envelope.Success {
		return multiagent.Result{}, fmt.Errorf("api %s: %s", envelope.Code, envelope.Message)
	}
	return envelope.Data, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
